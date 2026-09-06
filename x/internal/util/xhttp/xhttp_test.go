package xhttp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	coreLogger "github.com/go-gost/core/logger"
	xlogger "github.com/go-gost/x/logger"
)

func TestUploadQueueReorder(t *testing.T) {
	q := NewUploadQueue(16)

	// Push out of order; Read must still deliver the original byte order.
	q.Push(Packet{Payload: []byte("b"), Seq: 1})
	q.Push(Packet{Payload: []byte("c"), Seq: 2})
	q.Push(Packet{Payload: []byte("a"), Seq: 0})
	q.Push(Packet{Payload: []byte("d"), Seq: 3})

	out := make([]byte, 0, 16)
	buf := make([]byte, 1)
	for len(out) < 4 {
		n, err := q.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		out = append(out, buf[:n]...)
	}
	if string(out) != "abcd" {
		t.Fatalf("got %q, want abcd", out)
	}
}

func TestUploadQueuePartialRead(t *testing.T) {
	q := NewUploadQueue(16)
	q.Push(Packet{Payload: []byte("hello world"), Seq: 0})

	buf := make([]byte, 5)
	got := make([]byte, 0, 32)
	for i := 0; i < 3; i++ {
		n, err := q.Read(buf)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		got = append(got, buf[:n]...)
	}
	if string(got) != "hello world" {
		t.Fatalf("got %q, want %q", got, "hello world")
	}
}

// TestRegressionReadZero mirrors the xray regression test.
func TestRegressionReadZero(t *testing.T) {
	q := NewUploadQueue(10)
	if err := q.Push(Packet{Payload: []byte("x"), Seq: 0}); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 20)
	n, err := q.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("n =", n)
	}
}

func TestPaddingValidation(t *testing.T) {
	cfg := &Config{Path: "/x/"}

	// No x_padding at all -> invalid.
	req, _ := http.NewRequest(http.MethodGet, "http://localhost/x/sess", nil)
	ok, _, _ := cfg.ValidateRequestPadding(req)
	if ok {
		t.Fatal("expected invalid padding for request without padding")
	}

	// Valid padding inside the Referer query (default non-obfs placement).
	req2, _ := http.NewRequest(http.MethodGet, "http://localhost/x/sess", nil)
	req2.Header.Set("Referer", "http://localhost/x/sess?x_padding="+strings.Repeat("X", 500))
	ok2, _, _ := cfg.ValidateRequestPadding(req2)
	if !ok2 {
		t.Fatal("expected valid padding for Referer x_padding=500 X")
	}

	// Too-short padding -> invalid.
	req3, _ := http.NewRequest(http.MethodGet, "http://localhost/x/sess", nil)
	req3.Header.Set("Referer", "http://localhost/x/sess?x_padding="+strings.Repeat("X", 10))
	ok3, _, _ := cfg.ValidateRequestPadding(req3)
	if ok3 {
		t.Fatal("expected invalid padding for too-short padding")
	}

	// Explicit padding range (tokenish generation over that range must also
	// validate within the huffman tolerance).
	cfg2 := &Config{Path: "/x/", XPaddingMethod: string(PaddingMethodTokenish)}
	padding := GeneratePadding(PaddingMethodTokenish, 500)
	req4, _ := http.NewRequest(http.MethodGet, "http://localhost/x/sess", nil)
	req4.Header.Set("Referer", "http://localhost/x/sess?x_padding="+padding)
	ok4, _, _ := cfg2.ValidateRequestPadding(req4)
	if !ok4 {
		t.Fatal("expected valid tokenish padding")
	}
}

// ---- engine level loopback tests ----

func TestEchoPacketUpPlainH1(t *testing.T) {
	runEcho(t, &Config{Path: "/x/"}, nil, nil)
}

func TestEchoStreamUpPlainH1(t *testing.T) {
	runEcho(t, &Config{Path: "/x/", Mode: "stream-up"}, nil, nil)
}

func TestEchoStreamUpH2TLS(t *testing.T) {
	serverCert, caPool := selfSignedCert(t)

	srvTLS := &tls.Config{Certificates: []tls.Certificate{serverCert}, NextProtos: []string{"h2", "http/1.1"}}
	cliTLS := &tls.Config{RootCAs: caPool, ServerName: "localhost", NextProtos: []string{"h2", "http/1.1"}}

	runEcho(t, &Config{Path: "/x/"}, srvTLS, cliTLS)
}

func TestEchoStreamOneH2TLS(t *testing.T) {
	serverCert, caPool := selfSignedCert(t)

	srvTLS := &tls.Config{Certificates: []tls.Certificate{serverCert}, NextProtos: []string{"h2", "http/1.1"}}
	cliTLS := &tls.Config{RootCAs: caPool, ServerName: "localhost", NextProtos: []string{"h2", "http/1.1"}}

	runEcho(t, &Config{Path: "/x/", Mode: "stream-one"}, srvTLS, cliTLS)
}

func TestEchoPacketUpH1WithModePacketUpServer(t *testing.T) {
	// server restricted to packet-up, client auto over h1.1 -> packet-up.
	runEcho(t, &Config{Path: "/x/", Mode: "packet-up"}, nil, nil)
}

// runEcho starts an XHTTP server over a loopback listener, establishes one
// session with the client engine, and echoes a payload.
func runEcho(t *testing.T, serverCfg *Config, srvTLS *tls.Config, cliTLS *tls.Config) {
	t.Helper()

	srvLog, srvLogOut := newBufLogger(t)
	cliLog, cliLogOut := newBufLogger(t)

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

	server := NewServer(rawLn, tlsEnabled, serverCfg, BacklogServerOption(8), LoggerServerOption(srvLog))
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

	failf := func(format string, args ...any) {
		t.Helper()
		t.Logf("--- server log ---\n%s", srvLogOut())
		t.Logf("--- client log ---\n%s", cliLogOut())
		t.Fatalf(format, args...)
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
		failf("dial: %v", err)
	}
	defer conn.Close()

	accepted, ok := <-acceptCh
	if !ok {
		failf("no accepted session")
	}

	payload := []byte("hello xhttp echo!")
	if _, err := conn.Write(payload); err != nil {
		failf("write: %v", err)
	}

	// echo the payload back from the server side.
	echoBack := make([]byte, len(payload))
	if _, err := io.ReadFull(accepted, echoBack); err != nil {
		failf("server read: %v", err)
	}
	if !bytes.Equal(echoBack, payload) {
		failf("server got %q, want %q", echoBack, payload)
	}
	if _, err := accepted.Write(payload); err != nil {
		failf("server write: %v", err)
	}

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		failf("client read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		failf("client got %q, want %q", got, payload)
	}
}

func selfSignedCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost", Organization: []string{"xhttp-test"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	return cert, pool
}

// newBufLogger returns a logger writing to a locked in-memory buffer along
// with a snapshot function returning the accumulated output.
func newBufLogger(t *testing.T) (coreLogger.Logger, func() string) {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	out := func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
	w := &lockedWriter{mu: &mu, w: &buf}
	l := xlogger.NewLogger(
		xlogger.OutputOption(w),
		xlogger.LevelOption(coreLogger.TraceLevel),
		xlogger.FormatOption(coreLogger.TextFormat),
	)
	return l, out
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}
