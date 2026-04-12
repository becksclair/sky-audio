package vad

import (
	"bytes"
	"fmt"
	"testing"
)

// TestRingBufferWraparoundFlush tests the specific wraparound scenario
// where the buffer wraps during high load and Flush() is called.
func TestRingBufferWraparoundFlush(t *testing.T) {
	// Small buffer for easier wraparound testing
	const capacity = 100
	rb := NewRingBuffer(capacity)

	// Write pattern data - each byte is its sequence number for verification
	makePattern := func(start, n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte((start + i) % 256)
		}
		return b
	}

	// Phase 1: Fill buffer completely (pos=0, size=100)
	rb.Write(makePattern(0, capacity))

	// Phase 2: Write 30 more bytes to force wraparound
	// pos becomes 30, size stays at 100
	// Buffer now contains: [30..99] from pattern 0, [0..29] from pattern 100
	rb.Write(makePattern(100, 30))

	// Expected data after wraparound:
	// Oldest 70 bytes: positions 30-99 from original (values 30-99)
	// Newest 30 bytes: positions 0-29 overwritten (values 100-129)
	// So expected sequence is: [30, 31, ..., 99, 100, 101, ..., 129]

	data := rb.Flush()

	if len(data) != capacity {
		t.Fatalf("Expected %d bytes, got %d", capacity, len(data))
	}

	// Verify the data is in correct chronological order
	expected := makePattern(30, capacity)
	if !bytes.Equal(data, expected) {
		t.Errorf("Wraparound data mismatch!\nExpected: %v\nGot:      %v", expected, data)
	}
}

// TestRingBufferWraparoundMultipleLaps tests multiple wraparounds
func TestRingBufferWraparoundMultipleLaps(t *testing.T) {
	const capacity = 100
	rb := NewRingBuffer(capacity)

	makePattern := func(start, n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte((start + i) % 256)
		}
		return b
	}

	// Write 250 bytes (2.5 laps) - should end with last 100 bytes
	// Final state: pos=50, size=100
	// Contains: bytes 150-249
	rb.Write(makePattern(0, 250))

	data := rb.Flush()

	if len(data) != capacity {
		t.Fatalf("Expected %d bytes, got %d", capacity, len(data))
	}

	// Expected: 150-249
	expected := makePattern(150, capacity)
	if !bytes.Equal(data, expected) {
		t.Errorf("Multiple lap data mismatch!\nExpected: %v\nGot:      %v", expected, data)
	}
}

// TestRingBufferFlushAtVariousPositions tests Flush at different wrap positions
func TestRingBufferFlushAtVariousPositions(t *testing.T) {
	const capacity = 100

	for offset := 0; offset < capacity; offset += 10 {
		t.Run(fmt.Sprintf("offset_%d", offset), func(t *testing.T) {
			rb := NewRingBuffer(capacity)

			makePattern := func(start, n int) []byte {
				b := make([]byte, n)
				for i := range b {
					b[i] = byte((start + i) % 256)
				}
				return b
			}

			// Write offset bytes first
			rb.Write(makePattern(0, offset))

			// Now write capacity + 25 bytes to force wraparound at position offset
			rb.Write(makePattern(offset, capacity+25))

			// pos should now be (offset + 25) % capacity
			// Data should be the last 100 bytes: offset+25 to offset+124

			data := rb.Flush()

			if len(data) != capacity {
				t.Fatalf("Expected %d bytes, got %d", capacity, len(data))
			}

			expected := makePattern(offset+25, capacity)
			if !bytes.Equal(data, expected) {
				t.Errorf("Position %d data mismatch!\nExpected: %v\nGot:      %v", offset, expected, data)
			}
		})
	}
}
