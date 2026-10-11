// Package done provides a single-shot notification primitive.
package done

import "sync"

// Instance is a utility for notifications of something being done.
type Instance struct {
	access sync.Mutex
	c      chan struct{}
	closed bool
}

// New returns a new Instance.
func New() *Instance {
	return &Instance{c: make(chan struct{})}
}

// Done reports whether Close has been called.
func (d *Instance) Done() bool {
	select {
	case <-d.c:
		return true
	default:
		return false
	}
}

// Wait returns a channel that is closed when Close is called.
func (d *Instance) Wait() <-chan struct{} {
	return d.c
}

// Close marks this Instance done. It is safe to call multiple times.
func (d *Instance) Close() error {
	d.access.Lock()
	defer d.access.Unlock()

	if d.closed {
		return nil
	}
	d.closed = true
	close(d.c)
	return nil
}
