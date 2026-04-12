package vad

import (
	"bytes"
	"testing"
)

// TestRingBufferPartialWraparoundEdgeCase tests a subtle edge case:
// when buffer is not full but wraparound still happens during write.
func TestRingBufferPartialWraparoundEdgeCase(t *testing.T) {
	const capacity = 100
	rb := NewRingBuffer(capacity)

	makePattern := func(start, n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte((start + i) % 256)
		}
		return b
	}

	// Write 70 bytes (not full) - pos=70, size=70
	rb.Write(makePattern(0, 70))

	// Write another 50 bytes which will wrap:
	// - First 30 bytes go to positions 70-99
	// - Next 20 bytes wrap to positions 0-19
	// After: pos = (70+50)%100 = 20, size = min(120, 100) = 100
	rb.Write(makePattern(70, 50))

	// Data should be: 20-69 (original remaining) + 70-119 (new data)
	// = [20..69] + [70..99] + [100..119]
	// But size is capped at 100, so:
	// Oldest 100 bytes of [0..119] = [20..119]
	// Expected sequence: 20, 21, ..., 119

	data := rb.Flush()

	if len(data) != capacity {
		t.Fatalf("Expected %d bytes, got %d", capacity, len(data))
	}

	expected := makePattern(20, capacity)
	if !bytes.Equal(data, expected) {
		t.Errorf("Partial wraparound edge case failed!\nExpected: %v\nGot:      %v", expected, data)
	}
}

// TestRingBufferExactCapacityWrite tests writing exactly capacity bytes
func TestRingBufferExactCapacityWrite(t *testing.T) {
	const capacity = 100
	rb := NewRingBuffer(capacity)

	makePattern := func(start, n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte((start + i) % 256)
		}
		return b
	}

	// Write exactly capacity bytes
	rb.Write(makePattern(0, capacity))

	data := rb.Flush()

	if len(data) != capacity {
		t.Fatalf("Expected %d bytes, got %d", capacity, len(data))
	}

	expected := makePattern(0, capacity)
	if !bytes.Equal(data, expected) {
		t.Errorf("Exact capacity write failed!\nExpected: %v\nGot:      %v", expected, data)
	}
}

// TestRingBufferWriteAfterFlush tests behavior after a flush
func TestRingBufferWriteAfterFlush(t *testing.T) {
	const capacity = 100
	rb := NewRingBuffer(capacity)

	makePattern := func(start, n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte((start + i) % 256)
		}
		return b
	}

	// First fill and flush
	rb.Write(makePattern(0, 50))
	data1 := rb.Flush()
	if len(data1) != 50 || !bytes.Equal(data1, makePattern(0, 50)) {
		t.Fatalf("First flush failed")
	}

	// Now write new data - should start from pos=0
	rb.Write(makePattern(100, 60))

	data2 := rb.Flush()
	if len(data2) != 60 {
		t.Fatalf("Expected 60 bytes after second write, got %d", len(data2))
	}

	expected := makePattern(100, 60)
	if !bytes.Equal(data2, expected) {
		t.Errorf("Write after flush failed!\nExpected: %v\nGot:      %v", expected, data2)
	}
}

// TestRingBufferFlushNotFullAtOffset tests Flush when buffer not full and pos at various offsets
func TestRingBufferFlushNotFullAtOffset(t *testing.T) {
	const capacity = 100

	testCases := []struct {
		name      string
		firstWrite int
		secondWrite int
	}{
		{"pos0", 0, 50},      // pos starts at 0
		{"pos50", 50, 30},    // pos at 50
		{"pos90", 90, 5},     // near wrap
		{"pos95", 95, 3},     // almost wrapping
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rb := NewRingBuffer(capacity)

			makePattern := func(start, n int) []byte {
				b := make([]byte, n)
				for i := range b {
					b[i] = byte((start + i) % 256)
				}
				return b
			}

			// First write to position at offset
			rb.Write(makePattern(0, tc.firstWrite))

			// Second write
			rb.Write(makePattern(tc.firstWrite, tc.secondWrite))

			data := rb.Flush()

			totalSize := tc.firstWrite + tc.secondWrite
			if totalSize > capacity {
				totalSize = capacity
			}

			if len(data) != totalSize {
				t.Fatalf("Expected %d bytes, got %d", totalSize, len(data))
			}

			// If total <= capacity, data should be contiguous from 0
			if tc.firstWrite+tc.secondWrite <= capacity {
				expected := makePattern(0, tc.firstWrite+tc.secondWrite)
				if !bytes.Equal(data, expected) {
					t.Errorf("Data mismatch!\nExpected: %v\nGot:      %v", expected, data)
				}
			}
		})
	}
}
