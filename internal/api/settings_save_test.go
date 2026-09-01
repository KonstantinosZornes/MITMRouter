package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// 修复回归：设置接口不暴露 acctmap_enabled，PUT 保存不得把它静默关闭。
// P1-3: 修改全局 TTL 前必须重新校验已有 Generic 上游模板，避免保存后运行时注入失败。
func TestPutSettingsRejectsTTLDisabledByExistingGenericTemplate(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)
	initial := validDTO()
	if rec := f.do(t, http.MethodPut, "/api/settings", cookie, initial); rec.Code != http.StatusOK {
		t.Fatalf("set initial TTL: HTTP %d %s", rec.Code, rec.Body.String())
	}
	if _, err := f.st.CreateUpstream(ctxBG(), "generic-with-ttl", "generic", "http://gateway.example:8080", `{"username_template":"{user}-{ttl_min}"}`, true); err != nil {
		t.Fatalf("create generic upstream: %v", err)
	}

	dto := validDTO()
	dto.SessionTTLMin = 0
	rec := f.do(t, http.MethodPut, "/api/settings", cookie, dto)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("save TTL=0: HTTP %d %s, want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "session_ttl_min") {
		t.Fatalf("error=%s, want session_ttl_min validation", rec.Body.String())
	}
}

func TestPutSettingsPreservesAcctMapEnabled(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)
	if !f.holder.Current().AcctMapEnabled {
		t.Fatal("precondition: account mapping must default to enabled")
	}

	rec := f.do(t, http.MethodPut, "/api/settings", cookie, map[string]any{
		"hash_salt": "fixed-salt", "sid_len": 16, "session_ttl_min": 0,
		"no_marker_policy": "default_session", "marker_path_parts": []string{},
		"marker_headers": []string{"Authorization"}, "block_private_targets": true,
		"log_retention_days": 30,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save settings: HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if !f.holder.Current().AcctMapEnabled {
		t.Fatal("PUT /api/settings disabled AcctMapEnabled")
	}
	m, err := f.st.AllSettings(ctxBG())
	if err != nil {
		t.Fatal(err)
	}
	if m["acctmap_enabled"] != "true" {
		t.Errorf("acctmap_enabled persisted as %q, want true", m["acctmap_enabled"])
	}
}

// 修复回归：设置页回显的接入地址必须携带接入监听端口。
// 管理台与接入口分属两个监听——请求 Host 自带的管理台端口（如 :55667）
// 不得泄漏进回显地址，否则用户复制后客户端打到管理台只会收到 404。
func TestSettingsDefaultsAndPersistsPrivateTargetBlocking(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)

	initial := decode[settingsDTO](t, f.do(t, http.MethodGet, "/api/settings", cookie, nil))
	if initial.BlockPrivateTargets == nil || !*initial.BlockPrivateTargets {
		t.Fatalf("new settings must block private targets by default: %+v", initial.BlockPrivateTargets)
	}

	dto := validDTO()
	dto.BlockPrivateTargets = boolPtr(false)
	if rec := f.do(t, http.MethodPut, "/api/settings", cookie, dto); rec.Code != http.StatusOK {
		t.Fatalf("disable private-target blocking: HTTP %d %s", rec.Code, rec.Body.String())
	}
	if f.holder.Current().BlockPrivateTargets {
		t.Fatal("private-target blocking change was not applied")
	}
	stored, err := f.st.AllSettings(ctxBG())
	if err != nil {
		t.Fatal(err)
	}
	if stored["block_private_targets"] != "false" {
		t.Fatalf("stored block_private_targets=%q, want false", stored["block_private_targets"])
	}
}

// P2-3: 入站认证密码中的 URL 保留字符必须编码，生成的地址应可无损解析。
func TestSettingsIngressURLAuthEscapesCredentials(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)
	password := "name/p@ss?x#y"
	rec := f.do(t, http.MethodPut, "/api/settings", cookie, map[string]any{
		"hash_salt": "ingress-url-auth-salt", "sid_len": 16, "session_ttl_min": 0,
		"no_marker_policy": "default_session", "marker_path_parts": []string{},
		"marker_headers": []string{"Authorization"}, "block_private_targets": true,
		"log_retention_days": 30, "listen_auth": "user:" + password,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("set listen_auth: HTTP %d %s", rec.Code, rec.Body.String())
	}

	rec = f.do(t, http.MethodGet, "/api/settings", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get settings: HTTP %d %s", rec.Code, rec.Body.String())
	}
	out := decode[struct {
		IngressURLAuth string `json:"ingress_url_auth"`
	}](t, rec)
	u, err := url.Parse(out.IngressURLAuth)
	if err != nil {
		t.Fatalf("parse ingress_url_auth %q: %v", out.IngressURLAuth, err)
	}
	if u.Host != "example.com:55666" || u.User == nil {
		t.Fatalf("ingress_url_auth=%q, want host and userinfo", out.IngressURLAuth)
	}
	gotPassword, ok := u.User.Password()
	if u.User.Username() != "user" || !ok || gotPassword != password {
		t.Fatalf("parsed credentials user=%q password=%q present=%v, want user/password intact", u.User.Username(), gotPassword, ok)
	}
}

func TestSettingsEchoIngressURLUsesIngressPort(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)

	// 启用入站认证，让带凭据回显行有内容可断言
	rec := f.do(t, http.MethodPut, "/api/settings", cookie, map[string]any{
		"hash_salt": "echo-salt", "sid_len": 16, "session_ttl_min": 0,
		"no_marker_policy": "default_session", "marker_path_parts": []string{},
		"marker_headers": []string{"Authorization"}, "block_private_targets": true,
		"log_retention_days": 30, "listen_auth": "tester:secret123",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("enable listen_auth: HTTP %d %s", rec.Code, rec.Body.String())
	}

	cases := []struct{ name, host string }{
		{"无端口Host", "example.com"},
		{"管理台端口Host", "admin.example.com:55667"},
	}
	for _, c := range cases {
		hostname := strings.SplitN(c.host, ":", 2)[0] // 端口应被剥除，只保留主机名
		r := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
		r.Host = c.host
		r.AddCookie(cookie)
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: HTTP %d", c.name, rec.Code)
		}
		dto := decode[struct {
			IngressURL     string `json:"ingress_url"`
			IngressURLAuth string `json:"ingress_url_auth"`
		}](t, rec)
		if !strings.HasSuffix(dto.IngressURL, ":55666") || strings.Contains(dto.IngressURL, "55667") {
			t.Errorf("%s: ingress_url=%q, want 接入端口 55666 且不含管理台端口", c.name, dto.IngressURL)
		}
		for _, u := range []string{dto.IngressURL, dto.IngressURLAuth} {
			if !strings.Contains(u, hostname) || !strings.HasSuffix(u, ":55666") {
				t.Errorf("%s: 地址 %q 应保留请求主机名 %s 且端口为 55666", c.name, u, hostname)
			}
		}
		if !strings.Contains(dto.IngressURLAuth, "tester:secret123@") {
			t.Errorf("%s: 带认证地址缺凭据段: %q", c.name, dto.IngressURLAuth)
		}
	}
}
