package marker

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func mkReq(path string, headers map[string]string) *http.Request {
	r := httptest.NewRequest("POST", "https://api.openai.com"+path, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// 默认推荐形态：不配置路径片段 = 所有 URL 同一规则。
var defaultRules = Rules{
	PathParts: nil,
	Headers:   []string{"Authorization", "x-api-key", "api-key"},
}

func TestExtractPathAndHeaders(t *testing.T) {
	cases := []struct {
		name    string
		rules   *Rules
		path    string
		headers map[string]string
		want    string
	}{
		{
			name:    "empty parts matches any path",
			path:    "/any/random/path",
			headers: map[string]string{"Authorization": "Bearer sk-abc"},
			want:    "sk-abc",
		},
		{
			name:    "part at path start",
			rules:   &Rules{PathParts: []string{"/v1"}, Headers: []string{"Authorization"}},
			path:    "/v1/chat/completions",
			headers: map[string]string{"Authorization": "Bearer sk-abc"},
			want:    "sk-abc",
		},
		{
			name:    "part in path middle",
			rules:   &Rules{PathParts: []string{"/v1/messages"}, Headers: []string{"Authorization"}},
			path:    "/anthropic/v1/messages?beta=true",
			headers: map[string]string{"Authorization": "Bearer sk-mid"},
			want:    "sk-mid",
		},
		{
			name:    "part as last segment",
			rules:   &Rules{PathParts: []string{"/responses"}, Headers: []string{"Authorization"}},
			path:    "/backend-api/codex/responses",
			headers: map[string]string{"Authorization": "Bearer sk-lastseg"},
			want:    "sk-lastseg",
		},
		{
			name:    "plain contains matches partial suffix too",
			rules:   &Rules{PathParts: []string{"/v1/messages"}, Headers: []string{"Authorization"}},
			path:    "/v1/messagesX",
			headers: map[string]string{"Authorization": "Bearer sk-abc"},
			want:    "sk-abc",
		},
		{
			name:    "no listed part means no extraction",
			rules:   &Rules{PathParts: []string{"/v1/chat/completions"}, Headers: []string{"Authorization"}},
			path:    "/v1/embeddings",
			headers: map[string]string{"Authorization": "Bearer sk-abc"},
			want:    "",
		},
		{
			name:    "bearer scheme case-insensitive and value trimmed",
			path:    "/v1/responses",
			headers: map[string]string{"Authorization": "bearer   sk-low  "},
			want:    "sk-low",
		},
		{
			name:    "non-bearer authorization ignored falls through",
			path:    "/v1/chat/completions",
			headers: map[string]string{"Authorization": "Basic dXNlcjpwYXNz", "x-api-key": "sk-fb"},
			want:    "sk-fb",
		},
		{
			name:    "bearer without space ignored",
			path:    "/v1/chat/completions",
			headers: map[string]string{"Authorization": "Bearersk-x"},
			want:    "",
		},
		{
			name:    "x-api-key raw value trimmed",
			path:    "/v1/chat/completions",
			headers: map[string]string{"x-api-key": "  sk-xak  "},
			want:    "sk-xak",
		},
		{
			name:    "api-key used as last resort",
			path:    "/v1/completions",
			headers: map[string]string{"api-key": "sk-last"},
			want:    "sk-last",
		},
		{
			name:    "header priority follows configured order",
			path:    "/v1/chat/completions",
			headers: map[string]string{"Authorization": "Bearer sk-first", "x-api-key": "sk-second"},
			want:    "sk-first",
		},
		{
			name: "no headers at all",
			path: "/v1/chat/completions",
			want: "",
		},
		{
			name:    "empty header values skipped",
			path:    "/v1/chat/completions",
			headers: map[string]string{"Authorization": "", "x-api-key": ""},
			want:    "",
		},
		{
			name:    "empty header list extracts nothing",
			rules:   &Rules{PathParts: []string{"/v1"}, Headers: nil},
			path:    "/v1/chat/completions",
			headers: map[string]string{"Authorization": "Bearer sk-abc"},
			want:    "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rules := defaultRules
			if c.rules != nil {
				rules = *c.rules
			}
			if got := Extract(rules, mkReq(c.path, c.headers)); got != c.want {
				t.Errorf("Extract=%q want %q", got, c.want)
			}
		})
	}
}

func TestExtractHeaderCaseInsensitiveNames(t *testing.T) {
	r := httptest.NewRequest("POST", "https://h/v1/chat/completions", nil)
	r.Header.Set("X-API-KEY", "sk-upper")
	if got := Extract(defaultRules, r); got != "sk-upper" {
		t.Errorf("header lookup must be case-insensitive, got %q", got)
	}
}
