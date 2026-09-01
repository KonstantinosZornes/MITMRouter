package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// docs/012 v2 回归：全局增量开关已删除，设置接口不得再出现该字段；
// 增量直读由每个 source 的增量路径字段驱动。
func TestSettingsOmitIncrementalToggle(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)

	get := f.do(t, http.MethodGet, "/api/settings", cookie, nil)
	if get.Code != http.StatusOK {
		t.Fatalf("get settings: %d %s", get.Code, get.Body.String())
	}
	if strings.Contains(get.Body.String(), "incremental_enabled") {
		t.Fatalf("settings response still exposes incremental_enabled: %s", get.Body.String())
	}

	dto := validDTO()
	if rec := f.do(t, http.MethodPut, "/api/settings", cookie, dto); rec.Code != http.StatusOK {
		t.Fatalf("put settings: %d %s", rec.Code, rec.Body.String())
	}
	stored, err := f.st.AllSettings(context.Background())
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if _, ok := stored["incremental_enabled"]; ok {
		t.Fatalf("legacy incremental_enabled setting key survived: %v", stored)
	}
}
