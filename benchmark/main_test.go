package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

func TestParseStickyID(t *testing.T) {
	makeURL := func(username string) *url.URL {
		return &url.URL{Scheme: "http", Host: "127.0.0.1:18080", User: url.UserPassword(username, "test-password")}
	}
	cases := []struct {
		name     string
		platform string
		username string
		rule     stickyRule
		want     string
		wantErr  bool
	}{
		{"dataimpulse", "dataimpulse", "login__cr.us;sessid.abc123", stickyRule{}, "abc123", false},
		{"dataimpulse duplicate", "dataimpulse", "login__sessid.one;sessid.two", stickyRule{}, "", true},
		{"decodo", "decodo", "user-alice-country-us-session.ignored-session-abc123-sessionduration-10", stickyRule{}, "abc123", false},
		{"decodo duplicate", "decodo", "user-alice-session-one-session-two", stickyRule{}, "", true},
		{"1024proxy", "1024proxy", "api-key-region-US-sid-abc123-t-5", stickyRule{}, "abc123", false},
		{"1024proxy missing", "1024proxy", "api-key-region-US-t-5", stickyRule{}, "", true},
		{"resin", "resin", "Default.abc123", stickyRule{}, "abc123", false},
		{"resin missing", "resin", "Default", stickyRule{}, "", true},
		{"generic", "generic", "route-sid-abc123-end", stickyRule{prefix: "route-sid-", suffix: "-end"}, "abc123", false},
		{"generic mismatch", "generic", "route-other-abc123", stickyRule{prefix: "route-sid-"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseStickyID(tc.platform, makeURL(tc.username), tc.rule)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseStickyID(%q) = %q, want error", tc.username, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("ParseStickyID(%q) = %q, %v; want %q, nil", tc.username, got, err, tc.want)
			}
		})
	}
}

func TestDeterministicBytes(t *testing.T) {
	a := deterministicBytes("request", "run-a", "1")
	b := deterministicBytes("request", "run-a", "1")
	c := deterministicBytes("request", "run-a", "2")
	if len(a) != payloadBytes {
		t.Fatalf("payload bytes=%d, want %d", len(a), payloadBytes)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("same seed produced different payloads")
	}
	if bytes.Equal(a, c) {
		t.Fatal("different sequence produced identical payloads")
	}
	if got, want := sha256Hex(a), "36c3fcab042002298290cf5fbd319fa2160da9377a028e26ad5f6f02c8ec4719"; got != want {
		t.Fatalf("fixed payload hash=%s, want %s", got, want)
	}
}

func TestHTTPVirtualExitReturnsParsedStickyIDAndFullHash(t *testing.T) {
	s, shutdown := startTestBenchServer(t, nil)
	defer shutdown()

	username := "login__cr.us;sessid.http-sticky-id"
	upstreamURL := &url.URL{Scheme: "http", Host: s.listener.Addr().String(), User: url.UserPassword(username, "test-password")}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(upstreamURL)}}
	defer client.CloseIdleConnections()

	resp := doHTTPBenchmarkRequest(t, client, "http://benchmark.test/v1/responses?run=http-run&seq=1", "http-run", "1")
	defer resp.Body.Close()
	assertBenchmarkResponse(t, resp, "http-run", "1", "http-sticky-id")
	events := s.Events()
	if len(events) != 1 || events[0].StickyID != "http-sticky-id" || events[0].RequestHash != sha256Hex(deterministicBytes("request", "http-run", "1")) {
		t.Fatalf("unexpected safe event: %+v", events)
	}
}

func TestHTTPVirtualExitRejectsWrongTarget(t *testing.T) {
	s, shutdown := startTestBenchServer(t, nil)
	defer shutdown()

	upstreamURL := &url.URL{Scheme: "http", Host: s.listener.Addr().String(), User: url.UserPassword("login__sessid.id", "test-password")}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(upstreamURL)}}
	defer client.CloseIdleConnections()
	resp := doHTTPBenchmarkRequest(t, client, "http://wrong.test/not-benchmark?run=wrong-run&seq=1", "wrong-run", "1")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%q, want 400", resp.StatusCode, body)
	}
	if got := len(s.Events()); got != 0 {
		t.Fatalf("recorded events=%d, want 0", got)
	}
}

func TestConnectVirtualExitReturnsParsedStickyIDAndFullHashOverHTTP2(t *testing.T) {
	cert, roots := testCertificate(t)
	s, shutdown := startTestBenchServer(t, &cert)
	defer shutdown()

	tlsConn := connectTunnel(t, s.listener.Addr().String(), "benchmark.test:443", "login__cr.us;sessid.https-sticky-id", roots)
	defer tlsConn.Close()
	if got := tlsConn.ConnectionState().NegotiatedProtocol; got != "h2" {
		t.Fatalf("negotiated protocol=%q, want h2", got)
	}

	clientConn, err := (&http2.Transport{}).NewClientConn(tlsConn)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()
	req := benchmarkRequest(t, "https://benchmark.test/v1/responses?run=https-run&seq=2", "https-run", "2", "https")
	resp, err := clientConn.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("response protocol=%s, want HTTP/2", resp.Proto)
	}
	assertBenchmarkResponse(t, resp, "https-run", "2", "https-sticky-id")
	if got := len(s.Events()); got != 1 {
		t.Fatalf("recorded events=%d, want 1", got)
	}
}

func TestConnectVirtualExitRejectsWrongTarget(t *testing.T) {
	cert, _ := testCertificate(t)
	s, shutdown := startTestBenchServer(t, &cert)
	defer shutdown()

	conn, err := net.DialTimeout("tcp", s.listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	capturedAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("login__sessid.id:test-password"))
	if _, err := io.WriteString(conn, "CONNECT wrong.test:443 HTTP/1.1\r\nHost: wrong.test:443\r\nProxy-Authorization: "+capturedAuth+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("CONNECT status=%d body=%q, want 400", resp.StatusCode, body)
	}
}

func startTestBenchServer(t *testing.T, cert *tls.Certificate) (*benchServer, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &benchServer{
		listener: ln,
		platform: "dataimpulse",
		cert:     cert,
		logger:   slogDiscard(),
	}
	httpServer := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       10 * time.Second,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = httpServer.Serve(ln)
	}()
	return s, func() {
		_ = httpServer.Close()
		<-done
	}
}

func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func doHTTPBenchmarkRequest(t *testing.T, client *http.Client, target, runID, seq string) *http.Response {
	t.Helper()
	resp, err := client.Do(benchmarkRequest(t, target, runID, seq, "http"))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func benchmarkRequest(t *testing.T, target, runID, seq, marker string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(deterministicBytes("request", runID, seq)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer bench-marker-"+marker)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Bench-Run", runID)
	req.Header.Set("X-Bench-Seq", seq)
	req.Header.Set("X-Bench-Contract", "no-rewrite-v1")
	return req
}

func connectTunnel(t *testing.T, addr, target, username string, roots *x509.CertPool) *tls.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	capturedAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":test-password"))
	if _, err := io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\nProxy-Authorization: "+capturedAuth+"\r\n\r\n"); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	connectResp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	if connectResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(connectResp.Body)
		connectResp.Body.Close()
		_ = conn.Close()
		t.Fatalf("CONNECT status=%d body=%q", connectResp.StatusCode, body)
	}
	connectResp.Body.Close()
	tlsConn := tls.Client(&bufferedConn{Conn: conn, reader: reader}, &tls.Config{
		RootCAs:    roots,
		ServerName: "benchmark.test",
		NextProtos: []string{"h2"},
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	return tlsConn
}

func assertBenchmarkResponse(t *testing.T, resp *http.Response, runID, seq, stickyID string) {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type=%q, want application/octet-stream", got)
	}
	if got := resp.Header.Get("Content-Length"); got != "65536" {
		t.Fatalf("Content-Length=%q, want 65536", got)
	}
	if got := resp.Header.Get("X-Bench-Sticky-ID"); got != stickyID {
		t.Fatalf("sticky ID=%q, want %q", got, stickyID)
	}
	if got := resp.Header.Get("X-Bench-Run"); got != runID {
		t.Fatalf("run ID=%q, want %q", got, runID)
	}
	if got := resp.Header.Get("X-Bench-Seq"); got != seq {
		t.Fatalf("sequence=%q, want %q", got, seq)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	want := deterministicBytes("response", runID, seq)
	if !bytes.Equal(body, want) {
		t.Fatal("response body does not equal full deterministic 64 KiB payload")
	}
	if got, wantHash := resp.Header.Get("X-Bench-Response-Body-SHA256"), sha256Hex(body); got != wantHash {
		t.Fatalf("response body hash=%q, want %q", got, wantHash)
	}
	if got, wantHash := resp.Header.Get("X-Bench-Request-SHA256"), sha256Hex(deterministicBytes("request", runID, seq)); got != wantHash {
		t.Fatalf("request body hash=%q, want %q", got, wantHash)
	}
	if got := resp.Header.Get("X-Bench-Request-Bytes"); got != "65536" {
		t.Fatalf("request bytes=%q, want 65536", got)
	}
	if got := resp.Header.Get("X-Bench-Received-At"); got == "" {
		t.Fatal("missing received-at timestamp")
	}
}

func testCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 63))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "benchmark.test"},
		DNSNames:     []string{"benchmark.test"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return cert, roots
}
