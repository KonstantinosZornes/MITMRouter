package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"mitmrouter/internal/acctmap"
	"mitmrouter/internal/store"
)

type directSourceFixtureSyncer struct {
	sourceTestSyncer
	reconciled            []int64
	tested                []int64
	deleted               []int64
	updated               []int64
	updatedSet            []bool
	updatedClear          []bool
	updatedCfgHasEmptyDir bool
}

func (s *directSourceFixtureSyncer) Reconcile(context.Context) error { return nil }

func (s *directSourceFixtureSyncer) ReconcileSource(_ context.Context, id int64) error {
	s.reconciled = append(s.reconciled, id)
	return nil
}

func (s *directSourceFixtureSyncer) DeleteSource(_ context.Context, id int64) error {
	s.deleted = append(s.deleted, id)
	return nil
}

func (s *directSourceFixtureSyncer) UpdateSourceConfig(_ context.Context, row store.SyncSourceRow, _ string, _ string, set, clear bool) error {
	s.updated = append(s.updated, row.ID)
	s.updatedSet = append(s.updatedSet, set)
	s.updatedClear = append(s.updatedClear, clear)
	s.updatedCfgHasEmptyDir = row.DirectAuthDir == ""
	return nil
}

func (s *directSourceFixtureSyncer) TestDirectSource(_ context.Context, id int64) (string, error) {
	s.tested = append(s.tested, id)
	return "direct test summary", nil
}

// docs/012 v2：一个 source = 必填的 API 全量配置 + 选填的增量路径，
// 两者在同一份配置里并存；DSN 只写 secrets 不回显。
func TestSourceCRUDSupportsFullSyncWithOptionalIncrementalPath(t *testing.T) {
	f := newFixture(t)
	syncer := &directSourceFixtureSyncer{}
	f.api.d.Syncer = syncer
	cookie := f.login(t)

	// 缺 API 配置（base_url/api_key）必须被拒绝：全量同步是主干
	legacy := f.do(t, http.MethodPost, "/api/sources", cookie, map[string]any{
		"kind": acctmap.SourceKindCLIProxyAPI, "name": "no-api-config", "direct_auth_dir": t.TempDir(), "enabled": true,
	})
	if legacy.Code != http.StatusBadRequest {
		t.Fatalf("create without base_url/api_key: %d %s", legacy.Code, legacy.Body.String())
	}

	cpaDir := t.TempDir()
	cpaRec := f.do(t, http.MethodPost, "/api/sources", cookie, map[string]any{
		"kind": acctmap.SourceKindCLIProxyAPI, "name": "incr-cpa", "base_url": "http://cpa.example.test",
		"api_key": "management-key", "direct_auth_dir": cpaDir,
		"interval_s": 600, "enabled": true,
	})
	if cpaRec.Code != http.StatusOK {
		t.Fatalf("create CPA source: %d %s", cpaRec.Code, cpaRec.Body.String())
	}
	var cpaID struct {
		ID int64 `json:"id"`
	}
	if err := decodeJSON(cpaRec.Body.Bytes(), &cpaID); err != nil {
		t.Fatal(err)
	}

	dsn := "postgres://reader:secret@localhost/sub2api?sslmode=disable"
	dbRec := f.do(t, http.MethodPost, "/api/sources", cookie, map[string]any{
		"kind": acctmap.SourceKindSub2API, "name": "incr-sub2api", "base_url": "http://sub.example.test",
		"api_key": "admin-key", "direct_db_dsn": dsn,
		"interval_s": 600, "enabled": true,
	})
	if dbRec.Code != http.StatusOK {
		t.Fatalf("create Sub2API source: %d %s", dbRec.Code, dbRec.Body.String())
	}
	var dbID struct {
		ID int64 `json:"id"`
	}
	if err := decodeJSON(dbRec.Body.Bytes(), &dbID); err != nil {
		t.Fatal(err)
	}

	list := f.do(t, http.MethodGet, "/api/sources", cookie, nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list sources: %d %s", list.Code, list.Body.String())
	}
	body := list.Body.String()
	if !strings.Contains(body, cpaDir) || !strings.Contains(body, `"direct_db_configured":true`) {
		t.Fatalf("incremental path details missing: %s", body)
	}
	if strings.Contains(body, `"mode"`) {
		t.Fatalf("source response still exposes mode: %s", body)
	}
	if strings.Contains(body, "reader:secret") {
		t.Fatalf("direct DSN leaked in source response: %s", body)
	}
	var listed struct {
		Items []sourceDTO `json:"items"`
	}
	if err := decodeJSON(list.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	intervals := map[int64]int{}
	for _, source := range listed.Items {
		intervals[source.ID] = source.IntervalS
	}
	if intervals[cpaID.ID] != 600 || intervals[dbID.ID] != 600 {
		t.Fatalf("intervals=%v, want both 600 (interval_s is the full-sync interval)", intervals)
	}
	if len(syncer.reconciled) != 2 || syncer.reconciled[0] != cpaID.ID || syncer.reconciled[1] != dbID.ID {
		t.Fatalf("reconciled=%v, want [%d %d]", syncer.reconciled, cpaID.ID, dbID.ID)
	}

	// 测试增量路径走 ?target=incremental
	testRec := f.do(t, http.MethodPost, fmt.Sprintf("/api/sources/%d/test?target=incremental", cpaID.ID), cookie, nil)
	if testRec.Code != http.StatusOK || !strings.Contains(testRec.Body.String(), "direct test summary") {
		t.Fatalf("incremental source test: %d %s", testRec.Code, testRec.Body.String())
	}
	if len(syncer.tested) != 1 || syncer.tested[0] != cpaID.ID {
		t.Fatalf("direct tested=%v", syncer.tested)
	}

	// 更新：清除 DSN（direct_db_clear）透传到 manager
	clearRec := f.do(t, http.MethodPut, fmt.Sprintf("/api/sources/%d", dbID.ID), cookie, map[string]any{
		"kind": acctmap.SourceKindSub2API, "base_url": "http://sub.example.test",
		"direct_db_clear": true, "enabled": true,
	})
	if clearRec.Code != http.StatusOK {
		t.Fatalf("clear dsn update: %d %s", clearRec.Code, clearRec.Body.String())
	}
	if len(syncer.updated) != 1 || syncer.updated[0] != dbID.ID || !syncer.updatedClear[0] || syncer.updatedSet[0] {
		t.Fatalf("updated=%v set=%v clear=%v, want one clear for source %d",
			syncer.updated, syncer.updatedSet, syncer.updatedClear, dbID.ID)
	}

	if rec := f.do(t, http.MethodDelete, fmt.Sprintf("/api/sources/%d", cpaID.ID), cookie, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete source: %d %s", rec.Code, rec.Body.String())
	}
	if len(syncer.deleted) != 1 || syncer.deleted[0] != cpaID.ID {
		t.Fatalf("deleted=%v, want source %d", syncer.deleted, cpaID.ID)
	}
}

func decodeJSON(raw []byte, dst any) error {
	return json.Unmarshal(raw, dst)
}

// 存量直读源没有 base_url/api_key：必须仍可编辑（例如清空增量路径停 reader），
// 不被“补全 API 配置”的校验锁死（docs/012 v2 §10 迁移）。
func TestUpdateLegacyDirectSourceWithoutAPIConfig(t *testing.T) {
	f := newFixture(t)
	syncer := &directSourceFixtureSyncer{}
	f.api.d.Syncer = syncer
	cookie := f.login(t)

	// 直接在 store 里造一个存量直读源：无 base_url、无 api_key secret
	id, err := f.st.CreateSyncSourceConfig(context.Background(), store.SyncSourceConfig{
		Kind: acctmap.SourceKindCLIProxyAPI, Name: "legacy-direct", DirectAuthDir: t.TempDir(), IntervalS: 60, Enabled: true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	// 清空增量路径：不要求补全 API 配置
	clearRec := f.do(t, http.MethodPut, fmt.Sprintf("/api/sources/%d", id), cookie, map[string]any{
		"kind": acctmap.SourceKindCLIProxyAPI, "name": "legacy-direct", "direct_auth_dir": "", "enabled": true,
	})
	if clearRec.Code != http.StatusOK {
		t.Fatalf("update legacy source without api config: %d %s", clearRec.Code, clearRec.Body.String())
	}
	if len(syncer.updated) != 1 || syncer.updated[0] != id {
		t.Fatalf("updated=%v, want source %d", syncer.updated, id)
	}
	if !syncer.updatedCfgHasEmptyDir {
		t.Fatalf("update payload must carry the cleared auth dir")
	}

	// 反向：给存量源补 base_url 但不给 api_key（库中也没有）→ 拒绝
	addRec := f.do(t, http.MethodPut, fmt.Sprintf("/api/sources/%d", id), cookie, map[string]any{
		"kind": acctmap.SourceKindCLIProxyAPI, "base_url": "http://cpa.example.test", "enabled": true,
	})
	if addRec.Code != http.StatusBadRequest {
		t.Fatalf("add base_url without api_key: %d %s", addRec.Code, addRec.Body.String())
	}
}
