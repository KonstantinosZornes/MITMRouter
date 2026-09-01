package acctmap

import (
	"net/http/httptest"
	"testing"
)

func TestNormalizeCred(t *testing.T) {
	cases := []struct{ in, want string }{
		{"sk-abc", "sk-abc"},
		{"  sk-abc  ", "sk-abc"},
		{`"sk-abc"`, "sk-abc"},
		{"'sk-abc'", "sk-abc"},
		{"Bearer sk-abc", "sk-abc"},
		{"bearer sk-abc", "sk-abc"},
		{"BEARER\tsk-abc", "sk-abc"},
		{"Basic dXNlcjpwYXNz", "Basic dXNlcjpwYXNz"},
		{"token abc", "abc"},
	}
	for _, c := range cases {
		if got := NormalizeCred(c.in); got != c.want {
			t.Errorf("NormalizeCred(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestFingerprintStable(t *testing.T) {
	a := Fingerprint("openai", NormalizeCred("Bearer sk-x"))
	b := Fingerprint("openai", NormalizeCred(` "sk-x" `))
	if a != b {
		t.Fatalf("same key different forms must collide into one fp")
	}
	if a == Fingerprint("anthropic", NormalizeCred("sk-x")) {
		t.Fatalf("platform must participate in fp")
	}
}

func TestPlatformForHost(t *testing.T) {
	cases := []struct{ host, want string }{
		{"chatgpt.com", "openai"},
		{"Chatgpt.Com:443", "openai"},
		{"api.openai.com", "openai"},
		{"api.anthropic.com", "anthropic"},
		{"claude.ai", "anthropic"},
		{"generativelanguage.googleapis.com", "gemini"},
		{"cloudcode-pa.googleapis.com", "gemini"},
		{"us-east5-aiplatform.googleapis.com", "gemini"},
		{"api.x.ai", "grok"},
		{"cli-chat.grok.com", "grok"},
		{"api.kimi.com", "kimi"},
		{"api.moonshot.cn", "kimi"},
		{"api.deepseek.com", "deepseek"},
		{"open.bigmodel.cn", "glm"},
		{"dashscope.aliyuncs.com", "qwen"},
		{"apis.iflow.cn", "iflow"},
		{"evil-chatgpt.com.attacker.io", ""},
		{"example.com", ""},
	}
	for _, c := range cases {
		if got := PlatformForHost(c.host); got != c.want {
			t.Errorf("PlatformForHost(%q)=%q want %q", c.host, got, c.want)
		}
	}
}

func TestRegistryReloadAndLookup(t *testing.T) {
	r := New()
	atFP := Fingerprint("openai", "sk-a")
	rtFP := Fingerprint("openai", "rt-a")
	r.Reload([]Entry{{
		Platform: "openai", Account: "user@example.com",
		AtFp: atFP, RtFp: rtFP,
		Source: SourcePush, SourceType: SourceTypeCLIProxyAPI,
	}})
	e, ok := r.Lookup(atFP)
	if !ok || e.Account != "user@example.com" || e.AtFp != atFP {
		t.Fatalf("AT lookup miss: %v %v", e, ok)
	}
	e, ok = r.Lookup(rtFP)
	if !ok || e.Account != "user@example.com" || e.RtFp != rtFP {
		t.Fatalf("RT lookup miss: %v %v", e, ok)
	}
	if r.Len() != 1 {
		t.Fatalf("len=%d", r.Len())
	}

	// 同账号被两个来源实例收录 → 两行；任一指纹仍可命中
	r.Reload([]Entry{
		{Platform: "openai", Account: "user@example.com",
			AtFp: atFP, RtFp: rtFP, Source: "src:1", SourceType: SourceTypeCLIProxyAPI},
		{Platform: "openai", Account: "user@example.com",
			AtFp: atFP, RtFp: rtFP, Source: "src:2", SourceType: SourceTypeSub2API},
	})
	if r.Len() != 2 {
		t.Fatalf("rows=%d, want 2 (one per source instance)", r.Len())
	}
	if _, ok = r.Lookup(atFP); !ok {
		t.Fatal("AT fp must resolve across per-source rows")
	}
	if _, ok = r.Lookup(rtFP); !ok {
		t.Fatal("RT fp must resolve across per-source rows")
	}

	r.Reload(nil) // 快照对齐清空
	if _, ok = r.Lookup(atFP); ok {
		t.Fatal("entry must be gone after reload")
	}
	if _, ok = r.Lookup(rtFP); ok {
		t.Fatal("rt entry must be gone after reload")
	}
}

func TestExtractCredCarriers(t *testing.T) {
	req := httptest.NewRequest("POST", "https://x/v1beta/models/gemini:generateContent?key=AIzaKey", nil)
	if got := ExtractCred(req); got != "AIzaKey" {
		t.Errorf("query key: got %q", got)
	}

	req = httptest.NewRequest("POST", "https://x/", nil)
	req.Header.Set("x-goog-api-key", " AIzaH ")
	if got := ExtractCred(req); got != "AIzaH" {
		t.Errorf("goog header: got %q", got)
	}

	req = httptest.NewRequest("POST", "https://x/", nil)
	req.Header.Set("Authorization", "Bearer ya29.abc")
	if got := ExtractCred(req); got != "ya29.abc" {
		t.Errorf("bearer: got %q", got)
	}

	req = httptest.NewRequest("POST", "https://x/", nil)
	req.Header.Set("Authorization", "Basic <REDACTED>")
	if got := ExtractCred(req); got != "" {
		t.Errorf("basic authorization must not be treated as a platform credential, got %q", got)
	}

	req = httptest.NewRequest("POST", "https://x/?k=notkey", nil)
	if got := ExtractCred(req); got != "" {
		t.Errorf("no carrier must be empty, got %q", got)
	}
}
