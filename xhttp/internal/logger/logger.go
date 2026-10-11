// Package logger is a tiny pluggable logging hook.
//
// By default nothing is logged. Call Set from the dialer/listener SetLogger
// helpers to route messages into your own logger.
package logger

// Logf is the current logging sink. It is nil by default (silent).
var Logf func(format string, v ...any)

// Set replaces the current logging sink.
func Set(f func(format string, v ...any)) {
	Logf = f
}

// Printf forwards to the current sink, if any.
func Printf(format string, v ...any) {
	if Logf != nil {
		Logf(format, v...)
	}
}
