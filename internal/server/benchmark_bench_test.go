package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mitmrouter/internal/certca"
	"mitmrouter/internal/settings"
	"mitmrouter/internal/sticky"
	"mitmrouter/internal/store"
	"mitmrouter/internal/upstream"
)

// BenchmarkIndependentBenchmark measures the two supported client paths
// through a real MITMRouter and the root-level, no-egress benchmark.
// It is intentionally opt-in because setup builds and starts another process.
//
// Run:
//
//	MITMROUTER_RUN_BENCHMARK=1 GOCACHE="$PWD/.gocache" \
//	  go test ./internal/server -run '^$' -bench BenchmarkIndependentBenchmark \
//	  -benchtime 5s -count 3
func BenchmarkIndependentBenchmark(b *testing.B) {
	if os.Getenv("MITMROUTER_RUN_BENCHMARK") != "1" {
		b.Skip("set MITMROUTER_RUN_BENCHMARK=1 to run the independent benchmark load benchmark")
	}
	stack := newBenchmarkLoadStack(b)
	defer stack.close()

	marker := "bench-marker-load"
	wantSticky := sticky.Derive(stack.snap.Salt, marker, stack.snap.SIDLen)
	for _, scenario := range []struct {
		name   string
		target string
	}{
		{"http", "http://benchmark.test/v1/responses"},
		{"https", "https://benchmark.test/v1/responses"},
	} {
		for _, concurrency := range []int{1, 8, 64, 256} {
			b.Run(fmt.Sprintf("%s/conc-%d", scenario.name, concurrency), func(b *testing.B) {
				// Start every data point from a warm connection pool. Connection setup
				// is real and covered by the HTTPS scenario, but it must not randomly
				// dominate a steady-state latency sample just because it happened to
				// be the first request in a group.
				stack.transport.CloseIdleConnections()
				if err := stack.warm(scenario.name, scenario.target, marker, wantSticky); err != nil {
					b.Fatal(err)
				}
				runFixedConcurrencyBenchmark(b, stack, scenario.name, scenario.target, marker, wantSticky, concurrency)
			})
		}
	}
}

type benchmarkLatency struct {
	completed atomic.Int64
	failures  atomic.Int64
	ttfbSumNS atomic.Int64
	latSumNS  atomic.Int64
	ttfbMaxNS atomic.Int64
	latMaxNS  atomic.Int64

	errMu    sync.Mutex
	firstErr error
}

func (m *benchmarkLatency) recordSuccess(ttfb, total time.Duration) {
	m.completed.Add(1)
	m.ttfbSumNS.Add(ttfb.Nanoseconds())
	m.latSumNS.Add(total.Nanoseconds())
	atomicMaxDuration(&m.ttfbMaxNS, ttfb.Nanoseconds())
	atomicMaxDuration(&m.latMaxNS, total.Nanoseconds())
}

func (m *benchmarkLatency) recordFailure(err error) {
	m.failures.Add(1)
	m.errMu.Lock()
	if m.firstErr == nil {
		m.firstErr = err
	}
	m.errMu.Unlock()
}

func atomicMaxDuration(dst *atomic.Int64, candidate int64) {
	for {
		current := dst.Load()
		if candidate <= current || dst.CompareAndSwap(current, candidate) {
			return
		}
	}
}

// runFixedConcurrencyBenchmark uses an exact worker count, unlike RunParallel
// whose worker count follows GOMAXPROCS. This makes conc-1/8/64/256 comparable.
// TTFB is client-observed time from starting the request until Client.Do returns
// response headers. Total latency ends only after reading and verifying the full
// 64 KiB response body and its SHA-256.
func runFixedConcurrencyBenchmark(b *testing.B, stack *benchmarkLoadStack, scenario, target, marker, wantSticky string, concurrency int) {
	var sequence atomic.Uint64
	var next atomic.Int64
	var measurement benchmarkLatency
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for {
				i := next.Add(1) - 1
				if i >= int64(b.N) {
					return
				}
				seq := fmt.Sprint(sequence.Add(1))
				runID := "load-" + scenario
				ttfb, total, err := stack.one(target, runID, seq, marker, wantSticky, scenario == "https")
				if err != nil {
					measurement.recordFailure(err)
					continue
				}
				measurement.recordSuccess(ttfb, total)
			}
		}()
	}

	b.SetBytes(2 * benchmarkPayloadBytes) // request + response
	b.ResetTimer()
	started := time.Now()
	close(start)
	workers.Wait()
	elapsed := time.Since(started)
	b.StopTimer()

	completed := measurement.completed.Load()
	if completed > 0 {
		b.ReportMetric(float64(measurement.ttfbSumNS.Load())/float64(completed)/float64(time.Millisecond), "avg-ttfb-ms")
		b.ReportMetric(float64(measurement.latSumNS.Load())/float64(completed)/float64(time.Millisecond), "avg-lat-ms")
		// One request is one complete 64 KiB write plus one complete 64 KiB read.
		// For this request/response benchmark, IOPS means validated operations/s.
		b.ReportMetric(float64(completed)/elapsed.Seconds(), "iops")
	}
	b.ReportMetric(float64(measurement.ttfbMaxNS.Load())/float64(time.Millisecond), "max-ttfb-ms")
	b.ReportMetric(float64(measurement.latMaxNS.Load())/float64(time.Millisecond), "max-lat-ms")
	b.ReportMetric(float64(measurement.failures.Load()), "errors")
	if measurement.firstErr != nil {
		b.Fatalf("%d/%d benchmark requests failed; first error: %v", measurement.failures.Load(), b.N, measurement.firstErr)
	}
}

type benchmarkLoadStack struct {
	client    *http.Client
	transport *http.Transport
	front     *httptest.Server
	store     *store.Store
	stop      func()
	snap      settings.Snapshot
}

func newBenchmarkLoadStack(b testing.TB) *benchmarkLoadStack {
	b.Helper()
	certPath, keyPath, upstreamRoots := writeBenchmarkCertificate(b)
	upstreamAddr := reserveLoopbackAddr(b)
	stopUpstream := startBenchmarkProcess(b, upstreamAddr, certPath, keyPath)

	st, _, err := store.Bootstrap(b.TempDir())
	if err != nil {
		stopUpstream()
		b.Fatal(err)
	}
	snap := settings.DefaultSnapshot()
	snap.BlockPrivateTargets = false // test fixture only; production default stays true.
	snap.MarkerRules.Headers = []string{"Authorization"}
	snap.DefaultUpstream = "benchmark-upstream"
	holder := settings.NewHolder(snap)
	ca, err := certca.Ensure(context.Background(), st)
	if err != nil {
		st.Close()
		stopUpstream()
		b.Fatal(err)
	}
	srv := New(holder, ca, upstream.EmptyTable(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	up, err := upstream.FromRow(1, "benchmark-upstream", "dataimpulse",
		"http://bench-user__cr.us:bench-password@"+upstreamAddr, sql.NullString{}, true)
	if err != nil {
		st.Close()
		stopUpstream()
		b.Fatal(err)
	}
	srv.SwapUpstreams(upstream.NewTable([]*upstream.Upstream{up}, snap.DefaultUpstream))
	srv.transport.TLSClientConfig = &tls.Config{RootCAs: upstreamRoots, MinVersion: tls.VersionTLS12}

	front := httptest.NewServer(srv.Handler())
	frontURL, err := url.Parse(front.URL)
	if err != nil {
		front.Close()
		st.Close()
		stopUpstream()
		b.Fatal(err)
	}
	mitmRoots := x509.NewCertPool()
	if !mitmRoots.AppendCertsFromPEM(ca.CertificatePEM()) {
		front.Close()
		st.Close()
		stopUpstream()
		b.Fatal("add MITM CA to benchmark client trust pool")
	}
	transport := &http.Transport{
		Proxy:               http.ProxyURL(frontURL),
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		// Keep-alives are part of the production path and stay enabled for the
		// benchmark; this transport must be shared by all concurrency groups.
		TLSClientConfig: &tls.Config{RootCAs: mitmRoots, MinVersion: tls.VersionTLS12},
	}
	return &benchmarkLoadStack{
		client:    &http.Client{Transport: transport, Timeout: 30 * time.Second},
		transport: transport,
		front:     front,
		store:     st,
		stop:      stopUpstream,
		snap:      snap,
	}
}

func (s *benchmarkLoadStack) close() {
	s.transport.CloseIdleConnections()
	s.front.Close()
	_ = s.store.Close()
	s.stop()
}

func (s *benchmarkLoadStack) warm(scenario, target, marker, wantSticky string) error {
	_, _, err := s.one(target, "warm-"+scenario, "0", marker, wantSticky, scenario == "https")
	return err
}

func (s *benchmarkLoadStack) one(target, runID, seq, marker, wantSticky string, wantHTTP2 bool) (time.Duration, time.Duration, error) {
	started := time.Now()
	req, err := http.NewRequest(http.MethodPost, target+"?run="+runID+"&seq="+seq,
		bytes.NewReader(benchmarkBytes("request", runID, seq)))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+marker)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Bench-Run", runID)
	req.Header.Set("X-Bench-Seq", seq)
	req.Header.Set("X-Bench-Contract", "no-rewrite-v1")
	resp, err := s.client.Do(req)
	ttfb := time.Since(started)
	if err != nil {
		return ttfb, time.Since(started), err
	}
	if wantHTTP2 && resp.ProtoMajor != 2 {
		_ = resp.Body.Close()
		return ttfb, time.Since(started), fmt.Errorf("HTTPS response protocol=%s, want HTTP/2", resp.Proto)
	}
	err = checkBenchmarkResponse(resp, runID, seq, wantSticky)
	closeErr := resp.Body.Close()
	total := time.Since(started)
	if err != nil {
		return ttfb, total, err
	}
	if closeErr != nil {
		return ttfb, total, fmt.Errorf("close response body: %w", closeErr)
	}
	return ttfb, total, nil
}

// checkBenchmarkResponse verifies the complete response, including the
// body SHA-256. It is safe for benchmark worker goroutines: it returns errors
// instead of calling testing.TB methods from those goroutines.
func checkBenchmarkResponse(resp *http.Response, runID, seq, wantSticky string) error {
	if resp == nil {
		return fmt.Errorf("missing response")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status=%d body=%q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		return fmt.Errorf("Content-Type=%q, want application/octet-stream", got)
	}
	if got := resp.Header.Get("Content-Length"); got != "65536" {
		return fmt.Errorf("Content-Length=%q, want 65536", got)
	}
	if got := resp.Header.Get("X-Bench-Sticky-ID"); got != wantSticky {
		return fmt.Errorf("sticky ID=%q, want %q", got, wantSticky)
	}
	if got := resp.Header.Get("X-Bench-Run"); got != runID {
		return fmt.Errorf("run ID=%q, want %q", got, runID)
	}
	if got := resp.Header.Get("X-Bench-Seq"); got != seq {
		return fmt.Errorf("sequence=%q, want %q", got, seq)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}
	want := benchmarkBytes("response", runID, seq)
	if !bytes.Equal(body, want) {
		return fmt.Errorf("response body does not match full deterministic 64 KiB payload")
	}
	if got, wantHash := resp.Header.Get("X-Bench-Response-Body-SHA256"), benchmarkSHA256(body); got != wantHash {
		return fmt.Errorf("response body hash=%q, want %q", got, wantHash)
	}
	if got, wantHash := resp.Header.Get("X-Bench-Request-SHA256"), benchmarkSHA256(benchmarkBytes("request", runID, seq)); got != wantHash {
		return fmt.Errorf("request body hash=%q, want %q", got, wantHash)
	}
	if got := resp.Header.Get("X-Bench-Request-Bytes"); got != "65536" {
		return fmt.Errorf("request bytes=%q, want 65536", got)
	}
	if resp.Header.Get("X-Bench-Received-At") == "" {
		return fmt.Errorf("missing received-at timestamp")
	}
	return nil
}
