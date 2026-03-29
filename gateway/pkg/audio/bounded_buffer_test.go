package audio

import (
	"bytes"
	"testing"
)

func TestBoundedBuffer_Write_DropsBeyondCapButReportsFullLength(t *testing.T) {
	buf := newBoundedBuffer(100)

	// First write within capacity
	data1 := make([]byte, 50)
	n, err := buf.Write(data1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 50 {
		t.Errorf("expected n=50, got %d", n)
	}
	if buf.size != 50 {
		t.Errorf("expected size=50, got %d", buf.size)
	}

	// Second write partially fits (only 50 more bytes fit)
	data2 := make([]byte, 100)
	n, err = buf.Write(data2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should report full write to avoid io.ErrShortWrite
	if n != 100 {
		t.Errorf("expected n=100 (original length), got %d", n)
	}
	// But only 50 bytes should be stored
	if buf.size != 100 {
		t.Errorf("expected size=100 (at cap), got %d", buf.size)
	}

	// Third write when at capacity - should silently drop
	data3 := []byte("dropped data")
	n, err = buf.Write(data3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should report full write
	if n != len(data3) {
		t.Errorf("expected n=%d (original length), got %d", len(data3), n)
	}
	// Size should still be at cap
	if buf.size != 100 {
		t.Errorf("expected size=100 (at cap), got %d", buf.size)
	}

	// Verify only first 100 bytes are in buffer
	result := buf.Bytes()
	if len(result) != 100 {
		t.Errorf("expected 100 bytes in buffer, got %d", len(result))
	}
}

func TestBoundedBuffer_Write_UsedAsStderr(t *testing.T) {
	// Simulate using boundedBuffer as cmd.Stderr (like in ffmpeg runner)
	buf := newBoundedBuffer(64)

	// Write data that would cause io.ErrShortWrite if we returned truncated length
	data := bytes.Repeat([]byte("error message\n"), 10) // ~140 bytes
	n, err := buf.Write(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Must return full length to satisfy io.Writer contract
	if n != len(data) {
		t.Errorf("expected n=%d (full length for io.Writer contract), got %d", len(data), n)
	}

	// But buffer should only contain capped data
	result := buf.Bytes()
	if len(result) > 64 {
		t.Errorf("buffer should be capped at 64 bytes, got %d", len(result))
	}
}
