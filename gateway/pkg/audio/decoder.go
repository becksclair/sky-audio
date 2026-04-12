// Package audio provides audio format conversion utilities for STT processing.
// It supports multiple input formats (WAV, MP3, OGG, M4A, WebM, etc.) and converts
// them to 16-bit PCM at 16kHz mono, which is required by the STT workers.
package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"time"
)

const (
	// MaxDecodedPCMBytes limits decoded audio to prevent resource exhaustion.
	// 16kHz * 2 bytes * 1 channel * 600 seconds (10 min) = ~19MB
	MaxDecodedPCMBytes = 20 * 1024 * 1024

	// DecodeTimeout limits how long format conversion can take.
	DecodeTimeout = 30 * time.Second

	// TargetSampleRate is the required output sample rate (16kHz).
	TargetSampleRate = 16000

	// BytesPerSample is the size of a 16-bit PCM sample (2 bytes).
	BytesPerSample = 2

	// MaxStderrBytes limits stderr capture to prevent memory exhaustion.
	MaxStderrBytes = 64 * 1024
)

// DecodeResult holds the output of audio format conversion.
type DecodeResult struct {
	PCM        []byte
	SampleRate int32
}

// Decoder converts audio from various formats to 16-bit PCM at 16kHz mono.
type Decoder interface {
	// Name returns the decoder identifier (for logging/debugging).
	Name() string

	// Supports reports whether this decoder can handle the given format.
	Supports(format string) bool

	// Decode converts the input audio data to 16-bit PCM at 16kHz mono.
	// The format hint comes from detectAudioFormat; the decoder may ignore
	// it if it does its own format detection.
	Decode(ctx context.Context, data []byte, format string) (*DecodeResult, error)
}

// Format definitions for decoder support.
const (
	FormatWAV  = "wav"
	FormatMP3  = "mp3"
	FormatOGG  = "ogg"
	FormatFLAC = "flac"
	FormatM4A  = "m4a"
	FormatWebM = "webm"
	FormatOpus = "opus"
	Format3GP  = "3gp"
)

// DetectFormat identifies the audio format from magic bytes.
// It returns the format identifier or "unknown" if not recognized.
func DetectFormat(data []byte) string {
	if len(data) < 12 {
		return "unknown"
	}

	// WAV: RIFF....WAVE
	if bytes.Equal(data[0:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WAVE")) {
		return FormatWAV
	}

	// MP3: ID3 tag or MPEG sync word (0xFFE0)
	if bytes.Equal(data[0:3], []byte("ID3")) || (data[0] == 0xFF && (data[1]&0xE0) == 0xE0) {
		return FormatMP3
	}

	// OGG: OggS
	if bytes.Equal(data[0:4], []byte("OggS")) {
		return FormatOGG
	}

	// FLAC: fLaC
	if bytes.Equal(data[0:4], []byte("fLaC")) {
		return FormatFLAC
	}

	// MP4/M4A/3GP: ftyp box at offset 4 with major brand
	if len(data) >= 12 {
		// Check for ftyp signature at offset 4
		if bytes.Equal(data[4:8], []byte("ftyp")) {
			// Major brand is at offset 8 (4 bytes)
			brand := string(data[8:12])
			switch brand {
			case "M4A ", "m4a ", "isom", "mp42", "M4B ", "M4P ":
				return FormatM4A
			case "3gp4", "3gp5", "3gp6", "3g2a":
				return Format3GP
			case "iso5", "iso6", "avc1":
				// Could be video or audio; let ffmpeg figure it out
				return FormatM4A
			}
		}
	}

	// WebM/Matroska: EBML header (1A 45 DF A3)
	if len(data) >= 4 && bytes.Equal(data[0:4], []byte{0x1A, 0x45, 0xDF, 0xA3}) {
		return FormatWebM
	}

	return "unknown"
}

// IsFfmpegAvailable checks if ffmpeg is installed and available in PATH.
func IsFfmpegAvailable() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// FFmpegDecoder uses ffmpeg binary to convert audio to PCM.
type FFmpegDecoder struct {
	// CommandRunner allows test injection. If nil, uses default exec.CommandContext.
	CommandRunner func(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error)
}

// Name returns the decoder identifier.
func (d *FFmpegDecoder) Name() string {
	return "ffmpeg"
}

// Supports returns true for all non-WAV formats that ffmpeg handles.
// We let ffmpeg handle almost everything except pure WAV.
func (d *FFmpegDecoder) Supports(format string) bool {
	switch format {
	case FormatMP3, FormatOGG, FormatFLAC, FormatM4A, FormatWebM, FormatOpus, Format3GP:
		return true
	case "unknown":
		// Let ffmpeg try to probe unknown formats
		return true
	default:
		return false
	}
}

// Decode converts audio to 16-bit PCM at 16kHz mono using ffmpeg.
func (d *FFmpegDecoder) Decode(ctx context.Context, data []byte, format string) (*DecodeResult, error) {
	if len(data) == 0 {
		return nil, errors.New("empty input data")
	}

	// Create timeout context for decode operation
	ctx, cancel := context.WithTimeout(ctx, DecodeTimeout)
	defer cancel()

	runner := d.CommandRunner
	if runner == nil {
		runner = defaultFfmpegRunner
	}

	// ffmpeg arguments:
	// -nostdin: don't read from stdin (we provide input via pipe)
	// -v error: only show errors
	// -i pipe:0: read input from stdin
	// -f s16le: output format is signed 16-bit little-endian PCM
	// -ac 1: mono output
	// -ar 16000: 16kHz sample rate
	// -threads 1: limit to single thread for predictable CPU usage
	// pipe:1: write output to stdout
	args := []string{
		"-nostdin",
		"-v", "error",
		"-i", "pipe:0",
		"-f", "s16le",
		"-ac", "1",
		"-ar", fmt.Sprintf("%d", TargetSampleRate),
		"-threads", "1",
		"pipe:1",
	}

	stdout, stderr, err := runner(ctx, data, args...)
	if err != nil {
		// Truncate stderr for logging (avoid huge error messages)
		errMsg := string(stderr)
		if len(errMsg) > 500 {
			errMsg = errMsg[:500] + "..."
		}
		return nil, fmt.Errorf("ffmpeg decode failed: %w (stderr: %s)", err, errMsg)
	}

	// Check decoded size limits
	if len(stdout) > MaxDecodedPCMBytes {
		return nil, fmt.Errorf("decoded audio exceeds maximum size (%d bytes > %d bytes)",
			len(stdout), MaxDecodedPCMBytes)
	}

	return &DecodeResult{
		PCM:        stdout,
		SampleRate: TargetSampleRate,
	}, nil
}

// boundedBuffer is a writer that caps memory usage.
type boundedBuffer struct {
	buf  []byte
	cap  int
	size int
}

func newBoundedBuffer(cap int) *boundedBuffer {
	return &boundedBuffer{cap: cap}
}

func (b *boundedBuffer) Write(p []byte) (n int, err error) {
	if b.size >= b.cap {
		// Buffer is full - still report success to prevent io.Copy ErrShortWrite
		// The buffer is explicitly lossy; io.Copy callers must not stall.
		return len(p), nil
	}
	
	originalLen := len(p)
	remaining := b.cap - b.size
	if len(p) > remaining {
		p = p[:remaining]
	}
	b.buf = append(b.buf, p...)
	b.size += len(p)
	return originalLen, nil
}

func (b *boundedBuffer) Bytes() []byte {
	return b.buf
}

// defaultFfmpegRunner executes ffmpeg with stdin/stdout pipes.
// It properly handles context cancellation by killing the entire process group.
func defaultFfmpegRunner(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)

	// Create a new process group so we can kill all child processes on cancellation
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

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

	// Ensure process is reaped and pipes are closed even on panic
	defer func() {
		stdinPipe.Close()
		stdoutPipe.Close()
	}()

	// Create a channel to signal when the process has exited
	processDone := make(chan struct{})

	// Start a goroutine to handle context cancellation
	// This goroutine will kill the process group when the context is cancelled
	go func() {
		select {
		case <-ctx.Done():
			// Context cancelled - kill the entire process group
			// Use negative PID to kill the entire process group
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		case <-processDone:
			// Process exited normally, nothing to do
		}
	}()

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
	// Start with small buffer, grow as needed up to MaxDecodedPCMBytes
	stdout := make([]byte, 0, 64*1024)
	tmpBuf := make([]byte, 64*1024) // 64KB chunks

	var readErr error
	for {
		n, err := stdoutPipe.Read(tmpBuf)
		if n > 0 {
			stdout = append(stdout, tmpBuf[:n]...)
			if len(stdout) > MaxDecodedPCMBytes {
				// Force kill the process group, we've hit the limit
				if cmd.Process != nil {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				}
				// Drain stdin goroutine before returning
				<-stdinDone
				// Wait for process to fully terminate and reap it
				_ = cmd.Wait()
				close(processDone)
				return nil, stderrBuf.Bytes(), errors.New("decoded output exceeds size limit")
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

	// Close stdout pipe to signal we're done reading
	stdoutPipe.Close()

	// Wait for command to complete
	waitErr := cmd.Wait()

	// Signal that the process has exited (stops the kill goroutine)
	close(processDone)

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
