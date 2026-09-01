package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"mitmrouter/internal/reqid"
	"mitmrouter/internal/settings"
	"mitmrouter/internal/store"
	"mitmrouter/internal/upstream"
)

func testHolderAllowPrivateTargets() *settings.Holder {
	snap := settings.DefaultSnapshot()
	snap.BlockPrivateTargets = false
	return settings.NewHolder(snap)
}

func TestIngressConnectResponseDebugLogIsCredentialSafe(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(reqid.NewHandler(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	s := New(testHolderAllowPrivateTargets(), nil, upstream.EmptyTable(), nil, logger)
	tr := s.transport
	if tr.OnProxyConnectResponse == nil {
		t.Fatal("transport must log upstream CONNECT responses")
	}

	connectReq, err := http.NewRequest(http.MethodConnect, "https://target.example:443", nil)
	if err != nil {
		t.Fatal(err)
	}
	connectReq.Host = "target.example:443"
	connectReq = connectReq.WithContext(context.WithValue(reqid.With(connectReq.Context(), "request-123"), fwdMetaKey{}, &fwdMeta{
		viaUpstream: true,
		upstream: "residential-primary",
	}))
	upstreamURL, err := url.Parse("http://exit-user:secret-exit-password@exit.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{StatusCode: http.StatusProxyAuthRequired, Status: "407 Proxy Authentication Required"}
	if err := tr.OnProxyConnectResponse(connectReq.Context(), upstreamURL, connectReq, resp); err != nil {
		t.Fatalf("CONNECT response hook returned error: %v", err)
	}

	out := logs.String()
	for _, want := range []string{"upstream CONNECT response", "target.example:443", "residential-primary", "407", "request-123"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q: %s", want, out)
		}
	}
	for _, forbidden := range []string{"exit-user", "secret-exit-password"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("log leaked credential %q: %s", forbidden, out)
		}
	}
}

func TestTransportConnectResponseLogUsesRequestContext(t *testing.T) {
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Errorf("method = %s, want CONNECT", r.Method)
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(mockUpstream.Close)
	upstreamURL, err := url.Parse("http://exit-user:secret-exit-password@" + strings.TrimPrefix(mockUpstream.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	logger := slog.New(reqid.NewHandler(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	s := New(testHolderAllowPrivateTargets(), nil, upstream.EmptyTable(), nil, logger)
	tr := s.transport
	t.Cleanup(tr.CloseIdleConnections)
	req, err := http.NewRequest(http.MethodGet, "https://target.example/v1/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(reqid.With(req.Context(), "request-transport-429"), ctxKey{}, upstreamURL)
	ctx = context.WithValue(ctx, fwdMetaKey{}, &fwdMeta{viaUpstream: true, upstream: "residential-primary"})
	_, err = tr.RoundTrip(req.WithContext(ctx))
	if err == nil {
		t.Fatal("RoundTrip unexpectedly succeeded after CONNECT 429")
	}

	out := logs.String()
	for _, want := range []string{"upstream CONNECT response", "429", "residential-primary", "request-transport-429"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q: %s", want, out)
		}
	}
	for _, forbidden := range []string{"exit-user", "secret-exit-password"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("log leaked credential %q: %s", forbidden, out)
		}
	}
}

func TestTransportClassifiesUpstreamConnectRejection(t *testing.T) {
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Errorf("method = %s, want CONNECT", r.Method)
		}
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	t.Cleanup(mockUpstream.Close)
	upstreamURL, err := url.Parse(mockUpstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	s := New(testHolderAllowPrivateTargets(), nil, upstream.EmptyTable(), nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	tr := s.transport
	t.Cleanup(tr.CloseIdleConnections)
	req, err := http.NewRequest(http.MethodGet, "https://target.example/v1/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(req.Context(), ctxKey{}, upstreamURL)
	_, err = tr.RoundTrip(req.WithContext(ctx))
	if err == nil {
		t.Fatal("RoundTrip unexpectedly succeeded after CONNECT 407")
	}
	if got := forwardFailureClass(err); got != "upstream_connect_rejected" {
		t.Fatalf("forwardFailureClass(%v) = %q, want upstream_connect_rejected", err, got)
	}
}

func TestIngressRequestIDStaysInternalAndIsAudited(t *testing.T) {
	audit := make(chan store.LogEntry, 1)
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	srv := New(testHolderAllowPrivateTargets(), nil, upstream.EmptyTable(), audit, logger)
	routerSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(routerSrv.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/request-id" || r.URL.RawQuery != "check=1" {
			t.Errorf("upstream request = %s %s?%s, want POST /request-id?check=1", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("X-Client-Test"); got != "client-supplied" {
			t.Errorf("upstream request header = %q, want client-supplied", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-client-test" {
			t.Errorf("upstream content type = %q, want application/x-client-test", got)
		}
		if got := r.Header.Get("User-Agent"); got != "" {
			t.Errorf("upstream injected User-Agent %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "client-body" {
			t.Errorf("upstream request body = %q, err=%v; want client-body", body, err)
		}
		w.Header().Add("X-Origin-Test", "origin-first")
		w.Header().Add("X-Origin-Test", "origin-second")
		w.Header().Set("Trailer", "X-Origin-Trailer")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "origin-body")
		w.Header().Set("X-Origin-Trailer", "origin-trailer")
	}))
	t.Cleanup(origin.Close)

	upstreamURL, err := url.Parse(routerSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(upstreamURL)}}
	req, err := http.NewRequest(http.MethodPost, origin.URL+"/request-id?check=1", strings.NewReader("client-body"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Client-Test", "client-supplied")
	req.Header.Set("Content-Type", "application/x-client-test")
	// nil 切片是 net/http 对“明确缺少 User-Agent”的表示，
	// 可阻止客户端传输层注入默认值。
	req.Header["User-Agent"] = nil
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != "origin-body" {
		t.Fatalf("client response body = %q, err=%v; want origin-body", body, err)
	}
	if got := resp.Header.Values("X-Origin-Test"); !slices.Equal(got, []string{"origin-first", "origin-second"}) {
		t.Errorf("client response header values = %q, want origin-first/origin-second", got)
	}
	if got := resp.Trailer.Get("X-Origin-Trailer"); got != "origin-trailer" {
		t.Errorf("client response trailer = %q, want origin-trailer", got)
	}

	select {
	case entry := <-audit:
		if len(entry.ReqID) != 32 {
			t.Errorf("audited request ID = %q, want 32-char random hex", entry.ReqID)
		}
	case <-time.After(time.Second):
		t.Fatal("audit entry was not added to the audit channel")
	}
}

func TestIngressAuditTTFBPrecedesCompletedDuration(t *testing.T) {
	audit := make(chan store.LogEntry, 1)
	srv := New(testHolderAllowPrivateTargets(), nil, upstream.EmptyTable(), audit,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	routerSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(routerSrv.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(25 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(25 * time.Millisecond)
		_, _ = io.WriteString(w, "done")
	}))
	t.Cleanup(origin.Close)

	upstreamURL, err := url.Parse(routerSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(upstreamURL)}}
	resp, err := client.Get(origin.URL + "/ttfb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case entry := <-audit:
		if entry.TTFBMS == nil {
			t.Fatal("audited response is missing TTFB")
		}
		if *entry.TTFBMS < 15 {
			t.Fatalf("TTFB=%dms, want response-header delay", *entry.TTFBMS)
		}
		if entry.DurMS < *entry.TTFBMS+15 {
			t.Fatalf("duration=%dms TTFB=%dms, want body streaming time included", entry.DurMS, *entry.TTFBMS)
		}
	case <-time.After(time.Second):
		t.Fatal("audit entry was not added to the audit channel")
	}
}

func TestRespRecRecordsTTFBOnce(t *testing.T) {
	start := time.Now().Add(-25 * time.Millisecond)
	rec := newRespRec(httptest.NewRecorder(), nil, nil, context.Background(), "direct", "direct", start)
	rec.WriteHeader(http.StatusCreated)
	if rec.ttfbMS == nil || *rec.ttfbMS < 20 {
		t.Fatalf("explicit-header TTFB=%v, want at least 20ms", rec.ttfbMS)
	}
	first := *rec.ttfbMS
	time.Sleep(10 * time.Millisecond)
	_, _ = rec.Write([]byte("body"))
	rec.WriteHeader(http.StatusOK)
	if rec.ttfbMS == nil || *rec.ttfbMS != first {
		t.Fatalf("TTFB was overwritten: got=%v first=%d", rec.ttfbMS, first)
	}

	implicit := newRespRec(httptest.NewRecorder(), nil, nil, context.Background(), "direct", "direct", start)
	_, _ = implicit.Write([]byte("body"))
	if implicit.ttfbMS == nil || *implicit.ttfbMS < 20 {
		t.Fatalf("implicit-header TTFB=%v, want at least 20ms", implicit.ttfbMS)
	}

	flushed := newRespRec(httptest.NewRecorder(), nil, nil, context.Background(), "direct", "direct", start)
	flushed.Flush()
	if flushed.status != http.StatusOK || flushed.ttfbMS == nil || *flushed.ttfbMS < 20 {
		t.Fatalf("flush-first status=%d TTFB=%v, want 200 and at least 20ms", flushed.status, flushed.ttfbMS)
	}
}

func TestResponseBodyFailureLogIsRequestCorrelated(t *testing.T) {
	var logs bytes.Buffer
	meta := &fwdMeta{}
	body := &responseReadLogBody{
		ReadCloser: io.NopCloser(failingReader{}),
		meta:       meta,
		logger:     slog.New(reqid.NewHandler(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		ctx:        reqid.With(context.Background(), "response-body-request"),
		status:     http.StatusOK,
		route:      "upstream",
		upstream:   "residential-primary",
		started:    time.Now(),
	}
	if _, err := body.Read(make([]byte, 1)); err == nil {
		t.Fatal("Read unexpectedly succeeded")
	}
	out := logs.String()
	for _, want := range []string{"upstream response body read failed", "upstream_response_body", "response-body-request", "residential-primary"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q: %s", want, out)
		}
	}
	if got := internalErrorFromMeta(meta); got != "upstream_response_read" {
		t.Fatalf("internal error = %q, want upstream_response_read", got)
	}
}

func TestResponseBodyEOFClassifiesInternalError(t *testing.T) {
	meta := &fwdMeta{}
	body := &responseReadLogBody{ReadCloser: io.NopCloser(eofReader{}), meta: meta, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := body.Read(make([]byte, 1)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Read error = %v, want unexpected EOF", err)
	}
	if got := internalErrorFromMeta(meta); got != "upstream_response_eof" {
		t.Fatalf("internal error = %q, want upstream_response_eof", got)
	}
}

func TestResponseBodyCancelClassifiesInternalError(t *testing.T) {
	meta := &fwdMeta{}
	body := &responseReadLogBody{ReadCloser: io.NopCloser(cancelReader{}), meta: meta, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := body.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Read error = %v, want context canceled", err)
	}
	if got := internalErrorFromMeta(meta); got != "canceled" {
		t.Fatalf("internal error = %q, want canceled", got)
	}
}

func TestTunnelFailureDetails(t *testing.T) {
	t.Run("HTTP CONNECT rejection", func(t *testing.T) {
		err := &tunnelDialError{stage: "upstream_connect", upstreamStatus: http.StatusProxyAuthRequired, err: errors.New("rejected")}
		stage, status, rep := tunnelFailureDetails(err, true)
		if stage != "upstream_connect" || status != http.StatusProxyAuthRequired || rep != 0 {
			t.Fatalf("got stage=%q status=%d rep=%d", stage, status, rep)
		}
	})
	t.Run("SOCKS CONNECT rejection", func(t *testing.T) {
		err := &tunnelDialError{stage: "socks_connect", socksRep: 5, err: errors.New("rejected")}
		stage, status, rep := tunnelFailureDetails(err, true)
		if stage != "socks_connect" || status != 0 || rep != 5 {
			t.Fatalf("got stage=%q status=%d rep=%d", stage, status, rep)
		}
	})
}

func TestForwardFailureAuditsSafeFailureClass(t *testing.T) {
	audit := make(chan store.LogEntry, 1)
	srv := New(testHolderAllowPrivateTargets(), nil, upstream.EmptyTable(), audit,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	routerSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(routerSrv.Close)
	upstreamURL, err := url.Parse(routerSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(upstreamURL)}}
	resp, err := client.Get("http://127.0.0.1:1/unreachable")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
	select {
	case entry := <-audit:
		if entry.Status != 0 {
			t.Fatalf("audited status = %d, want 0 for local transport failure", entry.Status)
		}
		if entry.InternalError != "dial" {
			t.Fatalf("audited internal error = %q, want dial", entry.InternalError)
		}
		if len(entry.ReqID) != 32 {
			t.Fatalf("audited request ID = %q, want 32-char random hex", entry.ReqID)
		}
		if entry.TTFBMS == nil {
			t.Fatal("generated 502 must have a TTFB")
		}
	case <-time.After(time.Second):
		t.Fatal("failed forward was not added to audit channel")
	}
}

func TestUpstreamHTTPErrorKeepsHTTPStatusWithoutInternalError(t *testing.T) {
	for _, wantStatus := range []int{http.StatusUnauthorized, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(wantStatus), func(t *testing.T) {
			audit := make(chan store.LogEntry, 1)
			srv := New(testHolderAllowPrivateTargets(), nil, upstream.EmptyTable(), audit,
				slog.New(slog.NewTextHandler(io.Discard, nil)))
			routerSrv := httptest.NewServer(srv.Handler())
			t.Cleanup(routerSrv.Close)
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(wantStatus)
			}))
			t.Cleanup(origin.Close)

			upstreamURL, err := url.Parse(routerSrv.URL)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(upstreamURL)}}
			resp, err := client.Get(origin.URL + "/upstream-error")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != wantStatus {
				t.Fatalf("response status = %d, want %d", resp.StatusCode, wantStatus)
			}
			select {
			case entry := <-audit:
				if entry.Status != wantStatus {
					t.Fatalf("audited status = %d, want upstream status %d", entry.Status, wantStatus)
				}
				if entry.InternalError != "" {
					t.Fatalf("audited internal error = %q, want empty for upstream HTTP response", entry.InternalError)
				}
			case <-time.After(time.Second):
				t.Fatal("upstream HTTP error was not added to the audit channel")
			}
		})
	}
}

func TestResolveOutboundReportsDirectReason(t *testing.T) {
	s := New(testHolderAllowPrivateTargets(), nil, upstream.EmptyTable(), nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	snap := s.settings.Current()

	t.Run("policy direct", func(t *testing.T) {
		snap.NoMarkerPolicy = settings.PolicyDirect
		pu, _, name, reason := s.resolveOutboundWithContext(context.Background(), snap, "", identity{}, "127.0.0.1", "api.example:443")
		if pu != nil || name != "direct" || reason != "policy_direct" {
			t.Fatalf("got url=%v name=%q reason=%q", pu, name, reason)
		}
	})

	t.Run("private target blocked by default", func(t *testing.T) {
		snap.NoMarkerPolicy = settings.PolicyDefaultSession
		snap.BlockPrivateTargets = true
		_, _, _, reason, err := s.resolveOutboundDetailed(context.Background(), snap, "marker", identity{}, "127.0.0.1", "127.0.0.1:443")
		if !errors.Is(err, errPrivateTargetBlocked) || reason != "private_target_blocked" {
			t.Fatalf("reason=%q err=%v, want private_target_blocked", reason, err)
		}
	})

	t.Run("private target explicitly allowed", func(t *testing.T) {
		snap.BlockPrivateTargets = false
		pu, _, name, reason := s.resolveOutboundWithContext(context.Background(), snap, "marker", identity{}, "127.0.0.1", "127.0.0.1:443")
		if pu != nil || name != "direct" || reason != "private_target_allowed" {
			t.Fatalf("got url=%v name=%q reason=%q", pu, name, reason)
		}
	})

	t.Run("missing upstream", func(t *testing.T) {
		snap.BlockPrivateTargets = false
		pu, _, name, reason := s.resolveOutboundWithContext(context.Background(), snap, "marker", identity{}, "127.0.0.1", "api.example:443")
		if pu != nil || name != "direct" || reason != "no_upstream" {
			t.Fatalf("got url=%v name=%q reason=%q", pu, name, reason)
		}
	})
}

// P0-1: default_upstream 已配置但运行时表里找不到（缺失/停用）时，必须受控失败，
// 绝不能返回 nil URL 走直连暴露本机出口 IP。空 default_upstream 仍允许直连。
func TestResolveOutboundRejectsConfiguredButMissingUpstream(t *testing.T) {
	s := New(testHolderAllowPrivateTargets(), nil, upstream.EmptyTable(), nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	snap := s.settings.Current()
	snap.BlockPrivateTargets = false
	snap.DefaultUpstream = "configured-but-missing"

	pu, _, _, _, err := s.resolveOutboundDetailed(context.Background(),
		snap, "marker", identity{}, "203.0.113.7", "api.example:443")
	if err == nil {
		t.Fatal("configured-but-missing default upstream must fail controlled, not silently direct-connect")
	}
	if !errors.Is(err, errUpstreamConfig) {
		t.Fatalf("err=%v, want errUpstreamConfig", err)
	}
	if pu != nil {
		t.Fatalf("pu=%v, want nil (must not synthesize a direct route)", pu)
	}
}

func TestPrivateDNSResultIsBlockedBeforeUpstreamRouting(t *testing.T) {
	oldLookup := lookupNetIP
	lookupNetIP = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.0.0.5")}, nil
	}
	t.Cleanup(func() { lookupNetIP = oldLookup })

	holder := testHolderAllowPrivateTargets()
	snap := holder.Current()
	snap.BlockPrivateTargets = true
	snap.DefaultUpstream = "configured-upstream"
	holder.Set(snap)
	up, err := upstream.FromRow(1, "configured-upstream", "dataimpulse", "http://user__cr.us:pass@upstream.example:80", sql.NullString{}, true)
	if err != nil {
		t.Fatal(err)
	}
	s := New(holder, nil, upstream.NewTable([]*upstream.Upstream{up}, up.Name), nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	pu, _, _, reason, err := s.resolveOutboundDetailed(context.Background(), s.settings.Current(), "marker", identity{}, "203.0.113.7", "internal.example:443")
	if !errors.Is(err, errPrivateTargetBlocked) || reason != "private_target_blocked" || pu != nil {
		t.Fatalf("url=%v reason=%q err=%v, want private_target_blocked before upstream selection", pu, reason, err)
	}
}

func TestForwardFailureClass(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"timeout":         {err: &url.Error{Op: "Get", URL: "https://target.example", Err: context.DeadlineExceeded}, want: "timeout"},
		"canceled":        {err: context.Canceled, want: "canceled"},
		"eof":             {err: io.ErrUnexpectedEOF, want: "eof"},
		"dns":             {err: &net.DNSError{Name: "target.example", IsNotFound: true}, want: "dns"},
		"upstream rejection": {err: &url.Error{Op: "Get", URL: "https://target.example", Err: errText("Proxy Authentication Required")}, want: "upstream_connect_rejected"},
		"tls":             {err: errText("remote error: tls: handshake failure"), want: "tls"},
		"other":           {err: errText("connection reset by peer"), want: "transport"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := forwardFailureClass(tc.err); got != tc.want {
				t.Fatalf("forwardFailureClass(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestWithTunnelTargetResolutionKeepsInnerRequestCancellation(t *testing.T) {
	innerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner := httptest.NewRequest(http.MethodGet, "https://api.example/v1/test", nil).WithContext(innerCtx)
	want := publicTargetResolution{host: "api.example", ips: []netip.Addr{netip.MustParseAddr("203.0.113.10")}}
	tunnelCtx := context.WithValue(context.Background(), publicTargetResolutionKey{}, want)

	got := withTunnelTargetResolution(inner, tunnelCtx)
	resolution, ok := got.Context().Value(publicTargetResolutionKey{}).(publicTargetResolution)
	if !ok || resolution.host != want.host || len(resolution.ips) != 1 || resolution.ips[0] != want.ips[0] {
		t.Fatalf("tunnel resolution was not transferred: %#v", resolution)
	}
	cancel()
	select {
	case <-got.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("inner request cancellation was replaced by tunnel context")
	}
}

func TestCloneForwardRequestKeepsClientRequestUntouched(t *testing.T) {
	in := httptest.NewRequest(http.MethodPost, "http://client.example:8080/path?q=value", strings.NewReader("client-body"))
	in.Header.Set("X-Client-Test", "client-supplied")
	originalURL := in.URL.String()

	out := cloneForwardRequest(in, "https", "origin.example:443", context.Background())
	if in.URL.String() != originalURL {
		t.Fatalf("client request URL changed to %q, want %q", in.URL, originalURL)
	}
	if got := in.Header.Get("X-Client-Test"); got != "client-supplied" {
		t.Fatalf("client request header changed to %q", got)
	}
	if out.URL.Scheme != "https" || out.URL.Host != "origin.example:443" || out.URL.Path != "/path" || out.URL.RawQuery != "q=value" {
		t.Fatalf("routing clone URL = %s, want https://origin.example:443/path?q=value", out.URL)
	}
	if out.RequestURI != "" {
		t.Fatalf("routing clone RequestURI = %q, want empty for transport", out.RequestURI)
	}
	if got := out.Header.Get("X-Client-Test"); got != "client-supplied" {
		t.Fatalf("routing clone header = %q, want client-supplied", got)
	}
}

func TestWithFreshRequestIDReplacesTunnelRequestID(t *testing.T) {
	tunnelCtx := reqid.With(context.Background(), "connect-request-id")
	first := httptest.NewRequest(http.MethodPost, "https://api.example/v1/responses", nil).WithContext(tunnelCtx)
	firstID := reqid.From(withFreshInternalRequestID(first).Context())
	if firstID == "connect-request-id" || len(firstID) != 32 {
		t.Fatalf("inner request ID = %q, want a fresh 32-character ID", firstID)
	}
	second := httptest.NewRequest(http.MethodPost, "https://api.example/v1/responses", nil).WithContext(tunnelCtx)
	secondID := reqid.From(withFreshInternalRequestID(second).Context())
	if secondID == firstID {
		t.Fatalf("two inner HTTP requests reused request ID %q", firstID)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("upstream stream reset") }

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type cancelReader struct{}

func (cancelReader) Read([]byte) (int, error) { return 0, context.Canceled }

type errText string

func (e errText) Error() string { return string(e) }
