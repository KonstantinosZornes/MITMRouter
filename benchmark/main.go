// Command benchmark is a local, no-egress virtual upstream exit for
// MITMRouter benchmarks. It accepts real HTTP and CONNECT ingress traffic
// but never dials a destination: it validates each 64 KiB test request and
// returns a deterministic 64 KiB response plus benchmark metadata.
package main

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"

	"mitmrouter/internal/upstream"
)

const payloadBytes = 64 * 1024

// stickyRule tells the generic-platform parser how to isolate a sticky ID in
// the real username MITMRouter sent to the upstream exit.
type stickyRule struct {
	prefix string
	suffix string
}

type config struct {
	addr     string
	platform string
	tlsCert  string
	tlsKey   string
	generic  stickyRule
}

type benchServer struct {
	listener net.Listener
	platform string
	cert     *tls.Certificate
	generic  stickyRule
	logger   *slog.Logger

	eventsMu sync.Mutex
	events   []benchmarkEvent
}

// benchmarkEvent is safe to put in benchmark results: it records only test
// IDs and hashes, never a Bearer value, password, or Proxy-Authorization.
type benchmarkEvent struct {
	RunID        string    `json:"run_id"`
	Sequence     string    `json:"sequence"`
	StickyID     string    `json:"sticky_id"`
	ReceivedAt   time.Time `json:"received_at"`
	RequestHash  string    `json:"request_body_sha256"`
	ResponseHash string    `json:"response_body_sha256"`
}

type requestMeta struct {
	runID       string
	seq         string
	stickyID    string
	receivedAt  time.Time
	requestHash string
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.addr, "addr", "127.0.0.1:18080", "listen address")
	flag.StringVar(&cfg.platform, "platform", upstream.PlatformDataImpulse, "upstream platform: dataimpulse, decodo, 1024proxy, resin, generic")
	flag.StringVar(&cfg.tlsCert, "tls-cert", "", "PEM certificate for the virtual benchmark.test TLS origin")
	flag.StringVar(&cfg.tlsKey, "tls-key", "", "PEM private key for the virtual benchmark.test TLS origin")
	flag.StringVar(&cfg.generic.prefix, "generic-sticky-prefix", "", "generic platform username prefix before sticky ID")
	flag.StringVar(&cfg.generic.suffix, "generic-sticky-suffix", "", "generic platform username suffix after sticky ID")
	flag.Parse()

	if err := validateConfig(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "benchmark:", err)
		os.Exit(2)
	}

	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchmark listen:", err)
		os.Exit(1)
	}
	defer ln.Close()
	if err := requireLoopbackListener(ln); err != nil {
		fmt.Fprintln(os.Stderr, "benchmark:", err)
		os.Exit(2)
	}

	var cert *tls.Certificate
	if cfg.tlsCert != "" {
		loaded, err := tls.LoadX509KeyPair(cfg.tlsCert, cfg.tlsKey)
		if err != nil {
			fmt.Fprintln(os.Stderr, "benchmark load TLS certificate:", err)
			os.Exit(1)
		}
		cert = &loaded
	}

	s := &benchServer{
		listener: ln,
		platform: strings.ToLower(cfg.platform),
		cert:     cert,
		generic:  cfg.generic,
		logger:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	s.logger.Info("benchmark virtual upstream listening",
		"addr", ln.Addr().String(),
		"platform", s.platform,
		"connect_tls_ready", cert != nil,
		"egress", false,
	)
	if err := (&http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       30 * time.Second,
	}).Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.logger.Error("benchmark virtual upstream stopped", "err", err)
		os.Exit(1)
	}
}

func requireLoopbackListener(ln net.Listener) error {
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || addr.IP == nil || !addr.IP.IsLoopback() {
		return fmt.Errorf("listen address %q must be a loopback TCP address", ln.Addr())
	}
	return nil
}

func validateConfig(c config) error {
	platform := strings.ToLower(strings.TrimSpace(c.platform))
	switch platform {
	case upstream.PlatformDataImpulse, upstream.PlatformDecodo, upstream.Platform1024Proxy, upstream.PlatformResin:
	case upstream.PlatformGeneric:
		if c.generic.prefix == "" {
			return errors.New("generic platform requires -generic-sticky-prefix")
		}
	default:
		return fmt.Errorf("unsupported platform %q", c.platform)
	}
	if (c.tlsCert == "") != (c.tlsKey == "") {
		return errors.New("-tls-cert and -tls-key must be provided together")
	}
	return nil
}

func (s *benchServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		s.handleConnect(w, r)
		return
	}
	meta, err := s.readRequest(r)
	if err != nil {
		s.writeBadRequest(w, err)
		return
	}
	s.record(meta)
	s.writeResponse(w, meta)
}

func (s *benchServer) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Host != "benchmark.test:443" {
		http.Error(w, "benchmark CONNECT target must be benchmark.test:443", http.StatusBadRequest)
		return
	}
	if s.cert == nil {
		http.Error(w, "HTTPS benchmark requires -tls-cert and -tls-key", http.StatusNotImplemented)
		return
	}
	stickyID, err := s.stickyIDFromIngressAuth(r.Header.Get("Proxy-Authorization"))
	if err != nil {
		s.writeBadRequest(w, err)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "CONNECT hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return
	}
	if _, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = conn.Close()
		return
	}
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return
	}

	// http.ReadRequest may already have buffered TLS bytes. Keep them when
	// handing the connection to tls.Server; otherwise the first ClientHello can
	// be lost under load.
	tunnel := &bufferedConn{Conn: conn, reader: rw.Reader}
	if err := s.serveTLS(tunnel, stickyID); err != nil && !errors.Is(err, net.ErrClosed) {
		s.logger.Debug("benchmark CONNECT tunnel ended", "err", err)
	}
}

func (s *benchServer) serveTLS(conn net.Conn, stickyID string) error {
	defer conn.Close()
	tlsConn := tls.Server(conn, &tls.Config{
		Certificates: []tls.Certificate{*s.cert},
		NextProtos:   []string{"h2", "http/1.1"},
		MinVersion:   tls.VersionTLS12,
	})
	tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		return err
	}
	tlsConn.SetDeadline(time.Time{})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta, err := s.readRequestWithStickyID(r, stickyID, false)
		if err != nil {
			s.writeBadRequest(w, err)
			return
		}
		s.record(meta)
		s.writeResponse(w, meta)
	})
	if tlsConn.ConnectionState().NegotiatedProtocol == "h2" {
		(&http2.Server{
			IdleTimeout:     30 * time.Second,
			ReadIdleTimeout: 15 * time.Second,
			PingTimeout:     5 * time.Second,
		}).ServeConn(tlsConn, &http2.ServeConnOpts{Handler: handler})
		return nil
	}
	return (&http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       30 * time.Second,
	}).Serve(newSingleConnListener(tlsConn))
}

func (s *benchServer) readRequest(r *http.Request) (requestMeta, error) {
	stickyID, err := s.stickyIDFromIngressAuth(r.Header.Get("Proxy-Authorization"))
	if err != nil {
		return requestMeta{}, err
	}
	return s.readRequestWithStickyID(r, stickyID, true)
}

func (s *benchServer) readRequestWithStickyID(r *http.Request, stickyID string, ingressLayer bool) (requestMeta, error) {
	if r.Method != http.MethodPost {
		return requestMeta{}, fmt.Errorf("want POST, got %s", r.Method)
	}
	runID, seq := r.Header.Get("X-Bench-Run"), r.Header.Get("X-Bench-Seq")
	if runID == "" || seq == "" {
		return requestMeta{}, errors.New("missing X-Bench-Run or X-Bench-Seq")
	}
	if err := validateBusinessRequest(r, runID, seq, ingressLayer); err != nil {
		return requestMeta{}, err
	}
	body, err := readBenchmarkBody(r.Body)
	if err != nil {
		return requestMeta{}, err
	}
	if len(body) != payloadBytes {
		return requestMeta{}, fmt.Errorf("request body bytes=%d, want %d", len(body), payloadBytes)
	}
	want := deterministicBytes("request", runID, seq)
	if subtle.ConstantTimeCompare(body, want) != 1 {
		return requestMeta{}, errors.New("request body does not match deterministic 64 KiB payload")
	}
	if r.ContentLength != payloadBytes {
		return requestMeta{}, fmt.Errorf("Content-Length=%d, want %d", r.ContentLength, payloadBytes)
	}
	return requestMeta{
		runID:       runID,
		seq:         seq,
		stickyID:    stickyID,
		receivedAt:  time.Now().UTC(),
		requestHash: sha256Hex(body),
	}, nil
}

func readBenchmarkBody(body io.ReadCloser) ([]byte, error) {
	// http.Server has ReadTimeout for HTTP/1.1, but an HTTP/2 stream needs its
	// own limit. Closing the stream body interrupts io.ReadAll without leaving a
	// goroutine behind when a client dribbles the fixed 64 KiB payload forever.
	timedOut := make(chan struct{})
	timer := time.AfterFunc(30*time.Second, func() {
		close(timedOut)
		_ = body.Close()
	})
	defer timer.Stop()
	result, err := io.ReadAll(io.LimitReader(body, payloadBytes+1))
	select {
	case <-timedOut:
		return nil, errors.New("request body read timed out")
	default:
	}
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	return result, nil
}

func validateBusinessRequest(r *http.Request, runID, seq string, ingressLayer bool) error {
	if r.Host != "benchmark.test" {
		return fmt.Errorf("Host=%q, want benchmark.test", r.Host)
	}
	if r.URL.Path != "/v1/responses" {
		return fmt.Errorf("path=%q, want /v1/responses", r.URL.Path)
	}
	wantQuery := "run=" + url.QueryEscape(runID) + "&seq=" + url.QueryEscape(seq)
	if r.URL.RawQuery != wantQuery {
		return fmt.Errorf("raw query=%q, want %q", r.URL.RawQuery, wantQuery)
	}
	if ingressLayer {
		if r.URL.Scheme != "http" || r.URL.Host != "benchmark.test" {
			return fmt.Errorf("absolute request target=%q, want http://benchmark.test", r.URL.String())
		}
		if r.Header.Get("Proxy-Authorization") == "" {
			return errors.New("HTTP proxy request is missing Proxy-Authorization")
		}
	} else {
		if r.URL.IsAbs() {
			return fmt.Errorf("tunneled request must use origin-form, got %q", r.URL.String())
		}
		if r.Header.Get("Proxy-Authorization") != "" {
			return errors.New("Proxy-Authorization leaked into tunneled business request")
		}
	}
	if got := r.Header.Get("X-Bench-Contract"); got != "no-rewrite-v1" {
		return fmt.Errorf("X-Bench-Contract=%q, want no-rewrite-v1", got)
	}
	if r.Header.Get("Content-Type") != "application/octet-stream" {
		return fmt.Errorf("Content-Type=%q, want application/octet-stream", r.Header.Get("Content-Type"))
	}
	const markerPrefix = "Bearer bench-marker-"
	if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, markerPrefix) || len(auth) == len(markerPrefix) {
		return fmt.Errorf("missing benchmark Bearer marker (received headers: %s)", strings.Join(headerNames(r.Header), ","))
	}
	return nil
}

func headerNames(header http.Header) []string {
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (s *benchServer) record(meta requestMeta) {
	responseBody := deterministicBytes("response", meta.runID, meta.seq)
	s.eventsMu.Lock()
	s.events = append(s.events, benchmarkEvent{
		RunID:        meta.runID,
		Sequence:     meta.seq,
		StickyID:     meta.stickyID,
		ReceivedAt:   meta.receivedAt,
		RequestHash:  meta.requestHash,
		ResponseHash: sha256Hex(responseBody),
	})
	s.eventsMu.Unlock()
}

// Events returns a snapshot so the benchmark runner can calculate sticky-ID
// stability and cross-Marker collision metrics without retaining credentials.
func (s *benchServer) Events() []benchmarkEvent {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	return append([]benchmarkEvent(nil), s.events...)
}

func (s *benchServer) writeResponse(w http.ResponseWriter, meta requestMeta) {
	body := deterministicBytes("response", meta.runID, meta.seq)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.Header().Set("X-Bench-Request-Bytes", fmt.Sprint(payloadBytes))
	w.Header().Set("X-Bench-Request-SHA256", meta.requestHash)
	w.Header().Set("X-Bench-Response-Body-SHA256", sha256Hex(body))
	w.Header().Set("X-Bench-Sticky-ID", meta.stickyID)
	w.Header().Set("X-Bench-Received-At", meta.receivedAt.Format(time.RFC3339Nano))
	w.Header().Set("X-Bench-Run", meta.runID)
	w.Header().Set("X-Bench-Seq", meta.seq)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *benchServer) writeBadRequest(w http.ResponseWriter, err error) {
	// The error is safe: it only describes benchmark contract violations and
	// never includes credentials or a request body.
	http.Error(w, "benchmark contract failed: "+err.Error(), http.StatusBadRequest)
}

func (s *benchServer) stickyIDFromIngressAuth(value string) (string, error) {
	user, pass, err := basicCredentials(value)
	if err != nil {
		return "", err
	}
	// Reconstruct the URL from exactly the credentials received on this listener. The
	// parser intentionally never sees an expected account supplied by a test.
	receivedText := (&url.URL{
		Scheme: "http",
		Host:   s.listener.Addr().String(),
		User:   url.UserPassword(user, pass),
	}).String()
	received, err := url.Parse(receivedText)
	if err != nil {
		return "", fmt.Errorf("rebuild received sticky URL: %w", err)
	}
	return parseStickyID(s.platform, received, s.generic)
}

func basicCredentials(value string) (user, pass string, err error) {
	const prefix = "Basic "
	if len(value) <= len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return "", "", errors.New("missing Basic Proxy-Authorization header")
	}
	raw, err := base64.StdEncoding.DecodeString(value[len(prefix):])
	if err != nil {
		return "", "", errors.New("invalid Basic auth encoding")
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 || parts[0] == "" {
		return "", "", errors.New("invalid exit credentials")
	}
	return parts[0], parts[1], nil
}

func parseStickyID(platform string, received *url.URL, generic stickyRule) (string, error) {
	if received == nil || received.User == nil {
		return "", errors.New("received sticky URL has no username")
	}
	username := received.User.Username()
	switch strings.ToLower(platform) {
	case upstream.PlatformDataImpulse:
		return dataImpulseStickyID(username)
	case upstream.PlatformDecodo:
		return flatStickyID(username, decodoKeys, "session")
	case upstream.Platform1024Proxy:
		return flatStickyID(username, c1024Keys, "sid")
	case upstream.PlatformResin:
		platformName, id, ok := strings.Cut(username, ".")
		if !ok || platformName == "" || id == "" {
			return "", errors.New("resin username must be Platform.stickyID")
		}
		return id, nil
	case upstream.PlatformGeneric:
		if generic.prefix == "" || !strings.HasPrefix(username, generic.prefix) {
			return "", errors.New("generic username does not match sticky prefix")
		}
		id := strings.TrimPrefix(username, generic.prefix)
		if generic.suffix != "" {
			if !strings.HasSuffix(id, generic.suffix) {
				return "", errors.New("generic username does not match sticky suffix")
			}
			id = strings.TrimSuffix(id, generic.suffix)
		}
		if id == "" {
			return "", errors.New("generic sticky ID is empty")
		}
		return id, nil
	default:
		return "", fmt.Errorf("unsupported platform %q", platform)
	}
}

func dataImpulseStickyID(username string) (string, error) {
	_, params, ok := strings.Cut(username, "__")
	if !ok {
		return "", errors.New("dataimpulse username has no parameter section")
	}
	var id string
	for _, segment := range strings.Split(params, ";") {
		key, value, hasValue := strings.Cut(strings.TrimSpace(segment), ".")
		if key != "sessid" {
			continue
		}
		if !hasValue || value == "" {
			return "", errors.New("dataimpulse sessid is empty")
		}
		if id != "" {
			return "", errors.New("dataimpulse username has multiple sessid parameters")
		}
		id = value
	}
	if id == "" {
		return "", errors.New("dataimpulse username has no sessid parameter")
	}
	return id, nil
}

var decodoKeys = map[string]bool{
	"country": true, "city": true, "st": true, "state": true, "asn": true,
	"session": true, "sessionduration": true, "session_iplock": true,
}

var c1024Keys = map[string]bool{
	"region": true, "st": true, "city": true,
	"asn": true, "sid": true, "t": true,
}

func flatStickyID(username string, known map[string]bool, wanted string) (string, error) {
	if username == "" {
		return "", errors.New("username is empty")
	}
	tokens := strings.Split(username, "-")
	var id string
	for i := 0; i < len(tokens); i++ {
		if !known[tokens[i]] || i+1 >= len(tokens) {
			continue
		}
		key, value := tokens[i], tokens[i+1]
		i++ // a known key always consumes the next token as its value
		if key != wanted {
			continue
		}
		if value == "" {
			return "", fmt.Errorf("%s sticky ID is empty", wanted)
		}
		if id != "" {
			return "", fmt.Errorf("username has multiple %s parameters", wanted)
		}
		id = value
	}
	if id == "" {
		return "", fmt.Errorf("username has no %s parameter", wanted)
	}
	return id, nil
}

func deterministicBytes(kind, runID, seq string) []byte {
	seed := sha256.Sum256([]byte(kind + "\x00" + runID + "\x00" + seq))
	state := binary.LittleEndian.Uint64(seed[:8])
	if state == 0 {
		state = 0x9e3779b97f4a7c15
	}
	out := make([]byte, payloadBytes)
	for i := range out {
		// xorshift64* is deterministic and inexpensive; this is test data, not
		// cryptographic randomness.
		state ^= state >> 12
		state ^= state << 25
		state ^= state >> 27
		out[i] = byte((state * 0x2545F4914F6CDD1D) >> 56)
	}
	return out
}

func sha256Hex(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// singleConnListener is the smallest listener http.Server needs for one TLS
// tunnel. The connection is closed by the caller when Serve returns.
type singleConnListener struct {
	conn      net.Conn
	addr      net.Addr
	accepted  bool
	done      chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
}

func newSingleConnListener(conn net.Conn) *singleConnListener {
	return &singleConnListener{conn: conn, addr: conn.LocalAddr(), done: make(chan struct{})}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.accepted {
		l.accepted = true
		conn := &closeNotifyConn{Conn: l.conn, notify: l.close}
		l.mu.Unlock()
		return conn, nil
	}
	l.mu.Unlock()
	<-l.done // keep http.Server alive until the one tunnel connection closes
	return nil, net.ErrClosed
}

func (l *singleConnListener) close() { l.closeOnce.Do(func() { close(l.done) }) }

func (l *singleConnListener) Close() error {
	l.close()
	return nil
}

func (l *singleConnListener) Addr() net.Addr { return l.addr }

type closeNotifyConn struct {
	net.Conn
	notify func()
	once   sync.Once
}

func (c *closeNotifyConn) Close() error {
	c.once.Do(c.notify)
	return c.Conn.Close()
}
