package session

import "sync"

const defaultRingSize = 1024 * 1024 // 1MB

type RingBuffer struct {
	mu           sync.Mutex
	buf          []byte
	size         int
	w            int
	full         bool
	totalWritten int64
}

func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{
		buf:  make([]byte, size),
		size: size,
	}
}

func (r *RingBuffer) Write(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.totalWritten += int64(len(p))
	for _, b := range p {
		r.buf[r.w] = b
		r.w++
		if r.w >= r.size {
			r.w = 0
			r.full = true
		}
	}
}

func (r *RingBuffer) TotalWritten() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.totalWritten
}

func (r *RingBuffer) ResetTotalWritten() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.totalWritten = 0
}

func (r *RingBuffer) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.full {
		out := make([]byte, r.w)
		copy(out, r.buf[:r.w])
		return out
	}

	out := make([]byte, r.size)
	n := copy(out, r.buf[r.w:])
	copy(out[n:], r.buf[:r.w])
	return out
}
