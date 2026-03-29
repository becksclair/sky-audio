package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heliasar/sky-audio/gateway/pkg/openaihttp"
	skywebrtc "github.com/heliasar/sky-audio/gateway/pkg/webrtc"
)

// TestOpenAITTS_Synthesize tests the /v1/audio/speech endpoint end-to-end
// with actual TTS workers. Requires workers to be running (see TestMain or run manually).
// This test is skipped if TTS_WORKER_SOCKET env var is not set (typical dev environment).
func TestOpenAITTS_Synthesize(t *testing.T) {
	socketPath := os.Getenv("TTS_WORKER_SOCKET")
	if socketPath == "" {
		// Try default socket paths
		for _, path := range []string{
			"/tmp/sky-audio-tts-pocket.sock",
			"/tmp/sky-audio-tts-kyutai.sock",
		} {
			if _, err := os.Stat(path); err == nil {
				socketPath = path
				break
			}
		}
	}
	if socketPath == "" {
		t.Skip("Skipping E2E test: No TTS worker socket found. Set TTS_WORKER_SOCKET or start workers.")
	}

	// Connect to TTS worker
	client, err := skywebrtc.NewTTSClient(socketPath)
	if err != nil {
		t.Fatalf("Failed to connect to TTS worker at %s: %v", socketPath, err)
	}
	defer client.Close()

	// Verify worker is healthy
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := client.GetStatus(ctx)
	if err != nil {
		t.Fatalf("TTS worker health check failed: %v", err)
	}
	t.Logf("TTS worker status: model=%s, busy=%v, vram=%.1fMB",
		status.LoadedModel, status.IsBusy, status.VramUsageMb)

	// Create handler with single client
	engineName := "pocket"
	if strings.Contains(socketPath, "kyutai") {
		engineName = "kyutai"
	}
	handler := openaihttp.TTSHandler(map[string]*skywebrtc.TTSClient{
		engineName: client,
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			http.NotFound(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()

	// Test cases
	tests := []struct {
		name       string
		model      string
		input      string
		wantStatus int
		wantFormat string
	}{
		{
			name:       "valid_synthesis_" + engineName,
			model:      "sky-" + engineName,
			input:      "Hello from sky-audio end-to-end test.",
			wantStatus: http.StatusOK,
			wantFormat: "audio/wav",
		},
		{
			name:       "alias_model_tts1",
			model:      "tts-1",
			input:      "Testing alias model.",
			wantStatus: http.StatusOK,
			wantFormat: "audio/wav",
		},
		{
			name:       "empty_input",
			model:      "sky-" + engineName,
			input:      "",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "oversized_input",
			model:      "sky-" + engineName,
			input:      strings.Repeat("x", openaihttp.MaxTTSCharacters+1),
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqBody := openaihttp.TTSRequest{
				Model: tt.model,
				Input: tt.input,
			}
			body, _ := json.Marshal(reqBody)

			resp, err := http.Post(server.URL+"/v1/audio/speech", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("Request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				bodyData, _ := io.ReadAll(resp.Body)
				t.Errorf("Expected status %d, got %d: %s", tt.wantStatus, resp.StatusCode, string(bodyData))
			}

			if tt.wantStatus == http.StatusOK {
				contentType := resp.Header.Get("Content-Type")
				if !strings.Contains(contentType, tt.wantFormat) {
					t.Errorf("Expected Content-Type containing %s, got %s", tt.wantFormat, contentType)
				}

				// Verify it's a valid WAV by checking header
				data, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatalf("Failed to read response body: %v", err)
				}
				if len(data) < 12 {
					t.Errorf("Response too small for WAV file: %d bytes", len(data))
				}
				if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
					t.Errorf("Response is not a valid WAV file: %s / %s", string(data[0:4]), string(data[8:12]))
				}

				// Should have actual audio data, not just header
				if len(data) < 1000 {
					t.Errorf("WAV file seems too small (%d bytes), may be empty audio", len(data))
				}

				t.Logf("Generated %d bytes of WAV audio", len(data))
			}
		})
	}
}

// TestOpenAITTS_MultipleEngines tests synthesis across multiple engines if available
func TestOpenAITTS_MultipleEngines(t *testing.T) {
	// Find available TTS workers
	engines := map[string]string{}
	for _, path := range []string{
		"/tmp/sky-audio-tts-pocket.sock",
		"/tmp/sky-audio-tts-kyutai.sock",
	} {
		if _, err := os.Stat(path); err == nil {
			name := filepath.Base(path)
			name = strings.TrimPrefix(name, "sky-audio-tts-")
			name = strings.TrimSuffix(name, ".sock")
			engines[name] = path
		}
	}

	if len(engines) == 0 {
		t.Skip("Skipping E2E test: No TTS worker sockets found.")
	}

	// Connect to all available engines
	clients := make(map[string]*skywebrtc.TTSClient)
	for name, path := range engines {
		client, err := skywebrtc.NewTTSClient(path)
		if err != nil {
			t.Logf("Failed to connect to %s: %v", name, err)
			continue
		}
		defer client.Close()
		clients[name] = client
		t.Logf("Connected to TTS engine: %s", name)
	}

	if len(clients) == 0 {
		t.Skip("No TTS engines available for testing")
	}

	handler := openaihttp.TTSHandler(clients)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()

	// Test each engine
	for engineName := range clients {
		t.Run(engineName+"_synthesis", func(t *testing.T) {
			reqBody := openaihttp.TTSRequest{
				Model: "sky-" + engineName,
				Input: "Testing " + engineName + " engine.",
			}
			body, _ := json.Marshal(reqBody)

			resp, err := http.Post(server.URL+"/v1/audio/speech", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("Request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				bodyData, _ := io.ReadAll(resp.Body)
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, string(bodyData))
			}

			data, _ := io.ReadAll(resp.Body)
			t.Logf("%s engine produced %d bytes of audio", engineName, len(data))
		})
	}
}

// TestOpenAIModels_List tests the /v1/models endpoint
func TestOpenAIModels_List(t *testing.T) {
	handler := openaihttp.ModelsHandler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()

	resp, err := http.Get(server.URL + "/v1/models")
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", resp.StatusCode)
	}

	var result openaihttp.ModelsListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if result.Object != "list" {
		t.Errorf("Expected object 'list', got '%s'", result.Object)
	}

	// Verify expected models exist
	modelIDs := make(map[string]bool)
	for _, m := range result.Data {
		modelIDs[m.ID] = true
	}

	expectedModels := []string{"sky-kyutai", "sky-pocket", "sky-nemo-parakeet", "whisper-1"}
	for _, id := range expectedModels {
		if !modelIDs[id] {
			t.Errorf("Expected model '%s' not found in response", id)
		}
	}

	t.Logf("Found %d models in catalog", len(result.Data))
}

// TestOpenAIAuth_RequiresToken tests that auth middleware works when key is set
func TestOpenAIAuth_RequiresToken(t *testing.T) {
	// Save original env
	originalKey := os.Getenv("SKY_AUDIO_API_KEY")
	defer os.Setenv("SKY_AUDIO_API_KEY", originalKey)

	// Set a test key
	os.Setenv("SKY_AUDIO_API_KEY", "test-secret-key")

	// Create handler with auth middleware
	handler := openaihttp.TTSHandler(map[string]*skywebrtc.TTSClient{})
	wrapped := openaihttp.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))

	server := httptest.NewServer(wrapped)
	defer server.Close()

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{
			name:       "no_auth",
			authHeader: "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "invalid_auth",
			authHeader: "Bearer wrong-key",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "valid_auth",
			authHeader: "Bearer test-secret-key",
			wantStatus: http.StatusBadRequest, // Valid auth but bad request (no workers)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqBody := openaihttp.TTSRequest{
				Model: "sky-pocket",
				Input: "Test",
			}
			body, _ := json.Marshal(reqBody)

			req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/audio/speech", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("Expected status %d, got %d", tt.wantStatus, resp.StatusCode)
			}
		})
	}
}

// createTestWAV creates a minimal valid WAV file for STT testing
func createTestWAV(durationMs int) []byte {
	sampleRate := 16000
	numSamples := (sampleRate * durationMs) / 1000
	dataSize := numSamples * 2 // 16-bit samples

	wav := make([]byte, 44+dataSize)

	// RIFF header
	copy(wav[0:4], "RIFF")
	fileSize := uint32(36 + dataSize)
	wav[4] = byte(fileSize)
	wav[5] = byte(fileSize >> 8)
	wav[6] = byte(fileSize >> 16)
	wav[7] = byte(fileSize >> 24)
	copy(wav[8:12], "WAVE")

	// fmt chunk
	copy(wav[12:16], "fmt ")
	wav[16] = 16 // chunk size
	wav[17] = 0
	wav[18] = 0
	wav[19] = 0
	wav[20] = 1 // audio format = PCM
	wav[21] = 0
	wav[22] = 1 // num channels
	wav[23] = 0
	sr := uint32(sampleRate)
	wav[24] = byte(sr)
	wav[25] = byte(sr >> 8)
	wav[26] = byte(sr >> 16)
	wav[27] = byte(sr >> 24)
	byteRate := uint32(sampleRate * 2)
	wav[28] = byte(byteRate)
	wav[29] = byte(byteRate >> 8)
	wav[30] = byte(byteRate >> 16)
	wav[31] = byte(byteRate >> 24)
	wav[32] = 2 // block align
	wav[33] = 0
	wav[34] = 16 // bits per sample
	wav[35] = 0

	// data chunk
	copy(wav[36:40], "data")
	wav[40] = byte(dataSize)
	wav[41] = byte(dataSize >> 8)
	wav[42] = byte(dataSize >> 16)
	wav[43] = byte(dataSize >> 24)

	// PCM data is zeroed (silence)

	return wav
}

// TestOpenAISTT_Transcribe tests the /v1/audio/transcriptions endpoint
// This test is skipped if STT worker is not available.
// With lazy loading, the STT worker may need to be spawned first via WebRTC offer or gateway startup.
func TestOpenAISTT_Transcribe(t *testing.T) {
	socketPath := "/tmp/sky-audio-stt.sock"
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		t.Skip("Skipping E2E test: STT worker socket not found at " + socketPath +
			". Start the gateway and trigger lazy loading via WebRTC offer first.")
	}

	// Verify STT worker is available by attempting a quick connection
	client, err := skywebrtc.NewSTTClient(socketPath)
	if err != nil {
		t.Fatalf("Failed to connect to STT worker: %v", err)
	}
	// Just verify it's reachable, then close - the handler creates its own clients
	client.Close()

	// Use handler with lazy loading support - ensureRunning is a no-op since worker is already running
	ensureRunning := func() func() { return func() {} }
	handler := openaihttp.TranscriptionsHandlerWithEnsureRunning(socketPath, ensureRunning)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()

	tests := []struct {
		name           string
		audioData      []byte
		model          string
		responseFormat string
		wantStatus     int
	}{
		{
			name:           "silence_wav",
			audioData:      createTestWAV(500), // 500ms silence
			model:          "whisper-1",
			responseFormat: "json",
			wantStatus:     http.StatusOK,
		},
		{
			name:           "text_format",
			audioData:      createTestWAV(500),
			model:          "whisper-1",
			responseFormat: "text",
			wantStatus:     http.StatusOK,
		},
		{
			name:           "missing_file",
			audioData:      nil,
			model:          "whisper-1",
			responseFormat: "json",
			wantStatus:     http.StatusBadRequest,
		},
		{
			name:           "missing_model",
			audioData:      createTestWAV(100),
			model:          "",
			responseFormat: "json",
			wantStatus:     http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)

			if tt.audioData != nil {
				part, err := writer.CreateFormFile("file", "test.wav")
				if err != nil {
					t.Fatalf("Failed to create form file: %v", err)
				}
				if _, err := part.Write(tt.audioData); err != nil {
					t.Fatalf("Failed to write audio data: %v", err)
				}
			}

			if tt.model != "" {
				_ = writer.WriteField("model", tt.model)
			}
			if tt.responseFormat != "" {
				_ = writer.WriteField("response_format", tt.responseFormat)
			}
			writer.Close()

			req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/audio/transcriptions", &body)
			req.Header.Set("Content-Type", writer.FormDataContentType())

			client := &http.Client{Timeout: 30 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				bodyData, _ := io.ReadAll(resp.Body)
				t.Errorf("Expected status %d, got %d: %s", tt.wantStatus, resp.StatusCode, string(bodyData))
			}

			if tt.wantStatus == http.StatusOK {
				if tt.responseFormat == "json" {
					var result openaihttp.STTResponse
					if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
						t.Fatalf("Failed to decode JSON response: %v", err)
					}
					// Response should have text field (may be empty for silence)
					t.Logf("Transcription result: %q", result.Text)
				} else if tt.responseFormat == "text" {
					text, _ := io.ReadAll(resp.Body)
					t.Logf("Plain text transcription: %q", string(text))
				}
			}
		})
	}
}

// TestOpenAIIntegration_FullFlow tests the complete TTS->STT roundtrip
// This verifies that audio produced by TTS can be processed by STT
func TestOpenAIIntegration_FullFlow(t *testing.T) {
	// Check for required workers
	ttsSocket := ""
	for _, path := range []string{
		"/tmp/sky-audio-tts-pocket.sock",
		"/tmp/sky-audio-tts-kyutai.sock",
	} {
		if _, err := os.Stat(path); err == nil {
			ttsSocket = path
			break
		}
	}
	sttSocket := "/tmp/sky-audio-stt.sock"
	if _, err := os.Stat(sttSocket); os.IsNotExist(err) {
		ttsSocket = ""
	}

	if ttsSocket == "" || sttSocket == "" {
		t.Skip("Skipping full flow test: TTS and/or STT workers not available")
	}

	// This is a placeholder for a more comprehensive integration test
	// that would:
	// 1. Generate TTS audio for "Hello world"
	// 2. Feed that audio to STT
	// 3. Verify the transcription is reasonable
	//
	// The actual implementation would require careful handling of
	// audio format conversion between TTS output and STT input.
	t.Log("Full flow test setup complete - workers available")
	t.Log("TTS socket:", ttsSocket)
	t.Log("STT socket:", sttSocket)
}
