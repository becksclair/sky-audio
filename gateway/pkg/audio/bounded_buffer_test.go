package audio

import (
	"bytes"
	"testing"
)

func TestBoundedBuffer_Write_DropsBeyondCap(t *testing.T) {
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
	// Should report only what was actually written (100 bytes, but capped at 50 more)
	if n != 100 {
		t.Errorf("expected n=100 (original length), got %d", n)
	}
	// But only 50 bytes should be stored
	if buf.size != 100 {
		t.Errorf("expected size=100 (at cap), got %d", buf.size)
	}

	// Third write when at capacity - should return full length (not 0)
	// to prevent io.Copy ErrShortWrite while still dropping data
	data3 := []byte("dropped data")
	n, err = buf.Write(data3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should return full length to satisfy io.Copy contract (avoids ErrShortWrite)
	// even though data is actually dropped
	if n != len(data3) {
		t.Errorf("expected n=%d (full length to satisfy io.Copy), got %d", len(data3), n)
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

	// Write data up to capacity
	data := bytes.Repeat([]byte("error message\n"), 10) // ~140 bytes
	n, err := buf.Write(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// io.Copy treats n < len(data) && err == nil as a short write and will retry.
	// This boundedBuffer intentionally loses data to cap memory, so it reports
	// the full write length to avoid io.ErrShortWrite. Callers must be aware
	// that this writer may silently drop data beyond cap.
	if n != len(data) {
		t.Errorf("expected n=%d (full length to satisfy io.Copy), got %d", len(data), n)
	}

	// But buffer should only contain capped data
	result := buf.Bytes()
	if len(result) > 64 {
		t.Errorf("buffer should be capped at 64 bytes, got %d", len(result))
	}

	// Subsequent writes at capacity should return full length (not 0)
	// to prevent io.Copy ErrShortWrite while still dropping data
	n, err = buf.Write([]byte("more data"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 9 { // len("more data") == 9
		t.Errorf("expected n=9 (full length to satisfy io.Copy), got %d", n)
	}
}
