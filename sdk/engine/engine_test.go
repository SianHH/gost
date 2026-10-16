package engine

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-gost/sdk/register"
)

// freePort returns a TCP port that was free at the moment of the call.
func freePort(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer ln.Close()

	return ln.Addr().String()
}

func waitListening(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s", addr)
}

func waitDialFails(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return
		}
		c.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s is still accepting connections", addr)
}

func socks5Config(addr string) []byte {
	return []byte(fmt.Sprintf(
		`{"services":[{"name":"s1","addr":%q,"handler":{"type":"socks5"},"listener":{"type":"tcp"}}]}`,
		addr))
}

func TestStartStopReleasesPort(t *testing.T) {
	addr := freePort(t)
	e := New()

	if err := e.Start(Options{Config: socks5Config(addr)}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitListening(t, addr)

	if !e.Running() {
		t.Fatal("engine reports not running after a successful start")
	}
	if got := len(e.ServiceNames()); got != 1 {
		t.Fatalf("registered services = %d, want 1", got)
	}

	if err := e.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if e.Running() {
		t.Fatal("engine still reports running after stop")
	}

	// The port must be free again: this is what proves Stop actually closed
	// the listeners rather than only flipping a flag.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port not released after stop: %v", err)
	}
	ln.Close()
}

func TestStopIsIdempotent(t *testing.T) {
	addr := freePort(t)
	e := New()

	if err := e.Start(Options{Config: socks5Config(addr)}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("first stop: %v", err)
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	// Stopping an engine that was never started must also succeed.
	if err := New().Stop(); err != nil {
		t.Fatalf("stop on fresh engine: %v", err)
	}
}

func TestRestartAfterStop(t *testing.T) {
	addr := freePort(t)
	e := New()

	for i := range 3 {
		if err := e.Start(Options{Config: socks5Config(addr)}); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		waitListening(t, addr)

		if err := e.Stop(); err != nil {
			t.Fatalf("stop %d: %v", i, err)
		}
		waitDialFails(t, addr)
	}
}

func TestStartTwiceFails(t *testing.T) {
	addr := freePort(t)
	e := New()
	t.Cleanup(func() { e.Stop() })

	if err := e.Start(Options{Config: socks5Config(addr)}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := e.Start(Options{Config: socks5Config(addr)}); err == nil {
		t.Fatal("second start succeeded, want ErrAlreadyRunning")
	}
}

func TestInvalidConfigDoesNotStart(t *testing.T) {
	e := New()

	if err := e.Start(Options{Config: []byte(`{"services":`)}); err == nil {
		t.Fatal("start accepted a malformed config")
	}
	if e.Running() {
		t.Fatal("engine is running after a failed start")
	}
	// A failed start must leave the engine usable.
	addr := freePort(t)
	if err := e.Start(Options{Config: socks5Config(addr)}); err != nil {
		t.Fatalf("start after failure: %v", err)
	}
	waitListening(t, addr)
	if err := e.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// TestDurationStringAccepted guards the reason Start goes through
// x/config/parsing/parser instead of encoding/json: duration fields such as
// "timeout": "10s" only survive viper's decode hook.
func TestDurationStringAccepted(t *testing.T) {
	addr := freePort(t)
	cfg := []byte(fmt.Sprintf(
		`{"services":[{"name":"s1","addr":%q,"handler":{"type":"socks5"},"listener":{"type":"tcp"}}],`+
			`"hops":[{"name":"h1","reload":"10s","nodes":[{"name":"n1","addr":"127.0.0.1:1"}]}]}`,
		addr))

	e := New()
	t.Cleanup(func() { e.Stop() })

	if err := e.Start(Options{Config: cfg}); err != nil {
		t.Fatalf("start with duration string: %v", err)
	}
	waitListening(t, addr)
}

func TestLogSinkReceivesEntries(t *testing.T) {
	addr := freePort(t)

	var mu sync.Mutex
	var lines []string
	done := make(chan struct{}, 1)

	e := New()
	t.Cleanup(func() { e.Stop() })

	err := e.Start(Options{
		Config:   socks5Config(addr),
		LogLevel: LevelInfo,
		LogSink: func(level Level, msg string) {
			mu.Lock()
			lines = append(lines, msg)
			mu.Unlock()

			select {
			case done <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitListening(t, addr)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("log sink was never called")
	}

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(lines, "\n"), "listening on") {
		t.Fatalf("log sink output has no listening line: %q", lines)
	}
}

func TestSetLogLevelFilters(t *testing.T) {
	addr := freePort(t)

	var mu sync.Mutex
	var levels []Level

	e := New()
	t.Cleanup(func() { e.Stop() })
	e.SetLogLevel(LevelError)
	e.SetLogSink(func(level Level, msg string) {
		mu.Lock()
		levels = append(levels, level)
		mu.Unlock()
	})

	// Start emits "listening on ..." at info level, which the filter must drop.
	if err := e.Start(Options{
		Config:   socks5Config(addr),
		LogLevel: LevelError,
		LogSink: func(level Level, msg string) {
			mu.Lock()
			levels = append(levels, level)
			mu.Unlock()
		},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitListening(t, addr)

	// Give the pipeline time to deliver anything that slipped through.
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	for _, lv := range levels {
		if lv < LevelError {
			t.Fatalf("level %d below the configured minimum reached the host: %v", lv, levels)
		}
	}
}

// TestNoSinkDoesNotBlock covers the path where the library is started without a
// host callback: the pipe must keep draining, otherwise a full pipe buffer
// would block every goroutine that logs.
func TestNoSinkDoesNotBlock(t *testing.T) {
	addr := freePort(t)
	e := New()
	t.Cleanup(func() { e.Stop() })

	if err := e.Start(Options{Config: socks5Config(addr), Debug: true}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitListening(t, addr)
}

func TestDetectLevel(t *testing.T) {
	cases := []struct {
		in   string
		want Level
	}{
		{`{"level":"warn","msg":"x"}`, LevelWarn},
		{`{"level":"debug","msg":"x"}`, LevelDebug},
		{`{"level":"fatal","msg":"x"}`, LevelFatal},
		{`2026/10/17 03:00:00 plain text`, LevelInfo},
		{``, LevelInfo},
		{`{"msg":"no level field"}`, LevelInfo},
		{`{"level":"bogus"}`, LevelInfo},
	}

	for _, c := range cases {
		if got := detectLevel(c.in); got != c.want {
			t.Errorf("detectLevel(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestAPIProxyServesTraffic exercises the whole path: Start an HTTP proxy
// service, send a request through it, and confirm the upstream response comes
// back.
func TestAPIProxyServesTraffic(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "through-gost")
	}))
	defer upstream.Close()

	proxyAddr := freePort(t)
	cfg := []byte(fmt.Sprintf(
		`{"services":[{"name":"p1","addr":%q,"handler":{"type":"http"},"listener":{"type":"tcp"}}]}`,
		proxyAddr))

	e := New()
	t.Cleanup(func() { e.Stop() })

	if err := e.Start(Options{Config: cfg}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitListening(t, proxyAddr)

	proxyURL := "http://" + proxyAddr
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(mustURL(t, proxyURL))},
		Timeout:   10 * time.Second,
	}

	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "through-gost" {
		t.Fatalf("body = %q, want %q", body, "through-gost")
	}
}

// TestAPIServerStarts checks that Options.APIAddr brings up the embedded Web
// API, which is what gives the host a way to inspect and mutate the config at
// runtime without going through the C API.
func TestAPIServerStarts(t *testing.T) {
	svcAddr := freePort(t)
	apiAddr := freePort(t)

	e := New()
	t.Cleanup(func() { e.Stop() })

	err := e.Start(Options{
		Config:  socks5Config(svcAddr),
		APIAddr: apiAddr,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitListening(t, apiAddr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+apiAddr+"/config/services", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("api request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("api status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "s1") {
		t.Fatalf("api response does not mention the service: %s", body)
	}
}

// TestReloadKeepsRunning re-applies the configuration the engine was started
// with and confirms the service is still reachable afterwards.
func TestReloadKeepsRunning(t *testing.T) {
	addr := freePort(t)
	e := New()
	t.Cleanup(func() { e.Stop() })

	if err := e.Start(Options{Config: socks5Config(addr)}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitListening(t, addr)

	if err := e.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	waitListening(t, addr)

	if err := e.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := e.Reload(); err == nil {
		t.Fatal("reload succeeded on a stopped engine")
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
