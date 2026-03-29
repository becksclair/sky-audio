package e2e

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/heliasar/sky-audio/gateway/pkg/audio"
	"github.com/heliasar/sky-audio/gateway/pkg/openaihttp"
	skywebrtc "github.com/heliasar/sky-audio/gateway/pkg/webrtc"
)

// TestSTTFormatConversion_Comprehensive tests the STT endpoint with various input formats
// by generating test audio files using ffmpeg and verifying successful transcription.
// This test requires ffmpeg to be installed and STT workers to be available.
func TestSTTFormatConversion_Comprehensive(t *testing.T) {
	socketPath := "/tmp/sky-audio-stt.sock"
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		t.Skip("Skipping E2E test: STT worker socket not found at " + socketPath)
	}

	// Verify ffmpeg is available
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("Skipping E2E test: ffmpeg not found in PATH")
	}

	// Verify STT worker is available
	client, err := skywebrtc.NewSTTClient(socketPath)
	if err != nil {
		t.Fatalf("Failed to connect to STT worker: %v", err)
	}
	client.Close()

	// Create handler with lazy loading support
	ensureRunning := func() func() { return func() {} }
	handler := openaihttp.TranscriptionsHandlerWithEnsureRunning(socketPath, ensureRunning)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()

	tests := []struct {
		name         string
		format       string
		generateFunc func(t *testing.T) []byte
		wantStatus   int
		skipReason   string
	}{
		{
			name:   "wav_pcm_16khz_mono",
			format: "wav",
			generateFunc: func(t *testing.T) []byte {
				return generateTestAudio(t, "wav", "s16le", 16000, 1, 1000) // 1 second
			},
			wantStatus: http.StatusOK,
		},
		{
			name:   "wav_pcm_44khz_stereo_downmixed",
			format: "wav",
			generateFunc: func(t *testing.T) []byte {
				// Higher sample rate and stereo - tests resampling and downmixing
				return generateTestAudio(t, "wav", "s16le", 44100, 2, 500)
			},
			wantStatus: http.StatusOK,
		},
		{
			name:   "mp3_stereo_44khz",
			format: "mp3",
			generateFunc: func(t *testing.T) []byte {
				return generateTestAudio(t, "mp3", "", 44100, 2, 1000)
			},
			wantStatus: http.StatusOK,
		},
		{
			name:   "ogg_opus",
			format: "ogg",
			generateFunc: func(t *testing.T) []byte {
				return generateTestAudio(t, "ogg", "libopus", 48000, 1, 1000)
			},
			wantStatus: http.StatusOK,
		},
		{
			name:   "ogg_vorbis",
			format: "ogg",
			generateFunc: func(t *testing.T) []byte {
				return generateTestAudio(t, "ogg", "libvorbis", 44100, 2, 1000)
			},
			wantStatus: http.StatusOK,
		},
		{
			name:   "flac",
			format: "flac",
			generateFunc: func(t *testing.T) []byte {
				return generateTestAudio(t, "flac", "", 44100, 2, 1000)
			},
			wantStatus: http.StatusOK,
		},
		{
			name:   "m4a_aac",
			format: "m4a",
			generateFunc: func(t *testing.T) []byte {
				return generateTestAudio(t, "m4a", "aac", 44100, 2, 1000)
			},
			wantStatus: http.StatusOK,
		},
		{
			name:   "webm_opus",
			format: "webm",
			generateFunc: func(t *testing.T) []byte {
				return generateTestAudio(t, "webm", "libopus", 48000, 1, 1000)
			},
			wantStatus: http.StatusOK,
		},
		{
			name:   "opus_raw",
			format: "opus",
			generateFunc: func(t *testing.T) []byte {
				return generateTestAudio(t, "opus", "libopus", 48000, 1, 1000)
			},
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.skipReason != "" {
				t.Skip(tt.skipReason)
			}

			audioData := tt.generateFunc(t)
			if len(audioData) == 0 {
				t.Fatal("Generated audio data is empty")
			}

			t.Logf("Generated %d bytes of %s audio", len(audioData), tt.format)

			// Verify format detection works
			detected := audio.DetectFormat(audioData)
			t.Logf("Format detected as: %s", detected)

			// Build multipart request
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, err := writer.CreateFormFile("file", "test."+tt.format)
			if err != nil {
				t.Fatalf("Failed to create form file: %v", err)
			}
			if _, err := part.Write(audioData); err != nil {
				t.Fatalf("Failed to write audio data: %v", err)
			}
			_ = writer.WriteField("model", "whisper-1")
			writer.Close()

			req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/audio/transcriptions", &body)
			req.Header.Set("Content-Type", writer.FormDataContentType())

			client := &http.Client{Timeout: 45 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Request failed: %v", err)
			}
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("Expected status %d, got %d: %s", tt.wantStatus, resp.StatusCode, string(respBody))
			} else {
				t.Logf("Request succeeded: %s", string(respBody))
			}
		})
	}
}

// TestSTTFormatConversion_ErrorCases tests error handling for malformed files
func TestSTTFormatConversion_ErrorCases(t *testing.T) {
	socketPath := "/tmp/sky-audio-stt.sock"
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		t.Skip("Skipping E2E test: STT worker socket not found")
	}

	ensureRunning := func() func() { return func() {} }
	handler := openaihttp.TranscriptionsHandlerWithEnsureRunning(socketPath, ensureRunning)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()

	tests := []struct {
		name       string
		filename   string
		data       []byte
		wantStatus int
		wantError  string
	}{
		{
			name:       "empty_file",
			filename:   "empty.mp3",
			data:       []byte{},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "truncated_wav_header",
			filename:   "truncated.wav",
			data:       []byte("RIFF"),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "invalid_wav_no_data_chunk",
			filename:   "invalid.wav",
			data:       []byte("RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00"),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "random_bytes",
			filename:   "random.bin",
			data:       []byte("\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09"),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "corrupted_mp3",
			filename:   "corrupted.mp3",
			data:       []byte("ID3\x00\x00\x00\x00\x00\x00invalid data here"),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "fake_ogg",
			filename:   "fake.ogg",
			data:       []byte("OggS\x00\x00\x00\x00invalid ogg data"),
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, _ := writer.CreateFormFile("file", tt.filename)
			part.Write(tt.data)
			_ = writer.WriteField("model", "whisper-1")
			writer.Close()

			req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/audio/transcriptions", &body)
			req.Header.Set("Content-Type", writer.FormDataContentType())

			client := &http.Client{Timeout: 10 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Request failed: %v", err)
			}
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("Expected status %d, got %d: %s", tt.wantStatus, resp.StatusCode, string(respBody))
			} else {
				t.Logf("Got expected error: %s", string(respBody))
			}
		})
	}
}

// TestSTTFormatConversion_SecurityBoundaries tests size limits and resource constraints
func TestSTTFormatConversion_SecurityBoundaries(t *testing.T) {
	socketPath := "/tmp/sky-audio-stt.sock"
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		t.Skip("Skipping E2E test: STT worker socket not found")
	}

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("Skipping E2E test: ffmpeg not found")
	}

	ensureRunning := func() func() { return func() {} }
	handler := openaihttp.TranscriptionsHandlerWithEnsureRunning(socketPath, ensureRunning)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()

	tests := []struct {
		name       string
		setupFunc  func(t *testing.T) (*bytes.Buffer, string)
		wantStatus int
	}{
		{
			name: "oversized_upload_exceeds_25mb",
			setupFunc: func(t *testing.T) (*bytes.Buffer, string) {
				// Create a fake large file that exceeds 25MB
				// Since MP3 compresses silence too well, we create raw data
				// that looks like a valid format header but has large declared size
				var body bytes.Buffer
				writer := multipart.NewWriter(&body)
				part, _ := writer.CreateFormFile("file", "large.mp3")

				// Write valid MP3 header (ID3) followed by padding to exceed 25MB
				part.Write([]byte("ID3\x04\x00\x00\x00\x00\x00\x00")) // ID3v2.4 header
				// Add large padding to exceed 25MB
				padding := make([]byte, 26*1024*1024) // 26MB
				part.Write(padding)

				_ = writer.WriteField("model", "whisper-1")
				writer.Close()
				return &body, writer.FormDataContentType()
			},
			wantStatus: http.StatusBadRequest, // Should be rejected by size limit
		},
		{
			name: "valid_small_file",
			setupFunc: func(t *testing.T) (*bytes.Buffer, string) {
				audioData := generateTestAudio(t, "mp3", "", 16000, 1, 500) // 500ms
				var body bytes.Buffer
				writer := multipart.NewWriter(&body)
				part, _ := writer.CreateFormFile("file", "small.mp3")
				part.Write(audioData)
				_ = writer.WriteField("model", "whisper-1")
				writer.Close()
				return &body, writer.FormDataContentType()
			},
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, contentType := tt.setupFunc(t)

			req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/audio/transcriptions", body)
			req.Header.Set("Content-Type", contentType)

			client := &http.Client{Timeout: 60 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Request failed: %v", err)
			}
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("Expected status %d, got %d: %s", tt.wantStatus, resp.StatusCode, string(respBody))
			}
		})
	}
}

// TestSTTFormatConversion_RealSpeech tests with actual speech audio if available
func TestSTTFormatConversion_RealSpeech(t *testing.T) {
	socketPath := "/tmp/sky-audio-stt.sock"
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		t.Skip("Skipping E2E test: STT worker socket not found")
	}

	// Look for test speech files
	testFiles := []string{
		"/tmp/test-speech.wav",
		"/tmp/test-speech.mp3",
		"testdata/speech.wav",
	}

	var availableFile string
	for _, path := range testFiles {
		if _, err := os.Stat(path); err == nil {
			availableFile = path
			break
		}
	}

	if availableFile == "" {
		t.Skip("Skipping E2E test: No test speech files found. Place a test file at /tmp/test-speech.wav or similar.")
	}

	audioData, err := os.ReadFile(availableFile)
	if err != nil {
		t.Fatalf("Failed to read test file: %v", err)
	}

	t.Logf("Using test file: %s (%d bytes)", availableFile, len(audioData))

	// Detect format
	detected := audio.DetectFormat(audioData)
	t.Logf("Detected format: %s", detected)

	// If not WAV, convert it first using ffmpeg to verify our converter works
	if detected != audio.FormatWAV {
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			t.Skip("Test file is not WAV and ffmpeg not available for comparison")
		}

		// Verify ffmpeg can decode it
		cmd := exec.Command("ffmpeg", "-v", "error", "-i", availableFile, "-f", "null", "-")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("Warning: ffmpeg reports issues with the file: %v: %s", err, string(output))
		}
	}

	// Test with the actual file
	ensureRunning := func() func() { return func() {} }
	handler := openaihttp.TranscriptionsHandlerWithEnsureRunning(socketPath, ensureRunning)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", filepath.Base(availableFile))
	part.Write(audioData)
	_ = writer.WriteField("model", "whisper-1")
	_ = writer.WriteField("response_format", "verbose_json")
	writer.Close()

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d: %s", resp.StatusCode, string(respBody))
	} else {
		t.Logf("Transcription result: %s", string(respBody))
	}
}

// generateTestAudio creates test audio data using ffmpeg
// format: output container format (wav, mp3, ogg, etc.)
// codec: audio codec (libopus, aac, libvorbis, etc.) or "" for default
// sampleRate: output sample rate
// channels: number of channels
// durationMs: duration in milliseconds
func generateTestAudio(t *testing.T, format, codec string, sampleRate, channels, durationMs int) []byte {
	// Create a temporary output file
	tmpDir := os.TempDir()
	outputFile := filepath.Join(tmpDir, "test_audio_"+time.Now().Format("20060102150405.000")+"."+format)
	defer os.Remove(outputFile)

	args := []string{
		"-f", "lavfi",
		"-i", fmt.Sprintf("anullsrc=r=%d:cl=mono", sampleRate),
		"-t", fmt.Sprintf("%dms", durationMs),
		"-ar", strconv.Itoa(sampleRate),
		"-ac", strconv.Itoa(channels),
	}

	// Format-specific options
	switch format {
	case "wav":
		// WAV PCM 16-bit (pcm_s16le is the encoder name)
		args = append(args, "-c:a", "pcm_s16le")
	case "ogg":
		if codec == "libopus" {
			args = append(args, "-c:a", "libopus", "-b:a", "64k")
		} else if codec == "libvorbis" || codec == "" {
			// Try libvorbis first, but it may not be available
			args = append(args, "-c:a", "libvorbis", "-q:a", "4")
		}
	case "mp3":
		args = append(args, "-q:a", "4")
	case "m4a":
		if codec == "" {
			args = append(args, "-c:a", "aac")
		} else {
			args = append(args, "-c:a", codec)
		}
		args = append(args, "-b:a", "128k")
	case "flac":
		args = append(args, "-compression_level", "5")
	case "opus":
		args = append(args, "-c:a", "libopus", "-b:a", "64k")
	case "webm":
		args = append(args, "-c:a", "libopus", "-b:a", "64k")
	default:
		if codec != "" {
			args = append(args, "-c:a", codec)
		}
	}

	args = append(args, outputFile)

	cmd := exec.Command("ffmpeg", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("ffmpeg command: ffmpeg %s", strings.Join(args, " "))
		t.Logf("ffmpeg output: %s", string(output))
		t.Fatalf("Failed to generate %s audio: %v", format, err)
	}

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("Failed to read generated audio: %v", err)
	}

	return data
}

// TestSTTFormatConversion_FFMPEGAvailability checks ffmpeg availability and version
func TestSTTFormatConversion_FFMPEGAvailability(t *testing.T) {
	cmd := exec.Command("ffmpeg", "-version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg not available: %v", err)
	}

	lines := strings.Split(string(output), "\n")
	if len(lines) > 0 {
		t.Logf("ffmpeg version: %s", lines[0])
	}

	// Check available codecs
	cmd = exec.Command("ffmpeg", "-encoders")
	output, _ = cmd.CombinedOutput()

	codecs := []string{"libopus", "aac", "libvorbis", "flac"}
	for _, codec := range codecs {
		if strings.Contains(string(output), codec) {
			t.Logf("Codec available: %s", codec)
		} else {
			t.Logf("Codec NOT available: %s", codec)
		}
	}

	// Verify all expected codecs are present
	requiredCodecs := []string{"libopus", "aac", "libvorbis", "flac"}
	for _, codec := range requiredCodecs {
		if !strings.Contains(string(output), codec) {
			t.Errorf("Required codec not available: %s", codec)
		}
	}
}
