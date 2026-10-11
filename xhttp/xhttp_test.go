package xhttp_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	"xhttp/config"
	"xhttp/dialer"
	"xhttp/listener"
)

const testChunkSize = 32 * 1024

func newTestTLSConfig(t *testing.T, serverProtos, clientProtos []string) (server, client *tls.Config) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "example.com"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"example.com"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	server = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}},
		NextProtos:   serverProtos,
	}
	client = &tls.Config{
		RootCAs:    pool,
		ServerName: "example.com",
		NextProtos: clientProtos,
	}
	return server, client
}

func runRoundTrip(t *testing.T, mode string, useTLS, h2 bool, payload []byte) {
	t.Helper()

	serverCfg := &config.Config{
		Mode:               mode,
		ScMaxEachPostBytes: &config.Range{From: testChunkSize, To: testChunkSize},
	}
	clientCfg := &config.Config{
		Mode:               mode,
		ScMaxEachPostBytes: &config.Range{From: testChunkSize, To: testChunkSize},
	}

	var serverTLS, clientTLS *tls.Config
	if useTLS {
		serverProtos := []string{"http/1.1"}
		clientProtos := []string{"http/1.1"}
		if h2 {
			// Offer only h2 on the server, so that any fallback to HTTP/1.1
			// would break the ALPN handshake and fail the test.
			serverProtos = []string{"h2"}
			clientProtos = []string{"h2"}
		}
		serverTLS, clientTLS = newTestTLSConfig(t, serverProtos, clientProtos)
	}

	ln, err := listener.Listen(context.Background(), "127.0.0.1:0", serverCfg, serverTLS)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(c)
		}
	}()

	d := &dialer.Dialer{
		Address:   ln.Addr().String(),
		Config:    clientCfg,
		TLSConfig: clientTLS,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := d.DialContext(ctx)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	writeErr := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, bytes.NewReader(payload))
		writeErr <- err
	}()

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("write: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

func TestXHTTPRoundTrip(t *testing.T) {
	payload := make([]byte, 100*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		mode string
		tls  bool
		h2   bool
	}{
		{"auto-plain", "", false, false},
		{"packet-up-plain", "packet-up", false, false},
		{"stream-one-plain", "stream-one", false, false},
		{"stream-up-plain", "stream-up", false, false},
		{"packet-up-tls-h1", "packet-up", true, false},
		{"stream-one-tls-h1", "stream-one", true, false},
		{"stream-up-tls-h1", "stream-up", true, false},
		{"packet-up-tls-h2", "packet-up", true, true},
		{"stream-one-tls-h2", "stream-one", true, true},
		{"stream-up-tls-h2", "stream-up", true, true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			runRoundTrip(t, tc.mode, tc.tls, tc.h2, payload)
		})
	}
}

func TestXHTTPConcurrentSessions(t *testing.T) {
	payload := make([]byte, 16*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	serverCfg := &config.Config{Mode: "packet-up"}
	clientCfg := &config.Config{Mode: "packet-up"}

	ln, err := listener.Listen(context.Background(), "127.0.0.1:0", serverCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(c)
		}
	}()

	d := &dialer.Dialer{Address: ln.Addr().String(), Config: clientCfg}

	const n = 4
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			conn, err := d.DialContext(ctx)
			if err != nil {
				errCh <- err
				return
			}
			defer conn.Close()

			go func() {
				io.Copy(conn, bytes.NewReader(payload))
			}()

			got := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, got); err != nil {
				errCh <- err
				return
			}
			if !bytes.Equal(got, payload) {
				errCh <- err
				return
			}
			errCh <- nil
		}()
	}

	for i := 0; i < n; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}
}
