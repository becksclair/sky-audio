// Package openaihttp provides OpenAI-compatible HTTP endpoints for TTS/STT.
// This allows OpenClaw and other OpenAI-compatible clients to use sky-audio.
//
// OpenAI Compatibility:
//   - POST /v1/audio/speech - TTS synthesis (returns WAV audio)
//   - POST /v1/audio/transcriptions - STT transcription (multipart upload)
//   - GET /v1/models - List available models
//
// Model Mapping:
//   - sky-kyutai, tts-1-hd, gpt-4o-mini-tts -> Kyutai TTS (high quality)
//   - sky-pocket, tts-1 -> PocketTTS (fast)
//   - whisper-1, sky-nemo-parakeet -> NeMo Parakeet STT
//
// See docs/openclaw-integration.md for detailed setup instructions.
package openaihttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/heliasar/sky-audio/gateway/pkg/audio"
	skywebrtc "github.com/heliasar/sky-audio/gateway/pkg/webrtc"
)

// TTSRequest matches OpenAI's /v1/audio/speech request format
type TTSRequest struct {
	Model          string  `json:"model"`                     // e.g., "sky-kyutai", "sky-pocket"
	Input          string  `json:"input"`                     // text to synthesize
	Voice          string  `json:"voice,omitempty"`           // optional voice ID (uses worker default if empty)
	ResponseFormat string  `json:"response_format,omitempty"` // "wav", "mp3", "opus", "pcm"
	Speed          float64 `json:"speed,omitempty"`           // playback speed (not supported)
}

// TTSError is OpenAI-compatible error response
type TTSError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code,omitempty"`
	} `json:"error"`
}

// TTSResponseFormat for supported output formats
type TTSResponseFormat string

const (
	FormatWAV  TTSResponseFormat = "wav"
	FormatMP3  TTSResponseFormat = "mp3"
	FormatOpus TTSResponseFormat = "opus"
	FormatPCM  TTSResponseFormat = "pcm"
)

// Model mapping from OpenAI-style model IDs to internal engine names
var modelToEngine = map[string]string{
	"sky-kyutai":      "kyutai",
	"sky-pocket":      "pocket",
	"gpt-4o-mini-tts": "kyutai", // alias for compatibility
	"tts-1":           "pocket", // alias for compatibility
	"tts-1-hd":        "kyutai", // alias for compatibility
}

// MaxTTSCharacters is the maximum text length allowed for TTS
const MaxTTSCharacters = 4096

// MaxTTSRequestBytes is the maximum request body size
const MaxTTSRequestBytes = 51200 // 50KB

// TTSSynthesizer is the interface for TTS clients
type TTSSynthesizer interface {
	Synthesize(ctx context.Context, text, voice string) (pcm []byte, sampleRate int32, err error)
}

// ttsClientAdapter wraps skywebrtc.TTSClient to match our interface
type ttsClientAdapter struct {
	client *skywebrtc.TTSClient
}

func (a ttsClientAdapter) Synthesize(ctx context.Context, text, voice string) ([]byte, int32, error) {
	return a.client.Synthesize(ctx, text, voice)
}

// NewTTSSynthesizer creates a TTSSynthesizer from a TTSClient
func NewTTSSynthesizer(client *skywebrtc.TTSClient) TTSSynthesizer {
	return ttsClientAdapter{client: client}
}

// TTSHandler creates an OpenAI-compatible /v1/audio/speech handler
func TTSHandler(clients map[string]*skywebrtc.TTSClient) http.HandlerFunc {
	// Wrap clients to match the interface
	synthesizers := make(map[string]TTSSynthesizer, len(clients))
	for name, client := range clients {
		synthesizers[name] = NewTTSSynthesizer(client)
	}
	return TTSHandlerWithSynth(synthesizers)
}

// TTSHandlerWithSynth creates handler from TTSSynthesizer interface (for testing)
func TTSHandlerWithSynth(synthesizers map[string]TTSSynthesizer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeTTSError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Only POST requests are supported")
			return
		}

		// Limit request body size
		r.Body = http.MaxBytesReader(w, r.Body, MaxTTSRequestBytes)

		var req TTSRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				writeTTSError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body exceeds maximum size")
			} else {
				writeTTSError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Invalid JSON: %v", err))
			}
			return
		}

		// Validate request
		if err := validateTTSRequest(&req); err != nil {
			writeTTSError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		// Normalize and map model to engine
		engineName, voice, err := resolveEngineAndVoice(req.Model, req.Voice)
		if err != nil {
			writeTTSError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		synth, ok := synthesizers[engineName]
		if !ok {
			writeTTSError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Model '%s' engine '%s' is not available", req.Model, engineName))
			return
		}

		// Determine response format
		format := TTSResponseFormat(strings.ToLower(req.ResponseFormat))
		if format == "" {
			format = FormatWAV // We only support WAV (OpenAI default is MP3, but we deviate)
		}

		// Synthesize
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()

		pcm, sampleRate, err := synth.Synthesize(ctx, req.Input, voice)
		if err != nil {
			// Log detailed error server-side, return generic message to client
			// to avoid leaking internal paths and implementation details
			// (security: prevent information disclosure)
			log.Printf("[tts] synthesis error: %v", err)
			writeTTSError(w, http.StatusInternalServerError, "api_error", "Synthesis failed")
			return
		}

		// Encode response based on format
		var body []byte
		contentType := "audio/wav"
		switch format {
		case FormatWAV:
			var buf bytes.Buffer
			if err := audio.WriteWAV(&buf, pcm, sampleRate); err != nil {
				writeTTSError(w, http.StatusInternalServerError, "api_error", fmt.Sprintf("WAV encoding failed: %v", err))
				return
			}
			body = buf.Bytes()
			contentType = "audio/wav"
		case FormatPCM:
			body = pcm
			contentType = "audio/pcm; rate=" + strconv.Itoa(int(sampleRate))
		case FormatMP3, FormatOpus:
			// MP3 and Opus are not supported - return error instead of silent fallback
			// This is clearer for clients than returning WAV when MP3 was requested
			writeTTSError(w, http.StatusNotImplemented, "api_error",
				fmt.Sprintf("Format '%s' is not supported. Supported formats: wav, pcm", format))
			return
		default:
			writeTTSError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Unsupported response_format: '%s'", format))
			return
		}

		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(body); err != nil {
			// Client disconnected or write failed - log but don't try to write error
			return
		}
	}
}

func validateTTSRequest(req *TTSRequest) error {
	if strings.TrimSpace(req.Input) == "" {
		return errors.New("input is required")
	}

	if utf8.RuneCountInString(req.Input) > MaxTTSCharacters {
		return fmt.Errorf("input exceeds maximum length of %d characters", MaxTTSCharacters)
	}

	if strings.TrimSpace(req.Model) == "" {
		return errors.New("model is required")
	}

	return nil
}

func resolveEngineAndVoice(model, voice string) (engineName, resolvedVoice string, err error) {
	// Map model to engine
	engine, ok := modelToEngine[strings.ToLower(model)]
	if !ok {
		return "", "", fmt.Errorf("unknown model: %s (available: sky-kyutai, sky-pocket, tts-1, tts-1-hd, whisper-1)", model)
	}

	// If voice is empty, let the worker use its default
	return engine, strings.TrimSpace(voice), nil
}

// writeError writes an OpenAI-compatible error response
func writeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	err := TTSError{}
	err.Error.Type = errType
	err.Error.Message = message
	err.Error.Code = strconv.Itoa(status)
	_ = json.NewEncoder(w).Encode(err)
}

// writeTTSError is an alias for writeError for backward compatibility
func writeTTSError(w http.ResponseWriter, status int, errType, message string) {
	writeError(w, status, errType, message)
}
