package e2e

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	pb "github.com/heliasar/sky-audio/gateway/pkg/pb"
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

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to locate test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
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
