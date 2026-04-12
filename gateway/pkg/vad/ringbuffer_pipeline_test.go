package vad

import (
	"bytes"
	"testing"
)

// TestRingBufferSimulatedPipelineUsage simulates the exact usage pattern
// from pipeline.go to identify any wraparound issues
func TestRingBufferSimulatedPipelineUsage(t *testing.T) {
	// Same size as in pipeline.go: 16000 bytes (500ms of 16kHz 16-bit PCM)
	const ringBufferSize = 16000
	rb := NewRingBuffer(ringBufferSize)

	// Simulate incoming PCM frames of 20ms each = 640 bytes
	const frameSize = 640 // 20ms of 16kHz 16-bit mono

	makeFrame := func(seqNum int) []byte {
		frame := make([]byte, frameSize)
		// Fill with pattern based on sequence number
		for i := range frame {
			frame[i] = byte((seqNum + i) % 256)
		}
		return frame
	}

	// Write frames to fill the buffer (25 frames = 500ms)
	for i := 0; i < 25; i++ {
		rb.Write(makeFrame(i * frameSize))
	}

	// Buffer should now be exactly full
	// Now write 5 more frames to force wraparound
	for i := 25; i < 30; i++ {
		rb.Write(makeFrame(i * frameSize))
	}

	// Flush and verify
	flushed := rb.Flush()

	// Should have exactly ringBufferSize bytes
	if len(flushed) != ringBufferSize {
		t.Fatalf("Expected %d bytes, got %d", ringBufferSize, len(flushed))
	}

	// Build expected data - should be the last 16000 bytes written
	// We wrote 30 frames total = 19200 bytes, so last 16000 = bytes 3200-19199
	// But in terms of frame patterns:
	// Frames 5-29 (25 frames) = exactly ringBufferSize

	expected := make([]byte, 0, ringBufferSize)
	for i := 5; i < 30; i++ {
		expected = append(expected, makeFrame(i*frameSize)...)
	}

	if !bytes.Equal(flushed, expected) {
		t.Errorf("Pipeline simulation failed! Flushed data doesn't match expected")
		// Print first few bytes for debugging
		t.Errorf("First 20 flushed:  %v", flushed[:20])
		t.Errorf("First 20 expected: %v", expected[:20])
		t.Errorf("Last 20 flushed:   %v", flushed[ringBufferSize-20:])
		t.Errorf("Last 20 expected:  %v", expected[ringBufferSize-20:])
	}
}

// TestRingBufferPosTracking verifies position tracking during writes
func TestRingBufferPosTracking(t *testing.T) {
	const capacity = 20
	rb := NewRingBuffer(capacity)

	// Access internal state for verification (we'd need to export these or use reflection)
	// For now, let's trace manually:

	// Write 15 bytes: pos=15, size=15
	rb.Write([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14})

	// Write 10 more bytes (wraps):
	// First 5 go to positions 15-19
	// Next 5 wrap to positions 0-4
	// After: pos = (15+10)%20 = 5, size = 20 (capped)
	rb.Write([]byte{100, 101, 102, 103, 104, 105, 106, 107, 108, 109})

	// Flush should return: [5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 100, 101, 102, 103, 104, 105, 106, 107, 108, 109]
	flushed := rb.Flush()

	expected := []byte{5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 100, 101, 102, 103, 104, 105, 106, 107, 108, 109}

	if !bytes.Equal(flushed, expected) {
		t.Errorf("Pos tracking test failed!\nExpected: %v\nGot:      %v", expected, flushed)
	}
}
