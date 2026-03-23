package vad

// RingBuffer is a fixed-capacity circular byte buffer used to retain
// pre-speech audio so the first syllable is not clipped when VAD triggers.
type RingBuffer struct {
	buf  []byte
	cap  int
	pos  int // next write position
	size int // current occupancy
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
func (rb *RingBuffer) Flush() []byte {
	if rb.size == 0 {
		return nil
	}

	out := make([]byte, rb.size)
	start := (rb.pos - rb.size + rb.cap) % rb.cap

	if start+rb.size <= rb.cap {
		copy(out, rb.buf[start:start+rb.size])
	} else {
		first := rb.cap - start
		copy(out[:first], rb.buf[start:])
		copy(out[first:], rb.buf[:rb.size-first])
	}

	rb.pos = 0
	rb.size = 0
	return out
}
