package api

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"testing"

	"mitmrouter/internal/certca"
	"mitmrouter/internal/metrics"
	"mitmrouter/internal/settings"
	"mitmrouter/internal/store"
	"mitmrouter/internal/upstream"

	_ "modernc.org/sqlite"
)

// 设置的任一运维项写失败时，PUT 必须失败，不能返回成功并换入半套状态。
func TestPutSettingsReportsPerKeyPersistenceFailure(t *testing.T) {
	dir := t.TempDir()
	st, info, err := store.Bootstrap(dir)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	defer st.Close()

	db, err := sql.Open("sqlite", filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER block_metrics_insert
		BEFORE INSERT ON settings WHEN NEW.key = 'metrics_enabled'
		BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatalf("create insert trigger: %v", err)
	}
	if _, err := db.Exec(`CREATE TRIGGER block_metrics_update
		BEFORE UPDATE ON settings WHEN NEW.key = 'metrics_enabled'
		BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatalf("create update trigger: %v", err)
	}

	snap, err := settings.LoadFromStore(context.Background(), st, settings.DefaultSnapshot())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	ca, err := certca.Ensure(context.Background(), st)
	if err != nil {
		t.Fatalf("ensure ca: %v", err)
	}
	f := &fixture{st: st, holder: settings.NewHolder(snap), password: info.AdminPassword}
	f.api = New(Deps{
		Store: st, Settings: f.holder, CA: ca, Metrics: metrics.NewRegistry(),
		IngressPort: "55666", SwapUpstreams: func(*upstream.Table) {},
	})
	f.h = f.api.Router()
	cookie := f.login(t)

	rec := f.do(t, http.MethodPut, "/api/settings", cookie, validDTO())
	if rec.Code == http.StatusOK {
		t.Fatalf("settings PUT hid a per-key persistence failure: HTTP %d", rec.Code)
	}
}
