package syncer

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"mitmrouter/internal/acctmap"
	"mitmrouter/internal/store"
)

func TestManagerRunsCPADirectSourceAndStops(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	authDir := t.TempDir()
	sourceID, err := st.CreateSyncSourceConfig(ctx, store.SyncSourceConfig{
		Kind: acctmap.SourceKindCLIProxyAPI, Name: "direct-cpa-runtime", DirectAuthDir: authDir, IntervalS: 600, Enabled: true,
	}, "key")
	if err != nil {
		t.Fatal(err)
	}

	manager := New(st, acctmap.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		manager.Run(runCtx)
		close(done)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		manager.lifecycleMu.Lock()
		runtimeCount := len(manager.runtimes)
		manager.lifecycleMu.Unlock()
		if runtimeCount == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	manager.lifecycleMu.Lock()
	runtimeCount := len(manager.runtimes)
	manager.lifecycleMu.Unlock()
	if runtimeCount != 1 {
		t.Fatal("direct runtime did not start")
	}
	path := filepath.Join(authDir, "account.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex","email":"direct@example.com","access_token":"at-direct","refresh_token":"rt-direct"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, loadErr := st.LoadAcctMapAll(ctx)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		for _, row := range rows {
			if row.Source == acctmap.SourceInstancePrefix+strconv.FormatInt(sourceID, 10) && row.Account == "direct@example.com" && row.RtFP != "" {
				cancel()
				select {
				case <-done:
					return
				case <-time.After(2 * time.Second):
					t.Fatal("manager did not stop after context cancellation")
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatalf("direct source %d did not apply file update", sourceID)
}

// 回归测试：ensureDirectRuntime 去重分支拿到的是从未 run 过的 runtime，
// 清理不得等待只有 run 才会关闭的 done——对未启动 runtime 调 stop() 会永久阻塞。
func TestDirectRuntimeCloseUnstartedDoesNotBlock(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	manager := New(st, acctmap.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	runtime, err := manager.newDirectRuntime(ctx, store.SyncSourceRow{
		ID: 1, Kind: acctmap.SourceKindCLIProxyAPI, Name: "unstarted",
		DirectAuthDir: t.TempDir(), Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	finished := make(chan struct{})
	go func() {
		runtime.closeUnstarted()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("closeUnstarted blocked on a runtime that was never started")
	}
	select {
	case <-runtime.cpaDir.done:
	default:
		t.Fatal("cpa watcher was not released")
	}
}
