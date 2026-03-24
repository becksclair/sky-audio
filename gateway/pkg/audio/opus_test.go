package audio

import (
	"testing"
)

func TestOpusDecoderSilence(t *testing.T) {
	dec, err := NewOpusDecoder()
	if err != nil {
		t.Fatalf("NewOpusDecoder: %v", err)
	}

	// Minimal Opus silence frame (DTX/comfort noise)
	silenceFrame := []byte{0xF8, 0xFF, 0xFE}

	pcm, err := dec.Decode(silenceFrame)
	if err != nil {
		t.Fatalf("Decode silence: %v", err)
	}

	if len(pcm) == 0 {
		t.Fatal("Expected non-empty PCM output")
	}

	// At 16kHz, a 20ms frame = 320 samples = 640 bytes
	t.Logf("Decoded %d bytes of PCM (%d samples)", len(pcm), len(pcm)/2)

	if len(pcm) < 320 {
		t.Errorf("Expected at least 320 bytes (10ms), got %d", len(pcm))
	}
}

func TestOpusDecoderMultipleFrames(t *testing.T) {
	dec, err := NewOpusDecoder()
	if err != nil {
		t.Fatalf("NewOpusDecoder: %v", err)
	}

	silenceFrame := []byte{0xF8, 0xFF, 0xFE}
	totalBytes := 0

	for i := 0; i < 50; i++ { // 50 frames = 1 second at 20ms
		pcm, err := dec.Decode(silenceFrame)
		if err != nil {
			t.Fatalf("Decode frame %d: %v", i, err)
		}
		totalBytes += len(pcm)
	}

	// 1 second of 16kHz 16-bit mono = 32000 bytes
	t.Logf("50 frames decoded to %d bytes (expected ~32000)", totalBytes)

	if totalBytes < 25000 || totalBytes > 40000 {
		t.Errorf("Expected ~32000 bytes for 1s, got %d", totalBytes)
	}
}
