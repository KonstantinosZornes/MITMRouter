package syncer

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mitmrouter/internal/acctmap"
	"mitmrouter/internal/store"
)

// docs/012 v2：没有全局开关，也没有 api/direct 模式。增量 reader 的启停只由
// 「源启用 且 增量路径非空」决定：填了路径即启动，清空路径即停止。
func TestReconcileStartsStopsByIncrementalPath(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	dirA := t.TempDir()
	idA, err := st.CreateSyncSourceConfig(ctx, store.SyncSourceConfig{
		Kind: acctmap.SourceKindCLIProxyAPI, Name: "incr-on", DirectAuthDir: dirA, IntervalS: 600, Enabled: true,
	}, "key")
	if err != nil {
		t.Fatal(err)
	}
	idB, err := st.CreateSyncSourceConfig(ctx, store.SyncSourceConfig{
		Kind: acctmap.SourceKindCLIProxyAPI, Name: "incr-off", IntervalS: 600, Enabled: true,
	}, "key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSyncSourceConfig(ctx, store.SyncSourceConfig{
		Kind: acctmap.SourceKindCLIProxyAPI, Name: "incr-disabled", DirectAuthDir: t.TempDir(), IntervalS: 600, Enabled: false,
	}, "key"); err != nil {
		t.Fatal(err)
	}

	manager := New(st, acctmap.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		manager.stopAllDirectRuntimes()
		manager.lifecycleMu.Lock()
		manager.runCtx = nil
		manager.lifecycleMu.Unlock()
	}()
	manager.lifecycleMu.Lock()
	manager.runCtx = runCtx
	manager.lifecycleMu.Unlock()

	runtimeCount := func() int {
		manager.lifecycleMu.Lock()
		defer manager.lifecycleMu.Unlock()
		return len(manager.runtimes)
	}

	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if n := runtimeCount(); n != 1 {
		t.Fatalf("runtimes=%d, want only the enabled source with a path", n)
	}

	// 给未配置路径的源填上路径 → reader 启动
	rowB, ok, err := st.GetSyncSource(ctx, idB)
	if err != nil || !ok {
		t.Fatal(err)
	}
	rowB.DirectAuthDir = t.TempDir()
	if err := st.UpdateSyncSourceConfig(ctx, rowB, "", "", false, false); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReconcileSource(ctx, idB); err != nil {
		t.Fatal(err)
	}
	if n := runtimeCount(); n != 2 {
		t.Fatalf("runtimes=%d after filling path, want 2", n)
	}

	// 清掉路径 → reader 停止
	rowA, ok, err := st.GetSyncSource(ctx, idA)
	if err != nil || !ok {
		t.Fatal(err)
	}
	rowA.DirectAuthDir = ""
	if err := st.UpdateSyncSourceConfig(ctx, rowA, "", "", false, false); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReconcileSource(ctx, idA); err != nil {
		t.Fatal(err)
	}
	if n := runtimeCount(); n != 1 {
		t.Fatalf("runtimes=%d after clearing path, want 1", n)
	}
}

// 存量直读源可能缺 API 配置：全量没法跑，不发 HTTP，写 last_status 提示补全。
func TestSyncOneHintsMissingAPIConfigWithoutHTTP(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.CreateSyncSourceConfig(context.Background(), store.SyncSourceConfig{
		Kind: acctmap.SourceKindCLIProxyAPI, Name: "legacy-no-base-url", DirectAuthDir: t.TempDir(), Enabled: true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	m := New(st, acctmap.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.syncOne(context.Background(), id)
	if requests != 0 {
		t.Fatalf("source without base_url sent %d HTTP requests", requests)
	}
	row, ok, err := st.GetSyncSource(context.Background(), id)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if row.LastStatus == "" {
		t.Fatal("missing-api-config source must get a last_status hint")
	}
}

// 增量 reader 启动失败（如目录不存在）：last_status 置 error，但 last_sync_at
// 不动——reader 生命周期失败不是一次全量同步（docs/012 v2 §7）。
func TestReaderStartFailureKeepsLastSyncAt(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sourceID, err := st.CreateSyncSourceConfig(ctx, store.SyncSourceConfig{
		Kind: acctmap.SourceKindCLIProxyAPI, Name: "bad-dir", DirectAuthDir: "/nonexistent/auth-dir", IntervalS: 600, Enabled: true,
	}, "key")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.TouchSyncSource(ctx, sourceID, "ok: 1 accounts, 2 tokens"); err != nil {
		t.Fatal(err)
	}
	baseline, ok, err := st.GetSyncSource(ctx, sourceID)
	if err != nil || !ok {
		t.Fatal(err)
	}

	manager := New(st, acctmap.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	// reader 启动依赖 manager 运行上下文；这里只挂上下文不起调度循环
	manager.lifecycleMu.Lock()
	manager.runCtx = ctx
	manager.lifecycleMu.Unlock()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	row, ok, err := st.GetSyncSource(ctx, sourceID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if !strings.Contains(row.LastStatus, "error:") {
		t.Fatalf("reader start failure must set last_status, got %q", row.LastStatus)
	}
	if row.LastSyncAt != baseline.LastSyncAt {
		t.Fatalf("reader start failure bumped last_sync_at: %d -> %d", baseline.LastSyncAt, row.LastSyncAt)
	}
}

// 增量失败把 last_status 置 error，但 last_sync_at 不动——那两个字段只反映
// 全量同步（docs/012 v2 §7）。
func TestIncrementalFileFailureKeepsLastSyncAt(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	authDir := t.TempDir()
	sourceID, err := st.CreateSyncSourceConfig(ctx, store.SyncSourceConfig{
		Kind: acctmap.SourceKindCLIProxyAPI, Name: "incr-fail-status", DirectAuthDir: authDir, IntervalS: 600, Enabled: true,
	}, "key")
	if err != nil {
		t.Fatal(err)
	}
	// 模拟此前一次成功的全量同步，留下 last_sync_at 基线
	if err := st.TouchSyncSource(ctx, sourceID, "ok: 1 accounts, 2 tokens"); err != nil {
		t.Fatal(err)
	}
	baseline, ok, err := st.GetSyncSource(ctx, sourceID)
	if err != nil || !ok {
		t.Fatal(err)
	}

	manager := New(st, acctmap.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		manager.stopAllDirectRuntimes()
	}()
	manager.lifecycleMu.Lock()
	manager.runCtx = runCtx
	manager.lifecycleMu.Unlock()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	// 投一个 JSON 损坏的认证文件：增量读取失败
	path := filepath.Join(authDir, "broken.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex",`), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		row, ok, err := st.GetSyncSource(ctx, sourceID)
		if err != nil || !ok {
			t.Fatal(err)
		}
		if strings.Contains(row.LastStatus, "read failed") {
			if row.LastSyncAt != baseline.LastSyncAt {
				t.Fatalf("incremental failure bumped last_sync_at: %d -> %d", baseline.LastSyncAt, row.LastSyncAt)
			}
			cancel()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("broken auth file did not produce an incremental failure status")
}

// 增量成功不覆盖 last_sync_at/last_status：那两个字段只反映全量同步，
// 增量历史看更新记录页（docs/012 v2）。
func TestIncrementalFileSuccessLeavesLastStatusUntouched(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	authDir := t.TempDir()
	sourceID, err := st.CreateSyncSourceConfig(ctx, store.SyncSourceConfig{
		Kind: acctmap.SourceKindCLIProxyAPI, Name: "incr-quiet-status", DirectAuthDir: authDir, IntervalS: 600, Enabled: true,
	}, "key")
	if err != nil {
		t.Fatal(err)
	}

	manager := New(st, acctmap.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		manager.stopAllDirectRuntimes()
	}()
	manager.lifecycleMu.Lock()
	manager.runCtx = runCtx
	manager.lifecycleMu.Unlock()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(authDir, "account.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex","email":"quiet@example.com","access_token":"at-q","refresh_token":"rt-q"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, loadErr := st.LoadAcctMapAll(ctx)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		applied := false
		for _, row := range rows {
			if row.Source == acctmap.SourceInstancePrefix+strconv.FormatInt(sourceID, 10) && row.Account == "quiet@example.com" {
				applied = true
			}
		}
		if applied {
			row, _, err := st.GetSyncSource(ctx, sourceID)
			if err != nil {
				t.Fatal(err)
			}
			if row.LastSyncAt != 0 || row.LastStatus != "" {
				t.Fatalf("incremental success touched last_status: %q %d", row.LastStatus, row.LastSyncAt)
			}
			cancel()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("incremental file update was not applied")
}

// docs/012 v2 §3.5 验收：增量路径自身不发任何 HTTP——CPA 文件事件直读目录，
// 不碰源管理 API（全量同步是唯一发 HTTP 的路径，此测试不启动调度循环）。
func TestIncrementalPathIssuesNoHTTP(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	authDir := t.TempDir()
	sourceID, err := st.CreateSyncSourceConfig(ctx, store.SyncSourceConfig{
		Kind: acctmap.SourceKindCLIProxyAPI, Name: "incr-no-http", BaseURL: server.URL,
		DirectAuthDir: authDir, IntervalS: 600, Enabled: true,
	}, "key")
	if err != nil {
		t.Fatal(err)
	}

	manager := New(st, acctmap.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		manager.stopAllDirectRuntimes()
	}()
	manager.lifecycleMu.Lock()
	manager.runCtx = runCtx
	manager.lifecycleMu.Unlock()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(authDir, "no-http.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex","email":"no-http@example.com","access_token":"at","refresh_token":"rt"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, loadErr := st.LoadAcctMapAll(ctx)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		for _, row := range rows {
			if row.Source == acctmap.SourceInstancePrefix+strconv.FormatInt(sourceID, 10) && row.Account == "no-http@example.com" {
				if requests != 0 {
					t.Fatalf("incremental path sent %d HTTP requests", requests)
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("incremental file update was not applied")
}

// docs/012 v2 §3.5 验收：进程重启后按 sync_sources 持久化配置恢复增量 reader。
func TestRestartRestoresReaderFromStore(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateSyncSourceConfig(ctx, store.SyncSourceConfig{
		Kind: acctmap.SourceKindCLIProxyAPI, Name: "restart-restore", DirectAuthDir: t.TempDir(), IntervalS: 600, Enabled: true,
	}, "key"); err != nil {
		t.Fatal(err)
	}

	runtimeCount := func(m *Manager) int {
		m.lifecycleMu.Lock()
		defer m.lifecycleMu.Unlock()
		return len(m.runtimes)
	}

	// 第一段生命周期：reader 起来后整体关停（模拟进程退出）
	first := New(st, acctmap.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, cancel := context.WithCancel(context.Background())
	first.lifecycleMu.Lock()
	first.runCtx = runCtx
	first.lifecycleMu.Unlock()
	if err := first.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if n := runtimeCount(first); n != 1 {
		t.Fatalf("first run runtimes=%d, want 1", n)
	}
	cancel()
	first.stopAllDirectRuntimes()
	first.lifecycleMu.Lock()
	first.runCtx = nil
	first.lifecycleMu.Unlock()

	// 第二段生命周期：新 Manager 只靠 store 里的配置恢复
	second := New(st, acctmap.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx2, cancel2 := context.WithCancel(context.Background())
	defer func() {
		cancel2()
		second.stopAllDirectRuntimes()
	}()
	second.lifecycleMu.Lock()
	second.runCtx = runCtx2
	second.lifecycleMu.Unlock()
	if err := second.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if n := runtimeCount(second); n != 1 {
		t.Fatalf("runtimes after restart=%d, want reader restored from store", n)
	}
}
