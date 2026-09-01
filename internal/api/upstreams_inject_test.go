package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// 修复回归：Web UI 以字符串承载模板 JSON（textarea 原样入 payload），
// 创建与编辑（含掩码密码回填）都必须成功，与对象形态等价。
func TestGenericUpstreamAcceptsUIStringInject(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)

	// 创建：字符串形态
	rec := f.do(t, http.MethodPost, "/api/upstreams", cookie, map[string]any{
		"name": "gen-ui", "platform": "generic",
		"base_url": "http://user:oldpw@gw.example:8000",
		"inject":   `{"username_template":"{user}-sid-{sid}","password":"realpw"}`,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create with string inject: HTTP %d: %s", rec.Code, rec.Body.String())
	}
	id := decode[struct {
		ID int64 `json:"id"`
	}](t, rec).ID

	// 编辑：掩码密码 + 字符串形态 → 必须回填真实密码
	rec = f.do(t, http.MethodPut, "/api/upstreams/"+strconv.FormatInt(id, 10), cookie, map[string]any{
		"name": "gen-ui", "platform": "generic",
		"base_url": "http://user:____@gw.example:8000",
		"inject":   `{"username_template":"{user}-sid-{sid}","password":"____"}`,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("edit with masked string inject: HTTP %d: %s", rec.Code, rec.Body.String())
	}

	rows, err := f.st.ListUpstreams(ctxBG())
	if err != nil {
		t.Fatal(err)
	}
	idx := -1
	for i := range rows {
		if rows[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("created upstream not found")
	}
	if !strings.Contains(rows[idx].Inject.String, `"password":"realpw"`) {
		t.Errorf("masked password must merge back to real one, got inject=%s", rows[idx].Inject.String)
	}
	if !strings.Contains(rows[idx].BaseURL, "user:oldpw@") {
		t.Errorf("masked URL password must keep old value, got %s", rows[idx].BaseURL)
	}
}
