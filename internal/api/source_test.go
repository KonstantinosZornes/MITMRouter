package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mitmrouter/internal/acctmap"
)

type sourceTestSyncer struct {
	kind    string
	baseURL string
	key     string
	calls   int
}

func (s *sourceTestSyncer) TestSource(_ context.Context, kind, baseURL, key string) (string, error) {
	s.kind, s.baseURL, s.key = kind, baseURL, key
	s.calls++
	return "test summary", nil
}

func (s *sourceTestSyncer) Wake(int64) {}

func setupSourceTest(t *testing.T) (*fixture, *sourceTestSyncer, int64, *http.Cookie) {
	t.Helper()
	f := newFixture(t)
	syncer := &sourceTestSyncer{}
	f.api.d.Syncer = syncer
	cookie := f.login(t)
	id, err := f.st.CreateSyncSource(context.Background(), acctmap.SourceKindCLIProxyAPI, "source-test", "http://stored.example", "stored-key", 60, true)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	return f, syncer, id, cookie
}

// P1-7: 同步源 URL 只允许干净的 HTTP(S) 基地址，禁止 URL 凭据、query 和 fragment。
func TestSourceURLValidationRejectsCredentialsAndDecorations(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)
	badURLs := []string{
		"ftp://example.test",
		"https://user:secret@example.test",
		"https://example.test?api_key=secret",
		"https://example.test#secret",
	}
	for i, baseURL := range badURLs {
		rec := f.do(t, http.MethodPost, "/api/sources", cookie, map[string]any{
			"kind": acctmap.SourceKindCLIProxyAPI, "name": fmt.Sprintf("bad-source-%d", i), "base_url": baseURL, "api_key": "key",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("create %q: HTTP %d %s, want 400", baseURL, rec.Code, rec.Body.String())
		}
	}
	if rec := f.do(t, http.MethodGet, "/api/sources", cookie, nil); rec.Code != http.StatusOK {
		t.Fatalf("list sources: HTTP %d", rec.Code)
	} else if body := rec.Body.String(); strings.Contains(body, "secret") || strings.Contains(body, "api_key") {
		t.Fatalf("rejected URL credential/query echoed: %s", body)
	}

	id, err := f.st.CreateSyncSource(context.Background(), acctmap.SourceKindCLIProxyAPI, "update-source", "https://valid.example", "key", 60, true)
	if err != nil {
		t.Fatalf("seed source: %v", err)
	}
	rec := f.do(t, http.MethodPut, fmt.Sprintf("/api/sources/%d", id), cookie, map[string]any{
		"base_url": "https://user:secret@new.example", "api_key": "____",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update with URL credentials: HTTP %d %s, want 400", rec.Code, rec.Body.String())
	}
	row, ok, err := f.st.GetSyncSource(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("read source after rejected update: ok=%v err=%v", ok, err)
	}
	if row.BaseURL != "https://valid.example" {
		t.Fatalf("rejected update changed stored URL to %q", row.BaseURL)
	}
}

// P1-6: source 测试接口必须正确处理不存在源、打码 key、chunked body 和临时参数。
func TestSourceTestEndpointHandlesLifecycleInputs(t *testing.T) {
	t.Run("missing source returns not found", func(t *testing.T) {
		f := newFixture(t)
		f.api.d.Syncer = &sourceTestSyncer{}
		cookie := f.login(t)
		rec := f.do(t, http.MethodPost, "/api/sources/999999/test", cookie, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("missing source: HTTP %d %s, want 404", rec.Code, rec.Body.String())
		}
	})

	t.Run("masked key uses stored key", func(t *testing.T) {
		f, syncer, id, cookie := setupSourceTest(t)
		rec := f.do(t, http.MethodPost, fmt.Sprintf("/api/sources/%d/test", id), cookie, map[string]string{
			"kind": acctmap.SourceKindCLIProxyAPI, "base_url": "http://temporary.example/", "api_key": maskToken,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("masked key test: HTTP %d %s", rec.Code, rec.Body.String())
		}
		if syncer.key != "stored-key" {
			t.Fatalf("key=%q, want stored key", syncer.key)
		}
	})

	t.Run("chunked body is read", func(t *testing.T) {
		f, syncer, id, cookie := setupSourceTest(t)
		body := strings.NewReader(`{"kind":"sub2api","base_url":"http://chunked.example///","api_key":"chunked-key"}`)
		r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/sources/%d/test", id), body)
		r.ContentLength = -1
		r.AddCookie(cookie)
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("chunked body: HTTP %d %s", rec.Code, rec.Body.String())
		}
		if syncer.kind != acctmap.SourceKindSub2API || syncer.baseURL != "http://chunked.example" || syncer.key != "chunked-key" {
			t.Fatalf("got kind=%q base=%q key=%q, want chunked values", syncer.kind, syncer.baseURL, syncer.key)
		}
	})

	t.Run("temporary kind is normalized and validated", func(t *testing.T) {
		f, syncer, id, cookie := setupSourceTest(t)
		rec := f.do(t, http.MethodPost, fmt.Sprintf("/api/sources/%d/test", id), cookie, map[string]string{
			"kind": " " + strings.ToUpper(acctmap.SourceKindCLIProxyAPI) + " ", "base_url": "http://temporary.example///", "api_key": "temporary-key",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("temporary values: HTTP %d %s", rec.Code, rec.Body.String())
		}
		if syncer.kind != acctmap.SourceKindCLIProxyAPI || syncer.baseURL != "http://temporary.example" || syncer.key != "temporary-key" {
			t.Fatalf("got kind=%q base=%q key=%q, want normalized temporary values", syncer.kind, syncer.baseURL, syncer.key)
		}

		rec = f.do(t, http.MethodPost, fmt.Sprintf("/api/sources/%d/test", id), cookie, map[string]string{
			"kind": "unknown", "base_url": "http://temporary.example", "api_key": "temporary-key",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid temporary kind: HTTP %d %s, want 400", rec.Code, rec.Body.String())
		}
	})

}
