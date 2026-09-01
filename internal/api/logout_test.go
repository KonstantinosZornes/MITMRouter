package api

import (
	"net/http"
	"testing"
)

// P2-1: 退出必须让浏览器删除当前管理会话 Cookie，而不是只跳转登录页。
func TestLogoutClearsSessionCookie(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)

	rec := f.do(t, http.MethodPost, "/api/auth/logout", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: HTTP %d %s", rec.Code, rec.Body.String())
	}

	var cleared *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			cleared = c
			break
		}
	}
	if cleared == nil {
		t.Fatal("logout did not send a session-cookie deletion")
	}
	if cleared.Value != "" || cleared.MaxAge >= 0 || cleared.Path != "/" || !cleared.HttpOnly {
		t.Fatalf("logout cookie=%+v, want expired HttpOnly cookie at /", cleared)
	}
}
