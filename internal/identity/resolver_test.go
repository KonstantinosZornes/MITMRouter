package identity

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"mitmrouter/internal/acctmap"
	"mitmrouter/internal/marker"
)

type countingBody struct {
	io.Reader
	reads int
}

func (b *countingBody) Read(p []byte) (int, error) {
	b.reads++
	return b.Reader.Read(p)
}

func (b *countingBody) Close() error { return nil }

func TestResolveGeneralHeaderWinsWithoutReadingBody(t *testing.T) {
	body := &countingBody{Reader: strings.NewReader("refresh_token=rt-body")}
	req, err := http.NewRequest(http.MethodPost, "https://auth.x.ai/oauth2/token", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer header-token")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	got, _ := New().ResolveWithBody(req, "auth.x.ai", Options{
		MarkerRules: marker.Rules{Headers: []string{"Authorization"}},
	})
	if got.Credential != "header-token" || got.RuleID != "platform_carrier" {
		t.Fatalf("resolution=%+v", got)
	}
	if body.reads != 0 {
		t.Fatalf("general header match must not read body, reads=%d", body.reads)
	}
}

func TestResolveCustomCarrierNormalizesCredentialForAccountMap(t *testing.T) {
	const token = "diag-token"
	req, err := http.NewRequest(http.MethodGet, "https://api.openai.com/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Credential", "Bearer "+token)

	reg := acctmap.New()
	reg.Reload([]acctmap.Entry{{
		Platform: "openai",
		Account:  "account@example.com",
		AtFp:     acctmap.Fingerprint("openai", token),
	}})

	got, _ := New().ResolveWithBody(req, "api.openai.com", Options{
		MarkerRules:    marker.Rules{Headers: []string{"X-Credential"}},
		AcctMapEnabled: true,
		AcctMap:        reg,
	})
	if got.Credential != token || !got.Mapped || got.Account != "account@example.com" {
		t.Fatalf("custom carrier must use normalized credential: %+v", got)
	}
}

func TestMatchBodyRuleUsesExactHostAndLongestPath(t *testing.T) {
	rules := []bodyRule{
		{ID: "short", Host: "auth.x.ai", PathKey: "/oauth2", Method: http.MethodPost, ContentType: "application/x-www-form-urlencoded"},
		{ID: "long", Host: "auth.x.ai", PathKey: "/oauth2/token", Method: http.MethodPost, ContentType: "application/x-www-form-urlencoded"},
	}
	req, err := http.NewRequest(http.MethodPost, "https://auth.x.ai/oauth2/token?ignored=query", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	if got := matchBodyRuleFrom(rules, "auth.x.ai", req); got == nil || got.ID != "long" {
		t.Fatalf("longest matching path rule = %+v, want long", got)
	}
	if got := matchBodyRuleFrom(rules, "evil-auth.x.ai", req); got != nil {
		t.Fatalf("similar host must not match: %+v", got)
	}
}

func TestResolveURLMissDoesNotReadBody(t *testing.T) {
	body := &countingBody{Reader: strings.NewReader("refresh_token=rt-body")}
	req, err := http.NewRequest(http.MethodPost, "https://auth.x.ai/not-oauth", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	got, _ := New().ResolveWithBody(req, "auth.x.ai", Options{})
	if got.HasCredential() {
		t.Fatalf("unexpected URL-miss resolution: %+v", got)
	}
	if body.reads != 0 {
		t.Fatalf("URL miss must not read body, reads=%d", body.reads)
	}
}

func TestResolveGrokRefreshMapsAccountAndReplaysBody(t *testing.T) {
	const token = "rt-token"
	values := url.Values{
		"client_id":     {"client"},
		"grant_type":    {"refresh_token"},
		"refresh_token": {token},
	}
	raw := values.Encode()
	req, err := http.NewRequest(http.MethodPost, "https://auth.x.ai/oauth2/token", strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")

	originalBody := req.Body
	reg := acctmap.New()
	reg.Reload([]acctmap.Entry{{
		Platform: "grok", Account: "grok-account@example.com",
		RtFp: acctmap.Fingerprint("grok", token),
	}})

	got, replay := New().ResolveWithBody(req, "auth.x.ai", Options{AcctMapEnabled: true, AcctMap: reg})
	defer replay.Close()
	if req.Body != originalBody {
		t.Fatal("resolver replaced inbound request Body")
	}
	if got.Credential != token || !got.Mapped || got.Account != "grok-account@example.com" || got.RuleID != "grok.oauth.refresh" {
		t.Fatalf("resolution=%+v", got)
	}
	replayed, err := io.ReadAll(replay)
	if err != nil {
		t.Fatal(err)
	}
	if string(replayed) != raw {
		t.Fatalf("body changed after resolution: got %q want %q", replayed, raw)
	}
}

func TestResolveGrokRefreshUnmappedUsesCredential(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://auth.x.ai/oauth2/token", strings.NewReader("refresh_token=rt-unknown"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	got, replay := New().ResolveWithBody(req, "auth.x.ai", Options{AcctMapEnabled: true, AcctMap: acctmap.New()})
	defer replay.Close()
	if got.Credential != "rt-unknown" || got.Mapped || got.Account != "" {
		t.Fatalf("unmapped refresh token must remain credential subject: %+v", got)
	}
}

func TestResolveWithBodyOverLimitReplaysEntireRequest(t *testing.T) {
	raw := bytes.Repeat([]byte("x"), int(defaultBodyLimit+1))
	req, err := http.NewRequest(http.MethodPost, "https://auth.x.ai/oauth2/token", io.NopCloser(bytes.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1 // 模拟 chunked body，必须走前缀 + 原流回放
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	originalBody := req.Body

	got, replay := New().ResolveWithBody(req, "auth.x.ai", Options{})
	defer replay.Close()
	if got.HasCredential() {
		t.Fatalf("oversized body unexpectedly resolved credential: %+v", got)
	}
	if req.Body != originalBody {
		t.Fatal("resolver replaced inbound request Body")
	}
	replayed, err := io.ReadAll(replay)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(replayed, raw) {
		t.Fatalf("oversized body replay changed bytes: got %d want %d", len(replayed), len(raw))
	}
}

func TestSnapshotBodyOverLimitReplaysEntireRequest(t *testing.T) {
	raw := bytes.Repeat([]byte("x"), 128)
	req, err := http.NewRequest(http.MethodPost, "https://auth.x.ai/oauth2/token", io.NopCloser(bytes.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1 // 模拟 chunked body，必须走前缀 + 原流回放

	if _, replay, ok := snapshotBody(req.Body, req.ContentLength, 64); ok {
		t.Fatal("oversized body must not be accepted")
	} else {
		defer replay.Close()
		replayed, err := io.ReadAll(replay)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(replayed, raw) {
			t.Fatalf("oversized body replay changed bytes: got %d want %d", len(replayed), len(raw))
		}
	}
}
