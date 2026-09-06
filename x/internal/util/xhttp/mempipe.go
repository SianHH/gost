package xhttp

import (
	"io"
	"sync"
)

// memPipe is a bounded, in-memory byte pipe with a blocking writer.
//
// It is the upload-side transport buffer for one XHTTP session and replaces
// io.Pipe. io.Pipe is unbuffered and couples every application Write to a
// matching Read, so the packet-up uploader (which issues one HTTP POST per
// Read) emitted one POST per application write — with the 30ms inter-post
// pacing that caps bandwidth at writeChunk/30ms (~1 MB/s for 32KB writes).
//
// memPipe lets consecutive small application writes accumulate in a byte
// buffer (up to capacity) before the uploader drains them, so several writes
// are batched into a single large POST, exactly like xray's
// pipe.New(pipe.WithSizeLimit(...)) in dialer.go. Write blocks only when the
// buffer is full (backpressure), Read returns whatever is buffered as soon as
// data is available.
//
// Close() cleanly ends the stream: buffered data is still readable, after
// which Read returns io.EOF. CloseWithError aborts immediately: buffered data
// is dropped and pending/future Reads and Writes observe the error.
type memPipe struct {
	mu       sync.Mutex
	cond     *sync.Cond
	capacity int

	buf    []byte
	closed bool
	rerr   error // non-nil: Reads fail immediately (abort)
	werr   error // non-nil: Writes fail immediately
}

func newMemPipe(capacity int) (*memPipeReader, *memPipeWriter) {
	if capacity <= 0 {
		capacity = 1
	}
	p := &memPipe{capacity: capacity}
	p.cond = sync.NewCond(&p.mu)
	return &memPipeReader{p: p}, &memPipeWriter{p: p}
}

func (p *memPipe) write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	written := 0

	p.mu.Lock()
	defer p.mu.Unlock()

	for written < len(b) {
		if p.closed {
			err := p.werr
			if err == nil {
				err = io.ErrClosedPipe
			}
			return written, err
		}
		avail := p.capacity - len(p.buf)
		if avail <= 0 {
			p.cond.Wait() // full; wait for the reader to drain
			continue
		}
		n := avail
		if left := len(b) - written; n > left {
			n = left
		}
		p.buf = append(p.buf, b[written:written+n]...)
		written += n
		p.cond.Broadcast() // a reader may be waiting
	}
	return written, nil
}

func (p *memPipe) read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for {
		if p.rerr != nil {
			return 0, p.rerr
		}
		if len(p.buf) > 0 {
			break
		}
		if p.closed {
			return 0, io.EOF
		}
		p.cond.Wait()
	}

	n := copy(b, p.buf)
	p.buf = p.buf[n:]
	if len(p.buf) == 0 {
		p.buf = nil
	}
	p.cond.Broadcast() // writer may be blocked on a full buffer
	return n, nil
}

// close implements both clean Close and CloseWithError semantics.
func (p *memPipe) close(err error) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil
	}
	p.closed = true

	if err != nil {
		// Abort: drop anything not yet read and propagate the error to both
		// ends so blocked readers/writers wake up immediately.
		p.buf = nil
		p.rerr = err
		p.werr = err
	} else {
		p.werr = io.ErrClosedPipe
	}
	p.cond.Broadcast()
	return nil
}

type memPipeReader struct {
	p *memPipe
}

func (r *memPipeReader) Read(b []byte) (int, error) { return r.p.read(b) }

type memPipeWriter struct {
	p *memPipe
}

func (w *memPipeWriter) Write(b []byte) (int, error) { return w.p.write(b) }

// Close cleanly ends the upload stream. The reader sees io.EOF after the
// buffered data has been drained.
func (w *memPipeWriter) Close() error { return w.p.close(nil) }

// CloseWithError aborts the upload stream with the given error.
func (w *memPipeWriter) CloseWithError(err error) error {
	if err == nil {
		return w.p.close(nil)
	}
	return w.p.close(err)
}
