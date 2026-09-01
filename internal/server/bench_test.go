package server

// 进程内并发基准：测量「入站分流 → Marker 提取 → 转发管线 → 源站」全链路吞吐。
// 明文绝对式请求（等价于客户端经本机路由访问 http 目标），不含 TLS 握手开销。
// 运行: go test ./internal/server -bench BenchmarkForwardPipeline -benchtime 2s

import (
	"bufio"
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
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mitmrouter/internal/certca"
	"mitmrouter/internal/settings"
	"mitmrouter/internal/store"
	"mitmrouter/internal/upstream"
)

func benchStack(b *testing.B) (feURL, originURL string) {
	b.Helper()
	st, _, err := store.Bootstrap(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { st.Close() })

	snap := settings.DefaultSnapshot()
	holder := settings.NewHolder(snap)
	ca, err := certca.Ensure(context.Background(), st)
	if err != nil {
		b.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"ok\":true}\n\n")
	}))
	b.Cleanup(origin.Close)

	srv := New(holder, ca, upstream.EmptyTable(), nil, logger)
	fe := httptest.NewServer(srv.Handler())
	b.Cleanup(fe.Close)
	return fe.URL, origin.URL
}

func BenchmarkForwardPipeline(b *testing.B) {
	feURL, originURL := benchStack(b)
	target := originURL + "/v1/chat/completions"
	feU, _ := url.Parse(feURL)

	for _, conc := range []int{8, 64, 256, 1024} {
		b.Run(fmt.Sprintf("conc-%d", conc), func(b *testing.B) {
			tr := &http.Transport{
				Proxy:               http.ProxyURL(feU),
				MaxIdleConnsPerHost: 256,
			}
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr}

			var done, errs atomic.Int64
			var latUs atomic.Int64
			start := time.Now()

			var wg sync.WaitGroup
			for w := 0; w < conc; w++ {
				wg.Add(1)
				go func(seq int) {
					defer wg.Done()
					for i := seq; i < b.N; i += conc {
						t0 := time.Now()
						resp, err := client.Get(target)
						if err != nil {
							errs.Add(1)
							continue
						}
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						latUs.Add(time.Since(t0).Microseconds())
						done.Add(1)
					}
				}(w)
			}
			wg.Wait()

			dur := time.Since(start)
			n := float64(done.Load())
			if n > 0 {
				b.ReportMetric(n/dur.Seconds(), "req/s")
				b.ReportMetric(float64(latUs.Load())/n/1000, "avg-lat-ms")
				b.ReportMetric(float64(errs.Load()), "errors")
			}
		})
	}
}

const (
	longSSEChunks = 30
	longSSEDelay  = 50 * time.Millisecond
)

type tlsLoadStack struct {
	client *http.Client
	target string
	mock   *mockUpstream
}

// newTLSLoadStack 构建完整的生产数据路径：
// 客户端 TLS -> MITMRouter TLS 终结 -> HTTP CONNECT 模拟上游 ->
// 源站 TLS -> 持续刷新输出的 SSE 响应。所有证书均在本地生成，且仅由参与本测试的传输层信任。
func newTLSLoadStack(t testing.TB) *tlsLoadStack {
	t.Helper()
	st, _, err := store.Bootstrap(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	snap := settings.DefaultSnapshot()
	snap.BlockPrivateTargets = false // permit local test traffic through the configured upstream exit
	snap.DefaultUpstream = "mock-upstream"
	holder := settings.NewHolder(snap)
	ca, err := certca.Ensure(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.LeafForHost("stream.load.test")
	if err != nil {
		t.Fatal(err)
	}

	var originRequests atomic.Int64
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originRequests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		f := w.(http.Flusher)
		for i := 0; i < longSSEChunks; i++ {
			fmt.Fprintf(w, "data: {\"chunk\":%d}\n\n", i)
			f.Flush()
			if i+1 < longSSEChunks {
				time.Sleep(longSSEDelay)
			}
		}
	}))
	origin.EnableHTTP2 = true
	origin.TLS = &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		NextProtos:   []string{"h2", "http/1.1"},
	}
	origin.StartTLS()
	t.Cleanup(origin.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	auditCh := make(chan store.LogEntry, 4096)
	writerCtx, stopWriter := context.WithCancel(context.Background())
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		st.RunLogWriter(writerCtx, auditCh)
	}()
	t.Cleanup(func() {
		stopWriter()
		<-writerDone
	})
	srv := New(holder, ca, upstream.EmptyTable(), auditCh, logger)
	up, err := upstream.FromRow(1, "mock-upstream", "dataimpulse",
		"http://bench-user:bench-pass@127.0.0.1:1", sql.NullString{}, true)
	if err != nil {
		t.Fatal(err)
	}
	mock := startMockUpstream(t, origin.URL)
	up.BaseURL.Host = mock.listener.Addr().String()
	srv.SwapUpstreams(upstream.NewTable([]*upstream.Upstream{up}, snap.DefaultUpstream))

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.CertificatePEM()) {
		t.Fatal("failed to add benchmark CA to trust pool")
	}
	outbound := srv.transport
	outbound.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}

	front := httptest.NewServer(srv.Handler())
	t.Cleanup(front.Close)
	frontURL, err := url.Parse(front.URL)
	if err != nil {
		t.Fatal(err)
	}
	clientTransport := &http.Transport{
		Proxy:               http.ProxyURL(frontURL),
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		TLSClientConfig:     &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}
	t.Cleanup(clientTransport.CloseIdleConnections)

	load := &tlsLoadStack{
		client: &http.Client{Transport: clientTransport, Timeout: 20 * time.Second},
		target: "https://stream.load.test/v1/responses",
		mock:   mock,
	}
	// 先预热 TLS+HTTP/2 连接，让每轮测试针对长期复用的多路复用 SSE 流，
	// 而不是只测建立连接时的突发流量。
	if _, _, err := load.oneSSE(); err != nil {
		t.Fatalf("warm-up SSE failed: %v", err)
	}
	if got := originRequests.Load(); got != 1 {
		t.Fatalf("warm-up did not reach TLS origin, requests=%d", got)
	}
	return load
}

func (s *tlsLoadStack) oneSSE() (firstByte, complete time.Duration, err error) {
	started := time.Now()
	req, err := http.NewRequest(http.MethodGet, s.target, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer load-test-marker")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	if firstLine, err := br.ReadString('\n'); err != nil {
		rest, _ := io.ReadAll(br)
		return 0, 0, fmt.Errorf("read first SSE event: %w (status=%d content-type=%q body=%q)", err, resp.StatusCode, resp.Header.Get("Content-Type"), firstLine+string(rest))
	}
	firstByte = time.Since(started)
	if _, err := io.Copy(io.Discard, br); err != nil {
		return 0, 0, fmt.Errorf("drain SSE: %w", err)
	}
	return firstByte, time.Since(started), nil
}

type loadWaveResult struct {
	firstBytes []time.Duration
	completes  []time.Duration
	errs       []error
}

func runTLSLoadWave(s *tlsLoadStack, concurrency int) loadWaveResult {
	result := loadWaveResult{
		firstBytes: make([]time.Duration, 0, concurrency),
		completes:  make([]time.Duration, 0, concurrency),
	}
	start := make(chan struct{})
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			first, complete, err := s.oneSSE()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				result.errs = append(result.errs, err)
				return
			}
			result.firstBytes = append(result.firstBytes, first)
			result.completes = append(result.completes, complete)
		}()
	}
	close(start)
	wg.Wait()
	return result
}

func durationPercentile(values []time.Duration, percentile int) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	idx := (len(values)*percentile + 99) / 100
	if idx == 0 {
		idx = 1
	}
	return values[idx-1]
}

// TestTLSMITMViaMockUpstreamLongSSELoad 是一个明确且可复现的负载测试，
// 而非微基准测试。它验证真实 TLS MITM 和 CONNECT 上游下，16、64、256、512 条
// 活跃流的持续 SSE 并发能力。
func TestTLSMITMViaMockUpstreamLongSSELoad(t *testing.T) {
	if testing.Short() || os.Getenv("MITMROUTER_RUN_LOAD") != "1" {
		t.Skip("set MITMROUTER_RUN_LOAD=1 to run the long SSE load test")
	}
	s := newTLSLoadStack(t)
	for _, concurrency := range []int{16, 64, 256, 512} {
		beforeTunnels := s.mock.connects.Load()
		waveStarted := time.Now()
		result := runTLSLoadWave(s, concurrency)
		elapsed := time.Since(waveStarted)
		if len(result.errs) > 0 {
			t.Fatalf("concurrency=%d: %d/%d streams failed; first error: %v", concurrency, len(result.errs), concurrency, result.errs[0])
		}
		if got := len(result.completes); got != concurrency {
			t.Fatalf("concurrency=%d: completed %d streams", concurrency, got)
		}
		t.Logf("TLS MITM + CONNECT upstream + %dx%.0fms SSE: streams=%d elapsed=%v first-byte p50/p95=%v/%v complete p50/p95=%v/%v new-upstream-tunnels=%d",
			longSSEChunks, longSSEDelay.Seconds()*1000, concurrency, elapsed,
			durationPercentile(result.firstBytes, 50), durationPercentile(result.firstBytes, 95),
			durationPercentile(result.completes, 50), durationPercentile(result.completes, 95),
			s.mock.connects.Load()-beforeTunnels)
	}
}
