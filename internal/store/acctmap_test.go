package store

// acct_map 快照语义测试：唯一键 (platform, source, account, rt_fp, source_type)，
// 同键覆盖 / 旧有新无删除 / 每源隔离（重拉不误删）/ 推送通道按类型对齐。

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"mitmrouter/internal/acctmap"
)

func createAcctMapTestSource(t *testing.T, st *Store, ctx context.Context, name string) string {
	t.Helper()
	id, err := st.CreateSyncSource(ctx, acctmap.SourceKindCLIProxyAPI, name, "http://example.test", "management-key", 60, true)
	if err != nil {
		t.Fatalf("create source %q: %v", name, err)
	}
	return fmt.Sprintf("%s%d", acctmap.SourceInstancePrefix, id)
}

func TestReplaceSourceSnapshotSemantics(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	src1 := createAcctMapTestSource(t, st, ctx, "source-1")
	src2 := createAcctMapTestSource(t, st, ctx, "source-2")

	// 源 1 首轮：两账号
	if err := st.ReplaceSourceSnapshot(ctx, src1, acctmap.SourceTypeCLIProxyAPI, []AcctUpsert{
		{Platform: "openai", Account: "a@x.io", AtFP: "at-a1", RtFP: "rt-a1", AtHint: "…a1", RtHint: "…a1"},
		{Platform: "openai", Account: "b@x.io", RtFP: "rt-b1"},
	}); err != nil {
		t.Fatalf("snapshot src:1 #1: %v", err)
	}
	// 源 2（同类型另一实例）：与源 1 同账号同凭据 → 并存两行
	if err := st.ReplaceSourceSnapshot(ctx, src2, acctmap.SourceTypeCLIProxyAPI, []AcctUpsert{
		{Platform: "openai", Account: "a@x.io", AtFP: "at-a1", RtFP: "rt-a1"},
	}); err != nil {
		t.Fatalf("snapshot src:2: %v", err)
	}
	rows, err := st.LoadAcctMapAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows=%d, want 3 (two sources keep separate rows)", len(rows))
	}

	// 源 1 重拉：a 的 AT 更新（同 RT 同键 → 原地覆盖），b 消失（删除），
	// c 新出现（插入）；源 2 的行不受影响。
	if err := st.ReplaceSourceSnapshot(ctx, src1, acctmap.SourceTypeCLIProxyAPI, []AcctUpsert{
		{Platform: "openai", Account: "a@x.io", AtFP: "at-a2", RtFP: "rt-a1"},
		{Platform: "anthropic", Account: "c@x.io", RtFP: "rt-c1"},
	}); err != nil {
		t.Fatalf("snapshot src:1 #2: %v", err)
	}
	rows, _ = st.LoadAcctMapAll(ctx)
	got := map[string]AcctRow{}
	for _, r := range rows {
		got[r.Source+"\x00"+r.Platform+"\x00"+r.Account+"\x00"+r.RtFP] = r
	}
	a := got[src1+"\x00openai\x00a@x.io\x00rt-a1"]
	if a.AtFP != "at-a2" || a.AtHint != "" {
		t.Fatalf("same-key row must be updated in place: %+v", a)
	}
	if _, ok := got[src1+"\x00openai\x00b@x.io\x00rt-b1"]; ok {
		t.Fatal("vanished account must be deleted")
	}
	if _, ok := got[src1+"\x00anthropic\x00c@x.io\x00rt-c1"]; !ok {
		t.Fatal("new account must be inserted")
	}
	if _, ok := got[src2+"\x00openai\x00a@x.io\x00rt-a1"]; !ok {
		t.Fatal("other source's rows must survive re-pull of src:1")
	}

	// 删除来源实例 → 级联只清自己的行
	n, err := st.DeleteSourceRows(ctx, src2)
	if err != nil || n != 1 {
		t.Fatalf("DeleteSourceRows=%d err=%v, want 1", n, err)
	}
	if rows, _ = st.LoadAcctMapAll(ctx); len(rows) != 2 {
		t.Fatalf("after cascade rows=%d, want 2", len(rows))
	}
}

func TestReplaceAccountSnapshotPush(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	put := func(atFP, rtFP, stype string) {
		t.Helper()
		err := st.ReplaceAccountSnapshot(ctx, "openai", "p@x.io", stype,
			AcctUpsert{Platform: "openai", Account: "p@x.io", AtFP: atFP, RtFP: rtFP})
		if err != nil {
			t.Fatalf("push %s/%s@%s: %v", atFP, rtFP, stype, err)
		}
	}

	// 手动指定自定义类型
	put("at-1", "rt-1", "MyRelay")
	rows, _ := st.LoadAcctMapAll(ctx)
	if len(rows) != 1 || rows[0].SourceType != "MyRelay" || rows[0].Source != SourcePush {
		t.Fatalf("custom source_type row: %+v", rows)
	}

	// 同类型 AT 轮换（RT 不变 → 同键）→ 原地更新，不新增行
	put("at-2", "rt-1", "MyRelay")
	rows, _ = st.LoadAcctMapAll(ctx)
	if len(rows) != 1 || rows[0].AtFP != "at-2" {
		t.Fatalf("AT rotation must update in place: %+v", rows)
	}

	// 同类型 RT 轮换（键变化）→ 新行 + 旧行清除
	put("at-3", "rt-2", "MyRelay")
	rows, _ = st.LoadAcctMapAll(ctx)
	if len(rows) != 1 || rows[0].RtFP != "rt-2" {
		t.Fatalf("RT rotation must replace the row: %+v", rows)
	}

	// 不同类型互不影响 → 两行并存
	put("at-4", "rt-3", acctmap.SourceTypeCLIProxyAPI)
	rows, _ = st.LoadAcctMapAll(ctx)
	if len(rows) != 2 {
		t.Fatalf("different source_type must coexist: %+v", rows)
	}
}

// P1-1: 推送通道只更新一类凭据时，另一类凭据必须沿用当前快照。
func TestReplaceAccountSnapshotPushPreservesOmittedCredential(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	put := func(atFP, atHint, rtFP, rtHint string) {
		t.Helper()
		if err := st.ReplaceAccountSnapshot(ctx, "openai", "p@x.io", acctmap.SourceTypeCLIProxyAPI, AcctUpsert{
			Platform: "openai", Account: "p@x.io",
			AtFP: atFP, AtHint: atHint, RtFP: rtFP, RtHint: rtHint,
		}); err != nil {
			t.Fatalf("put AT=%q RT=%q: %v", atFP, rtFP, err)
		}
	}

	put("at-1", "…at1", "rt-1", "…rt1")
	put("at-2", "…at2", "", "")
	rows, err := st.LoadAcctMapAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].AtFP != "at-2" || rows[0].RtFP != "rt-1" {
		t.Fatalf("AT-only update must retain RT: %+v", rows)
	}
	if rows[0].RtHint != "…rt1" {
		t.Fatalf("AT-only update must retain RT hint: %+v", rows[0])
	}

	put("", "", "rt-2", "…rt2")
	rows, err = st.LoadAcctMapAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].AtFP != "at-2" || rows[0].RtFP != "rt-2" {
		t.Fatalf("RT-only update must retain AT: %+v", rows)
	}
	if rows[0].AtHint != "…at2" {
		t.Fatalf("RT-only update must retain AT hint: %+v", rows[0])
	}
}

// P1-1 边界：推送通道遗留多行脏数据时（旧行有凭据、新行该列为空——旧 bug 的
// 产物），单独更新一类凭据必须保留所有行中最近一次非空的另一类凭据。
func TestReplaceAccountSnapshotPushMergesLatestNonEmptyAcrossRows(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	seed := func(at, rt string, ts int64) {
		t.Helper()
		if _, err := st.db.ExecContext(ctx,
			`INSERT INTO acct_map(platform,account,at_fp,rt_fp,at_hint,rt_hint,source,source_type,updated_at)
			 VALUES('openai','p@x.io',?,?,?,?, 'api','Manual',?)`,
			at, rt, "", "", ts); err != nil {
			t.Fatal(err)
		}
	}
	reset := func() {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, `DELETE FROM acct_map`); err != nil {
			t.Fatal(err)
		}
	}

	// 旧行 AT+RT 齐全；新行 RT 为空。AT-only 更新须找回 rt-keep。
	seed("at-old", "rt-keep", 100)
	seed("at-newer", "", 200)
	if err := st.ReplaceAccountSnapshot(ctx, "openai", "p@x.io", "Manual", AcctUpsert{
		Platform: "openai", Account: "p@x.io", AtFP: "at-final",
	}); err != nil {
		t.Fatalf("at-only update: %v", err)
	}
	rows, _ := st.LoadAcctMapAll(ctx)
	if len(rows) != 1 || rows[0].AtFP != "at-final" || rows[0].RtFP != "rt-keep" {
		t.Fatalf("at-only update lost legacy RT: %+v", rows)
	}

	// 反向：新行 AT 为空，RT-only 更新须找回 at-keep。
	reset()
	seed("at-keep", "rt-old", 100)
	seed("", "rt-newer", 200)
	if err := st.ReplaceAccountSnapshot(ctx, "openai", "p@x.io", "Manual", AcctUpsert{
		Platform: "openai", Account: "p@x.io", RtFP: "rt-final",
	}); err != nil {
		t.Fatalf("rt-only update: %v", err)
	}
	rows, _ = st.LoadAcctMapAll(ctx)
	if len(rows) != 1 || rows[0].AtFP != "at-keep" || rows[0].RtFP != "rt-final" {
		t.Fatalf("rt-only update lost legacy AT: %+v", rows)
	}
}

// P1-5: source 与 API Key 必须同事务提交，任一侧失败都不能留下半成品。
func TestSyncSourceCRUDIsAtomicWithSecret(t *testing.T) {
	t.Run("create rollback", func(t *testing.T) {
		st := openTest(t)
		ctx := context.Background()
		if _, err := st.db.ExecContext(ctx, `
			CREATE TRIGGER fail_source_secret_insert
			BEFORE INSERT ON secrets
			WHEN NEW.key LIKE 'source_key_%'
			BEGIN SELECT RAISE(ABORT, 'injected secret failure'); END`); err != nil {
			t.Fatalf("create trigger: %v", err)
		}

		id, err := st.CreateSyncSource(ctx, acctmap.SourceKindCLIProxyAPI, "atomic-create", "http://example.test", "key", 60, true)
		if err == nil {
			t.Fatal("CreateSyncSource must fail when secret write fails")
		}
		if id != 0 {
			t.Fatalf("failed create returned id=%d", id)
		}
		if _, ok, err := st.GetSyncSource(ctx, 1); err != sql.ErrNoRows {
			t.Fatalf("source row survived failed create: ok=%v err=%v", ok, err)
		}
	})

	t.Run("update rollback", func(t *testing.T) {
		st := openTest(t)
		ctx := context.Background()
		id, err := st.CreateSyncSource(ctx, acctmap.SourceKindCLIProxyAPI, "atomic-update", "http://old.example", "old-key", 60, true)
		if err != nil {
			t.Fatalf("seed source: %v", err)
		}
		if _, err := st.db.ExecContext(ctx, `
			CREATE TRIGGER fail_source_secret_update
			BEFORE UPDATE ON secrets
			WHEN OLD.key LIKE 'source_key_%'
			BEGIN SELECT RAISE(ABORT, 'injected secret failure'); END`); err != nil {
			t.Fatalf("create trigger: %v", err)
		}

		_, ok, err := st.GetSyncSource(ctx, id)
		if err != nil || !ok {
			t.Fatalf("read source: ok=%v err=%v", ok, err)
		}
		updated, _, _ := st.GetSyncSource(ctx, id)
		updated.Name = "changed-name"
		updated.BaseURL = "http://new.example"
		if err := st.UpdateSyncSource(ctx, updated, "new-key"); err == nil {
			t.Fatal("UpdateSyncSource must fail when secret write fails")
		}
		got, ok, err := st.GetSyncSource(ctx, id)
		if err != nil || !ok {
			t.Fatalf("read rolled-back source: ok=%v err=%v", ok, err)
		}
		if got.Name != "atomic-update" || got.BaseURL != "http://old.example" {
			t.Fatalf("source update survived failed secret write: %+v", got)
		}
		key, err := st.GetSourceAPIKey(ctx, id)
		if err != nil || key != "old-key" {
			t.Fatalf("secret changed after failed update: key=%q err=%v", key, err)
		}
	})
}

func TestClearAcctMapFpAcrossSources(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	sources := []string{
		createAcctMapTestSource(t, st, ctx, "clear-source-1"),
		createAcctMapTestSource(t, st, ctx, "clear-source-2"),
	}
	for _, src := range sources {
		if err := st.ReplaceSourceSnapshot(ctx, src, acctmap.SourceTypeCLIProxyAPI, []AcctUpsert{
			{Platform: "openai", Account: "d@x.io", AtFP: "fp-shared", RtFP: "rt-" + src},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 清除 AT 指纹：两源的行都命中；RT 仍在 → 行保留、AT 清空
	_, _, ok, err := st.ClearAcctMapFp(ctx, "openai", "d@x.io", "fp-shared")
	if err != nil || !ok {
		t.Fatalf("clear=%v err=%v", ok, err)
	}
	rows, _ := st.LoadAcctMapAll(ctx)
	if len(rows) != 2 {
		t.Fatalf("rows=%d, want 2 (RT keeps rows alive)", len(rows))
	}
	for _, r := range rows {
		if r.AtFP != "" {
			t.Fatalf("at_fp must be cleared: %+v", r)
		}
	}
	// 再逐个清 RT：两列皆空的行删除
	for _, src := range sources {
		rt := "rt-" + src
		if _, _, ok, err := st.ClearAcctMapFp(ctx, "openai", "d@x.io", rt); err != nil || !ok {
			t.Fatalf("clear %s=%v err=%v", rt, ok, err)
		}
	}
	if rows, _ = st.LoadAcctMapAll(ctx); len(rows) != 0 {
		t.Fatalf("fully cleared rows must be deleted, got %d", len(rows))
	}
}

// P0-4: 删除同步源必须原子清理 source、secret 和 acct_map；删除完成后，
// 旧同步协程即使拿着 source ID，也不能再写回快照。
func TestDeleteSyncSourceCascadesAndRejectsStaleSnapshot(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	id, err := st.CreateSyncSource(ctx, acctmap.SourceKindCLIProxyAPI, "to-delete", "http://example.test", "management-key", 60, true)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	source := fmt.Sprintf("src:%d", id)
	if err := st.ReplaceSourceSnapshot(ctx, source, acctmap.SourceTypeCLIProxyAPI, []AcctUpsert{{
		Platform: "openai",
		Account:  "old@example.com",
		RtFP:     "rt-old",
	}}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	if err := st.DeleteSyncSource(ctx, id); err != nil {
		t.Fatalf("delete source: %v", err)
	}
	if rows, err := st.LoadAcctMapAll(ctx); err != nil {
		t.Fatalf("load mappings: %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("source mappings remain after delete: %+v", rows)
	}
	if _, ok, err := st.GetSyncSource(ctx, id); err != nil && err != sql.ErrNoRows {
		t.Fatalf("check source: %v", err)
	} else if ok {
		t.Fatal("deleted source still exists")
	}
	if _, err := st.GetSecret(ctx, SourceKeySecretKey(id)); err != ErrNotFound {
		t.Fatalf("source secret err=%v, want ErrNotFound", err)
	}

	if err := st.ReplaceSourceSnapshot(ctx, source, acctmap.SourceTypeCLIProxyAPI, []AcctUpsert{{
		Platform: "openai",
		Account:  "resurrected@example.com",
		RtFP:     "rt-new",
	}}); err == nil {
		t.Fatal("snapshot write for deleted source must fail")
	}
	if rows, err := st.LoadAcctMapAll(ctx); err != nil {
		t.Fatalf("reload mappings: %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("stale snapshot resurrected mappings: %+v", rows)
	}
}

// 修复回归：清除 RT 后 rt_fp 置空，若同键已存在空 RT 行不得触发主键冲突；
// 合并时非空 AT 信息优先保留，且被清指纹的 rt_hint 一并清空。
func TestClearAcctMapFpMergesEmptyRTKeyConflict(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	seed := func(atFP, atHint, rtFP string) {
		t.Helper()
		_, err := st.db.ExecContext(ctx,
			`INSERT INTO acct_map(platform,account,at_fp,rt_fp,at_hint,rt_hint,source,source_type,updated_at)
			 VALUES('openai','d@x.io',?,?,?,?, 'src:1','CLIProxyAPI',1)`,
			atFP, rtFP, atHint, "…"+rtFP)
		if err != nil {
			t.Fatal(err)
		}
	}

	// 场景一：被清行的 AT 非空 → 覆盖既有空 RT 行
	seed("at-a", "…a", "rt-x")
	seed("at-b", "…b", "")
	_, _, ok, err := st.ClearAcctMapFp(ctx, "openai", "d@x.io", "rt-x")
	if err != nil || !ok {
		t.Fatalf("clear=%v err=%v (unique conflict must be merged away)", ok, err)
	}
	rows, _ := st.LoadAcctMapAll(ctx)
	if len(rows) != 1 || rows[0].RtFP != "" || rows[0].AtFP != "at-a" || rows[0].AtHint != "…a" {
		t.Fatalf("merge must keep non-empty AT of the cleared row: %+v", rows)
	}
	if rows[0].RtHint != "" {
		t.Errorf("cleared RT must blank its hint, got %q", rows[0].RtHint)
	}

	// 场景二：被清行 AT 为空 → 保留既有空 RT 行的 AT
	if _, err := st.db.ExecContext(ctx,
		`DELETE FROM acct_map`); err != nil {
		t.Fatal(err)
	}
	seed("", "", "rt-y")
	seed("at-c", "…c", "")
	if _, _, ok, err := st.ClearAcctMapFp(ctx, "openai", "d@x.io", "rt-y"); err != nil || !ok {
		t.Fatalf("clear=%v err=%v", ok, err)
	}
	rows, _ = st.LoadAcctMapAll(ctx)
	if len(rows) != 1 || rows[0].AtFP != "at-c" || rows[0].AtHint != "…c" {
		t.Fatalf("existing empty-RT row's AT must survive: %+v", rows)
	}
}
