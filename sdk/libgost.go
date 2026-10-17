// Build the shared library with:
//
//	# Android (arm64-v8a, x86_64)
//	bash scripts/build-android.sh
//
//	# Windows / Linux / macOS, for desktop debugging
//	bash scripts/build-native.sh
//
// This file is the c-shared entry point. It must be package main with an empty
// main function, and the //export directives have to live here because cgo only
// processes them in the package being built as a shared library. All real logic
// sits in the engine package, which has no cgo and is therefore unit testable.
package main

import (
	"fmt"
	"os"
	"sync"
	"unsafe"

	"github.com/go-gost/sdk/engine"

	// Registers the built-in components. Build with -tags gost_all for the
	// full CLI-identical set.
	_ "github.com/go-gost/sdk/register"
)

/*
#include <stdlib.h>

// gost_log_cb is invoked on a dedicated goroutine, never on a GOST worker
// goroutine, so the callback may safely call back into this library — for
// example gost_stop. It must not block indefinitely, and it must not retain
// msg beyond the call.
//
// level: 0=trace 1=debug 2=info 3=warn 4=error 5=fatal
// msg:   NUL-terminated UTF-8
typedef void (*gost_log_cb)(int level, const char *msg);

// call_gost_log_cb exists because Go cannot call a C function pointer directly.
static void call_gost_log_cb(gost_log_cb fn, int level, const char *msg) {
	fn(level, msg);
}
*/
import "C"

var (
	eng = engine.New()

	// cbMu guards the host function pointer, which is read from the emitter's
	// goroutine while gost_set_log_callback writes it.
	cbMu sync.RWMutex
	cb   C.gost_log_cb

	// verString is allocated once and returned by gost_version. The caller
	// must not free it.
	verString *C.char
	verOnce   sync.Once
)

// gost_version returns the GOST version string. The returned pointer is owned
// by the library and must not be freed.
//
//export gost_version
func gost_version() *C.char {
	verOnce.Do(func() {
		verString = C.CString(engine.Version())
	})
	return verString
}

// gost_setenv sets an environment variable **inside the Go runtime**.
//
// The host cannot do this from outside. On Android the c-shared entry point
// passes an empty environment block — _rt0_<arch>_android_lib hardcodes argc=1
// and a NULL-terminated empty envv — so goenvs_unix() enumerates zero variables
// and syscall.envs, the snapshot os.Getenv reads, is permanently empty. Calling
// libc's setenv from Java/Kotlin writes to bionic's environ, which Go never
// looks at.
//
// That matters because os.TempDir() returns "/data/local/tmp" on Android when
// TMPDIR is unset (os/file_unix.go), and an app sandbox cannot write there. The
// engine stages its configuration through os.CreateTemp (engine.go), so every
// gost_start would fail with "permission denied" unless the host can inject a
// writable TMPDIR first.
//
// Setting HOME matters for the same reason: os.UserHomeDir() falls back to
// "/sdcard" on Android, which is also unwritable, and x/config/parsing/tls.go
// persists auto-generated certificates under $HOME/.gost.
//
// Call this before gost_start. Returns 0 on success, non-zero on failure, in
// which case *err receives a message the caller must release with gost_free.
//
//export gost_setenv
func gost_setenv(key, value *C.char, errOut **C.char) (rc C.int) {
	defer func() {
		if r := recover(); r != nil {
			setErr(errOut, panicError(r))
			rc = 1
		}
	}()

	if key == nil || value == nil {
		setErr(errOut, "gost: setenv: nil key or value")
		return 1
	}

	k, v := C.GoString(key), C.GoString(value)
	if err := os.Setenv(k, v); err != nil {
		setErr(errOut, fmt.Sprintf("gost: setenv %s: %v", k, err))
		return 1
	}

	setErr(errOut, "")
	return 0
}

// gost_set_log_callback installs the log sink. Passing NULL removes it.
// Installing a sink also captures the process's stderr so that log lines which
// bypass the structured logger still reach the host.
//
//export gost_set_log_callback
func gost_set_log_callback(fn C.gost_log_cb) {
	cbMu.Lock()
	cb = fn
	cbMu.Unlock()

	if fn == nil {
		eng.SetLogSink(nil)
		return
	}
	eng.SetLogSink(deliver)
}

// gost_set_log_level sets the minimum level forwarded to the log callback.
//
//export gost_set_log_level
func gost_set_log_level(level C.int) {
	eng.SetLogLevel(engine.Level(level))
}

// gost_log_level returns the current minimum forwarded level.
//
//export gost_log_level
func gost_log_level() C.int {
	return C.int(eng.LogLevel())
}

// gost_start starts GOST with the given configuration, which may be JSON or
// YAML. config may be NULL, in which case GOST falls back to the GOST_*
// environment variables and its default config search paths.
//
// Returns 0 on success and non-zero on failure, in which case *err receives a
// message that the caller must release with gost_free. On success *err is set
// to NULL.
//
// Starting an already running instance fails rather than restarting it, so the
// caller always knows when the previous listeners were released.
//
//export gost_start
func gost_start(config *C.char, errOut **C.char) (rc C.int) {
	defer func() {
		if r := recover(); r != nil {
			setErr(errOut, panicError(r))
			rc = 1
		}
	}()

	var cfg []byte
	if config != nil {
		cfg = []byte(C.GoString(config))
	}

	if err := eng.Start(engine.Options{
		Config:   cfg,
		LogLevel: eng.LogLevel(),
		LogSink:  deliver,
	}); err != nil {
		setErr(errOut, err.Error())
		return 1
	}

	setErr(errOut, "")
	return 0
}

// gost_stop shuts the instance down and releases every listening port. It is
// idempotent: stopping a stopped instance succeeds. It never blocks on the log
// callback, so calling it from inside the callback is safe.
//
//export gost_stop
func gost_stop(errOut **C.char) (rc C.int) {
	defer func() {
		if r := recover(); r != nil {
			setErr(errOut, panicError(r))
			rc = 1
		}
	}()

	if err := eng.Stop(); err != nil {
		setErr(errOut, err.Error())
		return 1
	}

	setErr(errOut, "")
	return 0
}

// gost_reload re-parses and re-applies the configuration passed to the last
// gost_start, mirroring POST /config/reload. Services added through the Web API
// since then are discarded. To switch to a different configuration, call
// gost_stop followed by gost_start.
//
//export gost_reload
func gost_reload(errOut **C.char) (rc C.int) {
	defer func() {
		if r := recover(); r != nil {
			setErr(errOut, panicError(r))
			rc = 1
		}
	}()

	if err := eng.Reload(); err != nil {
		setErr(errOut, err.Error())
		return 1
	}

	setErr(errOut, "")
	return 0
}

// gost_is_running returns 1 while an instance is up, 0 otherwise.
//
//export gost_is_running
func gost_is_running() (rc C.int) {
	defer func() {
		if recover() != nil {
			rc = 0
		}
	}()

	if eng.Running() {
		return 1
	}
	return 0
}

// gost_free releases a string returned through an err out-parameter.
//
//export gost_free
func gost_free(p *C.char) {
	if p != nil {
		C.free(unsafe.Pointer(p))
	}
}

// deliver forwards one entry to the host callback. It runs on the emitter's
// goroutine, which is what lets the host call back into this library safely.
func deliver(level engine.Level, msg string) {
	cbMu.RLock()
	fn := cb
	cbMu.RUnlock()

	if fn == nil {
		return
	}

	cmsg := C.CString(msg)
	defer C.free(unsafe.Pointer(cmsg))

	C.call_gost_log_cb(fn, C.int(level), cmsg)
}

// setErr writes message into the caller's error out-parameter. An empty message
// writes NULL so a well-behaved caller never reads a stale pointer.
func setErr(errOut **C.char, message string) {
	if errOut == nil {
		return
	}
	if message == "" {
		*errOut = nil
		return
	}
	*errOut = C.CString(message)
}

func panicError(r any) string {
	if err, ok := r.(error); ok {
		return "gost: panic: " + err.Error()
	}
	return "gost: panic"
}

func main() {}
