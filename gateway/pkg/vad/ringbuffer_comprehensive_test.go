package vad

import (
	"bytes"
	"testing"
)

// TestRingBufferFlushWithWraparound tests the exact scenario from the bug report:
// "If the buffer wraps during high load, Flush() may return incorrect data"
func TestRingBufferFlushWithWraparound(t *testing.T) {
	// Use a small capacity for clear testing
	const capacity = 20
	rb := NewRingBuffer(capacity)

	// Create data where each byte identifies its position
	data := make([]byte, capacity)
	for i := range data {
		data[i] = byte(i)
	}

	// Step 1: Fill buffer completely
	// pos=0, size=20 (after write, pos wraps to 0 because n >= cap path)
	rb.Write(data)

	// Step 2: Write 5 more bytes to force wraparound
	// These will overwrite positions 0-4
	extra := []byte{100, 101, 102, 103, 104}
	rb.Write(extra)

	// Expected state:
	// pos = 5 (because we wrote 5 bytes starting from pos=0 after the full overwrite)
	// size = 20 (capped at capacity)
	// Buffer contains: [100, 101, 102, 103, 104, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19]

	// Flush should return data in chronological order:
	// [5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 100, 101, 102, 103, 104]

	flushed := rb.Flush()

	if len(flushed) != capacity {
		t.Fatalf("Expected %d bytes, got %d", capacity, len(flushed))
	}

	// Build expected result
	expected := make([]byte, capacity)
	copy(expected[:15], []byte{5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19})
	copy(expected[15:], extra)

	if !bytes.Equal(flushed, expected) {
		t.Errorf("Flush returned incorrect data!\nExpected: %v\nGot:      %v", expected, flushed)
	}
}

// TestRingBufferWriteLargerThanCapacity tests writing data larger than capacity
func TestRingBufferWriteLargerThanCapacity(t *testing.T) {
	const capacity = 10
	rb := NewRingBuffer(capacity)

	// Write 15 bytes (5 more than capacity)
	data := make([]byte, 15)
	for i := range data {
		data[i] = byte(i)
	}
	rb.Write(data)

	// Should only keep last 10 bytes: [5, 6, 7, 8, 9, 10, 11, 12, 13, 14]
	flushed := rb.Flush()

	if len(flushed) != capacity {
		t.Fatalf("Expected %d bytes, got %d", capacity, len(flushed))
	}

	expected := []byte{5, 6, 7, 8, 9, 10, 11, 12, 13, 14}
	if !bytes.Equal(flushed, expected) {
		t.Errorf("Write larger than capacity returned incorrect data!\nExpected: %v\nGot:      %v", expected, flushed)
	}
}

// TestRingBufferSequentialFlushOperations tests multiple write/flush cycles
func TestRingBufferSequentialFlushOperations(t *testing.T) {
	const capacity = 100
	rb := NewRingBuffer(capacity)

	makePattern := func(start, n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte((start + i) % 256)
		}
		return b
	}

	// Cycle 1: Fill completely and flush
	rb.Write(makePattern(0, capacity))
	data1 := rb.Flush()
	if !bytes.Equal(data1, makePattern(0, capacity)) {
		t.Errorf("Cycle 1 failed")
	}

	// Cycle 2: Partial fill and flush
	rb.Write(makePattern(100, 50))
	data2 := rb.Flush()
	if !bytes.Equal(data2, makePattern(100, 50)) {
		t.Errorf("Cycle 2 failed")
	}

	// Cycle 3: Fill, wrap, flush
	rb.Write(makePattern(200, capacity))
	rb.Write(makePattern(200+capacity, 30))
	data3 := rb.Flush()
	expected3 := makePattern(200+30, capacity)
	if !bytes.Equal(data3, expected3) {
		t.Errorf("Cycle 3 failed: wraparound flush incorrect\nExpected: %v\nGot:      %v", expected3, data3)
	}

	// Cycle 4: Verify clean state after flush
	data4 := rb.Flush()
	if len(data4) != 0 {
		t.Errorf("Cycle 4 failed: expected empty flush after previous flush")
	}
}

// TestRingBufferManySmallWrites tests many small writes that cumulatively wrap
func TestRingBufferManySmallWrites(t *testing.T) {
	const capacity = 100
	rb := NewRingBuffer(capacity)

	// Write 10 bytes at a time, 15 times = 150 bytes total
	// This should result in last 100 bytes being kept
	for i := 0; i < 15; i++ {
		data := []byte{byte(i * 10), byte(i*10 + 1), byte(i*10 + 2), byte(i*10 + 3),
			byte(i*10 + 4), byte(i*10 + 5), byte(i*10 + 6), byte(i*10 + 7),
			byte(i*10 + 8), byte(i*10 + 9)}
		rb.Write(data)
	}

	flushed := rb.Flush()

	if len(flushed) != capacity {
		t.Fatalf("Expected %d bytes, got %d", capacity, len(flushed))
	}

	// Expected: bytes 50-149 (last 100 of 150 written)
	for i := 0; i < capacity; i++ {
		expected := byte(50 + i)
		if flushed[i] != expected {
			t.Errorf("Byte %d: expected %d, got %d", i, expected, flushed[i])
			break
		}
	}
}
