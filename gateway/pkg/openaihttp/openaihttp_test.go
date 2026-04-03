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

	skywebrtc "github.com/heliasar/sky-audio/gateway/pkg/webrtc"
)

// mockTTSSynthesizer implements TTSSynthesizer for testing
type mockTTSSynthesizer struct {
	shouldFail bool
	lastVoice  string
	lastText   string
	lastOpts   *skywebrtc.SynthesisOptions
}

func (m *mockTTSSynthesizer) Synthesize(ctx context.Context, text, voice string, opts *skywebrtc.SynthesisOptions) ([]byte, int32, error) {
	if m.shouldFail {
		return nil, 0, errors.New("synthesis failed")
	}
	m.lastText = text
	m.lastVoice = voice
	m.lastOpts = opts
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
	shouldFail       bool
	lastAudio        []byte
	lastSampleRate   int32
	streamEvents     []streamingSTTResponse
	streamShouldFail bool
	transcribeText   string
}

func (m *mockSTTTranscriber) Transcribe(ctx context.Context, audioData []byte, sampleRate int32) (string, error) {
	if m.shouldFail {
		return "", errors.New("transcription failed")
	}
	m.lastAudio = audioData
	m.lastSampleRate = sampleRate
	if m.transcribeText != "" {
		return m.transcribeText, nil
	}
	return "Hello, this is a test transcription.", nil
}

func (m *mockSTTTranscriber) TranscribeStream(ctx context.Context, audioData []byte, sampleRate int32, onTranscript func(text string, isFinal bool)) error {
	m.lastAudio = audioData
	m.lastSampleRate = sampleRate
	if m.streamShouldFail {
		return errors.New("stream transcription failed")
	}
	if len(m.streamEvents) == 0 {
		m.streamEvents = []streamingSTTResponse{{Text: "Hello, this is a test transcription.", IsFinal: true}}
	}
	for _, event := range m.streamEvents {
		onTranscript(event.Text, event.IsFinal)
	}
	return nil
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

func TestTranscriptionsHandlerSupportsStreaming(t *testing.T) {
	mock := &mockSTTTranscriber{
		streamEvents: []streamingSTTResponse{
			{Text: "hello", IsFinal: false},
			{Text: "hello world", IsFinal: true},
		},
	}
	handler := TranscriptionsHandlerWithTranscriber(mock)

	wavData := createMinimalWAV()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model", "whisper-1")
	_ = writer.WriteField("stream", "true")
	part, _ := writer.CreateFormFile("file", "test.wav")
	part.Write(wavData)
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected status %d, got %d", http.StatusOK, rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Expected Content-Type text/event-stream, got %q", got)
	}

	bodyStr := rr.Body.String()
	if !strings.Contains(bodyStr, "data: {\"text\":\"hello\",\"isFinal\":false}") {
		t.Fatalf("Expected partial transcript event, got %q", bodyStr)
	}
	if !strings.Contains(bodyStr, "data: {\"text\":\"hello world\",\"isFinal\":true}") {
		t.Fatalf("Expected final transcript event, got %q", bodyStr)
	}
	if !strings.Contains(bodyStr, "data: [DONE]") {
		t.Fatalf("Expected DONE sentinel, got %q", bodyStr)
	}
}

func TestTranscriptionsHandlerRejectsInvalidStreamValue(t *testing.T) {
	handler := TranscriptionsHandlerWithTranscriber(&mockSTTTranscriber{})

	wavData := createMinimalWAV()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model", "whisper-1")
	_ = writer.WriteField("stream", "definitely-not-a-bool")
	part, _ := writer.CreateFormFile("file", "test.wav")
	part.Write(wavData)
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("Expected status %d, got %d", http.StatusBadRequest, rr.Code)
	}
}

func TestTranscriptionsHandlerNormalizesEmptyTranscription(t *testing.T) {
	mock := &mockSTTTranscriber{transcribeText: " \n\t "}
	handler := TranscriptionsHandlerWithTranscriber(mock)

	wavData := createMinimalWAV()

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
		t.Fatalf("Expected status %d, got %d", http.StatusOK, rr.Code)
	}

	var resp STTResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if resp.Text != "[empty transcription]" {
		t.Fatalf("Expected normalized empty transcript, got %q", resp.Text)
	}
}

func TestTranscriptionsHandlerStreamingFailureWritesErrorEvent(t *testing.T) {
	mock := &mockSTTTranscriber{streamShouldFail: true}
	handler := TranscriptionsHandlerWithTranscriber(mock)

	wavData := createMinimalWAV()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model", "whisper-1")
	_ = writer.WriteField("stream", "true")
	part, _ := writer.CreateFormFile("file", "test.wav")
	part.Write(wavData)
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected status %d, got %d", http.StatusOK, rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "data: {\"error\":\"Transcription failed\"}") {
		t.Fatalf("Expected streaming error event, got %q", rr.Body.String())
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

// TestParseEmotionTag verifies emotion tag parsing functionality
func TestParseEmotionTag(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantEmotion  string
		wantStripped string
		wantFound    bool
	}{
		{
			name:         "no tag",
			input:        "Hello world",
			wantEmotion:  "",
			wantStripped: "Hello world",
			wantFound:    false,
		},
		{
			name:         "amusement tag",
			input:        "[emotion:amusement] Hello world",
			wantEmotion:  "amusement",
			wantStripped: "Hello world",
			wantFound:    true,
		},
		{
			name:         "whisper tag",
			input:        "[emotion:whisper] This is a secret",
			wantEmotion:  "whisper",
			wantStripped: "This is a secret",
			wantFound:    true,
		},
		{
			name:         "neutral tag",
			input:        "[emotion:neutral] Just regular text",
			wantEmotion:  "neutral",
			wantStripped: "Just regular text",
			wantFound:    true,
		},
		{
			name:         "unknown emotion tag - soft fallback",
			input:        "[emotion:unknown] Hello world",
			wantEmotion:  "",
			wantStripped: "Hello world",
			wantFound:    false,
		},
		{
			name:         "malformed tag - no closing bracket",
			input:        "[emotion:amusement Hello world",
			wantEmotion:  "",
			wantStripped: "[emotion:amusement Hello world",
			wantFound:    false,
		},
		{
			name:         "empty emotion label",
			input:        "[emotion:] Hello world",
			wantEmotion:  "",
			wantStripped: "Hello world",
			wantFound:    false,
		},
		{
			name:         "case insensitive",
			input:        "[emotion:AMUSEMENT] Hello world",
			wantEmotion:  "amusement",
			wantStripped: "Hello world",
			wantFound:    true,
		},
		{
			name:         "all supported emotions",
			input:        "[emotion:serenity] Peaceful text",
			wantEmotion:  "serenity",
			wantStripped: "Peaceful text",
			wantFound:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			emotion, stripped, found := parseEmotionTag(tt.input)
			if emotion != tt.wantEmotion {
				t.Errorf("parseEmotionTag() emotion = %q, want %q", emotion, tt.wantEmotion)
			}
			if stripped != tt.wantStripped {
				t.Errorf("parseEmotionTag() stripped = %q, want %q", stripped, tt.wantStripped)
			}
			if found != tt.wantFound {
				t.Errorf("parseEmotionTag() found = %v, want %v", found, tt.wantFound)
			}
		})
	}
}

// TestEmotionToKyutaiVoiceMap verifies all emotions have voice mappings
func TestEmotionToKyutaiVoiceMap(t *testing.T) {
	expectedEmotions := []string{
		"neutral", "amusement", "interest", "desire", "adoration",
		"contentment", "serenity", "pride", "whisper",
	}

	for _, emotion := range expectedEmotions {
		voice, ok := emotionToKyutaiVoice[emotion]
		if !ok {
			t.Errorf("Missing voice mapping for emotion: %s", emotion)
			continue
		}
		if voice == "" {
			t.Errorf("Empty voice mapping for emotion: %s", emotion)
		}
	}

	// Verify supportedEmotions matches emotionToKyutaiVoice keys
	if len(supportedEmotions) != len(emotionToKyutaiVoice) {
		t.Errorf("supportedEmotions (%d) and emotionToKyutaiVoice (%d) have different sizes",
			len(supportedEmotions), len(emotionToKyutaiVoice))
	}

	for emotion := range supportedEmotions {
		if _, ok := emotionToKyutaiVoice[emotion]; !ok {
			t.Errorf("Emotion %s in supportedEmotions but not in emotionToKyutaiVoice", emotion)
		}
	}
}

// TestTTSHandlerWithSynthEmotionParsing verifies emotion tags are parsed and stripped
func TestTTSHandlerWithSynthEmotionParsing(t *testing.T) {
	mock := &mockTTSSynthesizer{}
	handler := TTSHandlerWithSynth(map[string]TTSSynthesizer{
		"kyutai": mock,
	})

	reqBody := TTSRequest{Model: "sky-kyutai", Input: "[emotion:amusement] Hello world"}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
	}

	// Verify the emotion tag was stripped from the text
	if mock.lastText != "Hello world" {
		t.Errorf("Expected text 'Hello world' with stripped tag, got '%s'", mock.lastText)
	}

	// Verify the emotion voice was selected
	expectedVoice := "ears/p003/emo_amusement_freeform.wav"
	if mock.lastVoice != expectedVoice {
		t.Errorf("Expected voice '%s' for amusement emotion, got '%s'", expectedVoice, mock.lastVoice)
	}
}

// TestTTSHandlerWithSynthExplicitVoiceOverridesEmotion verifies explicit voice takes precedence
func TestTTSHandlerWithSynthExplicitVoiceOverridesEmotion(t *testing.T) {
	mock := &mockTTSSynthesizer{}
	handler := TTSHandlerWithSynth(map[string]TTSSynthesizer{
		"kyutai": mock,
	})

	reqBody := TTSRequest{
		Model: "sky-kyutai",
		Input: "[emotion:amusement] Hello world",
		Voice: "custom-voice-override",
	}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
	}

	// Verify the emotion tag was stripped
	if mock.lastText != "Hello world" {
		t.Errorf("Expected text 'Hello world' with stripped tag, got '%s'", mock.lastText)
	}

	// Verify explicit voice overrides emotion
	if mock.lastVoice != "custom-voice-override" {
		t.Errorf("Expected explicit voice 'custom-voice-override', got '%s'", mock.lastVoice)
	}
}

// TestTTSHandlerWithSynthUnknownEmotionSoftFallback verifies unknown emotions don't fail
func TestTTSHandlerWithSynthUnknownEmotionSoftFallback(t *testing.T) {
	mock := &mockTTSSynthesizer{}
	handler := TTSHandlerWithSynth(map[string]TTSSynthesizer{
		"kyutai": mock,
	})

	reqBody := TTSRequest{Model: "sky-kyutai", Input: "[emotion:unknown] Hello world"}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// Should succeed with soft fallback
	if rr.Code != http.StatusOK {
		t.Errorf("Expected status %d for unknown emotion (soft fallback), got %d", http.StatusOK, rr.Code)
	}

	// Tag should be stripped even for unknown emotion
	if mock.lastText != "Hello world" {
		t.Errorf("Expected text 'Hello world' with stripped tag, got '%s'", mock.lastText)
	}

	// Should use empty voice (worker default) since emotion was unknown
	if mock.lastVoice != "" {
		t.Errorf("Expected empty voice for unknown emotion fallback, got '%s'", mock.lastVoice)
	}
}

// TestTTSHandlerWithSynthSynthesisOptions verifies per-request tuning parameters
func TestTTSHandlerWithSynthSynthesisOptions(t *testing.T) {
	mock := &mockTTSSynthesizer{}
	handler := TTSHandlerWithSynth(map[string]TTSSynthesizer{
		"kyutai": mock,
	})

	reqBody := TTSRequest{
		Model:          "sky-kyutai",
		Input:          "Hello world",
		Temperature:    0.95,
		CfgCoef:        2.0,
		PaddingBetween: 1,
	}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
	}

	// Verify synthesis options were forwarded
	if mock.lastOpts == nil {
		t.Fatal("Expected synthesis options to be set")
	}
	if mock.lastOpts.Temperature != 0.95 {
		t.Errorf("Expected temperature 0.95, got %f", mock.lastOpts.Temperature)
	}
	if mock.lastOpts.CfgCoef != 2.0 {
		t.Errorf("Expected cfg_coef 2.0, got %f", mock.lastOpts.CfgCoef)
	}
	if mock.lastOpts.PaddingBetween != 1 {
		t.Errorf("Expected padding_between 1, got %d", mock.lastOpts.PaddingBetween)
	}
}

// TestTTSHandlerWithSynthSynthesisOptionsDefaults verifies defaults are applied
func TestTTSHandlerWithSynthSynthesisOptionsDefaults(t *testing.T) {
	mock := &mockTTSSynthesizer{}
	handler := TTSHandlerWithSynth(map[string]TTSSynthesizer{
		"kyutai": mock,
	})

	// Request without synthesis options
	reqBody := TTSRequest{Model: "sky-kyutai", Input: "Hello world"}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
	}

	// Verify options were passed through (worker applies defaults, not gateway)
	if mock.lastOpts == nil {
		t.Fatal("Expected synthesis options to be set")
	}
	// Note: Gateway no longer overrides 0 values - worker applies defaults
	// This allows explicit requests for temperature=0, cfg_coef=0
	if mock.lastOpts.Temperature != 0 {
		t.Errorf("Expected temperature 0 (passed through), got %f", mock.lastOpts.Temperature)
	}
	if mock.lastOpts.CfgCoef != 0 {
		t.Errorf("Expected cfg_coef 0 (passed through), got %f", mock.lastOpts.CfgCoef)
	}
	if mock.lastOpts.PaddingBetween != 0 {
		t.Errorf("Expected padding_between 0 (passed through), got %d", mock.lastOpts.PaddingBetween)
	}
}

// TestTTSHandlerWithSynthPocketIgnoresOptions verifies pocket engine doesn't get options
func TestTTSHandlerWithSynthPocketIgnoresOptions(t *testing.T) {
	mock := &mockTTSSynthesizer{}
	handler := TTSHandlerWithSynth(map[string]TTSSynthesizer{
		"pocket": mock,
	})

	reqBody := TTSRequest{
		Model:          "sky-pocket",
		Input:          "Hello world",
		Temperature:    0.95,
		CfgCoef:        2.0,
		PaddingBetween: 1,
	}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
	}

	// Pocket should get nil options (options are Kyutai-only)
	if mock.lastOpts != nil {
		t.Errorf("Expected nil options for pocket engine, got %+v", mock.lastOpts)
	}
}

// TestTTSHandlerWithSynthEmotionWithOptions verifies emotion and options work together
func TestTTSHandlerWithSynthEmotionWithOptions(t *testing.T) {
	mock := &mockTTSSynthesizer{}
	handler := TTSHandlerWithSynth(map[string]TTSSynthesizer{
		"kyutai": mock,
	})

	reqBody := TTSRequest{
		Model:          "sky-kyutai",
		Input:          "[emotion:desire] I want this",
		Temperature:    0.85,
		CfgCoef:        1.8,
		PaddingBetween: 2,
	}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
	}

	// Verify emotion was resolved
	expectedVoice := "ears/p003/emo_desire_freeform.wav"
	if mock.lastVoice != expectedVoice {
		t.Errorf("Expected voice '%s' for desire emotion, got '%s'", expectedVoice, mock.lastVoice)
	}

	// Verify text was stripped
	if mock.lastText != "I want this" {
		t.Errorf("Expected text 'I want this', got '%s'", mock.lastText)
	}

	// Verify options were set
	if mock.lastOpts == nil {
		t.Fatal("Expected synthesis options to be set")
	}
	if mock.lastOpts.Temperature != 0.85 {
		t.Errorf("Expected temperature 0.85, got %f", mock.lastOpts.Temperature)
	}
}

// TestResolveEngineAndVoice verifies engine and voice resolution
func TestResolveEngineAndVoice(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		voice      string
		emotion    string
		wantEngine string
		wantVoice  string
		wantErr    bool
	}{
		{
			name:       "basic pocket",
			model:      "sky-pocket",
			voice:      "",
			emotion:    "",
			wantEngine: "pocket",
			wantVoice:  "",
		},
		{
			name:       "kyutai with emotion",
			model:      "sky-kyutai",
			voice:      "",
			emotion:    "amusement",
			wantEngine: "kyutai",
			wantVoice:  "ears/p003/emo_amusement_freeform.wav",
		},
		{
			name:       "explicit voice overrides emotion",
			model:      "sky-kyutai",
			voice:      "custom-voice",
			emotion:    "amusement",
			wantEngine: "kyutai",
			wantVoice:  "custom-voice",
		},
		{
			name:       "kyutai no emotion uses empty voice",
			model:      "sky-kyutai",
			voice:      "",
			emotion:    "",
			wantEngine: "kyutai",
			wantVoice:  "",
		},
		{
			name:    "unknown model",
			model:   "sky-unknown",
			voice:   "",
			emotion: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine, voice, err := resolveEngineAndVoice(tt.model, tt.voice, tt.emotion)
			if tt.wantErr {
				if err == nil {
					t.Error("Expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}
			if engine != tt.wantEngine {
				t.Errorf("Expected engine %q, got %q", tt.wantEngine, engine)
			}
			if voice != tt.wantVoice {
				t.Errorf("Expected voice %q, got %q", tt.wantVoice, voice)
			}
		})
	}
}
