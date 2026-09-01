package api

// 账户↔出站绑定端点测试（docs/011 §4 / 验收 #11）：
// 校验（未知账号 404、非 egress 400、坏 mode 400、非 egress 的批量 id 404/400），
// 双向整体替换语义，写库后绑定快照被重建。

import (
	"encoding/json"
	"net/http"
	"testing"

	"mitmrouter/internal/acctegress"
	"mitmrouter/internal/acctmap"
	"mitmrouter/internal/store"
)

func seedEgressAPI(t *testing.T, f *fixture) {
	t.Helper()
	if _, err := f.st.CreateUpstream(ctxBG(), "eg-1", "plain", "http://u:p@10.254.0.9:9001", "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.CreateUpstream(ctxBG(), "di-1", "dataimpulse", "http://u__cr.us:p@10.254.0.9:9002", "", true); err != nil {
		t.Fatal(err)
	}
	f.api.rebuildTable(ctxBG())
	// 登记一个可路由的映射账号
	if err := f.st.ReplaceAccountSnapshot(ctxBG(), "openai", "a@x.io", acctmap.SourceTypeCLIProxyAPI,
		store.AcctUpsert{Platform: "openai", Account: "a@x.io", RtFP: "rt-a"}); err != nil {
		t.Fatal(err)
	}
}

func TestAcctEgressPutValidatesAndReplaces(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)
	seedEgressAPI(t, f)

	var swapped int
	f.api.d.SwapAcctEgress = func(*acctegress.Table) { swapped++ }

	// 坏 mode
	if rec := f.do(t, "PUT", "/api/acctegress/openai/a@x.io", c,
		map[string]any{"mode": "chaos", "egress_ids": []int64{1}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad mode: %d %s", rec.Code, rec.Body.String())
	}
	// 非 egress 的 ID
	if rec := f.do(t, "PUT", "/api/acctegress/openai/a@x.io", c,
		map[string]any{"mode": "sticky", "egress_ids": []int64{2}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-egress id: %d %s", rec.Code, rec.Body.String())
	}
	// 未知账号
	if rec := f.do(t, "PUT", "/api/acctegress/openai/nope@x.io", c,
		map[string]any{"mode": "sticky", "egress_ids": []int64{1}}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account: %d %s", rec.Code, rec.Body.String())
	}
	// 合法写入（缺省 mode → sticky）
	if rec := f.do(t, "PUT", "/api/acctegress/openai/A@X.io", c,
		map[string]any{"egress_ids": []int64{1, 1}}); rec.Code != http.StatusOK { // 大小写归一 + ID 去重
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	rows, err := f.st.ListAcctEgress(ctxBG())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Account != "a@x.io" || rows[0].EgressID != 1 || rows[0].Mode != store.EgressModeSticky {
		t.Fatalf("rows=%+v", rows)
	}
	if swapped == 0 {
		t.Fatal("binding snapshot must be rebuilt after write")
	}
}

func TestAcctEgressBatchDirectionAndList(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)
	seedEgressAPI(t, f)
	if err := f.st.ReplaceAccountSnapshot(ctxBG(), "anthropic", "b@x.io", acctmap.SourceTypeCLIProxyAPI,
		store.AcctUpsert{Platform: "anthropic", Account: "b@x.io", RtFP: "rt-b"}); err != nil {
		t.Fatal(err)
	}

	// 给 a 先粘滞绑 eg-1；再从 eg-1 方向批量关联 {a(random), b(random)}
	if err := f.st.ReplaceAccountBinding(ctxBG(), "openai", "a@x.io", store.EgressModeSticky, []int64{1}); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"mode": "random",
		"accounts": []map[string]string{
			{"platform": "OPENAI", "account": "a@x.io"}, // 已绑定：mode 保持 sticky
			{"platform": "anthropic", "account": "b@x.io"},
		},
	}
	if rec := f.do(t, "PUT", "/api/acctegress/egress/1", c, body); rec.Code != http.StatusOK {
		t.Fatalf("batch put: %d %s", rec.Code, rec.Body.String())
	}
	rowA, _ := f.st.ListAcctEgressByAccount(ctxBG(), "openai", "a@x.io")
	if len(rowA) != 1 || rowA[0].Mode != store.EgressModeSticky {
		t.Fatalf("existing mode must be preserved: %+v", rowA)
	}
	rowB, _ := f.st.ListAcctEgressByAccount(ctxBG(), "anthropic", "b@x.io")
	if len(rowB) != 1 || rowB[0].Mode != store.EgressModeRandom {
		t.Fatalf("new account adopts provided mode: %+v", rowB)
	}

	// 批量里混入未知账号：整单 404 且不落库
	bad := map[string]any{"accounts": []map[string]string{{"platform": "grok", "account": "zz@x.io"}}}
	if rec := f.do(t, "PUT", "/api/acctegress/egress/1", c, bad); rec.Code != http.StatusNotFound {
		t.Fatalf("batch with unknown account must 404: %d", rec.Code)
	}

	// 目标不是 egress 条目
	if rec := f.do(t, "PUT", "/api/acctegress/egress/2", c, map[string]any{"accounts": []map[string]string{}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-egress target must 400: %d", rec.Code)
	}

	// 列表回显：绑定聚合 + 计数
	rec := f.do(t, "GET", "/api/acctegress", c, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var resp struct {
		Items []struct {
			Platform    string   `json:"platform"`
			Account     string   `json:"account"`
			Mode        string   `json:"mode"`
			EgressIDs   []int64  `json:"egress_ids"`
			EgressNames []string `json:"egress_names"`
		} `json:"items"`
		Counts map[string]int `json:"counts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("items=%+v", resp.Items)
	}
	if resp.Counts["1"] != 2 {
		t.Fatalf("counts=%v want eg-1 x2", resp.Counts)
	}
	for _, it := range resp.Items {
		if len(it.EgressIDs) != 1 || it.EgressIDs[0] != 1 || it.EgressNames[0] != "eg-1" {
			t.Fatalf("item=%+v", it)
		}
	}
}

func TestAcctEgressDeleteClearsBinding(t *testing.T) {
	f := newFixture(t)
	c := f.login(t)
	seedEgressAPI(t, f)
	if err := f.st.ReplaceAccountBinding(ctxBG(), "openai", "a@x.io", store.EgressModeSticky, []int64{1}); err != nil {
		t.Fatal(err)
	}
	if rec := f.do(t, "DELETE", "/api/acctegress/openai/a@x.io", c, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	rows, _ := f.st.ListAcctEgress(ctxBG())
	if len(rows) != 0 {
		t.Fatalf("rows=%+v", rows)
	}
}
