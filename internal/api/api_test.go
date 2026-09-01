package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mitmrouter/internal/acctmap"
	"mitmrouter/internal/certca"
	"mitmrouter/internal/metrics"
	"mitmrouter/internal/settings"
	"mitmrouter/internal/store"
	"mitmrouter/internal/upstream"
)

type fixture struct {
	api     *API
	h       http.Handler
	st      *store.Store
	holder  *settings.Holder
	dataDir string

	mu       sync.Mutex
	swapped  []*upstream.Table
	password string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	st, info, err := store.Bootstrap(dataDir)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	snap, err := settings.LoadFromStore(ctx, st, settings.DefaultSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	holder := settings.NewHolder(snap)
	ca, err := certca.Ensure(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{st: st, holder: holder, password: info.AdminPassword, dataDir: dataDir}
	f.api = New(Deps{
		Store:         st,
		Settings:      holder,
		CA:            ca,
		SwapUpstreams: func(tb *upstream.Table) { f.mu.Lock(); f.swapped = append(f.swapped, tb); f.mu.Unlock() },
		Metrics:       metrics.NewRegistry(),
		IngressPort:   "55666",
	})
	f.h = f.api.Router()
	return f
}

func (f *fixture) lastTable() *upstream.Table {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.swapped) == 0 {
		return nil
	}
	return f.swapped[len(f.swapped)-1]
}

func (f *fixture) login(t *testing.T) *http.Cookie {
	t.Helper()
	rec := f.do(t, "POST", "/api/auth/login", nil, map[string]string{"password": f.password})
	if rec.Code != 200 {
		t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	t.Fatal("no session cookie issued")
	return nil
}

func (f *fixture) do(t *testing.T, method, path string, cookie *http.Cookie, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = "203.0.113.7:44444"
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return v
}

// P2-8: 请求体必须恰好包含一个 JSON 值，拒绝第二个 JSON 值和尾随垃圾。
func TestRejectsTrailingJSON(t *testing.T) {
	f := newFixture(t)
	body := bytes.NewBufferString(`{"password":"` + f.password + `"} {}`)
	r := httptest.NewRequest(http.MethodPost, "/api/auth/login", body)
	r.RemoteAddr = "203.0.113.7:44444"
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON: HTTP %d %s, want 400", rec.Code, rec.Body.String())
	}
}

func TestUnauthenticatedRequestsRejected(t *testing.T) {
	f := newFixture(t)
	endpoints := []struct{ method, path string }{
		{"GET", "/api/auth/me"},
		{"GET", "/api/settings"},
		{"PUT", "/api/settings"},
		{"POST", "/api/settings/reset-salt"},
		{"GET", "/api/upstreams"},
		{"POST", "/api/upstreams"},
		{"PUT", "/api/upstreams/1"},
		{"DELETE", "/api/upstreams/1"},
		{"POST", "/api/upstreams/1/default"},
		{"POST", "/api/upstreams/1/test"},
		{"GET", "/api/logs"},
		{"DELETE", "/api/logs"},
		{"GET", "/api/ca.pem"},
		{"GET", "/api/ca.crt"},
		{"GET", "/metrics"},
	}
	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			rec := f.do(t, ep.method, ep.path, nil, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("got %d want 401", rec.Code)
			}
			e := decode[struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}](t, rec)
			if e.Error.Code != "unauthorized" {
				t.Errorf("error code %q want unauthorized", e.Error.Code)
			}
		})
	}
	rec := f.do(t, "POST", "/api/auth/login", nil, map[string]string{"password": "x"})
	if rec.Code == http.StatusUnauthorized && strings.Contains(rec.Body.String(), "please log in") {
		t.Error("login endpoint itself must not require a session")
	}
}

func TestLoginLogoutCycle(t *testing.T) {
	f := newFixture(t)

	if rec := f.do(t, "POST", "/api/auth/login", nil, map[string]string{"password": "wrong"}); rec.Code != 401 {
		t.Fatalf("wrong password: got %d want 401", rec.Code)
	}
	if rec := f.do(t, "POST", "/api/auth/login", nil, map[string]string{}); rec.Code != 400 {
		t.Fatalf("empty password: got %d want 400", rec.Code)
	}
	r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader("{not-json"))
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	if rec.Code != 400 {
		t.Fatalf("malformed json: got %d want 400", rec.Code)
	}

	cookie := f.login(t)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Secure {
		t.Errorf("HTTP cookie flags wrong: %+v", cookie)
	}
	if me := f.do(t, "GET", "/api/auth/me", cookie, nil); me.Code != 200 {
		t.Errorf("/me with session: %d", me.Code)
	}

	if err := f.api.d.Store.SetSecret(context.Background(), "session_hmac_key", []byte("rotated")); err != nil {
		t.Fatal(err)
	}
	if me := f.do(t, "GET", "/api/auth/me", cookie, nil); me.Code != 401 {
		t.Error("cookie signed with old hmac key must be rejected after rotation")
	}
}

func TestHTTPSAdminSessionsUseSecureCookies(t *testing.T) {
	f := newFixture(t)

	login := httptest.NewRequest(http.MethodPost, "https://admin.example.test/api/auth/login", strings.NewReader(`{"password":"`+f.password+`"}`))
	login.RemoteAddr = "203.0.113.7:44444"
	login.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	f.h.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("HTTPS login failed: %d %s", loginRec.Code, loginRec.Body.String())
	}
	var session *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == cookieName {
			session = c
			break
		}
	}
	if session == nil || !session.Secure {
		t.Fatalf("HTTPS login must issue a Secure cookie, got %+v", session)
	}

	nearExpiry, err := f.api.signSession(time.Now().Add(sessionTTL / 3).Unix())
	if err != nil {
		t.Fatal(err)
	}
	renew := httptest.NewRequest(http.MethodGet, "https://admin.example.test/api/auth/me", nil)
	renew.RemoteAddr = "203.0.113.7:44444"
	renew.AddCookie(&http.Cookie{Name: cookieName, Value: nearExpiry})
	renewRec := httptest.NewRecorder()
	f.h.ServeHTTP(renewRec, renew)
	if renewRec.Code != http.StatusOK {
		t.Fatalf("HTTPS session renewal failed: %d %s", renewRec.Code, renewRec.Body.String())
	}
	for _, c := range renewRec.Result().Cookies() {
		if c.Name == cookieName {
			if !c.Secure {
				t.Fatal("HTTPS session renewal must retain the Secure cookie attribute")
			}
			return
		}
	}
	t.Fatal("near-expiry HTTPS session must be renewed")
}

func TestLoginBackoff(t *testing.T) {
	f := newFixture(t)
	ip := "198.51.100.9:999"

	bad := func() {
		r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"password":"nope"}`))
		r.RemoteAddr = ip
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, r)
		if rec.Code != 401 {
			t.Fatalf("expected 401, got %d", rec.Code)
		}
	}
	bad()
	if rec := f.goodLoginFrom(ip); rec.Code != 200 {
		t.Fatal("first failure must not lock the ip")
	}
	bad()
	bad()

	r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"password":"`+f.password+`"}`))
	r.RemoteAddr = ip
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked ip must get 429 even with correct password, got %d", rec.Code)
	}

	other := f.goodLoginFrom("198.51.100.8:1")
	if other.Code != 200 {
		t.Error("backoff must be per-ip")
	}

	f.api.clearFailures(remoteHostStr(ip))
	if rec := f.goodLoginFrom(ip); rec.Code != 200 {
		t.Errorf("clearFailures must unlock the ip, got %d", rec.Code)
	}
}

func TestLoginBackoffIsBoundedAndExpires(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < maxBackoffEntries+1; i++ {
		f.api.registerFailure(fmt.Sprintf("198.51.%d.%d", i/256, i%256))
	}
	f.api.backoffMu.Lock()
	entries := len(f.api.backoff)
	f.api.backoffMu.Unlock()
	if entries > maxBackoffEntries {
		t.Fatalf("backoff entries=%d, want at most %d", entries, maxBackoffEntries)
	}

	const staleIP = "203.0.113.99"
	f.api.registerFailure(staleIP)
	f.api.backoffMu.Lock()
	f.api.backoff[staleIP].last = time.Now().Add(-maxBackoff - time.Second)
	f.api.backoffMu.Unlock()
	if _, locked := f.api.backoffRemaining(staleIP); locked {
		t.Fatal("expired backoff record must not remain locked")
	}
	f.api.backoffMu.Lock()
	_, exists := f.api.backoff[staleIP]
	f.api.backoffMu.Unlock()
	if exists {
		t.Fatal("expired backoff record must be removed on access")
	}
}

func (f *fixture) goodLoginFrom(addr string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"password":"`+f.password+`"}`))
	r.RemoteAddr = addr
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	return rec
}

func TestSessionExpiryAndSlidingRenewal(t *testing.T) {
	f := newFixture(t)

	expired, err := f.api.signSession(time.Now().Add(-time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, "GET", "/api/auth/me", &http.Cookie{Name: cookieName, Value: expired}, nil)
	if rec.Code != 401 {
		t.Errorf("expired token must be 401, got %d", rec.Code)
	}

	tampered := expired + "x"
	rec = f.do(t, "GET", "/api/auth/me", &http.Cookie{Name: cookieName, Value: tampered}, nil)
	if rec.Code != 401 {
		t.Errorf("tampered signature must be 401, got %d", rec.Code)
	}

	halfLife, _ := f.api.signSession(time.Now().Add(sessionTTL / 3).Unix())
	rec = f.do(t, "GET", "/api/auth/me", &http.Cookie{Name: cookieName, Value: halfLife}, nil)
	if rec.Code != 200 {
		t.Fatalf("half-life token should work, got %d", rec.Code)
	}
	var renewed bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			renewed = true
		}
	}
	if !renewed {
		t.Error("token below half life must trigger sliding renewal Set-Cookie")
	}

	fresh, _ := f.api.signSession(time.Now().Add(sessionTTL - time.Hour).Unix())
	rec = f.do(t, "GET", "/api/auth/me", &http.Cookie{Name: cookieName, Value: fresh}, nil)
	if rec.Code != 200 {
		t.Fatalf("fresh token should work, got %d", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			t.Error("fresh token must NOT be renewed")
		}
	}
}

func TestChangePasswordRevokesSessions(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)

	cases := []struct {
		old, new string
		want     int
	}{
		{"", "longenough", 400},
		{"wrong-old", "longenough", 401},
		{f.password, "short", 400},
	}
	for _, c := range cases {
		rec := f.do(t, "POST", "/api/auth/password", cookie, map[string]string{"old_password": c.old, "new_password": c.new})
		if rec.Code != c.want {
			t.Errorf("change(%q→%q): got %d want %d", c.old, c.new, rec.Code, c.want)
		}
	}

	rec := f.do(t, "POST", "/api/auth/password", cookie, map[string]string{"old_password": f.password, "new_password": "brand-new-pw"})
	if rec.Code != 200 {
		t.Fatalf("valid change failed: %d %s", rec.Code, rec.Body.String())
	}
	if me := f.do(t, "GET", "/api/auth/me", cookie, nil); me.Code != 401 {
		t.Error("all sessions must be revoked by hmac key rotation")
	}
	f.password = "brand-new-pw"
	f.login(t)
}

func validDTO() settingsDTO {
	return settingsDTO{
		DefaultUpstream:            "",
		NoMarkerPolicy:             "default_session",
		MarkerPathParts:            []string{"/v1/chat/completions"},
		MarkerHeaders:              []string{"Authorization"},
		HashSalt:                   "salt-value",
		SIDLen:                     16,
		SessionTTLMin:              30,
		SaltRotateFailureThreshold: 2,
		LogRetentionDays:           30,
		ACLWhitelist:               []string{},
		ACLBlacklist:               []string{},
		BlockPrivateTargets:        boolPtr(true),
	}
}

func TestSettingsPutValidation(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)

	cases := []struct {
		name       string
		mutate     func(*settingsDTO)
		wantStatus int
		wantErr    string
	}{
		{"listen_auth missing colon", func(d *settingsDTO) { d.ListenAuth = "justuser" }, 400, "invalid_listen_auth"},
		{"listen_auth empty pass", func(d *settingsDTO) { d.ListenAuth = "user:" }, 400, "invalid_listen_auth"},
		{"bad policy", func(d *settingsDTO) { d.NoMarkerPolicy = "yolo" }, 400, "invalid_policy"},
		{"empty path parts is ok, but empty headers is not", func(d *settingsDTO) { d.MarkerPathParts = nil; d.MarkerHeaders = nil }, 400, "invalid_rules"},
		{"part needs slash", func(d *settingsDTO) { d.MarkerPathParts = []string{"v1/x"} }, 400, "invalid_rules"},
		{"empty headers", func(d *settingsDTO) { d.MarkerHeaders = []string{} }, 400, "invalid_rules"},
		{"empty salt", func(d *settingsDTO) { d.HashSalt = "" }, 400, "invalid_salt"},
		{"sid too small", func(d *settingsDTO) { d.SIDLen = 3 }, 400, "invalid_sidlen"},
		{"sid too big", func(d *settingsDTO) { d.SIDLen = 65 }, 400, "invalid_sidlen"},
		{"negative ttl", func(d *settingsDTO) { d.SessionTTLMin = -1 }, 400, "invalid_ttl"},
		{"ttl over week", func(d *settingsDTO) { d.SessionTTLMin = 10081 }, 400, "invalid_ttl"},
		{"rotation threshold over maximum", func(d *settingsDTO) { d.SaltRotateFailureThreshold = 101 }, 400, "invalid_salt_rotate_failure_threshold"},
		{"retention zero", func(d *settingsDTO) { d.LogRetentionDays = 0 }, 400, "invalid_retention"},
		{"bad acl entry", func(d *settingsDTO) { d.ACLBlacklist = []string{"10.0.0.0/33"} }, 400, "invalid_acl"},
		{"unknown default upstream", func(d *settingsDTO) { d.DefaultUpstream = "ghost" }, 409, "unknown_upstream"},
		// 监听 TLS：半配与坏文件必须在保存时被拒
		{"ingress cert without key", func(d *settingsDTO) { d.ListenTLSCert = "/tmp/x.pem" }, 400, "invalid_tls_pair"},
		{"admin key without cert", func(d *settingsDTO) { d.AdminTLSKey = "/tmp/x.key" }, 400, "invalid_tls_pair"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dto := validDTO()
			c.mutate(&dto)
			rec := f.do(t, "PUT", "/api/settings", cookie, dto)
			if rec.Code != c.wantStatus {
				t.Errorf("got %d want %d (%s)", rec.Code, c.wantStatus, rec.Body.String())
			}
			e := decode[struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}](t, rec)
			if e.Error.Code != c.wantErr {
				t.Errorf("error code=%q want %q", e.Error.Code, c.wantErr)
			}
		})
	}
}

func TestSettingsPutAppliesHotReload(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)

	dto := validDTO()
	dto.ListenAuth = "admin:s3cret"
	dto.ACLWhitelist = []string{"*.openai.com"}
	rec := f.do(t, "PUT", "/api/settings", cookie, dto)
	if rec.Code != 200 {
		t.Fatalf("put failed: %d %s", rec.Code, rec.Body.String())
	}
	snap := f.holder.Current()
	if snap.ListenAuth != "admin:s3cret" {
		t.Errorf("snapshot listen_auth=%q", snap.ListenAuth)
	}
	if !snap.ACLAllowed("api.openai.com") || snap.ACLAllowed("evil.com") || !snap.ACLIntercept("api.openai.com") || snap.ACLIntercept("evil.com") {
		t.Error("acl whitelist access rule not applied to live snapshot")
	}
	if f.st == nil {
		t.Fatal("store lost")
	}
	m, _ := f.st.AllSettings(context.Background())
	if m["log_retention_days"] != "30" || m["metrics_enabled"] != "false" {
		t.Errorf("ops settings not persisted: %v", m)
	}

	get := f.do(t, "GET", "/api/settings", cookie, nil)
	if !strings.Contains(get.Body.String(), `"listen_auth":"admin:s3cret"`) {
		t.Errorf("listen_auth must echo the configured password: %s", get.Body.String())
	}
	if !strings.Contains(get.Body.String(), `"acl_whitelist":["*.openai.com"]`) {
		t.Errorf("whitelist echo missing: %s", get.Body.String())
	}
}

func TestSettingsTLSPathRestartFlag(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)

	dto := validDTO()
	rec := f.do(t, "PUT", "/api/settings", cookie, dto)
	out := decode[struct {
		RestartRequired bool `json:"restart_required"`
	}](t, rec)
	if out.RestartRequired {
		t.Error("unchanged tls paths must not require restart")
	}

	cp, kp := genCertPair(t, t.TempDir(), "rr")
	dto.AdminTLSCert, dto.AdminTLSKey = cp, kp
	rec = f.do(t, "PUT", "/api/settings", cookie, dto)
	if rec.Code != 200 {
		t.Fatalf("valid pair must save: %d %s", rec.Code, rec.Body.String())
	}
	out = decode[struct {
		RestartRequired bool `json:"restart_required"`
	}](t, rec)
	if !out.RestartRequired {
		t.Error("changed tls paths must flag restart_required")
	}
}

// 监听地址已移出设置接口：GET 按本次请求的 Host 和当前接入 TLS 状态返回
// 可直接复制的完整接入地址；启用入站认证时额外提供含凭据版本。
func TestSettingsIngressURLEcho(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)

	get := f.do(t, "GET", "/api/settings", cookie, nil)
	out := decode[struct {
		IngressURL     string `json:"ingress_url"`
		IngressURLAuth string `json:"ingress_url_auth"`
	}](t, get)
	const reqHost = "example.com" // httptest.NewRequest 默认 Host
	if out.IngressURL != "http://"+reqHost+":55666" {
		t.Errorf("ingress_url = %q, want %q（回显必须带接入监听端口）", out.IngressURL, "http://"+reqHost+":55666")
	}
	if out.IngressURLAuth != "" {
		t.Errorf("auth disabled: ingress_url_auth must be empty, got %q", out.IngressURLAuth)
	}

	dto := validDTO()
	dto.ListenAuth = "alice:s3cret"
	if rec := f.do(t, "PUT", "/api/settings", cookie, dto); rec.Code != 200 {
		t.Fatalf("save auth: %d %s", rec.Code, rec.Body.String())
	}
	get2 := f.do(t, "GET", "/api/settings", cookie, nil)
	out2 := decode[struct {
		IngressURL     string `json:"ingress_url"`
		IngressURLAuth string `json:"ingress_url_auth"`
	}](t, get2)
	want := "http://alice:s3cret@" + reqHost + ":55666"
	if out2.IngressURLAuth != want {
		t.Errorf("ingress_url_auth = %q, want %q", out2.IngressURLAuth, want)
	}
}

func TestSettingsIngressURLUsesListenTLSNotAdminTLS(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)
	req := httptest.NewRequest(http.MethodGet, "https://admin.example.com/api/settings", nil)
	req.Host = "admin.example.com:55667"
	req.TLS = &tls.ConnectionState{}
	req.AddCookie(cookie)

	getURL := func() string {
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("get settings: %d %s", rec.Code, rec.Body.String())
		}
		return decode[struct {
			IngressURL string `json:"ingress_url"`
		}](t, rec).IngressURL
	}

	if got := getURL(); got != "http://admin.example.com:55666" {
		t.Errorf("HTTPS admin must not advertise an HTTPS ingress: got %q", got)
	}
	f.api.d.IngressTLS = true
	if got := getURL(); got != "https://admin.example.com:55666" {
		t.Errorf("TLS ingress must advertise HTTPS regardless of admin request: got %q", got)
	}
}

func TestMergeAuthCompatibility(t *testing.T) {
	cases := []struct{ old, in, want string }{
		{"admin:real", "admin:____", "admin:real"},
		{"admin:real", "admin:__unchanged__", "admin:real"},
		{"admin:real", "admin:newpw", "admin:newpw"},
		{"admin:real", "", ""},
		{"", "user:pw", "user:pw"},
		{"weird:no-colon-in-new", "u:p", "u:p"},
	}
	for _, c := range cases {
		if got := mergeAuth(c.old, c.in); got != c.want {
			t.Errorf("mergeAuth(%q,%q)=%q want %q", c.old, c.in, got, c.want)
		}
	}
}

func TestResetSalt(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)
	oldSalt := f.holder.Current().Salt

	rec := f.do(t, "POST", "/api/settings/reset-salt", cookie, nil)
	if rec.Code != 200 {
		t.Fatalf("reset-salt: %d", rec.Code)
	}
	out := decode[struct {
		HashSalt string `json:"hash_salt"`
	}](t, rec)
	if out.HashSalt == "" || out.HashSalt == oldSalt {
		t.Errorf("salt must change: old=%q new=%q", oldSalt, out.HashSalt)
	}
	if f.holder.Current().Salt != out.HashSalt {
		t.Error("live snapshot must carry the new salt immediately")
	}
}

func createUpstreamViaAPI(t *testing.T, f *fixture, cookie *http.Cookie, name, platform, baseURL string, inject any) int64 {
	t.Helper()
	body := map[string]any{"name": name, "platform": platform, "base_url": baseURL, "enabled": true}
	if inject != nil {
		body["inject"] = inject
	}
	rec := f.do(t, "POST", "/api/upstreams", cookie, body)
	if rec.Code != 200 {
		t.Fatalf("create %s: %d %s", name, rec.Code, rec.Body.String())
	}
	return decode[struct {
		ID int64 `json:"id"`
	}](t, rec).ID
}

func TestUpstreamListMasksPasswords(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)
	createUpstreamViaAPI(t, f, cookie, "homeus", "resin", "socks5://Default:topsecret@resin:2260", nil)
	createUpstreamViaAPI(t, f, cookie, "gen", "generic", "http://u:genpass@gw:8080",
		map[string]string{"username_template": "{user}-{sid}", "password": "injectpw"})

	rec := f.do(t, "GET", "/api/upstreams", cookie, nil)
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "topsecret") || strings.Contains(body, "genpass") || strings.Contains(body, "injectpw") {
		t.Fatalf("plaintext password leaked in listing: %s", body)
	}
	if !strings.Contains(body, "socks5://Default:____@resin:2260") {
		t.Errorf("base_url not masked correctly: %s", body)
	}
	if !strings.Contains(body, `"password":"____"`) {
		t.Errorf("inject password not masked: %s", body)
	}
}

func TestUpstreamCreateValidation(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)

	if _, err := f.do(t, "POST", "/api/upstreams", cookie, map[string]string{"name": "x"}), error(nil); err != nil {
		t.Fatal(err)
	} else if rec := f.do(t, "POST", "/api/upstreams", cookie, map[string]string{"name": "x"}); rec.Code != 400 {
		t.Errorf("missing base_url: got %d want 400", rec.Code)
	}

	createUpstreamViaAPI(t, f, cookie, "dup", "resin", "socks5://Default:t@r:1", nil)
	if rec := f.do(t, "POST", "/api/upstreams", cookie, map[string]string{
		"name": "dup", "platform": "resin", "base_url": "socks5://Other:t@r:2"}); rec.Code != 409 {
		t.Errorf("duplicate name: got %d want 409", rec.Code)
	}

	badCases := []struct{ platform, url string }{
		{"decodo", "http://alice:p@gate.decodo.com:7000"},
		{"resin", "socks5://:t@r:1"},
		{"nosuch", "http://a:b@h:1"},
		{"resin", "ftp://D:t@r:1"},
		{"generic", "http://a:b@h:1"},
		{"generic", "http://a:b@h:1"},
	}
	injects := []string{"", "", "", "", "", `{"username_template":"{user}-{bogus}"}`}
	for i, c := range badCases {
		rec := f.do(t, "POST", "/api/upstreams", cookie, map[string]any{
			"name": fmt.Sprintf("bad%d", i), "platform": c.platform, "base_url": c.url, "inject": injects[i],
		})
		if rec.Code != 400 {
			t.Errorf("case %d (%s,%s): got %d want 400 validation_failed", i, c.platform, c.url, rec.Code)
		}
	}
}

func TestUpstreamUpdateKeepsMaskedPassword(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)
	id := createUpstreamViaAPI(t, f, cookie, "homeus", "resin", "socks5://Default:realmask@resin:2260", nil)

	rec := f.do(t, "PUT", fmt.Sprintf("/api/upstreams/%d", id), cookie, map[string]any{
		"name": "homeus", "platform": "resin",
		"base_url": "socks5://Default:____@resin:2260", // 密码段打码回传
		"enabled":  true,
	})
	if rec.Code != 200 {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	rows, _ := f.st.ListUpstreams(context.Background())
	if rows[0].BaseURL != "socks5://Default:realmask@resin:2260" {
		t.Errorf("masked password must resolve to stored secret, got %q", rows[0].BaseURL)
	}

	rec = f.do(t, "PUT", fmt.Sprintf("/api/upstreams/%d", id), cookie, map[string]any{
		"base_url": "socks5://Default:freshmask@resin:2260", "enabled": true,
	})
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	rows, _ = f.st.ListUpstreams(context.Background())
	if rows[0].BaseURL != "socks5://Default:freshmask@resin:2260" {
		t.Errorf("explicit new password must win, got %q", rows[0].BaseURL)
	}

	if rec := f.do(t, "PUT", "/api/upstreams/424242", cookie, map[string]string{"name": "ghost"}); rec.Code != 404 {
		t.Errorf("missing id: got %d want 404", rec.Code)
	}
	if rec := f.do(t, "PUT", "/api/upstreams/badid", cookie, map[string]string{}); rec.Code != 400 {
		t.Errorf("bad id: got %d want 400", rec.Code)
	}
}

func TestGenericInjectPasswordMerge(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)
	id := createUpstreamViaAPI(t, f, cookie, "gen", "generic", "http://u:oldpw@gw:8080",
		map[string]string{"username_template": "{user}-{sid}", "password": "oldpw"})

	rec := f.do(t, "PUT", fmt.Sprintf("/api/upstreams/%d", id), cookie, map[string]any{
		"name": "gen", "platform": "generic", "base_url": "http://u:____@gw:8080",
		"inject":  map[string]string{"username_template": "{user}-{sid}", "password": "__unchanged__"},
		"enabled": true,
	})
	if rec.Code != 200 {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	rows, _ := f.st.ListUpstreams(context.Background())
	if !strings.Contains(rows[0].Inject.String, `"password":"oldpw"`) {
		t.Errorf("inject placeholder must merge back to real password: %s", rows[0].Inject.String)
	}
	if rows[0].BaseURL != "http://u:oldpw@gw:8080" {
		t.Errorf("url password merge broken: %q", rows[0].BaseURL)
	}
}

func TestUpstreamDefaultGuards(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)
	idA := createUpstreamViaAPI(t, f, cookie, "aaa", "resin", "socks5://A:t@a:1", nil)
	createUpstreamViaAPI(t, f, cookie, "bbb", "resin", "socks5://B:t@b:2", nil)

	settings.SaveSnapshot(context.Background(), f.st, func() settings.Snapshot {
		s := f.holder.Current()
		s.DefaultUpstream = "aaa"
		return s
	}())
	{
		s := f.holder.Current()
		s.DefaultUpstream = "aaa"
		f.holder.Set(s)
	}

	if rec := f.do(t, "DELETE", fmt.Sprintf("/api/upstreams/%d", idA), cookie, nil); rec.Code != 409 {
		t.Errorf("delete default: got %d want 409", rec.Code)
	}
	if rec := f.do(t, "PUT", fmt.Sprintf("/api/upstreams/%d", idA), cookie, map[string]any{"name": "aaa", "enabled": false}); rec.Code != 409 {
		t.Errorf("disable default: got %d want 409", rec.Code)
	}
	if rec := f.do(t, "POST", "/api/upstreams/424242/default", cookie, nil); rec.Code != 404 {
		t.Errorf("default on ghost id: got %d want 404", rec.Code)
	}

	rows, _ := f.st.ListUpstreams(context.Background())
	var idB int64
	for _, rw := range rows {
		if rw.Name == "bbb" {
			idB = rw.ID
		}
	}
	if rec := f.do(t, "POST", fmt.Sprintf("/api/upstreams/%d/default", idB), cookie, nil); rec.Code != 200 {
		t.Fatalf("switch default: %d %s", rec.Code, rec.Body.String())
	}
	if f.holder.Current().DefaultUpstream != "bbb" {
		t.Errorf("holder default=%q want bbb", f.holder.Current().DefaultUpstream)
	}
	if tbl := f.lastTable(); tbl == nil || tbl.DefaultName() != "bbb" {
		t.Error("upstream table snapshot not swapped after default switch")
	}

	if rec := f.do(t, "DELETE", fmt.Sprintf("/api/upstreams/%d", idA), cookie, nil); rec.Code != 200 {
		t.Errorf("delete non-default now allowed: %d", rec.Code)
	}
	if n, _ := f.st.CountUpstreams(context.Background()); n != 1 {
		t.Errorf("count after delete=%d", n)
	}
}

func TestDefaultRenameCascades(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)
	id := createUpstreamViaAPI(t, f, cookie, "primary", "resin", "socks5://P:t@p:1", nil)
	s := f.holder.Current()
	s.DefaultUpstream = "primary"
	f.holder.Set(s)
	settings.SaveSnapshot(context.Background(), f.st, s)

	rec := f.do(t, "PUT", fmt.Sprintf("/api/upstreams/%d", id), cookie, map[string]any{
		"name": "renamed", "platform": "resin", "base_url": "socks5://P:t@p:1", "enabled": true,
	})
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if got := f.holder.Current().DefaultUpstream; got != "renamed" {
		t.Errorf("default_upstream must follow rename, got %q", got)
	}
	m, _ := f.st.AllSettings(context.Background())
	if m["default_upstream"] != `"renamed"` {
		t.Errorf("persisted default=%q", m["default_upstream"])
	}
}

func TestDisabledEntryCannotBecomeDefault(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)
	rec := f.do(t, "POST", "/api/upstreams", cookie, map[string]any{
		"name": "off", "platform": "resin", "base_url": "socks5://O:t@o:1", "enabled": false})
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	id := decode[struct {
		ID int64 `json:"id"`
	}](t, rec).ID
	if rec := f.do(t, "POST", fmt.Sprintf("/api/upstreams/%d/default", id), cookie, nil); rec.Code != 409 {
		t.Errorf("disabled entry as default: got %d want 409", rec.Code)
	}
}

func TestTestUpstreamVerifiesInjectedCredential(t *testing.T) {
	var mu sync.Mutex
	var connectHost string
	var capturedAuth string
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "expect CONNECT", 405)
			return
		}
		mu.Lock()
		connectHost = r.Host
		capturedAuth = r.Header.Get("Proxy-Authorization")
		mu.Unlock()
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("cannot hijack")
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer mockUpstream.Close()

	f := newFixture(t)
	cookie := f.login(t)
	mockUpstreamHost := strings.TrimPrefix(mockUpstream.URL, "http://")
	id := createUpstreamViaAPI(t, f, cookie, "viaroute", "resin",
		"http://Default:tok@"+mockUpstreamHost, nil)

	rec := f.do(t, "POST", fmt.Sprintf("/api/upstreams/%d/test", id), cookie, nil)
	if rec.Code != 200 {
		t.Fatalf("test-upstream: %d %s", rec.Code, rec.Body.String())
	}
	out := decode[struct {
		EgressIP string `json:"egress_ip"`
		DurMS    int64  `json:"dur_ms"`
		Err      string `json:"err"`
	}](t, rec)
	if out.EgressIP != "" || out.Err == "" {
		t.Errorf("mock upstream cannot complete TLS: %+v", out)
	}

	mu.Lock()
	defer mu.Unlock()
	if connectHost != "api.ipify.org:443" {
		t.Errorf("CONNECT target=%q want api.ipify.org:443", connectHost)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("Default.healthcheck:tok"))
	if capturedAuth != want {
		t.Errorf("Proxy-Authorization=%q want %q", capturedAuth, want)
	}
}

// P2-4: 超大页码不能在 (page-1)*pageSize 溢出后引发切片 panic。
func TestAcctMapPaginationHandlesHugePage(t *testing.T) {
	f := newFixture(t)
	reg := acctmap.New()
	reg.Reload([]acctmap.Entry{{
		Platform: "openai", Account: "user@example.com", AtFp: "at-fp",
		Source: acctmap.SourcePush, SourceType: "manual",
	}})
	f.api.d.AcctMap = reg
	cookie := f.login(t)

	rec := f.do(t, http.MethodGet,
		fmt.Sprintf("/api/acctmap?page=%d&page_size=500", math.MaxInt), cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("huge page: HTTP %d %s", rec.Code, rec.Body.String())
	}
	out := decode[struct {
		Items []any `json:"items"`
		Total int   `json:"total"`
	}](t, rec)
	if out.Total != 1 || len(out.Items) != 0 {
		t.Fatalf("huge page result=%+v, want total=1 and empty items", out)
	}
}

// 绑定筛选按 (platform, account) 关联 acct_egress（与来源实例无关）；
// DELETE /api/acctegress 一键清空全部绑定行。
func TestAcctMapBindingFilterAndClear(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	reg := acctmap.New()
	reg.Reload([]acctmap.Entry{
		{Platform: "openai", Account: "bound@example.com", AtFp: "fp-a", Source: acctmap.SourcePush, SourceType: "manual"},
		{Platform: "openai", Account: "free@example.com", AtFp: "fp-b", Source: acctmap.SourcePush, SourceType: "manual"},
	})
	f.api.d.AcctMap = reg
	// ReplaceAccountBinding 的 GC 会清掉 acct_map 中不存在的账号的绑定，先落库再绑。
	if err := f.st.ReplaceAccountSnapshot(ctx, "openai", "bound@example.com", "manual",
		store.AcctUpsert{Platform: "openai", Account: "bound@example.com", AtFP: "fp-a", AtHint: "…fp-a"}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.ReplaceAccountBinding(ctx, "openai", "bound@example.com", "sticky", []int64{7}); err != nil {
		t.Fatal(err)
	}
	cookie := f.login(t)

	type page struct {
		Total int `json:"total"`
		Items []struct {
			Account string `json:"account"`
		} `json:"items"`
	}
	get := func(query string) page {
		rec := f.do(t, http.MethodGet, "/api/acctmap"+query, cookie, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: HTTP %d %s", query, rec.Code, rec.Body.String())
		}
		return decode[page](t, rec)
	}
	if p := get("?binding=bound"); p.Total != 1 || len(p.Items) != 1 || p.Items[0].Account != "bound@example.com" {
		t.Fatalf("bound filter: %+v", p)
	}
	if p := get("?binding=unbound"); p.Total != 1 || len(p.Items) != 1 || p.Items[0].Account != "free@example.com" {
		t.Fatalf("unbound filter: %+v", p)
	}
	if p := get("?binding=bogus"); p.Total != 2 {
		t.Fatalf("unknown binding value must be ignored: %+v", p)
	}

	rec := f.do(t, http.MethodDelete, "/api/acctegress", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear bindings: HTTP %d %s", rec.Code, rec.Body.String())
	}
	if rows, err := f.st.ListAcctEgress(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("after clear: rows=%d err=%v", len(rows), err)
	}
	if p := get("?binding=bound"); p.Total != 0 {
		t.Fatalf("bound filter after clear: %+v", p)
	}
}

func TestLogsEndpoints(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan store.LogEntry, 16)
	go f.st.RunLogWriter(ctx, ch)
	ttfb := int64(3)
	ch <- store.LogEntry{Ts: time.Now().UnixMilli(), Method: "POST", Host: "api.openai.com",
		Path: "/v1/chat/completions", Status: 200, DurMS: 5, TTFBMS: &ttfb, BytesOut: 10, HasMarker: true,
		AccountFP: "aaaa1111aaaa1111", Upstream: "homeus"}
	ch <- store.LogEntry{Ts: time.Now().UnixMilli(), Method: "GET", Host: "web.example",
		Path: "/", Status: 503, DurMS: 1, BytesOut: 0, HasMarker: false, AccountFP: "default", Upstream: "direct"}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rec := f.do(t, "GET", "/api/logs", cookie, nil)
		out := decode[struct {
			Total int64 `json:"total"`
		}](t, rec)
		if out.Total == 2 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	filtered := f.do(t, "GET", `/api/logs?q=openai&class=2xx&account=aaaa1111aaaa1111&upstream=homeus`, cookie, nil)
	out := decode[struct {
		Items []struct {
			Host      string `json:"host"`
			AccountFP string `json:"account_fp"`
			TTFBMS    *int64 `json:"ttfb_ms"`
		} `json:"items"`
		Total    int64 `json:"total"`
		Page     int   `json:"page"`
		PageSize int   `json:"page_size"`
	}](t, filtered)
	if out.Total != 1 || len(out.Items) != 1 || out.Items[0].AccountFP != "aaaa1111aaaa1111" {
		t.Errorf("filter mismatch: %+v", out)
	}
	if out.Items[0].TTFBMS == nil || *out.Items[0].TTFBMS != ttfb {
		t.Errorf("audit TTFB=%v, want %d", out.Items[0].TTFBMS, ttfb)
	}
	paged := f.do(t, "GET", `/api/logs?page=1&page_size=1`, cookie, nil)
	po := decode[struct {
		Page     int   `json:"page"`
		PageSize int   `json:"page_size"`
		Total    int64 `json:"total"`
	}](t, paged)
	if po.Page != 1 || po.PageSize != 1 || po.Total != 2 {
		t.Errorf("pagination envelope mismatch: %+v", po)
	}

	if rec := f.do(t, "DELETE", "/api/logs", cookie, nil); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	final := f.do(t, "GET", "/api/logs", cookie, nil)
	ft := decode[struct {
		Total int64 `json:"total"`
	}](t, final)
	if ft.Total != 0 {
		t.Errorf("clear failed: total=%d", ft.Total)
	}
	cancel()
}

func TestCACertDownloadEndpoints(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)

	rec := f.do(t, "GET", "/api/ca.pem", cookie, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("ca.pem: %d", rec.Code)
	}
	rec = f.do(t, "GET", "/api/ca.crt", cookie, nil)
	if rec.Code != 200 || len(rec.Body.Bytes()) < 100 {
		t.Fatalf("ca.crt: %d len=%d", rec.Code, len(rec.Body.Bytes()))
	}
}

func TestMetricsGate(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)

	if rec := f.do(t, "GET", "/metrics", cookie, nil); rec.Code != 404 {
		t.Fatalf("disabled metrics: got %d want 404", rec.Code)
	}
	if err := f.st.SetSetting(context.Background(), "metrics_enabled", "true"); err != nil {
		t.Fatal(err)
	}
	f.api.d.Metrics.Inc("probe_total", "probe", nil)
	rec := f.do(t, "GET", "/metrics", cookie, nil)
	if rec.Code != 200 {
		t.Fatalf("enabled metrics: %d", rec.Code)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") ||
		!strings.Contains(rec.Body.String(), "probe_total 1") {
		t.Errorf("metrics body unexpected: %s", rec.Body.String())
	}
}
