package openaihttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/heliasar/sky-audio/gateway/pkg/audio"
	skywebrtc "github.com/heliasar/sky-audio/gateway/pkg/webrtc"
)

// STTRequest matches OpenAI's /v1/audio/transcriptions request format
// Note: This is a multipart/form request, not JSON
type STTRequest struct {
	File           []byte  // audio file data
	Model          string  // model ID, e.g., "whisper-1" or "sky-nemo-parakeet"
	Language       string  // optional language code (e.g., "en", "fr")
	Prompt         string  // optional prompt to guide transcription
	ResponseFormat string  // "json", "text", "srt", "verbose_json", "vtt"
	Temperature    float64 // sampling temperature (not supported by most STT)
	Stream         bool    // stream partial/final transcripts over SSE
}

// STTResponse matches OpenAI's transcription response (json format)
type STTResponse struct {
	Text string `json:"text"`
}

// STTVerboseResponse matches OpenAI's verbose_json format
type STTVerboseResponse struct {
	Text     string    `json:"text"`
	Task     string    `json:"task"`
	Language string    `json:"language"`
	Duration float64   `json:"duration"`
	Segments []Segment `json:"segments,omitempty"`
}

// Segment represents a transcription segment
type Segment struct {
	ID               int     `json:"id"`
	Seek             int     `json:"seek"`
	Start            float64 `json:"start"`
	End              float64 `json:"end"`
	Text             string  `json:"text"`
	Tokens           []int   `json:"tokens,omitempty"`
	Temperature      float64 `json:"temperature"`
	AvgLogprob       float64 `json:"avg_logprob"`
	CompressionRatio float64 `json:"compression_ratio"`
	NoSpeechProb     float64 `json:"no_speech_prob"`
}

// MaxAudioFileSize is the maximum audio file upload size (25MB like OpenAI)
const MaxAudioFileSize = 25 * 1024 * 1024

// STTTranscriber is the interface for STT clients (for testing)
type STTTranscriber interface {
	Transcribe(ctx context.Context, audioData []byte, sampleRate int32) (string, error)
}

// STTStreamingTranscriber streams partial/final transcripts over a callback.
type STTStreamingTranscriber interface {
	TranscribeStream(ctx context.Context, audioData []byte, sampleRate int32, onTranscript func(text string, isFinal bool)) error
}

// sttTranscriber creates a new STT client per request to ensure thread safety
type sttTranscriber struct {
	socketPath string
}

func (t *sttTranscriber) Transcribe(ctx context.Context, audioData []byte, sampleRate int32) (string, error) {
	var finalText string
	if err := t.TranscribeStream(ctx, audioData, sampleRate, func(text string, isFinal bool) {
		if isFinal && strings.TrimSpace(text) != "" {
			finalText = text
		}
	}); err != nil {
		return "", err
	}
	if finalText == "" {
		return "", errors.New("final transcript not received")
	}
	return finalText, nil
}

func (t *sttTranscriber) TranscribeStream(ctx context.Context, audioData []byte, sampleRate int32, onTranscript func(text string, isFinal bool)) error {
	// Validate sample rate - must be one of the supported values
	// The STT worker expects 16000Hz specifically
	validSampleRates := []int32{8000, 16000, 22050, 44100, 48000}
	isValid := false
	for _, validRate := range validSampleRates {
		if sampleRate == validRate {
			isValid = true
			break
		}
	}
	if !isValid {
		return fmt.Errorf("invalid sample rate: %d Hz; must be one of: 8000, 16000, 22050, 44100, 48000", sampleRate)
	}

	client, err := skywebrtc.NewSTTClient(t.socketPath)
	if err != nil {
		return fmt.Errorf("connect to STT worker: %w", err)
	}
	defer client.Close()

	if err := client.StartStream(ctx); err != nil {
		return fmt.Errorf("start stream: %w", err)
	}

	chunkSize := 1024 * 1024
	if len(audioData) == 0 {
		if err := client.SendAudio(nil, true, sampleRate); err != nil {
			return fmt.Errorf("send audio: %w", err)
		}
	} else {
		for i := 0; i < len(audioData); i += chunkSize {
			end := i + chunkSize
			if end > len(audioData) {
				end = len(audioData)
			}
			isLast := end == len(audioData)
			if err := client.SendAudio(audioData[i:end], isLast, sampleRate); err != nil {
				return fmt.Errorf("send audio chunk: %w", err)
			}
		}
	}

	for {
		transcript, err := client.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("recv transcript: %w", err)
		}
		if transcript != nil && onTranscript != nil {
			onTranscript(transcript.Text, transcript.IsFinal)
		}
		if transcript != nil && transcript.IsFinal {
			return nil
		}
	}
}

// EnsureRunningFunc is called before using STT to ensure the worker is running (lazy loading).
// Returns a cleanup function that must be called when the session/request completes.
type EnsureRunningFunc func() (cleanup func())

// TranscriptionsHandler creates an OpenAI-compatible /v1/audio/transcriptions handler
// Creates a new STT client per request for thread safety
func TranscriptionsHandler(socketPath string) http.HandlerFunc {
	transcriber := &sttTranscriber{socketPath: socketPath}
	return TranscriptionsHandlerWithTranscriber(transcriber)
}

// TranscriptionsHandlerWithEnsureRunning creates handler with lazy STT loading support.
// The ensureRunning callback spawns the worker if needed and returns a cleanup function
// for idle timeout tracking. The cleanup MUST be called via defer.
func TranscriptionsHandlerWithEnsureRunning(socketPath string, ensureRunning EnsureRunningFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeTTSError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Only POST requests are supported")
			return
		}

		// Ensure STT is running (lazy load) and register session for idle tracking
		cleanup := ensureRunning()
		if cleanup == nil {
			writeTTSError(w, http.StatusServiceUnavailable, "api_error", "STT service unavailable")
			return
		}
		defer cleanup() // Ensure idle timeout is scheduled when request completes

		transcriber := &sttTranscriber{socketPath: socketPath}
		handleTranscription(w, r, transcriber)
	}
}

// TranscriptionsHandlerWithTranscriber creates handler from STTTranscriber interface
func TranscriptionsHandlerWithTranscriber(transcriber STTTranscriber) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeTTSError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Only POST requests are supported")
			return
		}
		handleTranscription(w, r, transcriber)
	}
}

func handleTranscription(w http.ResponseWriter, r *http.Request, transcriber STTTranscriber) {
	// Limit request body size to prevent unbounded disk writes
	r.Body = http.MaxBytesReader(w, r.Body, MaxAudioFileSize)

	// Parse multipart form
	if err := r.ParseMultipartForm(MaxAudioFileSize); err != nil {
		log.Printf("[stt] parse multipart error: %v", err)
		writeTTSError(w, http.StatusBadRequest, "invalid_request_error", "Failed to parse request")
		return
	}
	defer r.MultipartForm.RemoveAll()

	// Extract form values
	req, err := parseSTTRequest(r)
	if err != nil {
		writeTTSError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// Validate audio data
	if len(req.File) == 0 {
		writeTTSError(w, http.StatusBadRequest, "invalid_request_error", "Audio file is required")
		return
	}

	// Decode audio to PCM16 at 16kHz (required by NeMo/Parakeet)
	// Use a shorter timeout for decode to prevent resource exhaustion
	decodeCtx, decodeCancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer decodeCancel()
	pcmData, sampleRate, err := decodeAudioToPCM(decodeCtx, req.File)
	if err != nil {
		writeTTSError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Failed to decode audio: %v", err))
		return
	}

	// Transcribe
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	if req.Stream {
		streamingTranscriber, ok := transcriber.(STTStreamingTranscriber)
		if !ok {
			writeTTSError(w, http.StatusBadRequest, "invalid_request_error", "Streaming transcription is not supported by this handler")
			return
		}
		if err := writeStreamingTranscription(w, ctx, pcmData, sampleRate, streamingTranscriber); err != nil {
			log.Printf("[stt] streaming transcription error: %v", err)
		}
		return
	}

	text, err := transcriber.Transcribe(ctx, pcmData, sampleRate)
	if err != nil {
		// Log detailed error server-side, return generic message to client
		// to avoid leaking internal paths and implementation details
		// (security: prevent information disclosure)
		log.Printf("[stt] transcription error: %v", err)
		writeTTSError(w, http.StatusInternalServerError, "api_error", "Transcription failed")
		return
	}

	// Normalize empty transcripts so the caller can announce the result instead of silently swallowing it.
	text = strings.TrimSpace(text)
	if text == "" {
		log.Printf("[stt] empty transcript returned by worker")
		text = "[empty transcription]"
	}

	// Format response
	format := strings.ToLower(req.ResponseFormat)
	if format == "" {
		format = "json"
	}

	switch format {
	case "text":
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(text)); err != nil {
			log.Printf("[stt] write text response error: %v", err)
		}
	case "json":
		resp := STTResponse{Text: text}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("[stt] encode json response error: %v", err)
		}
	case "verbose_json":
		resp := STTVerboseResponse{
			Text:     text,
			Task:     "transcribe",
			Language: req.Language,
			Duration: float64(len(pcmData)) / float64(sampleRate*audio.BytesPerSample), // rough estimate
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("[stt] encode verbose json response error: %v", err)
		}
	case "srt", "vtt":
		// Simple SRT/VTT generation with single entry
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		duration := float64(len(pcmData)) / float64(sampleRate*audio.BytesPerSample)
		if format == "srt" {
			if _, err := fmt.Fprintf(w, "1\n00:00:00,000 --> %s\n%s\n", formatDurationSRT(duration), text); err != nil {
				log.Printf("[stt] write srt response error: %v", err)
			}
		} else {
			if _, err := fmt.Fprintf(w, "WEBVTT\n\n00:00:00.000 --> %s\n%s\n", formatDurationVTT(duration), text); err != nil {
				log.Printf("[stt] write vtt response error: %v", err)
			}
		}
	default:
		writeTTSError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Unsupported response_format: '%s'", format))
		return
	}
}

type streamingSTTResponse struct {
	Text    string `json:"text"`
	IsFinal bool   `json:"isFinal"`
}

func writeStreamingTranscription(w http.ResponseWriter, ctx context.Context, pcmData []byte, sampleRate int32, transcriber STTStreamingTranscriber) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeTTSError(w, http.StatusInternalServerError, "api_error", "Streaming unsupported by response writer")
		return fmt.Errorf("response writer does not support flushing")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var writeErr error

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	writeFrame := func(text string, isFinal bool) error {
		payload, err := json.Marshal(streamingSTTResponse{Text: text, IsFinal: isFinal})
		if err != nil {
			return fmt.Errorf("marshal streaming transcript: %w", err)
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	if err := transcriber.TranscribeStream(ctx, pcmData, sampleRate, func(text string, isFinal bool) {
		if writeErr != nil {
			return
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		if err := writeFrame(text, isFinal); err != nil {
			writeErr = err
			cancel()
			log.Printf("[stt] write streaming frame error: %v", err)
		}
	}); err != nil {
		if writeErr != nil {
			return writeErr
		}
		_, _ = fmt.Fprintf(w, "data: {\"error\":%q}\n\n", "Transcription failed")
		flusher.Flush()
		return err
	}

	if writeErr != nil {
		return writeErr
	}

	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func parseSTTRequest(r *http.Request) (*STTRequest, error) {
	req := &STTRequest{}

	// Get the file from form data
	file, header, err := r.FormFile("file")
	if err != nil {
		return nil, fmt.Errorf("audio file is required: %v", err)
	}
	defer file.Close()

	// Check file size
	if header.Size > MaxAudioFileSize {
		return nil, fmt.Errorf("audio file exceeds maximum size of %d bytes", MaxAudioFileSize)
	}

	// Read file data
	data, err := io.ReadAll(io.LimitReader(file, MaxAudioFileSize))
	if err != nil {
		return nil, fmt.Errorf("failed to read audio file: %v", err)
	}
	req.File = data

	// Get model (required)
	req.Model = r.FormValue("model")
	if strings.TrimSpace(req.Model) == "" {
		return nil, errors.New("model is required")
	}
	// Validate model is a known STT model
	if !isValidSTTModel(req.Model) {
		return nil, fmt.Errorf("unknown model: %s (available: whisper-1, sky-nemo-parakeet)", req.Model)
	}

	// Optional parameters
	req.Language = r.FormValue("language")
	req.Prompt = r.FormValue("prompt")
	req.ResponseFormat = r.FormValue("response_format")

	// Temperature (optional, ignored but parsed for compatibility)
	if tempStr := r.FormValue("temperature"); tempStr != "" {
		temp, err := strconv.ParseFloat(tempStr, 64)
		if err == nil {
			req.Temperature = temp
		}
	}

	if streamStr := strings.TrimSpace(r.FormValue("stream")); streamStr != "" {
		stream, err := strconv.ParseBool(streamStr)
		if err != nil {
			return nil, fmt.Errorf("invalid stream value: %q", streamStr)
		}
		req.Stream = stream
	}

	return req, nil
}

// isValidSTTModel checks if the model is a supported STT model
func isValidSTTModel(model string) bool {
	switch strings.ToLower(model) {
	case "whisper-1", "sky-nemo-parakeet":
		return true
	default:
		return false
	}
}

// decodeAudioToPCM decodes uploaded audio to PCM16 at 16kHz mono.
// It supports multiple input formats: WAV, MP3, OGG, FLAC, M4A, WebM, Opus, 3GP.
// Uses native decoder for WAV and ffmpeg for other formats.
func decodeAudioToPCM(ctx context.Context, data []byte) ([]byte, int32, error) {
	// Detect format from magic bytes
	format := audio.DetectFormat(data)

	// Use native WAV decoder for pure WAV files (fast path)
	if format == audio.FormatWAV {
		return audio.DecodeWAV(data)
	}

	// For all other formats, use ffmpeg
	decoder := &audio.FFmpegDecoder{}
	if !decoder.Supports(format) {
		return nil, 0, fmt.Errorf("unsupported audio format: %s (supported: wav, mp3, ogg, flac, m4a, webm, opus, 3gp)", format)
	}

	result, err := decoder.Decode(ctx, data, format)
	if err != nil {
		return nil, 0, fmt.Errorf("audio decode failed: %w", err)
	}

	return result.PCM, result.SampleRate, nil
}

func formatDurationSRT(seconds float64) string {
	ms := int(seconds * 1000)
	hours := ms / 3600000
	ms %= 3600000
	minutes := ms / 60000
	ms %= 60000
	secs := ms / 1000
	ms %= 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", hours, minutes, secs, ms)
}

func formatDurationVTT(seconds float64) string {
	ms := int(seconds * 1000)
	hours := ms / 3600000
	ms %= 3600000
	minutes := ms / 60000
	ms %= 60000
	secs := ms / 1000
	ms %= 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, secs, ms)
}
