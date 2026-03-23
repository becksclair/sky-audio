package vad

import (
	"testing"
)

// 16kHz, 16-bit mono = 32000 bytes/sec. 500ms = 16000 bytes.
const (
	sampleRate     = 16000
	bytesPerSample = 2
	bufferDuration = 500 // ms
	bufferBytes    = sampleRate * bytesPerSample * bufferDuration / 1000 // 16000
)

func makeSilence(n int) []byte { return make([]byte, n) }

func makeNoise(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 0x7F // loud-ish
	}
	return b
}

func TestRingBufferFlush(t *testing.T) {
	rb := NewRingBuffer(bufferBytes)

	// Write 500ms of silence (should fill the buffer)
	rb.Write(makeSilence(bufferBytes))

	// Flush should return exactly bufferBytes
	data := rb.Flush()
	if len(data) != bufferBytes {
		t.Fatalf("Expected buffer to contain exactly %d bytes (500ms of 16kHz 16-bit PCM), got %d bytes", bufferBytes, len(data))
	}
}

func TestRingBufferOverwrite(t *testing.T) {
	rb := NewRingBuffer(bufferBytes)

	// Write more than capacity — old data should be evicted
	rb.Write(makeSilence(bufferBytes))
	rb.Write(makeNoise(bufferBytes))

	data := rb.Flush()
	if len(data) != bufferBytes {
		t.Fatalf("Expected %d bytes, got %d", bufferBytes, len(data))
	}

	// All bytes should be noise (0x7F), not silence
	for i, b := range data {
		if b != 0x7F {
			t.Fatalf("Byte %d: expected 0x7F (noise), got 0x%02X", i, b)
		}
	}
}

func TestRingBufferPartialFill(t *testing.T) {
	rb := NewRingBuffer(bufferBytes)

	// Write less than capacity
	half := bufferBytes / 2
	rb.Write(makeNoise(half))

	data := rb.Flush()
	if len(data) != half {
		t.Fatalf("Expected %d bytes, got %d", half, len(data))
	}
}

func TestRingBufferFlushResetsState(t *testing.T) {
	rb := NewRingBuffer(bufferBytes)

	rb.Write(makeNoise(100))
	rb.Flush()

	// Second flush should be empty
	data := rb.Flush()
	if len(data) != 0 {
		t.Fatalf("Expected 0 bytes after second flush, got %d", len(data))
	}
}
