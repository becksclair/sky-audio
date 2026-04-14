package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/heliasar/sky-audio/gateway/pkg/pb"
	"github.com/heliasar/sky-audio/gateway/pkg/webrtc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// killCompetingGPUProcesses kills other audio workers that might be using GPU memory.
// This prevents CUDA out-of-memory errors when Kyutai TTS tries to allocate memory.
// Call this before running TTS tests that require GPU when other workers may be running.
func killCompetingGPUProcesses(t *testing.T) {
	t.Helper()

	// Patterns for processes to kill - only GPU-using workers
	// Note: We don't kill pocket-tts as it's CPU-based
	patterns := []string{
		"python.*nemo",
		"python.*stt-worker",
	}

	killedAny := false
	for _, pattern := range patterns {
		cmd := exec.Command("pkill", "-9", "-f", pattern)
		if err := cmd.Run(); err == nil {
			killedAny = true
		}
	}

	if killedAny {
		t.Log("Killed competing GPU processes, waiting for memory release...")
		time.Sleep(2 * time.Second)
	}
}

type ttsVoiceCatalog struct {
	Default string   `json:"default"`
	Voices  []string `json:"voices"`
}

type pocketLocalVoiceOverlay struct {
	Default string   `json:"default"`
	Voices  []string `json:"voices"`
	Preload []string `json:"preload"`
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to locate test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

var (
	pocketLocalVoiceSyncOnce   sync.Once
	pocketLocalVoiceSyncErr    error
	pocketLocalVoiceSyncOutput string
)

func syncPocketLocalVoicesForE2E(t *testing.T) {
	t.Helper()
	pocketLocalVoiceSyncOnce.Do(func() {
		cmd := exec.Command("python3", "workers/python-pocket-tts/sync_custom_voices.py")
		cmd.Dir = repoRoot(t)
		output, err := cmd.CombinedOutput()
		pocketLocalVoiceSyncOutput = strings.TrimSpace(string(output))
		if err != nil {
			pocketLocalVoiceSyncErr = fmt.Errorf("sync local Pocket voices: %w\n%s", err, string(output))
		}
	})
	if pocketLocalVoiceSyncErr != nil {
		t.Fatal(pocketLocalVoiceSyncErr)
	}
	t.Logf("Pocket local voice sync: %s", pocketLocalVoiceSyncOutput)
}

func loadTTSVoiceCatalog(t *testing.T, relativePath string) ttsVoiceCatalog {
	t.Helper()
	catalogPath := filepath.Join(repoRoot(t), relativePath)
	data, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatalf("read voice catalog %s: %v", catalogPath, err)
	}
	var catalog ttsVoiceCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatalf("decode voice catalog %s: %v", catalogPath, err)
	}
	if catalog.Default == "" || len(catalog.Voices) == 0 {
		t.Fatalf("voice catalog %s is empty", catalogPath)
	}
	return catalog
}

func loadPocketLocalVoiceOverlay(t *testing.T) pocketLocalVoiceOverlay {
	t.Helper()
	overlayPath := filepath.Join(repoRoot(t), "workers", "python-pocket-tts", "voices.local.json")
	data, err := os.ReadFile(overlayPath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("Skipping local Pocket overlay test: %s does not exist", overlayPath)
		}
		t.Fatalf("read Pocket local voice overlay %s: %v", overlayPath, err)
	}
	var overlay pocketLocalVoiceOverlay
	if err := json.Unmarshal(data, &overlay); err != nil {
		t.Fatalf("decode Pocket local voice overlay %s: %v", overlayPath, err)
	}
	if len(overlay.Voices) == 0 {
		t.Skipf("Skipping local Pocket overlay test: %s has no voices", overlayPath)
	}
	return overlay
}

func setupPocketLocalOverlayWorker(t *testing.T) (pocketLocalVoiceOverlay, ttsVoiceCatalog, string, *bytes.Buffer) {
	t.Helper()
	syncPocketLocalVoicesForE2E(t)
	overlay := loadPocketLocalVoiceOverlay(t)
	baseCatalog := loadTTSVoiceCatalog(t, "workers/python-pocket-tts/voices.json")
	socketPath, workerLog := startLocalPocketWorkerForOverlayTest(t)
	return overlay, baseCatalog, socketPath, workerLog
}

func pickVoiceSamples(catalog ttsVoiceCatalog, count int) []string {
	if count <= 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, count)
	appendVoice := func(voice string) {
		if len(out) >= count || voice == "" {
			return
		}
		if _, ok := seen[voice]; ok {
			return
		}
		seen[voice] = struct{}{}
		out = append(out, voice)
	}

	appendVoice(catalog.Default)
	for _, idx := range []int{0, len(catalog.Voices) / 2, len(catalog.Voices) - 1} {
		if idx >= 0 && idx < len(catalog.Voices) {
			appendVoice(catalog.Voices[idx])
		}
	}
	for _, voice := range catalog.Voices {
		appendVoice(voice)
		if len(out) >= count {
			break
		}
	}
	return out
}

func allTTSVoicesEnabled() bool {
	return os.Getenv("SKY_AUDIO_E2E_ALL_TTS_VOICES") == "1"
}

func startLocalPocketWorkerForOverlayTest(t *testing.T) (socketPath string, workerLog *bytes.Buffer) {
	t.Helper()

	socketPath = filepath.Join(os.TempDir(), fmt.Sprintf("sky-audio-tts-pocket-e2e-%d.sock", time.Now().UnixNano()))
	workerDir := filepath.Join(repoRoot(t), "workers", "python-pocket-tts")
	pythonPath := filepath.Join(workerDir, ".venv", "bin", "python")

	var cmd *exec.Cmd
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := os.Stat(pythonPath); err == nil {
		cmd = exec.CommandContext(ctx, pythonPath, "main.py")
	} else {
		cmd = exec.CommandContext(ctx, "uv", "run", "python", "main.py")
	}
	cmd.Dir = workerDir
	cmd.Env = append(os.Environ(), "TTS_POCKET_SOCKET="+socketPath)
	logBuf := &bytes.Buffer{}
	cmd.Stdout = logBuf
	cmd.Stderr = logBuf
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start Pocket worker: %v", err)
	}

	waitErr := make(chan error, 1)
	go func() {
		waitErr <- cmd.Wait()
	}()

	t.Cleanup(func() {
		cancel()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-waitErr:
		case <-time.After(10 * time.Second):
		}
		_ = os.Remove(socketPath)
	})

	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-waitErr:
			if err != nil {
				t.Fatalf("Pocket worker exited early: %v\n%s", err, logBuf.String())
			}
			t.Fatalf("Pocket worker exited before readiness check completed:\n%s", logBuf.String())
		default:
		}
		if _, err := os.Stat(socketPath); err == nil {
			client, err := webrtc.NewTTSClient(socketPath)
			if err == nil {
				healthCtx, healthCancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, statusErr := client.GetStatus(healthCtx)
				healthCancel()
				_ = client.Close()
				if statusErr == nil {
					return socketPath, logBuf
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for Pocket worker at %s; logs:\n%s", socketPath, logBuf.String())
	return "", nil
}

func runTTSWorkerDirect(t *testing.T, socketPath, engineName, text, voice string) (int, int32, bool, error) {
	t.Helper()

	if _, err := os.Stat(socketPath); err != nil {
		t.Skipf("%s worker not available at %s: %v", engineName, socketPath, err)
	}

	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial %s: %v", engineName, err)
	}
	defer conn.Close()

	client := pb.NewTextToSpeechClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stream, err := client.SynthesizeStream(ctx)
	if err != nil {
		t.Skipf("SynthesizeStream: %v", err)
	}

	if err := stream.Send(&pb.TextToken{Text: text, IsEos: true, Voice: voice}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	var totalPCM int
	var sampleRate int32
	var gotEOS bool
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			return totalPCM, sampleRate, gotEOS, nil
		}
		if err != nil {
			return totalPCM, sampleRate, gotEOS, err
		}
		if chunk.IsEndOfSpeech {
			gotEOS = true
			sampleRate = chunk.SampleRate
			return totalPCM, sampleRate, gotEOS, nil
		}
		totalPCM += len(chunk.PcmData)
		sampleRate = chunk.SampleRate
	}
}

func assertTTSVoiceAccepted(t *testing.T, socketPath, engineName, voice string) {
	t.Helper()
	totalPCM, sampleRate, gotEOS, err := runTTSWorkerDirect(t, socketPath, engineName, "", voice)
	if err != nil {
		t.Fatalf("voice %q rejected: %v", voice, err)
	}
	if totalPCM != 0 {
		t.Fatalf("voice %q returned %d PCM bytes for empty text, want 0", voice, totalPCM)
	}
	if sampleRate == 0 {
		t.Fatalf("voice %q returned zero sample rate", voice)
	}
	if !gotEOS {
		t.Fatalf("voice %q did not end stream", voice)
	}
}

func assertTTSVoiceSynthesizes(t *testing.T, socketPath, engineName, voice string) {
	t.Helper()
	totalPCM, sampleRate, gotEOS, err := runTTSWorkerDirect(
		t,
		socketPath,
		engineName,
		"Hello, this is a test.",
		voice,
	)
	if err != nil {
		t.Fatalf("voice %q synth failed: %v", voice, err)
	}
	if totalPCM == 0 {
		t.Fatal("Expected non-empty PCM audio")
	}
	if sampleRate == 0 {
		t.Fatal("Expected non-zero sample rate")
	}
	if !gotEOS {
		t.Error("Expected is_end_of_speech=true")
	}

	durationSec := float64(totalPCM) / float64(sampleRate*2)
	t.Logf("[%s] voice=%s PCM=%d sample_rate=%d duration=%.2fs", engineName, voice, totalPCM, sampleRate, durationSec)
	if durationSec < 0.1 {
		t.Errorf("Audio too short: %.2fs", durationSec)
	}
}

func assertTTSVoiceRejected(t *testing.T, socketPath, engineName, voice string) {
	t.Helper()
	_, _, _, err := runTTSWorkerDirect(t, socketPath, engineName, "hello", voice)
	if err == nil {
		t.Fatalf("voice %q unexpectedly succeeded", voice)
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("voice %q error code = %s, want %s (%v)", voice, status.Code(err), codes.InvalidArgument, err)
	}
}

func extractWarmedVoicesFromLog(t *testing.T, logOutput string) []string {
	t.Helper()
	const marker = "warmed="
	start := strings.LastIndex(logOutput, marker)
	if start == -1 {
		t.Fatalf("worker log missing warmed marker:\n%s", logOutput)
	}
	start += len(marker)
	end := strings.Index(logOutput[start:], ")")
	if end == -1 {
		t.Fatalf("worker log missing warmed terminator:\n%s", logOutput)
	}
	raw := strings.TrimSpace(logOutput[start : start+end])
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	voices := make([]string, 0, len(parts))
	for _, part := range parts {
		voice := strings.TrimSpace(part)
		if voice != "" {
			voices = append(voices, voice)
		}
	}
	return voices
}

func TestTTSWorkerDirectPocket(t *testing.T) {
	assertTTSVoiceSynthesizes(t, "/tmp/sky-audio-tts-pocket.sock", "PocketTTS", "")
}

func TestTTSWorkerDirectKyutai(t *testing.T) {
	killCompetingGPUProcesses(t)
	assertTTSVoiceSynthesizes(t, "/tmp/sky-audio-tts-kyutai.sock", "KyutaiTTS", "")
}

func TestTTSWorkerPocketVoiceCatalog(t *testing.T) {
	catalog := loadTTSVoiceCatalog(t, "workers/python-pocket-tts/voices.json")
	voices := pickVoiceSamples(catalog, 4)
	if allTTSVoicesEnabled() {
		voices = catalog.Voices
	}
	for _, voice := range voices {
		voice := voice
		t.Run(voice, func(t *testing.T) {
			assertTTSVoiceAccepted(t, "/tmp/sky-audio-tts-pocket.sock", "PocketTTS", voice)
		})
	}
}

func TestTTSWorkerKyutaiVoiceCatalog(t *testing.T) {
	catalog := loadTTSVoiceCatalog(t, "workers/python-kyutai-tts/voices.json")
	voices := pickVoiceSamples(catalog, 4)
	if allTTSVoicesEnabled() {
		voices = catalog.Voices
	}
	for _, voice := range voices {
		voice := voice
		t.Run(voice, func(t *testing.T) {
			assertTTSVoiceAccepted(t, "/tmp/sky-audio-tts-kyutai.sock", "KyutaiTTS", voice)
		})
	}
}

func TestTTSWorkerPocketVoiceSynthesisSubset(t *testing.T) {
	catalog := loadTTSVoiceCatalog(t, "workers/python-pocket-tts/voices.json")
	for _, voice := range pickVoiceSamples(catalog, 3) {
		voice := voice
		t.Run(voice, func(t *testing.T) {
			assertTTSVoiceSynthesizes(t, "/tmp/sky-audio-tts-pocket.sock", "PocketTTS", voice)
		})
	}
}

func TestTTSWorkerKyutaiVoiceSynthesisSubset(t *testing.T) {
	killCompetingGPUProcesses(t)
	catalog := loadTTSVoiceCatalog(t, "workers/python-kyutai-tts/voices.json")
	for _, voice := range pickVoiceSamples(catalog, 3) {
		voice := voice
		t.Run(voice, func(t *testing.T) {
			assertTTSVoiceSynthesizes(t, "/tmp/sky-audio-tts-kyutai.sock", "KyutaiTTS", voice)
		})
	}
}

func TestTTSWorkerPocketRejectsUnknownVoice(t *testing.T) {
	assertTTSVoiceRejected(t, "/tmp/sky-audio-tts-pocket.sock", "PocketTTS", "not-a-real-pocket-voice")
}

func TestTTSWorkerKyutaiRejectsUnknownVoice(t *testing.T) {
	assertTTSVoiceRejected(t, "/tmp/sky-audio-tts-kyutai.sock", "KyutaiTTS", "not/a/real/kyutai.wav")
}

func TestTTSWorkerPocketLocalOverlayVoicesSynthesize(t *testing.T) {
	overlay, _, socketPath, _ := setupPocketLocalOverlayWorker(t)

	for _, voice := range overlay.Voices {
		voice := voice
		t.Run(voice, func(t *testing.T) {
			assertTTSVoiceSynthesizes(t, socketPath, "PocketTTS-local", voice)
		})
	}
}

func TestTTSWorkerPocketLocalOverlayVoicesPreloadedOnStartup(t *testing.T) {
	overlay, baseCatalog, socketPath, workerLog := setupPocketLocalOverlayWorker(t)

	// Exercise one cloned voice after startup so the test proves both the preload evidence
	// and a successful first synthesis against the fresh worker.
	assertTTSVoiceSynthesizes(t, socketPath, "PocketTTS-local", overlay.Voices[0])

	logOutput := workerLog.String()
	warmedVoices := extractWarmedVoicesFromLog(t, logOutput)
	expectedDefault := strings.TrimSpace(overlay.Default)
	if expectedDefault == "" {
		expectedDefault = baseCatalog.Default
	}
	expectedWarmed := map[string]struct{}{expectedDefault: {}}
	for _, voice := range overlay.Preload {
		expectedWarmed[voice] = struct{}{}
	}

	if !strings.Contains(logOutput, fmt.Sprintf("local_catalog=%d", len(overlay.Voices))) {
		t.Fatalf("worker log did not report expected local catalog size %d:\n%s", len(overlay.Voices), logOutput)
	}
	if !strings.Contains(logOutput, fmt.Sprintf("preload=%d", len(overlay.Preload))) {
		t.Fatalf("worker log did not report expected preload size %d:\n%s", len(overlay.Preload), logOutput)
	}
	if !strings.Contains(logOutput, fmt.Sprintf("cache=%d", len(expectedWarmed))) {
		t.Fatalf("worker log did not report expected warmed cache size %d:\n%s", len(expectedWarmed), logOutput)
	}
	if len(warmedVoices) != len(expectedWarmed) {
		t.Fatalf("unexpected warmed voice count %d, want %d:\n%s", len(warmedVoices), len(expectedWarmed), logOutput)
	}
	for _, voice := range warmedVoices {
		if _, ok := expectedWarmed[voice]; !ok {
			t.Fatalf("worker warmed unexpected voice %q:\n%s", voice, logOutput)
		}
	}
	for voice := range expectedWarmed {
		if !strings.Contains(logOutput, voice) {
			t.Fatalf("worker log did not include expected warmed voice %q:\n%s", voice, logOutput)
		}
	}
}
