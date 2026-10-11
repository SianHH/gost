package listener

import (
	"container/heap"
	"errors"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
)

// Packet is one element of the upload queue: either a stream reader (stream-up)
// or a sequenced chunk of payload (packet-up).
type Packet struct {
	Reader  io.ReadCloser
	Payload []byte
	Seq     uint64
}

// uploadQueue is a specialised priority queue + channel that reorders generic
// packets by their sequence number.
//
// closed and reader are accessed from both the request-handling goroutines
// (Push/Close) and the session reader goroutine (Read), so they are atomic.
type uploadQueue struct {
	reader          atomic.Pointer[io.ReadCloser]
	nomore          bool
	pushedPackets   chan Packet
	writeCloseMutex sync.Mutex
	heap            uploadHeap
	nextSeq         uint64
	closed          atomic.Bool
	maxPackets      int
}

func newUploadQueue(maxPackets int) *uploadQueue {
	return &uploadQueue{
		pushedPackets: make(chan Packet, maxPackets),
		heap:          uploadHeap{},
		nextSeq:       0,
		maxPackets:    maxPackets,
	}
}

// Push adds a packet. Adding a packet with a non-nil Reader switches the queue
// into stream mode and rejects any further packets.
func (q *uploadQueue) Push(p Packet) error {
	q.writeCloseMutex.Lock()
	defer q.writeCloseMutex.Unlock()

	if q.closed.Load() {
		return errors.New("packet queue closed")
	}
	if q.nomore {
		return errors.New("reader already exists")
	}
	if p.Reader != nil {
		q.nomore = true
	}
	q.pushedPackets <- p
	return nil
}

// Close closes the queue. Pending packets are still drained.
func (q *uploadQueue) Close() error {
	q.writeCloseMutex.Lock()
	defer q.writeCloseMutex.Unlock()

	if !q.closed.Swap(true) {
		runtime.Gosched() // give Read() a chance to pick up the packet
	drain:
		for {
			select {
			case p := <-q.pushedPackets:
				if p.Reader != nil {
					q.reader.Store(&p.Reader)
				}
			default:
				break drain
			}
		}
		close(q.pushedPackets)
	}
	if r := q.reader.Load(); r != nil {
		return (*r).Close()
	}
	return nil
}

// Read implements io.Reader, returning packets in sequence order.
func (q *uploadQueue) Read(b []byte) (int, error) {
	if r := q.reader.Load(); r != nil {
		return (*r).Read(b)
	}

	if q.closed.Load() {
		return 0, io.EOF
	}

	if len(q.heap) == 0 {
		packet, more := <-q.pushedPackets
		if !more {
			return 0, io.EOF
		}
		if packet.Reader != nil {
			q.reader.Store(&packet.Reader)
			return packet.Reader.Read(b)
		}
		heap.Push(&q.heap, packet)
	}

	for len(q.heap) > 0 {
		packet := heap.Pop(&q.heap).(Packet)
		n := 0

		if packet.Seq == q.nextSeq {
			n = copy(b, packet.Payload)

			if n < len(packet.Payload) {
				// partial read
				packet.Payload = packet.Payload[n:]
				heap.Push(&q.heap, packet)
			} else {
				q.nextSeq = packet.Seq + 1
			}

			return n, nil
		}

		// misordered packet
		if packet.Seq > q.nextSeq {
			if len(q.heap) > q.maxPackets {
				// The reassembly buffer is too large; constrain memory usage by
				// tearing down the connection and hoping the application retries.
				return 0, errors.New("packet queue is too large")
			}
			heap.Push(&q.heap, packet)
			packet2, more := <-q.pushedPackets
			if !more {
				return 0, io.EOF
			}
			heap.Push(&q.heap, packet2)
		}
	}

	return 0, nil
}

// uploadHeap is a min-heap of Packets keyed by Seq.
type uploadHeap []Packet

func (h uploadHeap) Len() int           { return len(h) }
func (h uploadHeap) Less(i, j int) bool { return h[i].Seq < h[j].Seq }
func (h uploadHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *uploadHeap) Push(x any) {
	*h = append(*h, x.(Packet))
}

func (h *uploadHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}
