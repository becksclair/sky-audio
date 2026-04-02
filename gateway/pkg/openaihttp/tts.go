// Package openaihttp provides OpenAI-compatible HTTP endpoints for TTS/STT.
// This allows OpenClaw and other OpenAI-compatible clients to use sky-audio.
//
// OpenAI Compatibility:
//   - POST /v1/audio/speech - TTS synthesis (returns OGG/Opus audio by default)
//   - POST /v1/audio/transcriptions - STT transcription (multipart upload)
//   - GET /v1/models - List available models
//
// Model Mapping:
//   - sky-kyutai, tts-1-hd, gpt-4o-mini-tts -> Kyutai TTS (high quality)
//   - sky-pocket, tts-1 -> PocketTTS (fast)
//   - whisper-1, sky-nemo-parakeet -> NeMo Parakeet STT
//
// Audio Formats:
//   - Default: opus (Ogg/Opus container)
//   - Supported: opus, mp3, aac, flac, wav, pcm
//
// See docs/openclaw-integration.md for detailed setup instructions.
package openaihttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/heliasar/sky-audio/gateway/pkg/audio"
	skywebrtc "github.com/heliasar/sky-audio/gateway/pkg/webrtc"
)

// TTSRequest matches OpenAI's /v1/audio/speech request format
type TTSRequest struct {
	Model           string  `json:"model"`                      // e.g., "sky-kyutai", "sky-pocket"
	Input           string  `json:"input"`                      // text to synthesize
	Voice           string  `json:"voice,omitempty"`            // optional voice ID (uses worker default if empty)
	ResponseFormat  string  `json:"response_format,omitempty"`  // "wav", "mp3", "opus", "pcm"
	Speed           float64 `json:"speed,omitempty"`            // playback speed (not supported)
	Temperature     float32 `json:"temperature,omitempty"`      // synthesis temperature (Kyutai only)
	CfgCoef         float32 `json:"cfg_coef,omitempty"`         // classifier-free guidance coefficient (Kyutai only)
	PaddingBetween  int32   `json:"padding_between,omitempty"`  // padding between segments (Kyutai only)
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
	FormatOpus TTSResponseFormat = "opus"  // Default: Ogg/Opus, best for voice
	FormatMP3  TTSResponseFormat = "mp3"   // Widely compatible
	FormatAAC  TTSResponseFormat = "aac"  // AAC in ADTS
	FormatFLAC TTSResponseFormat = "flac" // Lossless compression
	FormatWAV  TTSResponseFormat = "wav"  // Uncompressed PCM in WAV
	FormatPCM  TTSResponseFormat = "pcm"  // Raw PCM s16le
)

// DefaultResponseFormat is the default audio format for TTS.
const DefaultResponseFormat = FormatOpus

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

// Emotion labels supported for Kyutai TTS voice selection
var supportedEmotions = map[string]bool{
	"neutral":     true,
	"amusement":   true,
	"interest":    true,
	"desire":      true,
	"adoration":   true,
	"contentment": true,
	"serenity":    true,
	"pride":       true,
	"whisper":     true,
}

// emotionToKyutaiVoice maps emotion labels to Kyutai voice paths.
// The whisper emotion maps to an existing whisper voice from the expresso catalog.
var emotionToKyutaiVoice = map[string]string{
	"neutral":     "ears/p003/emo_neutral_freeform.wav",
	"amusement":   "ears/p003/emo_amusement_freeform.wav",
	"interest":    "ears/p003/emo_interest_freeform.wav",
	"desire":      "ears/p003/emo_desire_freeform.wav",
	"adoration":   "ears/p003/emo_adoration_freeform.wav",
	"contentment": "ears/p003/emo_contentment_freeform.wav",
	"serenity":    "ears/p003/emo_serenity_freeform.wav",
	"pride":       "ears/p003/emo_pride_freeform.wav",
	"whisper":     "expresso/ex01-ex02_whisper_001_channel1_579s.wav",
}

// defaultKyutaiVoice is the fallback voice when no emotion tag is present.
const defaultKyutaiVoice = "ears/p003/emo_amusement_freeform.wav"

// parseEmotionTag checks if text starts with [emotion:<label>] and returns the
// emotion label, the text with the tag stripped, and a boolean indicating if a
// valid tag was found. Only the nine supported emotion labels are recognized.
// Unknown emotion labels are treated as if no tag was present (soft fallback).
func parseEmotionTag(text string) (emotion, strippedText string, found bool) {
	const prefix = "[emotion:"
	if !strings.HasPrefix(text, prefix) {
		return "", text, false
	}

	// Find the closing bracket
	endIdx := strings.Index(text, "]")
	if endIdx == -1 {
		// Malformed tag, treat as literal text
		return "", text, false
	}

	label := strings.ToLower(strings.TrimSpace(text[len(prefix):endIdx]))
	stripped := strings.TrimSpace(text[endIdx+1:])

	if !supportedEmotions[label] {
		// Unknown emotion: soft fallback, strip the tag but return no emotion
		log.Printf("[tts] unknown emotion label %q, using default voice", label)
		return "", stripped, false
	}

	return label, stripped, true
}

// sanitizeTTSInput normalizes text before synthesis to avoid sentence-tail artifacts.
func sanitizeTTSInput(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return text
	}

	// Normalize line breaks and collapse excessive whitespace.
	text = strings.ReplaceAll(text, "\r\n", " ")
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\t", " ")
	text = strings.Join(strings.Fields(text), " ")

	// Strip common markdown wrappers that sound terrible aloud.
	text = strings.Trim(text, "`*_~")

	// Remove trailing punctuation and symbol noise that often causes TTS glitches.
	for len(text) > 0 {
		r, size := utf8.DecodeLastRuneInString(text)
		if unicode.IsPunct(r) || unicode.IsSymbol(r) || unicode.IsSpace(r) {
			text = strings.TrimSpace(text[:len(text)-size])
			continue
		}
		break
	}

	return text
}

// TTSSynthesizer is the interface for TTS clients
type TTSSynthesizer interface {
	Synthesize(ctx context.Context, text, voice string, opts *skywebrtc.SynthesisOptions) (pcm []byte, sampleRate int32, err error)
}

// ttsClientAdapter wraps skywebrtc.TTSClient to match our interface
type ttsClientAdapter struct {
	client *skywebrtc.TTSClient
}

func (a ttsClientAdapter) Synthesize(ctx context.Context, text, voice string, opts *skywebrtc.SynthesisOptions) ([]byte, int32, error) {
	return a.client.Synthesize(ctx, text, voice, opts)
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

		// Parse emotion tag from input (gateway owns this)
		emotion, strippedInput, _ := parseEmotionTag(req.Input)

		// Validate that text remains after emotion tag stripping
		if strings.TrimSpace(strippedInput) == "" {
			writeTTSError(w, http.StatusBadRequest, "invalid_request_error", "input is required (empty after removing emotion tag)")
			return
		}

		strippedInput = sanitizeTTSInput(strippedInput)
		if strippedInput == "" {
			writeTTSError(w, http.StatusBadRequest, "invalid_request_error", "input is required after cleanup")
			return
		}

		// Normalize and map model to engine
		engineName, voice, err := resolveEngineAndVoice(req.Model, req.Voice, emotion)
		if err != nil {
			writeTTSError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		synth, ok := synthesizers[engineName]
		if !ok {
			writeTTSError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Model '%s' engine '%s' is not available", req.Model, engineName))
			return
		}

		// Build synthesis options (nil for non-Kyutai engines)
		var opts *skywebrtc.SynthesisOptions
		if engineName == "kyutai" {
			opts = &skywebrtc.SynthesisOptions{
				Temperature:    req.Temperature,
				CfgCoef:        req.CfgCoef,
				PaddingBetween: req.PaddingBetween,
			}
			// Note: We don't override zero values here - the worker applies defaults.
			// This allows explicit requests for temperature=0, cfg_coef=0.
		}

		// Determine response format (default is opus per OpenAI best practices for voice)
		// Fall back to WAV if ffmpeg is not available
		format := TTSResponseFormat(strings.ToLower(req.ResponseFormat))
		if format == "" {
			if audio.IsFfmpegAvailable() {
				format = DefaultResponseFormat
			} else {
				format = FormatWAV
			}
		}

		// Validate format
		encoder := &audio.FFmpegEncoder{}
		if !encoder.Supports(string(format)) {
			writeTTSError(w, http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("Unsupported response_format: '%s'. Supported: opus, mp3, aac, flac, wav, pcm", format))
			return
		}

		// Synthesize
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()

		pcm, sampleRate, err := synth.Synthesize(ctx, strippedInput, voice, opts)
		if err != nil {
			// Log detailed error server-side, return generic message to client
			// to avoid leaking internal paths and implementation details
			// (security: prevent information disclosure)
			log.Printf("[tts] synthesis error: %v", err)
			writeTTSError(w, http.StatusInternalServerError, "api_error", "Synthesis failed")
			return
		}

		// Encode to requested format
		encodeCtx, encodeCancel := context.WithTimeout(r.Context(), 35*time.Second)
		defer encodeCancel()

		result, err := encoder.Encode(encodeCtx, pcm, sampleRate, string(format))
		if err != nil {
			log.Printf("[tts] encode error: %v", err)
			writeTTSError(w, http.StatusInternalServerError, "api_error", "Audio encoding failed")
			return
		}

		body := result.Body
		contentType := result.ContentType

		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(body); err != nil {
			// Client disconnected or write failed - log but don't try to write error
			log.Printf("[tts] write response error: %v", err)
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

func resolveEngineAndVoice(model, voice, emotion string) (engineName, resolvedVoice string, err error) {
	// Map model to engine
	engine, ok := modelToEngine[strings.ToLower(model)]
	if !ok {
		return "", "", fmt.Errorf("unknown model: %s (available: sky-kyutai, sky-pocket, tts-1, tts-1-hd, whisper-1)", model)
	}

	// If explicit voice is provided, it takes precedence over emotion-derived voice
	trimmedVoice := strings.TrimSpace(voice)
	if trimmedVoice != "" {
		return engine, trimmedVoice, nil
	}

	// For Kyutai, resolve emotion to voice if no explicit voice provided
	if engine == "kyutai" && emotion != "" {
		if emotionVoice, ok := emotionToKyutaiVoice[emotion]; ok {
			return engine, emotionVoice, nil
		}
	}

	// Return empty voice (worker will use its default)
	return engine, "", nil
}

// writeError writes an OpenAI-compatible error response
func writeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	err := TTSError{}
	err.Error.Type = errType
	err.Error.Message = message
	err.Error.Code = strconv.Itoa(status)
	if encodeErr := json.NewEncoder(w).Encode(err); encodeErr != nil {
		log.Printf("[openaihttp] failed to encode error response: %v", encodeErr)
	}
}

// writeTTSError is an alias for writeError for backward compatibility
func writeTTSError(w http.ResponseWriter, status int, errType, message string) {
	writeError(w, status, errType, message)
}
