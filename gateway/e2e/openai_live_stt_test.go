package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/heliasar/sky-audio/gateway/pkg/openaihttp"
	skywebrtc "github.com/heliasar/sky-audio/gateway/pkg/webrtc"
)

type openAILiveSTTBaseline struct {
	Fixture                  string  `json:"fixture"`
	Model                    string  `json:"model"`
	ExpectedFirstWord        string  `json:"expected_first_word"`
	ExpectedTranscript       string  `json:"expected_transcript"`
	ExpectedDurationSeconds  float64 `json:"expected_duration_seconds"`
	DurationToleranceSeconds float64 `json:"duration_tolerance_seconds"`
	MinWordCount             int     `json:"min_word_count"`
}

type sttRequestMetrics struct {
	headersElapsed time.Duration
	totalElapsed   time.Duration
	uploadBytes    int
	responseBytes  int
}

func TestSTTPipelineMatchesSavedOpenAIBaseline(t *testing.T) {
	socketPath := startLocalSTTWorkerForBaselineTest(t)
	baseline := loadOpenAIBaseline(t)

	ensureRunning := func() func() { return func() {} }
	server := httptest.NewServer(openaihttp.TranscriptionsHandlerWithEnsureRunning(socketPath, ensureRunning))
	defer server.Close()

	result, metrics := transcribeFixtureViaLocalSTT(t, server.URL, baseline)

	t.Run("first_word_decoding", func(t *testing.T) {
		firstWord := firstToken(result.Text)
		if normalizeToken(firstWord) != normalizeToken(baseline.ExpectedFirstWord) {
			t.Fatalf("first word mismatch: got %q want %q", firstWord, baseline.ExpectedFirstWord)
		}
	})

	t.Run("full_sentence_transcription", func(t *testing.T) {
		got := normalizeTranscript(result.Text)
		want := normalizeTranscript(baseline.ExpectedTranscript)
		if got != want {
			t.Fatalf("transcript mismatch:\n  got: %q\n want: %q", result.Text, baseline.ExpectedTranscript)
		}
	})

	t.Run("timing_and_stats", func(t *testing.T) {
		if metrics.headersElapsed <= 0 || metrics.totalElapsed <= 0 {
			t.Fatalf("invalid timings: headers_elapsed=%s total_elapsed=%s", metrics.headersElapsed, metrics.totalElapsed)
		}
		if result.Duration <= 0 {
			t.Fatalf("invalid duration: %f", result.Duration)
		}
		durationDelta := absFloat(result.Duration - baseline.ExpectedDurationSeconds)
		if durationDelta > baseline.DurationToleranceSeconds {
			t.Fatalf("duration delta too large: got=%0.3fs want=%0.3fs tolerance=%0.3fs", result.Duration, baseline.ExpectedDurationSeconds, baseline.DurationToleranceSeconds)
		}

		wordCount := countTranscriptWords(result.Text)
		if wordCount < baseline.MinWordCount {
			t.Fatalf("word count too low: got=%d want>=%d", wordCount, baseline.MinWordCount)
		}
		if strings.TrimSpace(result.Text) == "" {
			t.Fatal("expected non-empty transcript")
		}

		shared := sharedTranscriptTokens(result.Text, baseline.ExpectedTranscript)
		t.Logf("fixture=%s model=%s transcript_words=%d shared_tokens=%d duration=%0.3fs duration_delta=%0.3fs ttfb=%s total=%s upload_bytes=%d response_bytes=%d transcript=%q",
			baseline.Fixture,
			baseline.Model,
			wordCount,
			len(shared),
			result.Duration,
			durationDelta,
			metrics.headersElapsed,
			metrics.totalElapsed,
			metrics.uploadBytes,
			metrics.responseBytes,
			result.Text,
		)
	})
}

func transcribeFixtureViaLocalSTT(t *testing.T, serverURL string, baseline openAILiveSTTBaseline) (openaihttp.STTVerboseResponse, sttRequestMetrics) {
	t.Helper()
	fixturePath, normalizedWAV, _ := loadRealtimeFixture(t)
	if filepath.Base(fixturePath) != baseline.Fixture {
		t.Fatalf("fixture mismatch: baseline expects %q but loaded %q", baseline.Fixture, filepath.Base(fixturePath))
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filepath.Base(fixturePath))
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(normalizedWAV); err != nil {
		t.Fatalf("write fixture data: %v", err)
	}
	if err := writer.WriteField("model", baseline.Model); err != nil {
		t.Fatalf("write model: %v", err)
	}
	if err := writer.WriteField("response_format", "verbose_json"); err != nil {
		t.Fatalf("write response_format: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, serverURL+"/v1/audio/transcriptions", &body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{Timeout: 120 * time.Second}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("local STT transcription request failed: %v", err)
	}
	headersElapsed := time.Since(started)
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	totalElapsed := time.Since(started)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("local STT transcription status %d: %s", resp.StatusCode, string(respBody))
	}

	var result openaihttp.STTVerboseResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		t.Fatalf("decode verbose_json response: %v", err)
	}

	return result, sttRequestMetrics{
		headersElapsed: headersElapsed,
		totalElapsed:   totalElapsed,
		uploadBytes:    len(normalizedWAV),
		responseBytes:  len(respBody),
	}
}

func loadOpenAIBaseline(t *testing.T) openAILiveSTTBaseline {
	t.Helper()
	baselinePath := filepath.Join(repoRoot(t), "gateway", "e2e", "testdata", "sky_voice_test.openai-baseline.json")
	baselineData, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}

	var baseline openAILiveSTTBaseline
	if err := json.Unmarshal(baselineData, &baseline); err != nil {
		t.Fatalf("decode baseline: %v", err)
	}
	return baseline
}

func startLocalSTTWorkerForBaselineTest(t *testing.T) string {
	t.Helper()

	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("sky-audio-stt-baseline-%d.sock", time.Now().UnixNano()))
	workerDir := filepath.Join(repoRoot(t), "workers", "python-nemo")
	workerDevice := strings.TrimSpace(os.Getenv("SKY_AUDIO_E2E_STT_DEVICE"))
	if workerDevice == "" {
		workerDevice = "cpu"
	}

	pythonPath := filepath.Join(workerDir, ".venv", "bin", "python")
	if _, err := os.Stat(pythonPath); err != nil {
		t.Fatalf("python worker runtime not found at %s: %v", pythonPath, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, pythonPath, "main.py")
	cmd.Dir = workerDir
	cmd.Env = append(os.Environ(), "STT_SOCKET="+socketPath, "STT_DEVICE="+workerDevice)
	var workerLog bytes.Buffer
	cmd.Stdout = &workerLog
	cmd.Stderr = &workerLog
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start STT worker: %v", err)
	}

	t.Cleanup(func() {
		cancel()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
		_ = os.Remove(socketPath)
	})

	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			t.Fatalf("STT worker exited early: %s", workerLog.String())
		}
		if _, err := os.Stat(socketPath); err == nil {
			client, err := skywebrtc.NewSTTClient(socketPath)
			if err == nil {
				healthCtx, healthCancel := context.WithTimeout(context.Background(), 5*time.Second)
				streamErr := client.StartStream(healthCtx)
				healthCancel()
				_ = client.Close()
				if streamErr == nil {
					return socketPath
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for STT worker at %s; logs:\n%s", socketPath, workerLog.String())
	return ""
}

func firstToken(text string) string {
	for _, token := range strings.Fields(text) {
		normalized := normalizeToken(token)
		if normalized != "" {
			return normalized
		}
	}
	return ""
}

func countTranscriptWords(text string) int {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	count := 0
	for _, word := range words {
		if word != "" {
			count++
		}
	}
	return count
}

func normalizeToken(token string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(token) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func normalizeTranscript(text string) string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	if len(words) == 0 {
		return ""
	}
	return strings.Join(words, " ")
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
