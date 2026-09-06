package xhttp

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// TestThroughputPacketUpH1 measures one-directional upload throughput of a
// single XHTTP packet-up session over plaintext HTTP/1.1 on loopback — the
// exact data path suspected to cap at ~1MB/s. The app payload is written in
// writeChunk chunks (like the gost relay pipeHalf writes). Run explicitly:
//
//	go test ./internal/util/xhttp/ -run Throughput -v -timeout 10m
//
// The total transfer size can be overridden with XHTTP_BENCH_MB.
func TestThroughputPacketUpH1(t *testing.T) {
	if os.Getenv("XHTTP_BENCH_MB") == "" {
		t.Skip("set XHTTP_BENCH_MB to run the throughput benchmark")
	}
	mib := int64(16)
	if v := os.Getenv("XHTTP_BENCH_MB"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			mib = n
		}
	}
	var writeChunk int64 = 32 * 1024 // mirrors relay xnet.Pipe chunk (bufferSize/2)

	for _, cfg := range []*Config{
		{Path: "/x/"},                    // mode auto over h1.1 -> packet-up
		{Path: "/x/", Mode: "packet-up"}, // explicit packet-up
		{Path: "/x/", Mode: "stream-up"}, // stream-up for reference
	} {
		modeName := "auto"
		if cfg.Mode != "" {
			modeName = cfg.Mode
		}
		t.Run(modeName, func(t *testing.T) {
			t.Logf("mode=%s chunk=%d total=%dMiB", modeName, writeChunk, mib)
			mbps, duration := runUploadBench(t, cfg, nil, nil, mib, writeChunk)
			t.Logf("mode=%s upload throughput: %.1f MB/s over %.2fs", modeName, mbps, duration.Seconds())
		})
	}
}

// TestThroughputStreamUpH2TLS measures upload throughput for HTTP/2 TLS
// (stream-up) for reference comparison.
func TestThroughputStreamUpH2TLS(t *testing.T) {
	if os.Getenv("XHTTP_BENCH_MB") == "" {
		t.Skip("set XHTTP_BENCH_MB to run the throughput benchmark")
	}
	serverCert, caPool := selfSignedCert(t)
	srvTLS := &tls.Config{Certificates: []tls.Certificate{serverCert}, NextProtos: []string{"h2", "http/1.1"}}
	cliTLS := &tls.Config{RootCAs: caPool, ServerName: "localhost", NextProtos: []string{"h2", "http/1.1"}}

	mib := int64(16)
	mbps, duration := runUploadBench(t, &Config{Path: "/x/"}, srvTLS, cliTLS, mib, 32*1024)
	t.Logf("h2-tls upload throughput: %.1f MB/s over %.2fs", mbps, duration.Seconds())
}

// runUploadBench drives totalMiB MiB of data from the client engine into the
// server engine in writeChunk sized writes and returns MB/s + duration.
func runUploadBench(t *testing.T, serverCfg *Config, srvTLS *tls.Config, cliTLS *tls.Config,
	totalMiB, writeChunk int64) (float64, time.Duration) {
	t.Helper()

	srvLog, _ := newBufLogger(t)
	cliLog, _ := newBufLogger(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	tlsEnabled := srvTLS != nil
	var rawLn net.Listener = ln
	if tlsEnabled {
		rawLn = tls.NewListener(ln, srvTLS)
	}

	server := NewServer(rawLn, tlsEnabled, serverCfg, BacklogServerOption(32), LoggerServerOption(srvLog))
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve() }()
	defer server.Close()

	acceptCh := make(chan net.Conn, 8)
	go func() {
		for {
			conn, err := server.Accept()
			if err != nil {
				return
			}
			acceptCh <- conn
		}
	}()

	addr := ln.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	baseConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}

	var tlsCfg *tls.Config
	if tlsEnabled {
		tlsCfg = cliTLS
	}
	dialExtra := func(ectx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ectx, "tcp", addr)
	}

	conn, err := Dial(ctx, DialOptions{
		Config:    serverCfg,
		BaseConn:  baseConn,
		ExtraDial: dialExtra,
		TLSConfig: tlsCfg,
		URLHost:   addr,
		Logger:    cliLog,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	accepted, ok := <-acceptCh
	if !ok {
		t.Fatal("no accepted session")
	}
	defer accepted.Close()

	total := totalMiB << 20
	var got atomic.Int64
	drainDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 128*1024)
		for got.Load() < total {
			n, err := accepted.Read(buf)
			if n > 0 {
				got.Add(int64(n))
			}
			if err != nil {
				drainDone <- err
				return
			}
		}
		drainDone <- nil
	}()

	payload := make([]byte, writeChunk)
	for i := range payload {
		payload[i] = byte(i)
	}

	start := time.Now()
	written := int64(0)
	for written < total {
		n, err := conn.Write(payload)
		if err != nil {
			t.Fatalf("write at %d: %v", written, err)
		}
		written += int64(n)
	}

	if err := <-drainDone; err != nil && err != io.EOF {
		t.Fatalf("drain: %v", err)
	}
	dur := time.Since(start)

	if got.Load() != total {
		t.Fatalf("server received %d bytes, want %d", got.Load(), total)
	}

	mbps := float64(total) / dur.Seconds() / (1024 * 1024)
	return mbps, dur
}
