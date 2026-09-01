package store

import (
	"context"
	"fmt"
	"testing"

	"mitmrouter/internal/acctmap"
)

func TestApplyAccountDeltaIsSourceScopedAndIdempotent(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	sourceOne := createAcctMapTestSource(t, st, ctx, "direct-delta-one")
	sourceTwo := createAcctMapTestSource(t, st, ctx, "direct-delta-two")

	for _, source := range []string{sourceOne, sourceTwo} {
		if err := st.ReplaceSourceSnapshot(ctx, source, acctmap.SourceTypeCLIProxyAPI, []AcctUpsert{{
			Platform: "openai", Account: "user@example.com", AtFP: "old-at", RtFP: "old-rt",
		}}); err != nil {
			t.Fatal(err)
		}
	}

	up := AcctUpsert{Platform: "openai", Account: "user@example.com", AtFP: "new-at", RtFP: "new-rt"}
	if err := st.ApplyAccountDelta(ctx, sourceOne, acctmap.SourceTypeCLIProxyAPI, up); err != nil {
		t.Fatalf("ApplyAccountDelta: %v", err)
	}
	if err := st.ApplyAccountDelta(ctx, sourceOne, acctmap.SourceTypeCLIProxyAPI, up); err != nil {
		t.Fatalf("repeat ApplyAccountDelta: %v", err)
	}

	rows, err := st.LoadAcctMapAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%+v, want one row per source", rows)
	}
	seen := make(map[string]AcctRow, len(rows))
	for _, row := range rows {
		seen[row.Source] = row
	}
	if got := seen[sourceOne]; got.AtFP != "new-at" || got.RtFP != "new-rt" {
		t.Fatalf("source one row=%+v, want new credentials", got)
	}
	if got := seen[sourceTwo]; got.AtFP != "old-at" || got.RtFP != "old-rt" {
		t.Fatalf("source two row=%+v, must remain untouched", got)
	}

	if err := st.ApplyAccountDelta(ctx, sourceOne, acctmap.SourceTypeCLIProxyAPI, AcctUpsert{
		Platform: "openai", Account: "user@example.com",
	}); err != nil {
		t.Fatalf("clear ApplyAccountDelta: %v", err)
	}
	rows, err = st.LoadAcctMapAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Source != sourceTwo {
		t.Fatalf("cleared source rows=%+v, want only source two", rows)
	}
}

func TestDirectSourceConfigStoresPerSourceDSN(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	id, err := st.CreateSyncSourceConfig(ctx, SyncSourceConfig{
		Kind:        acctmap.SourceKindSub2API,
		Name:        "direct-sub2api",
		DirectDBDSN: "postgres://reader:secret@db.example/sub2api?sslmode=require",
		IntervalS:   60,
		Enabled:     true,
	}, "api-key")
	if err != nil {
		t.Fatalf("create direct source: %v", err)
	}
	row, ok, err := st.GetSyncSource(ctx, id)
	if err != nil || !ok {
		t.Fatalf("get direct source: ok=%v err=%v", ok, err)
	}
	if row.DirectDBSecret != SourceDirectDBSecretKey(id) {
		t.Fatalf("direct source row=%+v", row)
	}
	got, err := st.GetSourceDirectDB(ctx, id)
	if err != nil {
		t.Fatalf("get direct DSN: %v", err)
	}
	if got != "postgres://reader:secret@db.example/sub2api?sslmode=require" {
		t.Fatalf("direct DSN=%q", got)
	}

	if err := st.DeleteSyncSource(ctx, id); err != nil {
		t.Fatalf("delete direct source: %v", err)
	}
	if _, err := st.GetSecret(ctx, SourceDirectDBSecretKey(id)); err != ErrNotFound {
		t.Fatalf("direct secret after delete: %v", err)
	}
	if _, err := st.GetSecret(ctx, SourceKeySecretKey(id)); err != ErrNotFound {
		t.Fatalf("api key secret after delete: %v", err)
	}
}

// docs/012 v2：API 密钥与增量 DSN 并存；清除 DSN 不影响 API 密钥，反之亦然。
func TestUpdateSourceConfigKeepsAPIKeyWhenClearingDSN(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	id, err := st.CreateSyncSourceConfig(ctx, SyncSourceConfig{
		Kind: acctmap.SourceKindSub2API, Name: "coexist-source", BaseURL: "http://example.test",
		DirectDBDSN: "postgres://reader:secret@db.example/sub2api?sslmode=require",
		IntervalS:   600, Enabled: true,
	}, "key-one")
	if err != nil {
		t.Fatal(err)
	}
	row, ok, err := st.GetSyncSource(ctx, id)
	if err != nil || !ok {
		t.Fatal(err)
	}

	// 换 API key：DSN 不受影响
	if err := st.UpdateSyncSourceConfig(ctx, row, "key-two", "", false, false); err != nil {
		t.Fatalf("update api key: %v", err)
	}
	if key, err := st.GetSourceAPIKey(ctx, id); err != nil || key != "key-two" {
		t.Fatalf("api key after rotate=%q err=%v", key, err)
	}
	if dsn, err := st.GetSourceDirectDB(ctx, id); err != nil || dsn == "" {
		t.Fatalf("dsn must survive api key rotate: %q %v", dsn, err)
	}

	// 清除 DSN：API key 保留，DSN secret 删干净
	row, ok, err = st.GetSyncSource(ctx, id)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if err := st.UpdateSyncSourceConfig(ctx, row, "", "", false, true); err != nil {
		t.Fatalf("clear dsn: %v", err)
	}
	if _, err := st.GetSourceDirectDB(ctx, id); err != ErrNotFound {
		t.Fatalf("dsn secret after clear: %v", err)
	}
	if key, err := st.GetSourceAPIKey(ctx, id); err != nil || key != "key-two" {
		t.Fatalf("api key must survive dsn clear: %q %v", key, err)
	}
	row, ok, err = st.GetSyncSource(ctx, id)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if row.DirectDBSecret != "" {
		t.Fatalf("direct_db_secret column after clear=%q", row.DirectDBSecret)
	}

	// 重新填 DSN：增量恢复
	if err := st.UpdateSyncSourceConfig(ctx, row, "", "postgres://reader:secret@db.example/sub2api?sslmode=require", true, false); err != nil {
		t.Fatalf("set dsn: %v", err)
	}
	if dsn, err := st.GetSourceDirectDB(ctx, id); err != nil || dsn == "" {
		t.Fatalf("dsn after re-set: %q %v", dsn, err)
	}
}

func TestDirectSourceConfigSupportsIndependentSources(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	idOne, err := st.CreateSyncSourceConfig(ctx, SyncSourceConfig{
		Kind: acctmap.SourceKindCLIProxyAPI, Name: "direct-cpa-one", DirectAuthDir: "/tmp/cpa-one", Enabled: true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	idTwo, err := st.CreateSyncSourceConfig(ctx, SyncSourceConfig{
		Kind: acctmap.SourceKindCLIProxyAPI, Name: "direct-cpa-two", DirectAuthDir: "/tmp/cpa-two", Enabled: true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListSyncSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := map[int64]SyncSourceRow{}
	for _, row := range rows {
		if row.ID == idOne || row.ID == idTwo {
			found[row.ID] = row
		}
	}
	if found[idOne].DirectAuthDir != "/tmp/cpa-one" || found[idTwo].DirectAuthDir != "/tmp/cpa-two" {
		t.Fatalf("independent direct rows=%+v", found)
	}
}

func TestApplyAccountDeltaRequiresExistingSource(t *testing.T) {
	st := openTest(t)
	err := st.ApplyAccountDelta(context.Background(), fmt.Sprintf("src:%d", 99999), acctmap.SourceTypeSub2API, AcctUpsert{
		Platform: "openai", Account: "user@example.com", AtFP: "at", RtFP: "rt",
	})
	if err == nil {
		t.Fatal("missing source must reject account delta")
	}
}
