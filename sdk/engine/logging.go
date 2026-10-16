package engine

import (
	"bufio"
	"log"
	"os"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/go-gost/core/logger"
	xlogger "github.com/go-gost/x/logger"
)

// Log lines reach the host through a pipe rather than through a custom logger,
// and the reason is a constraint in core/logger: SetDefault stores into a
// sync/atomic.Value, which panics the moment it is handed two different
// concrete types. x/config/loader.Load calls SetDefault on every load with an
// *slogLogger from x/config/parsing/logger, so anything this package installs
// has to be an *slogLogger as well — no logger.LoggerGroup, no bespoke
// implementation.
//
// So: point the process's stderr *variables* (os.Stderr, gin's writers, the
// standard library log package) at a pipe, read the read end on a dedicated
// goroutine, and hand each line to the emitter. That captures every line that
// reaches stderr, including the logger the config parser built — it resolves an
// unset output to the os.Stderr variable at Load time, so it lands in the pipe
// like everything else.
//
// File descriptor 2 is left untouched, so the host application's own native
// stderr keeps working regardless of what this library does.

// The pipe is process-wide and created at most once, because the write end
// stays assigned to os.Stderr for the lifetime of the process and a later Start
// must not create a second pipe that the earlier assignment would not follow.
// The reader is therefore decoupled from any single Engine: it hands lines to
// whichever emitter is currently subscribed.
var (
	pipeOnce sync.Once
	pipeErr  error
	pipeW    *os.File

	// activeEmitter is the emitter currently receiving piped lines, or nil.
	activeMu      sync.Mutex
	activeEmitter *emitter
)

// installLogging points stderr at the shared pipe and builds the logger the
// engine falls back to. Callers must hold e.mu.
func (e *Engine) installLogging() error {
	pipeOnce.Do(func() {
		var r *os.File
		r, pipeW, pipeErr = os.Pipe()
		if pipeErr != nil {
			return
		}

		os.Stderr = pipeW
		gin.DefaultErrorWriter = pipeW
		gin.DefaultWriter = pipeW
		log.SetOutput(pipeW)

		go drainPipe(r)
	})
	if pipeErr != nil {
		return pipeErr
	}

	activeMu.Lock()
	activeEmitter = e.emitter
	activeMu.Unlock()

	// The fallback logger runs at trace level so nothing is lost before the
	// level filter, which lives in the emitter.
	e.defaultLogger = xlogger.NewLogger(
		xlogger.OutputOption(pipeW),
		xlogger.LevelOption(logger.TraceLevel),
	)

	return nil
}

// unsubscribe detaches this engine from the pipe. Lines keep being drained but
// are discarded until another engine subscribes.
func (e *Engine) unsubscribe() {
	activeMu.Lock()
	if activeEmitter == e.emitter {
		activeEmitter = nil
	}
	activeMu.Unlock()
}

// drainPipe reads whole lines and forwards them to the subscribed emitter. It
// runs for the lifetime of the process: the write end stays assigned to
// os.Stderr, so a reader must always exist or the pipe buffer would fill and
// block every writer.
func drainPipe(r *os.File) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		activeMu.Lock()
		em := activeEmitter
		activeMu.Unlock()

		if em != nil {
			em.deliver(detectLevel(line), line)
		}
	}

	// A read error is unrecoverable and there is nowhere useful to report it;
	// stop the reader rather than spinning on a half-closed handle.
}

// toCoreLevel maps a Level to the core logger's level type.
func toCoreLevel(level Level) logger.LogLevel {
	switch level {
	case LevelTrace:
		return logger.TraceLevel
	case LevelDebug:
		return logger.DebugLevel
	case LevelInfo:
		return logger.InfoLevel
	case LevelWarn:
		return logger.WarnLevel
	case LevelError:
		return logger.ErrorLevel
	default:
		return logger.FatalLevel
	}
}
