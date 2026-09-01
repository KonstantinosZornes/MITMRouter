package server

// 集成测试：覆盖 DESIGN §11 的四类场景。
// 全部走明文绝对式请求（等价真实客户端接入方式），不依赖外部网络。

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mitmrouter/internal/certca"
	"mitmrouter/internal/settings"
	"mitmrouter/internal/store"
	"mitmrouter/internal/trace"
	"mitmrouter/internal/upstream"
)

// ---- 公共脚手架 ----

type stack struct {
	st     *store.Store
	ca     *certca.Authority
	holder *settings.Holder
	srv    *Server
	feURL  string // 前端入口（Server.Handler 的 httptest 地址）
}

func newStack(t *testing.T) *stack {
	t.Helper()
	st, _, err := store.Bootstrap(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	snap := settings.DefaultSnapshot()
	// 大多数集成场景使用进程内的 127.0.0.1 源站；需要测试私网目标的用例
	// 会显式打开生产环境的安全默认配置。
	snap.BlockPrivateTargets = false
	holder := settings.NewHolder(snap)
	ca, err := certca.Ensure(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(holder, ca, upstream.EmptyTable(), nil, logger)

	fe := httptest.NewServer(srv.Handler())
	t.Cleanup(fe.Close)
	return &stack{st: st, ca: ca, holder: holder, srv: srv, feURL: fe.URL}
}

// ingressClient 返回把 front 当作 HTTP 接入口使用的客户端（发绝对式请求）。
func ingressClient(t *testing.T, feURL string) *http.Client {
	t.Helper()
	u, err := url.Parse(feURL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(u)},
		Timeout:   15 * time.Second,
	}
}

// registerUpstream 以内存条目替换上游表（聚焦路由行为，绕过 DB）。
func (s *stack) registerUpstream(t *testing.T, name, platform, baseURL string) {
	t.Helper()
	u, err := upstream.FromRow(1, name, platform, baseURL, sql.NullString{}, true)
	if err != nil {
		t.Fatal(err)
	}
	old := s.holder.Current()
	s.srv.SwapUpstreams(upstream.NewTable([]*upstream.Upstream{u}, old.DefaultUpstream))
}

// ---- 场景一：入站认证两态 ----

func TestInboundAuthTwoStates(t *testing.T) {
	s := newStack(t)

	snap := s.holder.Current()
	snap.ListenAuth = "tester:secret123"
	s.holder.Set(snap)

	client := ingressClient(t, s.feURL)

	req, _ := http.NewRequest("GET", "http://any.example.com/v1/chat/completions", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("expected 407 without credentials, got %d", resp.StatusCode)
	}
	if pa := resp.Header.Get("Proxy-Authenticate"); !strings.Contains(pa, `Basic realm="sticky-mitm"`) {
		t.Fatalf("missing Proxy-Authenticate header: %q", pa)
	}

	req2, _ := http.NewRequest("GET", "http://any.example.com/v1/chat/completions", nil)
	req2.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("tester:secret123")))
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode == http.StatusProxyAuthRequired {
		t.Fatal("valid credentials rejected")
	}

	snap2 := s.holder.Current()
	snap2.ListenAuth = ""
	s.holder.Set(snap2)
	req3, _ := http.NewRequest("GET", "http://any.example.com/v1/chat/completions", nil)
	resp3, err := client.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode == http.StatusProxyAuthRequired {
		t.Fatal("still intercepted after auth disabled")
	}
}

// ---- mock 上游：进程内 CONNECT 记录器 + 转发 ----

type mockUpstream struct {
	listener net.Listener
	authCh   chan string
	connects atomic.Int64
}

func startMockUpstream(t testing.TB, dstURL string) *mockUpstream {
	t.Helper()
	dst, err := url.Parse(dstURL)
	if err != nil {
		t.Fatal(err)
	}
	m := &mockUpstream{authCh: make(chan string, 32)}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m.listener = ln
	target := dst.Host

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				select {
				case m.authCh <- req.Header.Get("Proxy-Authorization"):
				default:
				}
				if req.Method != http.MethodConnect {
					return
				}
				m.connects.Add(1)
				io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
				dstConn, derr := net.Dial("tcp", target)
				if derr != nil {
					return
				}
				defer dstConn.Close()
				go func() { io.Copy(dstConn, br); dstConn.Close() }()
				io.Copy(c, dstConn)
			}(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return m
}

// collectAuths 收集 n 条凭据（带超时）。
func collectAuths(m *mockUpstream, n int, max time.Duration) []string {
	var out []string
	deadline := time.After(max)
	for len(out) < n {
		select {
		case raw := <-m.authCh:
			if dec, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, "Basic ")); err == nil {
				raw = "Basic " + string(dec)
			}
			a := raw
			out = append(out, a)
		case <-deadline:
			return out
		}
	}
	return out
}

// setDefaultUpstream 更新快照中的默认上游指向。
func (s *stack) setDefaultUpstream(t *testing.T, name string) {
	t.Helper()
	snap := s.holder.Current()
	snap.DefaultUpstream = name
	s.holder.Set(snap)
}

// ---- 场景二：Marker 粘滞断言 ----

func TestStickyViaMockUpstream(t *testing.T) {
	s := newStack(t)
	o := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "OK")
	}))
	t.Cleanup(o.Close)

	mock := startMockUpstream(t, o.URL)
	host := mock.listener.Addr().String()
	s.registerUpstream(t, "mock-di", "dataimpulse",
		"http://mockuser__cr.us:pw@"+host)
	s.setDefaultUpstream(t, "mock-di")

	client := ingressClient(t, s.feURL)
	get := func(mk string) {
		req, _ := http.NewRequest("GET", "http://target.example.com/v1/chat/completions", nil)
		if mk != "" {
			req.Header.Set("Authorization", "Bearer "+mk)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	get("sk-alpha-0001")
	get("sk-alpha-0001")
	get("sk-beta-00222222")

	seen := collectAuths(mock, 3, 3*time.Second)
	if len(seen) != 3 {
		t.Fatalf("upstream should receive exactly 3 requests, got %d", len(seen))
	}
	extract := func(auth string) string {
		i := strings.Index(auth, "sessid.")
		rest := auth[i+len("sessid."):]
		j := strings.Index(rest, ":")
		if j < 0 {
			j = len(rest)
		}
		return rest[:j]
	}
	s1, s2, s3 := extract(seen[0]), extract(seen[1]), extract(seen[2])
	if !strings.Contains(seen[0], "__cr.us") {
		t.Errorf("country param lost: %s", seen[0])
	}
	if s1 != s2 {
		t.Errorf("same Marker produced different sessid: %s vs %s", s1, s2)
	}
	if s1 == s3 {
		t.Errorf("different Markers produced same sessid: %s", s1)
	}
}

// newOriginEcho 极简源站。
func newOriginEcho(t *testing.T) *httptest.Server {
	t.Helper()
	o := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ORIGIN-OK")
	}))
	t.Cleanup(o.Close)
	return o
}

func TestTraceDisabledByDefault(t *testing.T) {
	if s := newStack(t); s.srv.trace != nil {
		t.Fatal("new server must not trace unless explicitly attached")
	}
}

func TestPlaintextTraceStreamsRequestAndResponse(t *testing.T) {
	s := newStack(t)
	tracePath := filepath.Join(t.TempDir(), "debug.trace")
	tw, err := trace.Open(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tw.Close() })
	s.srv.AttachTrace(tw)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Origin-Token", "visible-response-header")
		fmt.Fprintf(w, "reply:%s", body)
	}))
	t.Cleanup(origin.Close)

	req, err := http.NewRequest(http.MethodPost, origin.URL+"/v1/responses?debug=1", strings.NewReader(`{"input":"visible-request-body"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer visible-request-header")
	resp, err := ingressClient(t, s.feURL).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{
		"request.start", "url=", "debug=1",
		"visible-request-header", "visible-request-body",
		"response.start status=200", "visible-response-header", "reply:",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("trace missing %q:\n%s", want, text)
		}
	}
}

func TestInformationalResponseRelayed(t *testing.T) {
	s := newStack(t)
	snap := s.holder.Current()
	snap.BlockPrivateTargets = false
	s.holder.Set(snap)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", "</style.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set("X-Origin-Final", "final")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "origin-body")
	}))
	t.Cleanup(origin.Close)

	frontURL, err := url.Parse(s.feURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", frontURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "GET %s/early HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", origin.URL, strings.TrimPrefix(origin.URL, "http://")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	request := &http.Request{Method: http.MethodGet}
	early, err := http.ReadResponse(br, request)
	if err != nil {
		t.Fatal(err)
	}
	if early.StatusCode != http.StatusEarlyHints || early.Header.Get("Link") != "</style.css>; rel=preload" {
		t.Fatalf("informational response = %d headers=%v", early.StatusCode, early.Header)
	}
	_ = early.Body.Close()
	final, err := http.ReadResponse(br, request)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Body.Close()
	body, err := io.ReadAll(final.Body)
	if err != nil || final.StatusCode != http.StatusOK || final.Header.Get("X-Origin-Final") != "final" || string(body) != "origin-body" {
		t.Fatalf("final response status=%d headers=%v body=%q err=%v", final.StatusCode, final.Header, body, err)
	}
}

func TestRequestTrailerRelayed(t *testing.T) {
	s := newStack(t)
	received := make(chan http.Header, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		received <- r.Trailer.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(origin.Close)

	frontURL, err := url.Parse(s.feURL)
	if err != nil {
		t.Fatal(err)
	}
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", frontURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "POST %s/echo HTTP/1.1\r\nHost: %s\r\nTransfer-Encoding: chunked\r\nTrailer: X-Request-Trailer\r\nConnection: close\r\n\r\n5\r\nhello\r\n0\r\nX-Request-Trailer: preserved\r\n\r\n", origin.URL, originURL.Host); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if got := (<-received).Get("X-Request-Trailer"); got != "preserved" {
		t.Fatalf("origin request trailer = %q, want preserved", got)
	}
}

func TestHTTPUpgradeRelayed(t *testing.T) {
	s := newStack(t)
	snap := s.holder.Current()
	snap.BlockPrivateTargets = false
	s.holder.Set(snap)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("origin response writer cannot hijack")
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n"); err != nil {
			t.Error(err)
			return
		}
		if err := rw.Flush(); err != nil {
			t.Error(err)
			return
		}
		buf := make([]byte, 32)
		n, err := conn.Read(buf)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = conn.Write(buf[:n])
	}))
	t.Cleanup(origin.Close)

	frontURL, err := url.Parse(s.feURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", frontURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "GET %s/upgrade HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n", origin.URL, strings.TrimPrefix(origin.URL, "http://")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	response, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols || response.Header.Get("Upgrade") != "echo" {
		t.Fatalf("upgrade response = %d headers=%v", response.StatusCode, response.Header)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("upgrade echo = %q, err=%v", buf, err)
	}
}

// ---- 场景三：SSE 渐进透传 ----

func TestSSERelayedProgressively(t *testing.T) {
	s := newStack(t)
	var releaseOnce sync.Once
	release := make(chan struct{})
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	o := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		time.Sleep(30 * time.Millisecond)
		fmt.Fprint(w, "data: second\n\n")
	}))
	t.Cleanup(func() { closeRelease(); o.Close() })

	client := ingressClient(t, s.feURL)
	target := o.URL + "/v1/responses"
	resp, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	start := time.Now()
	line, err := br.ReadString('\n')
	firstAt := time.Since(start)
	if err != nil || !strings.Contains(line, "first") {
		t.Fatalf("first chunk unexpected: %q err=%v", line, err)
	}
	if firstAt > 2*time.Second {
		t.Fatalf("first chunk too slow (%v), looks like full buffering", firstAt)
	}
	closeRelease() // 放行第二块
	rest, err := io.ReadAll(br)
	if err != nil || !strings.Contains(string(rest), "second") {
		t.Fatalf("second chunk missing: %q err=%v", rest, err)
	}
}

// ---- 场景四：私网目标强制直连旁路 ----

func TestInvalidUpstreamConfigurationNeverFallsBackDirect(t *testing.T) {
	s := newStack(t)

	// 模拟已经存在的错误记录（例如有人手工修改了 SQLite 数据库）。
	// 1024proxy 无法向空用户名注入会话参数；此时必须返回错误，不能静默把请求改成直连。
	up, err := upstream.FromRow(1, "broken-1024", "1024proxy", "http://:pw@unused.invalid:80", sql.NullString{}, true)
	if err != nil {
		t.Fatal(err)
	}
	s.srv.SwapUpstreams(upstream.NewTable([]*upstream.Upstream{up}, up.Name))
	snap := s.holder.Current()
	snap.DefaultUpstream = up.Name
	s.holder.Set(snap)

	pu, _, name, reason, err := s.srv.resolveOutboundDetailed(context.Background(), snap, "marker", identity{}, "203.0.113.7", "api.example:443")
	if !errors.Is(err, errUpstreamConfig) {
		t.Fatalf("route error=%v, want errUpstreamConfig", err)
	}
	if pu != nil || name != up.Name || reason != "injection_failed" {
		t.Fatalf("got url=%v name=%q reason=%q", pu, name, reason)
	}
}

func TestPrivateCONNECTRejectedBeforeTunnel(t *testing.T) {
	s := newStack(t)
	snap := s.holder.Current()
	snap.BlockPrivateTargets = true
	s.holder.Set(snap)
	audit := make(chan store.LogEntry, 1)
	s.srv.audit = audit

	upstreamAddr, err := urlParseFE(s.feURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", upstreamAddr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprint(conn, "CONNECT 127.0.0.1:1 HTTP/1.1\r\nHost: 127.0.0.1:1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("private CONNECT status=%d want 403; body=%q", resp.StatusCode, body)
	}
	select {
	case entry := <-audit:
		if entry.Method != http.MethodConnect || entry.Host != "127.0.0.1:1" || entry.Status != 0 || entry.InternalError != "private_target_blocked" {
			t.Fatalf("unexpected blocked CONNECT audit entry: %+v", entry)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked CONNECT was not added to audit")
	}
}

func TestPrivateTargetIsBlockedByDefault(t *testing.T) {
	s := newStack(t)
	snap := s.holder.Current()
	snap.BlockPrivateTargets = true
	s.holder.Set(snap)
	var hits atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "ORIGIN-OK")
	}))
	t.Cleanup(origin.Close)

	resp, err := ingressClient(t, s.feURL).Get(origin.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("private target status=%d want 403; body=%q", resp.StatusCode, body)
	}
	if hits.Load() != 0 {
		t.Fatalf("blocked private target reached origin %d times", hits.Load())
	}
}

func TestPrivateTargetCanBeExplicitlyAllowed(t *testing.T) {
	s := newStack(t)
	origin := newOriginEcho(t)
	mock := startMockUpstream(t, origin.URL)
	host := mock.listener.Addr().String()
	s.registerUpstream(t, "mock-di", "dataimpulse", "http://mockuser:pw@"+host)
	s.setDefaultUpstream(t, "mock-di")
	snap := s.holder.Current()
	snap.BlockPrivateTargets = false
	s.holder.Set(snap)

	resp, err := ingressClient(t, s.feURL).Get(origin.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "ORIGIN-OK") {
		t.Fatalf("explicitly allowed private target direct connection failed: %s", body)
	}
	select {
	case a := <-mock.authCh:
		t.Fatalf("allowed private target must not go through upstream, but received: %s", a)
	default:
	}
}
