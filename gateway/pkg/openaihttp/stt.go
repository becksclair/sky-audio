package openaihttp

import (
	"bytes"
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

// sttTranscriber creates a new STT client per request to ensure thread safety
type sttTranscriber struct {
	socketPath string
}

func (t *sttTranscriber) Transcribe(ctx context.Context, audioData []byte, sampleRate int32) (string, error) {
	// Create a new client for this request (ensures thread safety)
	client, err := skywebrtc.NewSTTClient(t.socketPath)
	if err != nil {
		return "", fmt.Errorf("connect to STT worker: %w", err)
	}
	defer client.Close()

	// Start the gRPC stream
	if err := client.StartStream(ctx); err != nil {
		return "", fmt.Errorf("start stream: %w", err)
	}

	// Stream audio chunks to the worker to avoid gRPC 4MB message limit
	chunkSize := 1024 * 1024 // 1MB chunks
	if len(audioData) == 0 {
		if err := client.SendAudio(nil, true, sampleRate); err != nil {
			return "", fmt.Errorf("send audio: %w", err)
		}
	} else {
		for i := 0; i < len(audioData); i += chunkSize {
			end := i + chunkSize
			if end > len(audioData) {
				end = len(audioData)
			}
			isLast := end == len(audioData)
			if err := client.SendAudio(audioData[i:end], isLast, sampleRate); err != nil {
				return "", fmt.Errorf("send audio chunk: %w", err)
			}
		}
	}

	// Receive transcripts until we get a final result
	var finalText string
	for {
		transcript, err := client.Recv()
		if err != nil {
			// Handle clean stream closure (EOF) - return accumulated text
			if errors.Is(err, io.EOF) {
				return finalText, nil
			}
			return "", fmt.Errorf("recv transcript: %w", err)
		}

		// Accumulate non-empty transcripts
		if transcript.Text != "" {
			finalText = transcript.Text
		}

		// Stop when we get the final result
		if transcript.IsFinal {
			break
		}
	}

	return finalText, nil
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

	text, err := transcriber.Transcribe(ctx, pcmData, sampleRate)
	if err != nil {
		// Log detailed error server-side, return generic message to client
		// to avoid leaking internal paths and implementation details
		// (security: prevent information disclosure)
		log.Printf("[stt] transcription error: %v", err)
		writeTTSError(w, http.StatusInternalServerError, "api_error", "Transcription failed")
		return
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
			Duration: float64(len(pcmData)) / float64(sampleRate*2), // rough estimate
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
		duration := float64(len(pcmData)) / float64(sampleRate*2)
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
		return decodeWAV(data)
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

// decodeWAV parses simple WAV files and extracts PCM data
func decodeWAV(data []byte) ([]byte, int32, error) {
	if len(data) < 44 {
		return nil, 0, errors.New("WAV file too small")
	}

	// Check RIFF header
	if !bytes.Equal(data[0:4], []byte("RIFF")) {
		return nil, 0, errors.New("invalid RIFF header")
	}

	// Check WAVE format
	if !bytes.Equal(data[8:12], []byte("WAVE")) {
		return nil, 0, errors.New("invalid WAVE format")
	}

	// Parse fmt chunk
	var sampleRate int32
	var bitsPerSample int16
	var numChannels int16

	// Walk through chunks to find fmt and data
	offset := 12
	for offset < len(data)-8 {
		chunkID := string(data[offset : offset+4])
		// Parse chunk size as int64 for 32-bit platform safety
		chunkSize := int64(uint32(data[offset+4]) | uint32(data[offset+5])<<8 | uint32(data[offset+6])<<16 | uint32(data[offset+7])<<24)

		// Validate chunk size doesn't overflow file bounds (prevents infinite loops on malformed files)
		if chunkSize < 0 || int64(offset)+8+chunkSize > int64(len(data)) {
			return nil, 0, errors.New("invalid chunk size")
		}
		if chunkSize == 0 {
			return nil, 0, errors.New("zero-size chunk")
		}

		if chunkID == "fmt " {
			// Need at least 16 bytes for fmt chunk data, and we read up to offset+23
			if chunkSize < 16 || int64(offset)+24 > int64(len(data)) {
				return nil, 0, errors.New("invalid fmt chunk")
			}
			// Validate audio format is PCM (1)
			audioFormat := int16(data[offset+8]) | int16(data[offset+9])<<8
			if audioFormat != 1 {
				return nil, 0, fmt.Errorf("unsupported WAV audio format %d (only PCM/format 1 is supported)", audioFormat)
			}
			numChannels = int16(data[offset+10]) | int16(data[offset+11])<<8
			sampleRate = int32(data[offset+12]) | int32(data[offset+13])<<8 | int32(data[offset+14])<<16 | int32(data[offset+15])<<24
			bitsPerSample = int16(data[offset+22]) | int16(data[offset+23])<<8
			// Validate sample rate is positive
			if sampleRate <= 0 {
				return nil, 0, fmt.Errorf("invalid sample rate: %d", sampleRate)
			}
		} else if chunkID == "data" {
			if int64(offset)+8+chunkSize > int64(len(data)) {
				return nil, 0, errors.New("invalid data chunk")
			}
			chunkSizeInt := int(chunkSize)
			pcmData := data[offset+8 : offset+8+chunkSizeInt]

			// Convert to mono 16kHz if needed
			pcmData, sampleRate, err := convertToMono16kHz(pcmData, sampleRate, numChannels, bitsPerSample)
			if err != nil {
				return nil, 0, fmt.Errorf("WAV conversion failed: %w", err)
			}
			return pcmData, sampleRate, nil
		}

		offset += 8 + int(chunkSize)
		if chunkSize%2 == 1 {
			offset++ // Pad byte
		}
	}

	return nil, 0, errors.New("data chunk not found")
}

// convertToMono16kHz converts audio to mono 16-bit 16kHz PCM
func convertToMono16kHz(data []byte, sampleRate int32, channels int16, bitsPerSample int16) ([]byte, int32, error) {
	targetRate := int32(16000)

	// Guard against invalid sample rate to prevent division by zero and OOM attacks
	// Valid sample rates are typically 8000-192000 Hz
	if sampleRate < 4000 || sampleRate > 384000 {
		return nil, 0, fmt.Errorf("unsupported sample rate: %d (must be between 4000 and 384000)", sampleRate)
	}

	// If already 16-bit mono at target rate, return as-is
	if channels == 1 && bitsPerSample == 16 && sampleRate == targetRate {
		return data, sampleRate, nil
	}

	// Convert to 16-bit first
	var samples []int16
	if bitsPerSample == 8 {
		// 8-bit unsigned to 16-bit signed
		samples = make([]int16, len(data))
		for i, b := range data {
			samples[i] = int16(b-128) << 8
		}
	} else if bitsPerSample == 16 {
		samples = make([]int16, len(data)/2)
		for i := range samples {
			samples[i] = int16(data[i*2]) | int16(data[i*2+1])<<8
		}
	} else {
		return nil, 0, fmt.Errorf("unsupported bits per sample: %d (supported: 8, 16)", bitsPerSample)
	}

	// Convert to mono
	if channels > 1 {
		monoSamples := make([]int16, len(samples)/int(channels))
		for i := 0; i < len(monoSamples); i++ {
			sum := int32(0)
			for ch := 0; ch < int(channels); ch++ {
				sum += int32(samples[i*int(channels)+ch])
			}
			monoSamples[i] = int16(sum / int32(channels))
		}
		samples = monoSamples
	}

	// Resample if needed (simple nearest-neighbor for now)
	if sampleRate != targetRate {
		ratio := float64(targetRate) / float64(sampleRate)
		targetLen := int(float64(len(samples)) * ratio)
		resampled := make([]int16, targetLen)
		for i := 0; i < targetLen; i++ {
			srcIdx := int(float64(i) / ratio)
			if srcIdx < len(samples) {
				resampled[i] = samples[srcIdx]
			}
		}
		samples = resampled
	}

	// Convert back to bytes
	result := make([]byte, len(samples)*2)
	for i, s := range samples {
		result[i*2] = byte(s)
		result[i*2+1] = byte(s >> 8)
	}

	return result, targetRate, nil
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
