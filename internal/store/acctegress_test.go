package store

// acct_egress 绑定表测试（docs/011）：
// 双向整体替换语义、删出站级联、账号消失级联 GC（覆盖手动删/快照消失/清凭据/删源
// 全部删除通道）、同步空快照保护。

import (
	"context"
	"fmt"
	"testing"

	"mitmrouter/internal/acctmap"
)

func setupEgressFixture(t *testing.T, ctx context.Context, st *Store) (src string, srcID int64) {
	t.Helper()
	eg1 := mustCreateUpstream(t, st, ctx, "eg-1")
	eg2 := mustCreateUpstream(t, st, ctx, "eg-2")
	_ = eg1
	_ = eg2
	src = createAcctMapTestSource(t, st, ctx, "source-eg")
	if _, err := fmt.Sscanf(src, acctmap.SourceInstancePrefix+"%d", &srcID); err != nil {
		t.Fatalf("parse source id from %q: %v", src, err)
	}
	if err := st.ReplaceSourceSnapshot(ctx, src, acctmap.SourceTypeCLIProxyAPI, []AcctUpsert{
		{Platform: "openai", Account: "a@x.io", RtFP: "rt-a"},
		{Platform: "openai", Account: "b@x.io", RtFP: "rt-b"},
	}); err != nil {
		t.Fatalf("seed acct_map: %v", err)
	}
	return src, srcID
}

func mustCreateUpstream(t *testing.T, st *Store, ctx context.Context, name string) int64 {
	t.Helper()
	id, err := st.CreateUpstream(ctx, name, "plain", "http://u:p@10.254.0.9:9000", "", true)
	if err != nil {
		t.Fatalf("create egress %q: %v", name, err)
	}
	return id
}

func bindingCount(t *testing.T, st *Store, ctx context.Context) int {
	t.Helper()
	rows, err := st.ListAcctEgress(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func TestReplaceAccountBindingReplacesWholesale(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	setupEgressFixture(t, ctx, st)

	if err := st.ReplaceAccountBinding(ctx, "openai", "a@x.io", EgressModeSticky, []int64{1, 2}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	rows, _ := st.ListAcctEgressByAccount(ctx, "openai", "a@x.io")
	if len(rows) != 2 || rows[0].Mode != EgressModeSticky {
		t.Fatalf("after bind: %+v", rows)
	}

	// 整体替换：换成单条 + 随机模式，旧行不得残留
	if err := st.ReplaceAccountBinding(ctx, "openai", "a@x.io", EgressModeRandom, []int64{2}); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	rows, _ = st.ListAcctEgressByAccount(ctx, "openai", "a@x.io")
	if len(rows) != 1 || rows[0].EgressID != 2 || rows[0].Mode != EgressModeRandom {
		t.Fatalf("after rebind: %+v", rows)
	}
}

func TestDeleteUpstreamCascadesBindings(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	setupEgressFixture(t, ctx, st)
	if err := st.ReplaceAccountBinding(ctx, "openai", "a@x.io", EgressModeSticky, []int64{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteUpstream(ctx, 1); err != nil {
		t.Fatalf("delete upstream: %v", err)
	}
	rows, _ := st.ListAcctEgressByAccount(ctx, "openai", "a@x.io")
	if len(rows) != 1 || rows[0].EgressID != 2 {
		t.Fatalf("binding rows referencing deleted upstream must vanish: %+v", rows)
	}
}

func TestBindingsGcWhenAccountDeletedManually(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	setupEgressFixture(t, ctx, st)
	if err := st.ReplaceAccountBinding(ctx, "openai", "a@x.io", EgressModeSticky, []int64{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteAcctMapAccount(ctx, "openai", "a@x.io", ""); err != nil {
		t.Fatal(err)
	}
	if n := bindingCount(t, st, ctx); n != 0 {
		t.Fatalf("orphan binding must be GC'd with account deletion, left=%d", n)
	}
}

func TestBindingsSurviveWhileAnySourceKeepsAccount(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	src, _ := setupEgressFixture(t, ctx, st)
	src2 := createAcctMapTestSource(t, st, ctx, "source-eg-2")
	if err := st.ReplaceSourceSnapshot(ctx, src2, acctmap.SourceTypeSub2API, []AcctUpsert{
		{Platform: "openai", Account: "a@x.io", RtFP: "rt-a-2"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceAccountBinding(ctx, "openai", "a@x.io", EgressModeSticky, []int64{1}); err != nil {
		t.Fatal(err)
	}
	// 只删来源 1 的行：来源 2 还有该账号 → 绑定保留
	if _, err := st.DeleteAcctMapAccount(ctx, "openai", "a@x.io", src); err != nil {
		t.Fatal(err)
	}
	if n := bindingCount(t, st, ctx); n != 1 {
		t.Fatalf("binding must survive while another source keeps the account, got %d", n)
	}
}

func TestBindingsGcOnClearAllCredentials(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	setupEgressFixture(t, ctx, st)
	if err := st.ReplaceAccountBinding(ctx, "openai", "b@x.io", EgressModeRandom, []int64{2}); err != nil {
		t.Fatal(err)
	}
	// 账号 b 只有 RT 一列；清掉即行消失 → 绑定被 GC
	_, _, ok, err := st.ClearAcctMapFp(ctx, "openai", "b@x.io", "rt-b")
	if err != nil || !ok {
		t.Fatalf("clear: ok=%v err=%v", ok, err)
	}
	if n := bindingCount(t, st, ctx); n != 0 {
		t.Fatalf("binding must die with last credential row, left=%d", n)
	}
}

func TestBindingsGcOnReplaceSourceSnapshotRemovingLastRow(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	src, _ := setupEgressFixture(t, ctx, st)
	if err := st.ReplaceAccountBinding(ctx, "openai", "a@x.io", EgressModeSticky, []int64{1}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceAccountBinding(ctx, "openai", "b@x.io", EgressModeSticky, []int64{2}); err != nil {
		t.Fatal(err)
	}
	// 快照对齐把 b 拉掉、保留 a → 仅 a 的绑定存活
	if err := st.ReplaceSourceSnapshot(ctx, src, acctmap.SourceTypeCLIProxyAPI, []AcctUpsert{
		{Platform: "openai", Account: "a@x.io", RtFP: "rt-a-new"},
	}); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.ListAcctEgress(ctx)
	if len(rows) != 1 || rows[0].Account != "a@x.io" {
		t.Fatalf("expected only a@x.io binding to survive: %+v", rows)
	}
}

func TestBindingsGcOnSyncSourceDeleted(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	_, srcID := setupEgressFixture(t, ctx, st)
	if err := st.ReplaceAccountBinding(ctx, "openai", "a@x.io", EgressModeSticky, []int64{1}); err != nil {
		t.Fatal(err)
	}
	// 源是这些账号的唯一来源：删源 → 映射行消失 → 绑定同事务消失
	if err := st.DeleteSyncSource(ctx, srcID); err != nil {
		t.Fatal(err)
	}
	if n := bindingCount(t, st, ctx); n != 0 {
		t.Fatalf("bindings must be GC'd when the sole owning source is deleted, left=%d", n)
	}
}

func TestBindingBindingOnlyForExistingRowsOnWrite(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	setupEgressFixture(t, ctx, st)
	// 给一个 acct_map 里不存在的账号写绑定：GC 应立即清掉（自愈防御）
	if err := st.ReplaceAccountBinding(ctx, "openai", "ghost@x.io", EgressModeSticky, []int64{1}); err != nil {
		t.Fatal(err)
	}
	if n := bindingCount(t, st, ctx); n != 0 {
		t.Fatalf("ghost-account binding must not persist, left=%d", n)
	}
}

func TestReplaceEgressAccountsPreservesExistingModes(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	setupEgressFixture(t, ctx, st)
	// 先给 a 粘滞绑定 eg-1；再从出站方向批量关联 {a(random 声明), b(新)}
	if err := st.ReplaceAccountBinding(ctx, "openai", "a@x.io", EgressModeSticky, []int64{1}); err != nil {
		t.Fatal(err)
	}
	err := st.ReplaceEgressAccounts(ctx, 1, []EgressAssign{
		{Platform: "openai", Account: "a@x.io", Mode: EgressModeRandom}, // 已绑定：mode 保持 sticky
		{Platform: "openai", Account: "b@x.io", Mode: EgressModeRandom}, // 新绑定：采用 random
	})
	if err != nil {
		t.Fatalf("batch bind: %v", err)
	}
	rowA, _ := st.ListAcctEgressByAccount(ctx, "openai", "a@x.io")
	if len(rowA) != 1 || rowA[0].Mode != EgressModeSticky {
		t.Fatalf("existing account mode must be preserved: %+v", rowA)
	}
	rowB, _ := st.ListAcctEgressByAccount(ctx, "openai", "b@x.io")
	if len(rowB) != 1 || rowB[0].Mode != EgressModeRandom {
		t.Fatalf("new account adopts provided mode: %+v", rowB)
	}

	// 出站方向移除全部账户：绑定整份消失
	if err := st.ReplaceEgressAccounts(ctx, 1, nil); err != nil {
		t.Fatal(err)
	}
	if n := bindingCount(t, st, ctx); n != 0 {
		t.Fatalf("removing all accounts from egress must clear bindings, left=%d", n)
	}
}

func TestReplaceSourceSnapshotGuardedDefersEmptySnapshot(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	src, _ := setupEgressFixture(t, ctx, st)
	if err := st.ReplaceAccountBinding(ctx, "openai", "a@x.io", EgressModeSticky, []int64{1}); err != nil {
		t.Fatal(err)
	}

	const threshold = 3
	// 第 1、2 次空快照：保护期内，映射与绑定都必须原样保留
	for i := int64(1); i <= 2; i++ {
		skipped, streak, err := st.ReplaceSourceSnapshotGuarded(ctx, src, acctmap.SourceTypeCLIProxyAPI, nil, threshold)
		if err != nil {
			t.Fatalf("empty #%d: %v", i, err)
		}
		if !skipped || streak != i {
			t.Fatalf("empty #%d: skipped=%v streak=%d want skipped=true streak=%d", i, skipped, streak, i)
		}
		accts, err := st.LoadAcctMapAll(ctx)
		if err != nil {
			t.Fatal(err)
		}
		foundA := false
		for _, r := range accts {
			if r.Account == "a@x.io" && r.Source == src {
				foundA = true
			}
		}
		if !foundA {
			t.Fatalf("empty snapshot #%d must NOT clear rows under protection", i)
		}
	}
	if n := bindingCount(t, st, ctx); n != 1 {
		t.Fatalf("bindings must survive protected empties, left=%d", n)
	}

	// 第 3 次：达到阈值 → 真正清空并级联清绑定
	skipped, streak, err := st.ReplaceSourceSnapshotGuarded(ctx, src, acctmap.SourceTypeCLIProxyAPI, nil, threshold)
	if err != nil || skipped {
		t.Fatalf("threshold crossing: skipped=%v err=%v", skipped, err)
	}
	if streak != 3 {
		t.Fatalf("streak=%d want 3", streak)
	}
	if n := bindingCount(t, st, ctx); n != 0 {
		t.Fatalf("threshold crossing must cascade-clear bindings, left=%d", n)
	}
	accts, _ := st.LoadAcctMapAll(ctx)
	for _, r := range accts {
		if r.Source == src {
			t.Fatalf("threshold crossing must clear source rows: %+v", r)
		}
	}
}

func TestReplaceSourceSnapshotGuardedNonEmptyResetsStreak(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	src, _ := setupEgressFixture(t, ctx, st)

	// 空 ×2 后恢复非空快照：计数必须归零，下次闪断要重新数满阈值
	for range 2 {
		skipped, _, err := st.ReplaceSourceSnapshotGuarded(ctx, src, acctmap.SourceTypeCLIProxyAPI, nil, 3)
		if err != nil || !skipped {
			t.Fatalf("protected empty: skipped=%v err=%v", skipped, err)
		}
	}
	if _, _, err := st.ReplaceSourceSnapshotGuarded(ctx, src, acctmap.SourceTypeCLIProxyAPI, []AcctUpsert{
		{Platform: "openai", Account: "a@x.io", RtFP: "rt-a-re"},
	}, 3); err != nil {
		t.Fatal(err)
	}
	skipped, streak, err := st.ReplaceSourceSnapshotGuarded(ctx, src, acctmap.SourceTypeCLIProxyAPI, nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !skipped || streak != 1 {
		t.Fatalf("streak must restart after non-empty snapshot: skipped=%v streak=%d", skipped, streak)
	}
}

func TestReplaceSourceSnapshotGuardedThresholdOneAndEmptySource(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	src, _ := setupEgressFixture(t, ctx, st)

	// threshold=1：首次空快照立即清（旧行为）
	skipped, _, err := st.ReplaceSourceSnapshotGuarded(ctx, src, acctmap.SourceTypeCLIProxyAPI, nil, 1)
	if err != nil || skipped {
		t.Fatalf("threshold=1 clears immediately: skipped=%v err=%v", skipped, err)
	}
	if n := bindingCount(t, st, ctx); n != 0 {
		t.Fatalf("left=%d", n)
	}

	// 本来就空的源收到空快照：无事可做且不算 skip
	skipped, _, err = st.ReplaceSourceSnapshotGuarded(ctx, src, acctmap.SourceTypeCLIProxyAPI, nil, 3)
	if err != nil || skipped {
		t.Fatalf("already-empty source: skipped=%v err=%v", skipped, err)
	}
}

func TestSyncEmptyClearThresholdClampAndDefault(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if got := st.SyncEmptyClearThreshold(ctx); got != 3 {
		t.Fatalf("default threshold=%d want 3", got)
	}
	if err := st.SetSetting(ctx, "sync_empty_clear_threshold", "99"); err != nil {
		t.Fatal(err)
	}
	if got := st.SyncEmptyClearThreshold(ctx); got != 99 {
		t.Fatalf("custom threshold=%d want 99", got)
	}
	_ = st.SetSetting(ctx, "sync_empty_clear_threshold", "-5")
	if got := st.SyncEmptyClearThreshold(ctx); got != 3 {
		t.Fatalf("invalid threshold falls back to default, got %d", got)
	}
}
