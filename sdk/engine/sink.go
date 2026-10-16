package engine

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Level is the log severity handed to the host callback. The values are stable
// so the C and Dart sides never have to parse strings.
type Level int

const (
	LevelTrace Level = iota
	LevelDebug
	LevelInfo
	LevelWarn
	LevelError
	LevelFatal
)

// Valid reports whether l is one of the defined levels.
func (l Level) Valid() bool { return l >= LevelTrace && l <= LevelFatal }

// logQueueSize bounds the number of pending log entries.
const logQueueSize = 2048

// LogHandler receives one log entry. It is called from the emitter's delivery
// goroutine, never from a GOST worker goroutine.
type LogHandler func(level Level, msg string)

type logEntry struct {
	level Level
	msg   string
}

// emitter turns raw stderr lines into host callbacks.
//
// The handoff is asynchronous on purpose. The callback is host code (a Dart
// callback reached through cgo) and must stay free to call back into the SDK —
// for example to stop the engine — without deadlocking against a GOST goroutine
// that is merely trying to log. Nothing on the logging path may block, so a
// full queue drops entries and counts them instead.
//
// With no callback installed, lines are dropped; the pipe keeps draining so
// writers never block.
type emitter struct {
	mu      sync.Mutex
	handler LogHandler
	ch      chan logEntry
	stop    chan struct{}
	running bool
	dropped atomic.Int64

	// min is the lowest level forwarded to the host.
	min atomic.Int32
}

func newEmitter() *emitter {
	e := &emitter{}
	e.min.Store(int32(LevelInfo))

	return e
}

// setHandler installs or clears the host callback. The delivery goroutine runs
// only while a callback is installed.
func (e *emitter) setHandler(h LogHandler) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.handler = h
	if h == nil {
		e.stopLocked()
		return
	}
	e.startLocked()
}

// reset clears the callback at shutdown.
func (e *emitter) reset() { e.setHandler(nil) }

func (e *emitter) startLocked() {
	if e.running {
		return
	}
	e.ch = make(chan logEntry, logQueueSize)
	e.stop = make(chan struct{})
	e.running = true

	go e.drain(e.ch, e.stop)
}

func (e *emitter) stopLocked() {
	if !e.running {
		return
	}
	e.running = false
	close(e.stop)
	e.ch = nil
	e.stop = nil
}

// setLevel sets the lowest level forwarded to the host.
func (e *emitter) setLevel(level Level) {
	if level.Valid() {
		e.min.Store(int32(level))
	}
}

// level returns the current minimum forwarded level.
func (e *emitter) level() Level { return Level(e.min.Load()) }

// deliver accepts one line from the pipe. It never blocks: with no callback
// installed the line is simply dropped, and a full queue counts it instead of
// waiting for room.
func (e *emitter) deliver(level Level, msg string) {
	if level < e.level() {
		return
	}

	e.mu.Lock()
	ch := e.ch
	e.mu.Unlock()

	if ch == nil {
		return
	}

	select {
	case ch <- logEntry{level: level, msg: msg}:
	default:
		e.dropped.Add(1)
	}
}

// drain delivers entries until stop is closed. Shutdown never waits on the
// handler: a slow or blocking host callback must not stall Stop.
func (e *emitter) drain(ch <-chan logEntry, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case entry := <-ch:
			e.mu.Lock()
			h := e.handler
			e.mu.Unlock()

			if h == nil {
				continue
			}
			if n := e.dropped.Swap(0); n > 0 {
				h(LevelWarn, "gost: dropped "+strconv.FormatInt(n, 10)+" log entries (queue full)")
			}
			h(entry.level, entry.msg)
		}
	}
}

// levelNames maps the level strings emitted by x/logger onto Level constants.
var levelNames = map[string]Level{
	"trace": LevelTrace,
	"debug": LevelDebug,
	"info":  LevelInfo,
	"warn":  LevelWarn,
	"error": LevelError,
	"fatal": LevelFatal,
}

// detectLevel recovers the severity of a line that was written to stderr.
//
// Lines arrive as text because the config's logger owns the output format and
// this package cannot replace it (see logging.go for why). x/logger defaults to
// JSON, so its records carry `"level":"info"` and the severity is recoverable
// with a substring scan. Anything else — text format, or third-party output — is
// reported as info.
func detectLevel(line string) Level {
	if len(line) < 8 || line[0] != '{' {
		return LevelInfo
	}
	i := strings.Index(line, `"level":"`)
	if i < 0 {
		return LevelInfo
	}
	rest := line[i+len(`"level":"`):]
	j := strings.IndexByte(rest, '"')
	if j <= 0 {
		return LevelInfo
	}
	if lv, ok := levelNames[rest[:j]]; ok {
		return lv
	}
	return LevelInfo
}
