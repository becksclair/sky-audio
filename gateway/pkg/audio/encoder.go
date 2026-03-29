// Package audio provides audio format conversion utilities for STT/TTS processing.
package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"time"
)

const (
	// EncodeTimeout limits how long audio encoding can take.
	EncodeTimeout = 30 * time.Second

	// MaxEncodedAudioBytes limits encoded audio output.
	// 16kHz * 2 bytes * 1 channel * 600 seconds (10 min) compressed = ~5MB worst case
	MaxEncodedAudioBytes = 50 * 1024 * 1024
)

// EncodeResult holds the output of audio encoding.
type EncodeResult struct {
	Body        []byte
	ContentType string
}

// Encoder converts PCM audio to various output formats.
type Encoder interface {
	// Encode converts PCM data to the target format.
	// format: "wav", "pcm", "mp3", "opus", "aac", "flac"
	Encode(ctx context.Context, pcm []byte, sampleRate int32, format string) (*EncodeResult, error)

	// Supports reports whether this encoder can handle the given format.
	Supports(format string) bool
}

// FFmpegEncoder uses ffmpeg to encode audio to various formats.
type FFmpegEncoder struct {
	// CommandRunner allows test injection. If nil, uses default exec.CommandContext.
	CommandRunner func(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error)
}

// Supports reports supported formats.
func (e *FFmpegEncoder) Supports(format string) bool {
	switch format {
	case "wav", "pcm", "mp3", "opus", "aac", "flac", "ogg":
		return true
	default:
		return false
	}
}

// Encode converts PCM to the target format.
func (e *FFmpegEncoder) Encode(ctx context.Context, pcm []byte, sampleRate int32, format string) (*EncodeResult, error) {
	if len(pcm) == 0 {
		return nil, errors.New("empty input data")
	}

	// Normalize format (ogg is an alias for opus)
	normalizedFormat := format
	if normalizedFormat == "ogg" {
		normalizedFormat = "opus"
	}

	// Fast path for PCM (pass-through)
	if normalizedFormat == "pcm" {
		return &EncodeResult{
			Body:        pcm,
			ContentType: "audio/pcm; rate=" + strconv.Itoa(int(sampleRate)),
		}, nil
	}

	// Fast path for WAV (native implementation)
	if normalizedFormat == "wav" {
		var buf bytes.Buffer
		if err := WriteWAV(&buf, pcm, sampleRate); err != nil {
			return nil, fmt.Errorf("wav encoding failed: %w", err)
		}
		return &EncodeResult{
			Body:        buf.Bytes(),
			ContentType: "audio/wav",
		}, nil
	}

	// Compressed formats via ffmpeg
	ctx, cancel := context.WithTimeout(ctx, EncodeTimeout)
	defer cancel()

	runner := e.CommandRunner
	if runner == nil {
		runner = defaultFfmpegEncodeRunner
	}

	// Build ffmpeg arguments based on format
	args := buildEncodeArgs(normalizedFormat, sampleRate)

	stdout, stderr, err := runner(ctx, pcm, args...)
	if err != nil {
		errMsg := string(stderr)
		if len(errMsg) > 500 {
			errMsg = errMsg[:500] + "..."
		}
		return nil, fmt.Errorf("ffmpeg encode failed: %w (stderr: %s)", err, errMsg)
	}

	if len(stdout) > MaxEncodedAudioBytes {
		return nil, fmt.Errorf("encoded audio exceeds maximum size (%d bytes > %d bytes)",
			len(stdout), MaxEncodedAudioBytes)
	}

	contentType := formatToContentType(normalizedFormat)
	return &EncodeResult{
		Body:        stdout,
		ContentType: contentType,
	}, nil
}

// buildEncodeArgs creates ffmpeg command arguments for the target format.
func buildEncodeArgs(format string, sampleRate int32) []string {
	// Common input args: raw PCM s16le, mono, specific sample rate
	inputArgs := []string{
		"-nostdin",
		"-v", "error",
		"-f", "s16le",
		"-ac", "1",
		"-ar", strconv.Itoa(int(sampleRate)),
		"-i", "pipe:0",
	}

	// Output format args
	var outputArgs []string
	switch format {
	case "opus":
		// Ogg Opus - best for voice, streaming friendly
		outputArgs = []string{
			"-f", "ogg",
			"-c:a", "libopus",
			"-b:a", "24k", // 24kbps is good for voice
			"-vbr", "on",
			"-application", "voip", // optimize for voice
			"-threads", "1",
			"pipe:1",
		}
	case "mp3":
		// MP3 - widely compatible
		outputArgs = []string{
			"-f", "mp3",
			"-c:a", "libmp3lame",
			"-q:a", "4", // VBR quality (0=best, 9=worst)
			"-threads", "1",
			"pipe:1",
		}
	case "aac":
		// AAC - ADTS container for streaming
		outputArgs = []string{
			"-f", "adts",
			"-c:a", "aac",
			"-b:a", "24k",
			"-threads", "1",
			"pipe:1",
		}
	case "flac":
		// FLAC - lossless compression
		outputArgs = []string{
			"-f", "flac",
			"-c:a", "flac",
			"-compression_level", "5",
			"-threads", "1",
			"pipe:1",
		}
	default:
		// Fallback to wav via ffmpeg (shouldn't happen with proper validation)
		outputArgs = []string{
			"-f", "wav",
			"-threads", "1",
			"pipe:1",
		}
	}

	return append(inputArgs, outputArgs...)
}

// formatToContentType returns the MIME type for the format.
func formatToContentType(format string) string {
	switch format {
	case "opus":
		return "audio/ogg; codecs=opus"
	case "mp3":
		return "audio/mpeg"
	case "aac":
		return "audio/aac"
	case "flac":
		return "audio/flac"
	case "wav":
		return "audio/wav"
	case "pcm":
		return "audio/pcm"
	default:
		return "application/octet-stream"
	}
}

// defaultFfmpegEncodeRunner executes ffmpeg for encoding with stdin/stdout pipes.
func defaultFfmpegEncodeRunner(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)

	// Set up stdin pipe
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("ffmpeg stdin pipe: %w", err)
	}

	// Set up stdout capture
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}

	// Set up stderr capture with bounded buffer to prevent memory exhaustion
	stderrBuf := newBoundedBuffer(MaxStderrBytes)
	cmd.Stderr = stderrBuf

	// Start the command
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("ffmpeg start: %w", err)
	}

	// Write input data to stdin in a goroutine
	stdinDone := make(chan error, 1)
	go func() {
		defer close(stdinDone)
		_, err := stdinPipe.Write(stdin)
		if err != nil {
			stdinDone <- fmt.Errorf("ffmpeg stdin write: %w", err)
			return
		}
		if err := stdinPipe.Close(); err != nil {
			stdinDone <- fmt.Errorf("ffmpeg stdin close: %w", err)
		}
	}()

	// Read stdout with a limit to prevent memory exhaustion
	// Start with small buffer, grow as needed up to MaxEncodedAudioBytes
	stdout := make([]byte, 0, 64*1024)
	tmpBuf := make([]byte, 64*1024) // 64KB chunks

	var readErr error
	for {
		n, err := stdoutPipe.Read(tmpBuf)
		if n > 0 {
			stdout = append(stdout, tmpBuf[:n]...)
			if len(stdout) > MaxEncodedAudioBytes {
				// Force kill the process, we've hit the limit
				_ = cmd.Process.Kill()
				// Drain stdin goroutine before returning
				<-stdinDone
				// Wait for process to fully terminate and reap it
				_ = cmd.Wait()
				return nil, stderrBuf.Bytes(), errors.New("encoded output exceeds size limit")
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			readErr = fmt.Errorf("ffmpeg stdout read: %w", err)
			break
		}
	}

	// Wait for command to complete
	waitErr := cmd.Wait()

	// Always drain stdin goroutine before returning
	<-stdinDone

	// If we had a read error, return that
	if readErr != nil {
		return nil, stderrBuf.Bytes(), readErr
	}

	// If ffmpeg exited with error, return that
	if waitErr != nil {
		return nil, stderrBuf.Bytes(), waitErr
	}

	// Successful completion - ignore any stdin errors since ffmpeg succeeded
	return stdout, stderrBuf.Bytes(), nil
}
