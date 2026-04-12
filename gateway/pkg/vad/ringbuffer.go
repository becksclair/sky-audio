package vad

// RingBuffer is a fixed-capacity circular byte buffer used to retain
// pre-speech audio so the first syllable is not clipped when VAD triggers.
//
// The buffer maintains data in chronological order even when it wraps around.
// When the buffer fills and new data arrives, the oldest bytes are overwritten.
// The Flush() method correctly reconstructs the chronological order even when
// data wraps around the end of the underlying array.
type RingBuffer struct {
	buf  []byte
	cap  int
	pos  int // next write position (0 <= pos < cap)
	size int // current occupancy (0 <= size <= cap)
}

func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		panic("ringbuffer: capacity must be > 0")
	}
	return &RingBuffer{
		buf: make([]byte, capacity),
		cap: capacity,
	}
}

// Write appends data to the ring buffer, overwriting oldest bytes if full.
func (rb *RingBuffer) Write(data []byte) {
	n := len(data)
	if n == 0 {
		return
	}

	// If incoming data is >= capacity, only keep the last cap bytes.
	if n >= rb.cap {
		copy(rb.buf, data[n-rb.cap:])
		rb.pos = 0
		rb.size = rb.cap
		return
	}

	// Bulk copy with wrap-around.
	first := rb.cap - rb.pos
	if n <= first {
		copy(rb.buf[rb.pos:], data)
	} else {
		copy(rb.buf[rb.pos:], data[:first])
		copy(rb.buf, data[first:])
	}
	rb.pos = (rb.pos + n) % rb.cap
	rb.size += n
	if rb.size > rb.cap {
		rb.size = rb.cap
	}
}

// Flush returns all buffered data in chronological order and resets the buffer.
//
// The wraparound handling works as follows:
//   - start = (pos - size + cap) % cap gives the index of the oldest byte
//   - If start+size <= cap, data is contiguous from start to start+size
//   - Otherwise, data wraps: first part from start to cap, second part from 0
//
// This correctly handles all cases including partial fill, full buffer, and wraparound.
func (rb *RingBuffer) Flush() []byte {
	if rb.size == 0 {
		return nil
	}

	out := make([]byte, rb.size)
	// Calculate start position of oldest data, accounting for wraparound.
	// The +rb.cap ensures the result is non-negative before modulo.
	start := (rb.pos - rb.size + rb.cap) % rb.cap

	if start+rb.size <= rb.cap {
		// Data is contiguous - copy in one operation
		copy(out, rb.buf[start:start+rb.size])
	} else {
		// Data wraps around end of buffer - copy in two parts
		first := rb.cap - start // bytes from start to end of buffer
		copy(out[:first], rb.buf[start:])
		copy(out[first:], rb.buf[:rb.size-first])
	}

	rb.pos = 0
	rb.size = 0
	return out
}
