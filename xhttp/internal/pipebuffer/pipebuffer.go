// Package pipebuffer implements the batching buffer used by the xhttp
// "packet-up" upload path.
//
// Application writes are accumulated in memory (up to a size limit, after which
// writers block) so that the uploader can pack many small Write calls into a
// single HTTP POST, which is essential for reasonable upstream throughput.
package pipebuffer

import (
	"io"
	"sync"
)

// Buffer is a bounded, blocking byte buffer.
type Buffer struct {
	mu     sync.Mutex
	cond   *sync.Cond
	data   []byte
	limit  int
	closed bool
	err    error
}

// New returns a Buffer that holds at most limit bytes before writers block.
func New(limit int) *Buffer {
	if limit < 1 {
		limit = 1
	}
	b := &Buffer{limit: limit}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// Write appends p, blocking while the buffer is full. It always writes the
// whole slice or returns an error.
func (b *Buffer) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		b.mu.Lock()
		for len(b.data) >= b.limit && !b.closed {
			b.cond.Wait()
		}
		if b.closed {
			b.mu.Unlock()
			if written > 0 {
				return written, nil
			}
			return 0, io.ErrClosedPipe
		}
		space := b.limit - len(b.data)
		if space > len(p) {
			space = len(p)
		}
		b.data = append(b.data, p[:space]...)
		written += space
		p = p[space:]
		b.cond.Broadcast()
		b.mu.Unlock()
	}
	return written, nil
}

// ReadChunk blocks until data is available or the buffer is closed, then
// returns at most max bytes. On a closed, drained buffer it returns the stored
// error if any, otherwise io.EOF.
func (b *Buffer) ReadChunk(max int) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for len(b.data) == 0 && !b.closed {
		b.cond.Wait()
	}

	if len(b.data) == 0 {
		if b.err != nil {
			return nil, b.err
		}
		return nil, io.EOF
	}

	n := len(b.data)
	if max > 0 && n > max {
		n = max
	}

	out := make([]byte, n)
	copy(out, b.data[:n])
	b.data = b.data[n:]
	if len(b.data) == 0 {
		b.data = nil
	}
	b.cond.Broadcast()
	return out, nil
}

// Close closes the buffer for writing. Already buffered data is still
// delivered before io.EOF is reported.
func (b *Buffer) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.closed = true
		b.cond.Broadcast()
	}
	return nil
}

// Interrupt aborts the buffer immediately: pending data is dropped and readers
// observe io.ErrClosedPipe.
func (b *Buffer) Interrupt() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err == nil {
		b.err = io.ErrClosedPipe
	}
	b.closed = true
	b.data = nil
	b.cond.Broadcast()
}
