package server

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"context"

	"mitmrouter/internal/certca"
	"mitmrouter/internal/settings"
	"mitmrouter/internal/store"
	"mitmrouter/internal/upstream"
)

// 双端口硬拆语义：接入面与管理面互不越界。

func newSplitServer(t *testing.T) *Server {
	t.Helper()
	st, _, err := store.Bootstrap(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ca, err := certca.Ensure(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	holder := settings.NewHolder(settings.DefaultSnapshot())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(holder, ca, upstream.EmptyTable(), nil, logger)
}

func TestIngressPlaneServesStaticNoticeOnly(t *testing.T) {
	srv := newSplitServer(t)
	fe := httptest.NewServer(srv.Handler())
	t.Cleanup(fe.Close)

	for _, path := range []string{"/", "/ui", "/ui/", "/api/settings", "/metrics"} {
		resp, err := http.Get(fe.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200 static notice", path, resp.StatusCode)
		}
		b := string(body)
		if !strings.Contains(b, "Ingress Port") {
			t.Errorf("GET %s must return the static ingress-port notice, got: %.80s", path, b)
		}
		if strings.Contains(b, "<div id=\"app\">") || strings.Contains(b, `"error"`) {
			t.Errorf("GET %s must not serve the admin UI or API payloads, got: %.120s", path, b)
		}
	}
}

func TestAdminPlaneRejectsTunnelTraffic(t *testing.T) {
	srv := newSplitServer(t)

	// 绝对式请求（正代用法）必须被拒
	req, _ := http.NewRequest(http.MethodGet, "http://api.ipify.org/", nil)
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("absolute-form on admin plane = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "admin_no_ingress") {
		t.Errorf("absolute-form rejection must carry admin_no_ingress code, got: %s", rec.Body.String())
	}
}

func TestAdminPlaneRejectsCONNECT(t *testing.T) {
	srv := newSplitServer(t)
	ad := httptest.NewServer(srv.AdminHandler())
	t.Cleanup(ad.Close)

	rawReq := "CONNECT api.openai.com:443 HTTP/1.1\r\nHost: api.openai.com:443\r\n\r\n"
	conn, err := net.Dial("tcp", ad.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(rawReq)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	n, _ := conn.Read(buf)
	if respText := string(buf[:n]); !strings.Contains(respText, "404") {
		t.Errorf("CONNECT on admin plane must get 404, got: %.80s", respText)
	}
}

func TestAdminPlaneStillServesUIAndAPIRoutes(t *testing.T) {
	srv := newSplitServer(t)

	// origin-form 管理路径正常进入管理面（UI 未构建 → 占位页）
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /ui on admin plane = %d, want 200", rec.Code)
	}
	// /api/* 必须进入管理路由器，而不是静态提示页
	rec2 := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/nope", nil))
	if strings.Contains(rec2.Body.String(), "Ingress Port") {
		t.Error("/api/* on admin plane must hit the admin router, not the static notice")
	}
}
