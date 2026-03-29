package openaihttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockTTSSynthesizer implements TTSSynthesizer for testing
type mockTTSSynthesizer struct {
	shouldFail bool
	lastVoice  string
	lastText   string
}

func (m *mockTTSSynthesizer) Synthesize(ctx context.Context, text, voice string) ([]byte, int32, error) {
	if m.shouldFail {
		return nil, 0, errors.New("synthesis failed")
	}
	m.lastText = text
	m.lastVoice = voice
	// Return fake PCM data (sine wave-ish pattern)
	pcm := make([]byte, 32000) // 1 second at 16kHz mono16
	for i := range pcm {
		pcm[i] = byte(i % 256)
	}
	return pcm, 16000, nil
}

func TestTTSHandlerWithSynthRejectsOversizedBody(t *testing.T) {
	handler := TTSHandlerWithSynth(map[string]TTSSynthesizer{})

	// Create a request with body larger than MaxTTSRequestBytes
	largeBody := make([]byte, MaxTTSRequestBytes+1000)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(largeBody))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// Should reject with 4xx error (may be 400 or 413 depending on reader behavior)
	if rr.Code < 400 || rr.Code >= 500 {
		t.Errorf("Expected 4xx status for oversized body, got %d", rr.Code)
	}
}

func TestTTSHandlerWithSynthRejectsInvalidJSON(t *testing.T) {
	handler := TTSHandlerWithSynth(map[string]TTSSynthesizer{})

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status %d, got %d", http.StatusBadRequest, rr.Code)
	}

	body := rr.Body.String()
	if !strings.Contains(body, "error") {
		t.Errorf("Expected error response, got: %s", body)
	}
}

func TestTTSHandlerWithSynthRejectsEmptyText(t *testing.T) {
	handler := TTSHandlerWithSynth(map[string]TTSSynthesizer{})

	reqBody := TTSRequest{Model: "sky-pocket", Input: "   "}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status %d, got %d", http.StatusBadRequest, rr.Code)
	}
}

func TestTTSHandlerWithSynthRejectsTextTooLong(t *testing.T) {
	handler := TTSHandlerWithSynth(map[string]TTSSynthesizer{})

	reqBody := TTSRequest{
		Model: "sky-pocket",
		Input: strings.Repeat("a", MaxTTSCharacters+1),
	}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status %d, got %d", http.StatusBadRequest, rr.Code)
	}
}

func TestTTSHandlerWithSynthRejectsUnknownModel(t *testing.T) {
	mock := &mockTTSSynthesizer{}
	handler := TTSHandlerWithSynth(map[string]TTSSynthesizer{
		"pocket": mock,
	})

	// Unknown model should return error with helpful message
	reqBody := TTSRequest{Model: "unknown-engine", Input: "Hello world"}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// Should return 400 with error about unknown model
	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status %d for unknown model, got %d: %s", http.StatusBadRequest, rr.Code, rr.Body.String())
	}

	// Verify error message mentions available models
	respBody := rr.Body.String()
	if !strings.Contains(respBody, "unknown model") {
		t.Errorf("Expected error to mention 'unknown model', got: %s", respBody)
	}

	// Verify synthesizer was NOT called
	if mock.lastText != "" {
		t.Errorf("Synthesizer should not have been called for unknown model, but got: '%s'", mock.lastText)
	}
}

func TestTTSHandlerWithSynthHandlesSynthesizeError(t *testing.T) {
	handler := TTSHandlerWithSynth(map[string]TTSSynthesizer{
		"pocket": &mockTTSSynthesizer{shouldFail: true},
	})

	reqBody := TTSRequest{Model: "sky-pocket", Input: "Hello world"}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("Expected status %d, got %d", http.StatusInternalServerError, rr.Code)
	}
}

func TestTTSHandlerWithSynthUsesDefaultEngineAndWritesWAV(t *testing.T) {
	mock := &mockTTSSynthesizer{}
	handler := TTSHandlerWithSynth(map[string]TTSSynthesizer{
		"pocket": mock,
	})

	// Test explicitly requesting WAV format (backward compatibility)
	reqBody := TTSRequest{Model: "sky-pocket", Input: "Hello world", ResponseFormat: "wav"}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
	}

	// Check content type
	contentType := rr.Header().Get("Content-Type")
	if contentType != "audio/wav" {
		t.Errorf("Expected Content-Type 'audio/wav', got '%s'", contentType)
	}

	// Check that synthesizer was called with correct text
	if mock.lastText != "Hello world" {
		t.Errorf("Synthesizer should not have been called for unknown model, got '%s'", mock.lastText)
	}

	// Verify WAV header
	bodyData := rr.Body.Bytes()
	if len(bodyData) < 12 {
		t.Fatalf("Response body too short for WAV file: %d bytes", len(bodyData))
	}

	// Check RIFF header
	if string(bodyData[0:4]) != "RIFF" {
		t.Errorf("WAV file does not start with RIFF header: %s", string(bodyData[0:4]))
	}

	// Check WAVE format
	if string(bodyData[8:12]) != "WAVE" {
		t.Errorf("WAV file does not have WAVE format: %s", string(bodyData[8:12]))
	}
}

func TestTTSHandlerWithSynthForwardsVoice(t *testing.T) {
	mock := &mockTTSSynthesizer{}
	handler := TTSHandlerWithSynth(map[string]TTSSynthesizer{
		"kyutai": mock,
	})

	reqBody := TTSRequest{Model: "sky-kyutai", Input: "Hello world", Voice: "custom-voice-1"}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
	}

	if mock.lastVoice != "custom-voice-1" {
		t.Errorf("Expected voice 'custom-voice-1', got '%s'", mock.lastVoice)
	}
}

func TestModelsHandlerReturnsList(t *testing.T) {
	handler := ModelsHandler()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
	}

	contentType := rr.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("Expected Content-Type 'application/json', got '%s'", contentType)
	}

	var resp ModelsListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if resp.Object != "list" {
		t.Errorf("Expected object 'list', got '%s'", resp.Object)
	}

	if len(resp.Data) == 0 {
		t.Error("Expected non-empty model list")
	}

	// Check for expected models
	foundKyutai := false
	foundPocket := false
	for _, m := range resp.Data {
		if m.ID == "sky-kyutai" {
			foundKyutai = true
		}
		if m.ID == "sky-pocket" {
			foundPocket = true
		}
	}

	if !foundKyutai {
		t.Error("Expected to find 'sky-kyutai' in model list")
	}
	if !foundPocket {
		t.Error("Expected to find 'sky-pocket' in model list")
	}
}

func TestModelsHandlerRejectsPost(t *testing.T) {
	handler := ModelsHandler()

	req := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status %d, got %d", http.StatusMethodNotAllowed, rr.Code)
	}
}

// mockSTTTranscriber implements STTTranscriber for testing
type mockSTTTranscriber struct {
	shouldFail     bool
	lastAudio      []byte
	lastSampleRate int32
}

func (m *mockSTTTranscriber) Transcribe(ctx context.Context, audioData []byte, sampleRate int32) (string, error) {
	if m.shouldFail {
		return "", errors.New("transcription failed")
	}
	m.lastAudio = audioData
	m.lastSampleRate = sampleRate
	return "Hello, this is a test transcription.", nil
}

func TestTranscriptionsHandlerRequiresFile(t *testing.T) {
	handler := TranscriptionsHandlerWithTranscriber(&mockSTTTranscriber{})

	// Request without file
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status %d, got %d", http.StatusBadRequest, rr.Code)
	}
}

func TestTranscriptionsHandlerReturnsJSON(t *testing.T) {
	mock := &mockSTTTranscriber{}
	handler := TranscriptionsHandlerWithTranscriber(mock)

	// Create a simple WAV file
	wavData := createMinimalWAV()

	// Build multipart form
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model", "whisper-1")
	part, _ := writer.CreateFormFile("file", "test.wav")
	part.Write(wavData)
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d: %s", http.StatusOK, rr.Code, rr.Body.String())
	}

	var resp STTResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if resp.Text != "Hello, this is a test transcription." {
		t.Errorf("Expected specific text, got: %s", resp.Text)
	}
}

func TestTranscriptionsHandlerSupportsTextFormat(t *testing.T) {
	mock := &mockSTTTranscriber{}
	handler := TranscriptionsHandlerWithTranscriber(mock)

	// Create a simple WAV file
	wavData := createMinimalWAV()

	// Build multipart form with text format
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model", "whisper-1")
	_ = writer.WriteField("response_format", "text")
	part, _ := writer.CreateFormFile("file", "test.wav")
	part.Write(wavData)
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
	}

	contentType := rr.Header().Get("Content-Type")
	if contentType != "text/plain" {
		t.Errorf("Expected Content-Type 'text/plain', got '%s'", contentType)
	}

	bodyStr := rr.Body.String()
	if bodyStr != "Hello, this is a test transcription." {
		t.Errorf("Expected plain text response, got: %s", bodyStr)
	}
}

func TestTranscriptionsHandlerRejectsGet(t *testing.T) {
	handler := TranscriptionsHandlerWithTranscriber(&mockSTTTranscriber{})

	req := httptest.NewRequest(http.MethodGet, "/v1/audio/transcriptions", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status %d, got %d", http.StatusMethodNotAllowed, rr.Code)
	}
}

func TestTranscriptionsHandlerHandlesError(t *testing.T) {
	mock := &mockSTTTranscriber{shouldFail: true}
	handler := TranscriptionsHandlerWithTranscriber(mock)

	// Create a simple WAV file
	wavData := createMinimalWAV()

	// Build multipart form
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model", "whisper-1")
	part, _ := writer.CreateFormFile("file", "test.wav")
	part.Write(wavData)
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("Expected status %d, got %d", http.StatusInternalServerError, rr.Code)
	}
}

// createMinimalWAV creates a minimal valid WAV file for testing
func createMinimalWAV() []byte {
	// RIFF header + WAVE + fmt chunk + data chunk
	// Minimal mono 16-bit 16kHz 0.1 second silence
	data := make([]byte, 44+3200) // 44 byte header + 0.1s at 16kHz mono16

	// RIFF header
	copy(data[0:4], []byte("RIFF"))
	fileSize := uint32(len(data) - 8)
	data[4] = byte(fileSize)
	data[5] = byte(fileSize >> 8)
	data[6] = byte(fileSize >> 16)
	data[7] = byte(fileSize >> 24)
	copy(data[8:12], []byte("WAVE"))

	// fmt chunk
	copy(data[12:16], []byte("fmt "))
	data[16] = 16 // chunk size
	data[17] = 0
	data[18] = 0
	data[19] = 0
	data[20] = 1 // audio format = PCM
	data[21] = 0
	data[22] = 1 // num channels = 1
	data[23] = 0
	sampleRate := uint32(16000)
	data[24] = byte(sampleRate)
	data[25] = byte(sampleRate >> 8)
	data[26] = byte(sampleRate >> 16)
	data[27] = byte(sampleRate >> 24)
	byteRate := uint32(32000) // 16000 * 2 bytes * 1 channel
	data[28] = byte(byteRate)
	data[29] = byte(byteRate >> 8)
	data[30] = byte(byteRate >> 16)
	data[31] = byte(byteRate >> 24)
	data[32] = 2 // block align
	data[33] = 0
	data[34] = 16 // bits per sample
	data[35] = 0

	// data chunk
	copy(data[36:40], []byte("data"))
	dataSize := uint32(len(data) - 44)
	data[40] = byte(dataSize)
	data[41] = byte(dataSize >> 8)
	data[42] = byte(dataSize >> 16)
	data[43] = byte(dataSize >> 24)

	// PCM data is already zero (silence)

	return data
}

// createWAVWithSampleRate creates a WAV file with a specific sample rate for testing
func createWAVWithSampleRate(sampleRate uint32) []byte {
	// RIFF header + WAVE + fmt chunk + data chunk
	data := make([]byte, 44+3200)

	// RIFF header
	copy(data[0:4], []byte("RIFF"))
	fileSize := uint32(len(data) - 8)
	data[4] = byte(fileSize)
	data[5] = byte(fileSize >> 8)
	data[6] = byte(fileSize >> 16)
	data[7] = byte(fileSize >> 24)
	copy(data[8:12], []byte("WAVE"))

	// fmt chunk
	copy(data[12:16], []byte("fmt "))
	data[16] = 16 // chunk size
	data[17] = 0
	data[18] = 0
	data[19] = 0
	data[20] = 1 // audio format = PCM
	data[21] = 0
	data[22] = 1 // num channels = 1
	data[23] = 0
	data[24] = byte(sampleRate)
	data[25] = byte(sampleRate >> 8)
	data[26] = byte(sampleRate >> 16)
	data[27] = byte(sampleRate >> 24)
	byteRate := sampleRate * 2 // 16-bit mono
	data[28] = byte(byteRate)
	data[29] = byte(byteRate >> 8)
	data[30] = byte(byteRate >> 16)
	data[31] = byte(byteRate >> 24)
	data[32] = 2 // block align
	data[33] = 0
	data[34] = 16 // bits per sample
	data[35] = 0

	// data chunk
	copy(data[36:40], []byte("data"))
	dataSize := uint32(len(data) - 44)
	data[40] = byte(dataSize)
	data[41] = byte(dataSize >> 8)
	data[42] = byte(dataSize >> 16)
	data[43] = byte(dataSize >> 24)

	return data
}

// TestModelsRetrieveHandler tests the GET /v1/models/{model} endpoint
func TestModelsRetrieveHandler(t *testing.T) {
	handler := ModelsRetrieveHandler()

	tests := []struct {
		name       string
		modelID    string
		wantStatus int
		wantID     string
	}{
		{
			name:       "valid_model_sky_kyutai",
			modelID:    "sky-kyutai",
			wantStatus: http.StatusOK,
			wantID:     "sky-kyutai",
		},
		{
			name:       "valid_model_whisper",
			modelID:    "whisper-1",
			wantStatus: http.StatusOK,
			wantID:     "whisper-1",
		},
		{
			name:       "invalid_model",
			modelID:    "nonexistent-model",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/models/"+tt.modelID, nil)
			req.SetPathValue("model", tt.modelID)
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rr.Code, tt.wantStatus)
			}

			if tt.wantStatus == http.StatusOK {
				var model Model
				if err := json.Unmarshal(rr.Body.Bytes(), &model); err != nil {
					t.Fatalf("Unmarshal: %v", err)
				}
				if model.ID != tt.wantID {
					t.Errorf("model.ID = %s, want %s", model.ID, tt.wantID)
				}
			}
		})
	}
}

func TestModelsRetrieveHandlerMethodNotAllowed(t *testing.T) {
	handler := ModelsRetrieveHandler()

	req := httptest.NewRequest(http.MethodPost, "/v1/models/sky-kyutai", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusMethodNotAllowed)
	}
}

// TestOptionalAuthMiddleware tests the authentication middleware
func TestOptionalAuthMiddleware(t *testing.T) {
	// Create a simple handler that returns 200
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	})

	tests := []struct {
		name           string
		apiKey         string
		authHeader     string
		wantStatus     int
		wantMiddleware bool
	}{
		{
			name:       "no_auth_required",
			apiKey:     "",
			authHeader: "",
			wantStatus: http.StatusOK,
		},
		{
			name:       "valid_token",
			apiKey:     "secret-key",
			authHeader: "Bearer secret-key",
			wantStatus: http.StatusOK,
		},
		{
			name:       "missing_auth_header",
			apiKey:     "secret-key",
			authHeader: "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "invalid_token",
			apiKey:     "secret-key",
			authHeader: "Bearer wrong-key",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "malformed_auth_header",
			apiKey:     "secret-key",
			authHeader: "Basic secret-key",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set env var for this test
			if tt.apiKey != "" {
				t.Setenv("SKY_AUDIO_API_KEY", tt.apiKey)
			} else {
				t.Setenv("SKY_AUDIO_API_KEY", "")
			}

			// Create new middleware with current env
			middleware := OptionalAuthMiddleware(testHandler)

			req := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rr := httptest.NewRecorder()

			middleware.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d, body: %s", rr.Code, tt.wantStatus, rr.Body.String())
			}
		})
	}
}

func TestOptionalAuthMiddlewareHealthCheckBypass(t *testing.T) {
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Setenv("SKY_AUDIO_API_KEY", "secret-key")
	middleware := OptionalAuthMiddleware(testHandler)

	// Health check should bypass auth
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	rr := httptest.NewRecorder()

	middleware.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("health check bypass failed: status = %d, want %d", rr.Code, http.StatusOK)
	}
}

// TestOptionalAuthMiddlewareQueryParam verifies query parameter token auth works
func TestOptionalAuthMiddlewareQueryParam(t *testing.T) {
	t.Setenv("SKY_AUDIO_API_KEY", "test-api-key")

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	middleware := OptionalAuthMiddleware(inner)

	// Request with valid token in query parameter (for EventSource/SSE)
	req := httptest.NewRequest(http.MethodGet, "/api/endpoint?token=test-api-key", nil)
	rr := httptest.NewRecorder()

	middleware.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("query param auth failed: status = %d, want %d", rr.Code, http.StatusOK)
	}

	// Request with invalid token in query parameter
	req = httptest.NewRequest(http.MethodGet, "/api/endpoint?token=wrong-key", nil)
	rr = httptest.NewRecorder()

	middleware.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("invalid query param should fail: status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}

	// Request without token should fail
	req = httptest.NewRequest(http.MethodGet, "/api/endpoint", nil)
	rr = httptest.NewRecorder()

	middleware.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("missing token should fail: status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
}

// TestOptionalAuthMiddlewareHeaderPrecedence verifies header takes precedence over query param
func TestOptionalAuthMiddlewareHeaderPrecedence(t *testing.T) {
	t.Setenv("SKY_AUDIO_API_KEY", "test-api-key")

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	middleware := OptionalAuthMiddleware(inner)

	// Request with valid header and invalid query param (header wins)
	req := httptest.NewRequest(http.MethodGet, "/api/endpoint?token=wrong-key", nil)
	req.Header.Set("Authorization", "Bearer test-api-key")
	rr := httptest.NewRecorder()

	middleware.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("header should take precedence: status = %d, want %d", rr.Code, http.StatusOK)
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
	byteRate := uint32(sampleRate * 2) // 16-bit mono
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

// TestTranscriptionsHandlerWithEnsureRunning verifies lazy loading callback integration
func TestTranscriptionsHandlerWithEnsureRunning(t *testing.T) {
	ensureCalled := false
	cleanupCalled := false

	ensureRunning := func() func() {
		ensureCalled = true
		return func() {
			cleanupCalled = true
		}
	}

	handler := TranscriptionsHandlerWithEnsureRunning("/tmp/test.sock", ensureRunning)

	// Create multipart request with audio
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "test.wav")
	part.Write(createTestWAV(100))
	writer.WriteField("model", "whisper-1")
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if !ensureCalled {
		t.Error("ensureRunning was not called")
	}
	if !cleanupCalled {
		t.Error("cleanup was not called")
	}
}

// TestTranscriptionsHandlerWithEnsureRunningUnavailable verifies 503 when STT unavailable
func TestTranscriptionsHandlerWithEnsureRunningUnavailable(t *testing.T) {
	ensureRunning := func() func() {
		return nil // STT unavailable
	}

	handler := TranscriptionsHandlerWithEnsureRunning("/tmp/test.sock", ensureRunning)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "test.wav")
	part.Write(createTestWAV(100))
	writer.WriteField("model", "whisper-1")
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when STT unavailable, got %d", rr.Code)
	}
}
