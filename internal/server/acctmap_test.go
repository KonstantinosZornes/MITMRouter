package server

// 账号映射（acctmap）热路径集成测试：
// 命中映射表的凭据必须以 "platform/account" 派生粘滞身份，且与 v3 Marker 公式隔离；
// 未命中凭据维持 v3 行为；盐值轮换按账号级生效。

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"mitmrouter/internal/acctmap"
	"mitmrouter/internal/settings"
	"mitmrouter/internal/sticky"
	"mitmrouter/internal/store"
)

func newAcctTestServer(t *testing.T, entries []acctmap.Entry) (*Server, *settings.Holder) {
	t.Helper()
	s := newSaltTestServer()
	reg := acctmap.New()
	reg.Reload(entries)
	s.AttachAcctMap(reg)
	return s, s.settings
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestAcctMapResolveOutboundIsolation(t *testing.T) {
	srv, holder := newAcctTestServer(t, []acctmap.Entry{{
		Platform: "openai", Account: "a@b.io",
		AtFp:   acctmap.Fingerprint("openai", "sk-live-1"),
		Source: acctmap.SourcePush, SourceType: acctmap.SourceTypeCLIProxyAPI,
	}})
	snap := holder.Current()

	const cred = "sk-live-1"
	pf := acctmap.PlatformForHost("api.openai.com")
	if pf != "openai" {
		t.Fatalf("platform=%q", pf)
	}
	e, ok := srv.acctMap.Lookup(acctmap.Fingerprint(pf, cred))
	if !ok || e.Account != "a@b.io" {
		t.Fatalf("registry lookup failed: %+v %v", e, ok)
	}
	ident := identity{key: pf + "/" + e.Account, mapped: true}

	_, accMapped, _ := srv.resolveOutbound(snap, cred, ident, "9.9.9.9", "api.openai.com")
	want := sticky.Derive(sticky.CombineSalt(snap.Salt, 0)+"#a", ident.key, snap.SIDLen)
	if accMapped != want {
		t.Fatalf("mapped identity mismatch: got %s want %s", accMapped, want)
	}

	// 与 v3 纯 Marker 公式必然不同（"#a" 域分隔）
	_, accLegacy, _ := srv.resolveOutbound(snap, cred, identity{}, "9.9.9.9", "api.openai.com")
	if accMapped == accLegacy {
		t.Fatal("mapped identity must differ from legacy marker hash")
	}

	// 同账号不同凭据 → 同一身份键 → 同一 hash
	_, sameAcct, _ := srv.resolveOutbound(snap, "sk-live-other-key",
		identity{key: pf + "/" + e.Account, mapped: true}, "9.9.9.9", "api.openai.com")
	if sameAcct != accMapped {
		t.Fatalf("same account different key must share identity: %s vs %s", sameAcct, accMapped)
	}

	// 未命中凭据走 v3 公式
	_, miss, _ := srv.resolveOutbound(snap, "sk-unknown", identity{}, "9.9.9.9", "api.openai.com")
	if miss != sticky.Derive(sticky.CombineSalt(snap.Salt, 0), "sk-unknown", snap.SIDLen) {
		t.Fatal("unknown credential must fall back to legacy derivation")
	}
}

func TestAcctMapRotationIsAccountScoped(t *testing.T) {
	srv, holder := newAcctTestServer(t, []acctmap.Entry{{
		Platform: "openai", Account: "r@x.io",
		AtFp:   acctmap.Fingerprint("openai", "sk-r1"),
		RtFp:   acctmap.Fingerprint("openai", "rt-r1"),
		Source: acctmap.SourcePush, SourceType: acctmap.SourceTypeCLIProxyAPI,
	}})
	snap := holder.Current()

	idA := identity{key: "openai/r@x.io", mapped: true}
	_, a0, _ := srv.resolveOutbound(snap, "sk-r1", idA, "9.9.9.9", "chatgpt.com")
	srv.markerSalts.Rotate(idA.key)
	_, a1, _ := srv.resolveOutbound(snap, "sk-r1", idA, "9.9.9.9", "chatgpt.com")
	if a0 == a1 {
		t.Fatal("rotation must change mapped identity")
	}

	// 同账号第二把 key 共享轮换后的身份
	_, a2, _ := srv.resolveOutbound(snap, "sk-r2", idA, "9.9.9.9", "chatgpt.com")
	if a2 != a1 {
		t.Fatalf("second credential of same account must follow rotation: %s vs %s", a2, a1)
	}
}

func TestAcctMapGrokRefreshBodyForward(t *testing.T) {
	s := newStack(t)
	bodyCh := make(chan string, 1)
	leaf, err := s.ca.LeafForHost("auth.x.ai")
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyCh <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	origin.TLS = &tls.Config{Certificates: []tls.Certificate{*leaf}}
	origin.StartTLS()
	defer origin.Close()

	mock := startMockUpstream(t, origin.URL)
	const refreshToken = "rt-grok-e2e"
	reg := acctmap.New()
	reg.Reload([]acctmap.Entry{{
		Platform: "grok", Account: "grok-owner@example.com",
		RtFp: acctmap.Fingerprint("grok", refreshToken),
	}})
	s.srv.AttachAcctMap(reg)
	audit := make(chan store.LogEntry, 1)
	s.srv.audit = audit
	s.registerUpstream(t, "mock-di", "dataimpulse", "http://mockuser__cr.us:pw@"+mock.listener.Addr().String())
	s.setDefaultUpstream(t, "mock-di")

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(s.ca.CertificatePEM()) {
		t.Fatal("failed to load test CA")
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(mustURL(t, s.feURL)),
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}}
	// 上行请求经 mock CONNECT 到测试 TLS 源站，也必须信任同一测试 CA。
	s.srv.transport.TLSClientConfig = &tls.Config{RootCAs: pool}
	form := url.Values{"client_id": {"client"}, "grant_type": {"refresh_token"}, "refresh_token": {refreshToken}}
	req, _ := http.NewRequest(http.MethodPost, "https://auth.x.ai/oauth2/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("upstream response status=%d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()

	select {
	case got := <-bodyCh:
		if got != form.Encode() {
			t.Fatalf("origin body changed: got %q want %q", got, form.Encode())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("origin did not receive request")
	}
	select {
	case entry := <-audit:
		if entry.Account != "grok-owner@example.com" || !entry.HasMarker {
			t.Fatalf("audit entry=%+v", entry)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("audit entry not emitted")
	}

	seen := collectAuths(mock, 1, 3*time.Second)
	if len(seen) != 1 || !strings.Contains(seen[0], "sessid.") {
		t.Fatalf("unexpected upstream auth: %q", seen)
	}
	snap := s.holder.Current()
	want := sticky.Derive(sticky.CombineSalt(snap.Salt, 0)+"#a", "grok/grok-owner@example.com", snap.SIDLen)
	if !strings.Contains(seen[0], "sessid."+want) {
		t.Fatalf("body-derived mapped SID missing: %q", seen[0])
	}
}

func TestAcctMapEndToEndForward(t *testing.T) {
	o := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer o.Close()

	mock := startMockUpstream(t, o.URL)
	host := mock.listener.Addr().String()

	s := newStack(t)
	reg := acctmap.New()
	reg.Reload([]acctmap.Entry{{
		Platform: "anthropic", Account: "e2e@x.io",
		AtFp:   acctmap.Fingerprint("anthropic", "sk-e2e"),
		Source: acctmap.SourcePush, SourceType: acctmap.SourceTypeCLIProxyAPI,
	}})
	s.srv.AttachAcctMap(reg)
	audit := make(chan store.LogEntry, 1)
	s.srv.audit = audit
	s.registerUpstream(t, "mock-di", "dataimpulse", "http://mockuser__cr.us:pw@"+host)
	s.setDefaultUpstream(t, "mock-di")

	// 客户端信任本机路由的自签 CA，使 MITM 链路可走通
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(s.ca.CertificatePEM()) {
		t.Fatal("failed to load test CA")
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(mustURL(t, s.feURL)),
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}}
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	req.Header.Set("x-api-key", "sk-e2e")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case entry := <-audit:
		if entry.Account != "e2e@x.io" {
			t.Fatalf("audit account=%q, want mapped account", entry.Account)
		}
		if entry.AccountFP == "" {
			t.Fatal("audit must retain derived session ID")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("audit entry not emitted")
	}

	// 出口侧（模拟上游出口平台）必须收到以账号级身份派生的会话 ID，
	// 且该 ID 与 v3 Marker 公式推导值不同（"#a" 域分隔生效）。
	seen := collectAuths(mock, 1, 3*time.Second)
	if len(seen) != 1 {
		t.Fatalf("upstream received %d requests", len(seen))
	}
	i := strings.Index(seen[0], "sessid.")
	if i < 0 {
		t.Fatalf("no sessid in auth %q", seen[0])
	}
	rest := seen[0][i+len("sessid."):]
	j := strings.IndexByte(rest, ':')
	if j < 0 {
		j = len(rest)
	}
	got := rest[:j]

	snap := s.holder.Current()
	wantMapped := sticky.Derive(sticky.CombineSalt(snap.Salt, 0)+"#a", "anthropic/e2e@x.io", snap.SIDLen)
	wantLegacy := sticky.Derive(sticky.CombineSalt(snap.Salt, 0), "sk-e2e", snap.SIDLen)
	if got != wantMapped {
		t.Fatalf("account-scoped identity mismatch: got %s want %s", got, wantMapped)
	}
	if got == wantLegacy {
		t.Fatal("identity must not equal legacy marker hash")
	}
}
