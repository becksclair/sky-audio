package audio

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestDetectFormat(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		expected string
	}{
		{
			name:     "WAV format",
			data:     []byte("RIFFxxxxWAVE"),
			expected: FormatWAV,
		},
		{
			name:     "MP3 with ID3 tag",
			data:     append([]byte("ID3xxx"), make([]byte, 6)...),
			expected: FormatMP3,
		},
		{
			name:     "MP3 with sync word",
			data:     append([]byte{0xFF, 0xE0, 0x00, 0x00}, make([]byte, 8)...),
			expected: FormatMP3,
		},
		{
			name:     "OGG format",
			data:     append([]byte("OggSxxxx"), make([]byte, 4)...),
			expected: FormatOGG,
		},
		{
			name:     "FLAC format",
			data:     append([]byte("fLaCxxxx"), make([]byte, 4)...),
			expected: FormatFLAC,
		},
		{
			name:     "M4A format (ftyp M4A )",
			data:     []byte{0x00, 0x00, 0x00, 0x00, 'f', 't', 'y', 'p', 'M', '4', 'A', ' '},
			expected: FormatM4A,
		},
		{
			name:     "M4A format (ftyp m4a )",
			data:     []byte{0x00, 0x00, 0x00, 0x00, 'f', 't', 'y', 'p', 'm', '4', 'a', ' '},
			expected: FormatM4A,
		},
		{
			name:     "3GP format (ftyp 3gp4)",
			data:     []byte{0x00, 0x00, 0x00, 0x00, 'f', 't', 'y', 'p', '3', 'g', 'p', '4'},
			expected: Format3GP,
		},
		{
			name:     "WebM format (EBML header)",
			data:     append([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x00, 0x00, 0x00, 0x00}, make([]byte, 4)...),
			expected: FormatWebM,
		},
		{
			name:     "Empty data",
			data:     []byte{},
			expected: "unknown",
		},
		{
			name:     "Too short for detection",
			data:     []byte("RIFF"),
			expected: "unknown",
		},
		{
			name:     "Unknown format",
			data:     []byte("XXXXYYYYZZZZ"),
			expected: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DetectFormat(tt.data)
			if result != tt.expected {
				t.Errorf("DetectFormat() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestFFmpegDecoder_Supports(t *testing.T) {
	decoder := &FFmpegDecoder{}

	tests := []struct {
		format   string
		expected bool
	}{
		{FormatMP3, true},
		{FormatOGG, true},
		{FormatFLAC, true},
		{FormatM4A, true},
		{FormatWebM, true},
		{FormatOpus, true},
		{Format3GP, true},
		{"unknown", true}, // ffmpeg can try to probe unknown formats
		{FormatWAV, false},
		{"custom", false},
	}

	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			result := decoder.Supports(tt.format)
			if result != tt.expected {
				t.Errorf("Supports(%q) = %v, want %v", tt.format, result, tt.expected)
			}
		})
	}
}

func TestFFmpegDecoder_Decode_EmptyInput(t *testing.T) {
	decoder := &FFmpegDecoder{}
	ctx := context.Background()

	_, err := decoder.Decode(ctx, []byte{}, FormatMP3)
	if err == nil {
		t.Error("Expected error for empty input, got nil")
	}
}

func TestFFmpegDecoder_Decode_WithMockRunner(t *testing.T) {
	mockPCM := []byte{0x01, 0x02, 0x03, 0x04}
	decoder := &FFmpegDecoder{
		CommandRunner: func(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
			return mockPCM, []byte{}, nil
		},
	}

	ctx := context.Background()
	result, err := decoder.Decode(ctx, []byte("fake mp3 data"), FormatMP3)

	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if !bytes.Equal(result.PCM, mockPCM) {
		t.Errorf("PCM mismatch: got %v, want %v", result.PCM, mockPCM)
	}

	if result.SampleRate != TargetSampleRate {
		t.Errorf("SampleRate = %d, want %d", result.SampleRate, TargetSampleRate)
	}
}

func TestFFmpegDecoder_Decode_RunnerError(t *testing.T) {
	decoder := &FFmpegDecoder{
		CommandRunner: func(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
			return nil, []byte("error: invalid format"), errors.New("ffmpeg failed")
		},
	}

	ctx := context.Background()
	_, err := decoder.Decode(ctx, []byte("fake data"), FormatMP3)

	if err == nil {
		t.Error("Expected error when runner fails, got nil")
	}

	if !bytes.Contains([]byte(err.Error()), []byte("ffmpeg decode failed")) {
		t.Errorf("Error should mention ffmpeg failure, got: %v", err)
	}
}

func TestFFmpegDecoder_Decode_OutputSizeLimit(t *testing.T) {
	// Create output that exceeds MaxDecodedPCMBytes
	hugePCM := make([]byte, MaxDecodedPCMBytes+1000)
	decoder := &FFmpegDecoder{
		CommandRunner: func(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
			return hugePCM, []byte{}, nil
		},
	}

	ctx := context.Background()
	_, err := decoder.Decode(ctx, []byte("fake data"), FormatMP3)

	if err == nil {
		t.Error("Expected error for oversized output, got nil")
	}

	if !bytes.Contains([]byte(err.Error()), []byte("exceeds maximum size")) {
		t.Errorf("Error should mention size limit, got: %v", err)
	}
}

func TestIsFfmpegAvailable(t *testing.T) {
	// This test just exercises the function - actual availability depends on environment
	// We can't assert a specific result without controlling the test environment
	_ = IsFfmpegAvailable()
}
