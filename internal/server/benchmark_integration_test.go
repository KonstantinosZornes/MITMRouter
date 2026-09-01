package server

// This test deliberately starts the root-level benchmark binary instead
// of a test helper. It proves MITMRouter can use the independent virtual
// upstream for both documented client scenarios without that upstream ever
// dialing a real target.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"mitmrouter/internal/certca"
	"mitmrouter/internal/settings"
	"mitmrouter/internal/sticky"
	"mitmrouter/internal/store"
	"mitmrouter/internal/upstream"
)

const benchmarkPayloadBytes = 64 * 1024

func TestIndependentBenchmarkThroughMITMRouter(t *testing.T) {
	if testing.Short() || os.Getenv("MITMROUTER_RUN_BENCHMARK") != "1" {
		t.Skip("set MITMROUTER_RUN_BENCHMARK=1 to start the independent benchmark integration test")
	}

	certPath, keyPath, upstreamRoots := writeBenchmarkCertificate(t)
	upstreamAddr := reserveLoopbackAddr(t)
	stopUpstream := startBenchmarkProcess(t, upstreamAddr, certPath, keyPath)
	defer stopUpstream()

	st, _, err := store.Bootstrap(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	snap := settings.DefaultSnapshot()
	snap.BlockPrivateTargets = false // local fixture only; production default remains true.
	snap.MarkerRules.Headers = []string{"Authorization"}
	snap.DefaultUpstream = "benchmark-upstream"
	holder := settings.NewHolder(snap)
	ca, err := certca.Ensure(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(holder, ca, upstream.EmptyTable(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	up, err := upstream.FromRow(1, "benchmark-upstream", "dataimpulse",
		"http://bench-user__cr.us:bench-password@"+upstreamAddr, sqlNullString(), true)
	if err != nil {
		t.Fatal(err)
	}
	srv.SwapUpstreams(upstream.NewTable([]*upstream.Upstream{up}, snap.DefaultUpstream))
	// The independent server presents the local benchmark.test test certificate
	// after CONNECT. Trust only that certificate for MITMRouter's outbound hop.
	srv.transport.TLSClientConfig = &tls.Config{RootCAs: upstreamRoots, MinVersion: tls.VersionTLS12}

	front := httptest.NewServer(srv.Handler())
	defer front.Close()
	frontURL, err := url.Parse(front.URL)
	if err != nil {
		t.Fatal(err)
	}
	mitmRoots := x509.NewCertPool()
	if !mitmRoots.AppendCertsFromPEM(ca.CertificatePEM()) {
		t.Fatal("add MITM CA to benchmark client trust pool")
	}
	transport := &http.Transport{
		Proxy:               http.ProxyURL(frontURL),
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 16,
		TLSClientConfig:     &tls.Config{RootCAs: mitmRoots, MinVersion: tls.VersionTLS12},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second}

	marker := "bench-marker-one"
	wantSticky := sticky.Derive(snap.Salt, marker, snap.SIDLen)
	for _, scenario := range []struct {
		name   string
		target string
		runID  string
		seq    string
	}{
		// benchmark.test intentionally maps to no production account platform. This
		// ensures Resolver falls through to MarkerRules and preserves the exact
		// benchmark Bearer marker for the independent upstream to validate.
		{"http", "http://benchmark.test/v1/responses?run=integration-http&seq=1", "integration-http", "1"},
		{"https", "https://benchmark.test/v1/responses?run=integration-https&seq=2", "integration-https", "2"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			req := benchmarkRequest(t, scenario.target, scenario.runID, scenario.seq, marker)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if scenario.name == "https" && resp.ProtoMajor != 2 {
				t.Fatalf("HTTPS benchmark response protocol=%s, want HTTP/2", resp.Proto)
			}
			assertIndependentBenchmarkResponse(t, resp, scenario.runID, scenario.seq, wantSticky)
		})
	}
}

func sqlNullString() sql.NullString { return sql.NullString{} }

func benchmarkRequest(t testing.TB, target, runID, seq, marker string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target,
		bytes.NewReader(benchmarkBytes("request", runID, seq)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+marker)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Bench-Run", runID)
	req.Header.Set("X-Bench-Seq", seq)
	req.Header.Set("X-Bench-Contract", "no-rewrite-v1")
	return req
}

func assertIndependentBenchmarkResponse(t testing.TB, resp *http.Response, runID, seq, wantSticky string) {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Bench-Sticky-ID"); got != wantSticky {
		t.Fatalf("sticky ID=%q, want %q", got, wantSticky)
	}
	if got := resp.Header.Get("X-Bench-Run"); got != runID {
		t.Fatalf("run ID=%q, want %q", got, runID)
	}
	if got := resp.Header.Get("X-Bench-Seq"); got != seq {
		t.Fatalf("sequence=%q, want %q", got, seq)
	}
	if got := resp.Header.Get("Content-Length"); got != "65536" {
		t.Fatalf("Content-Length=%q, want 65536", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	want := benchmarkBytes("response", runID, seq)
	if !bytes.Equal(body, want) {
		t.Fatal("response body does not match the complete deterministic 64 KiB payload")
	}
	if got, wantHash := resp.Header.Get("X-Bench-Response-Body-SHA256"), benchmarkSHA256(body); got != wantHash {
		t.Fatalf("response body hash=%q, want %q", got, wantHash)
	}
	if got, wantHash := resp.Header.Get("X-Bench-Request-SHA256"), benchmarkSHA256(benchmarkBytes("request", runID, seq)); got != wantHash {
		t.Fatalf("request body hash=%q, want %q", got, wantHash)
	}
}

func benchmarkBytes(kind, runID, seq string) []byte {
	seed := sha256.Sum256([]byte(kind + "\x00" + runID + "\x00" + seq))
	state := binary.LittleEndian.Uint64(seed[:8])
	if state == 0 {
		state = 0x9e3779b97f4a7c15
	}
	out := make([]byte, benchmarkPayloadBytes)
	for i := range out {
		state ^= state >> 12
		state ^= state << 25
		state ^= state >> 27
		out[i] = byte((state * 0x2545F4914F6CDD1D) >> 56)
	}
	return out
}

func benchmarkSHA256(body []byte) string { return fmt.Sprintf("%x", sha256.Sum256(body)) }

func reserveLoopbackAddr(t testing.TB) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func startBenchmarkProcess(t testing.TB, addr, certPath, keyPath string) func() {
	t.Helper()
	root := repositoryRoot(t)
	binary := filepath.Join(t.TempDir(), "benchmark")
	build := exec.Command("go", "build", "-o", binary, "./benchmark")
	build.Dir = root
	build.Env = append(os.Environ(), "GOCACHE="+filepath.Join(root, ".gocache"))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build benchmark: %v\n%s", err, output)
	}

	cmd := exec.Command(binary,
		"-addr", addr,
		"-platform", "dataimpulse",
		"-tls-cert", certPath,
		"-tls-key", keyPath,
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		select {
		case err := <-exited:
			t.Fatalf("benchmark exited before listening: %v\n%s", err, output.String())
		case <-deadline.C:
			t.Fatalf("benchmark did not listen on %s\n%s", addr, output.String())
		case <-ticker.C:
		}
	}
	return func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Errorf("benchmark did not stop\n%s", output.String())
		}
	}
}

func repositoryRoot(t testing.TB) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(cwd, "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("locate repository root from %s: %v", cwd, err)
	}
	return root
}

func writeBenchmarkCertificate(t testing.TB) (certPath, keyPath string, roots *x509.CertPool) {
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
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath = filepath.Join(dir, "benchmark.test.pem"), filepath.Join(dir, "benchmark.test-key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots = x509.NewCertPool()
	roots.AddCert(leaf)
	return certPath, keyPath, roots
}
