package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"mitmrouter/internal/settings"
	"mitmrouter/internal/store"

	_ "modernc.org/sqlite"
)

func TestDefaultRenamePersistenceFailureIsReportedAndRolledBack(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)
	id := createUpstreamViaAPI(t, f, cookie, "primary", "resin", "socks5://P:t@p:2260", nil)

	snap := f.holder.Current()
	snap.DefaultUpstream = "primary"
	f.holder.Set(snap)
	if err := settings.SaveSnapshot(context.Background(), f.st, snap); err != nil {
		t.Fatalf("save default upstream: %v", err)
	}

	db, err := sql.Open("sqlite", filepath.Join(f.dataDir, "router.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`CREATE TRIGGER block_default_upstream_update
		BEFORE UPDATE ON settings WHEN NEW.key = 'default_upstream'
		BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		db.Close()
		t.Fatalf("create trigger: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}

	rec := f.do(t, http.MethodPut, fmt.Sprintf("/api/upstreams/%d", id), cookie, map[string]any{
		"name": "renamed", "platform": "resin", "base_url": "socks5://P:t@p:2260", "enabled": true,
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("rename with persistence failure: HTTP %d %s, want 500", rec.Code, rec.Body.String())
	}

	rows, err := f.st.ListUpstreams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "primary" {
		t.Fatalf("upstream update must roll back, rows=%+v", rows)
	}
	if got := f.holder.Current().DefaultUpstream; got != "primary" {
		t.Fatalf("runtime default=%q, want primary", got)
	}
	settingsMap, err := f.st.AllSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := settingsMap["default_upstream"]; got != `"primary"` {
		t.Fatalf("persisted default=%q, want %q", got, `"primary"`)
	}
}

func TestUpstreamWithAccountBindingCannotChangePlatform(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)
	id := createUpstreamViaAPI(t, f, cookie, "bound", "plain", "http://proxy.example:8080", nil)
	if err := f.st.ReplaceAccountSnapshot(context.Background(), "openai", "account@example.com", "CLIProxyAPI", store.AcctUpsert{
		Platform: "openai", Account: "account@example.com", RtFP: "rt-fp",
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := f.st.ReplaceAccountBinding(context.Background(), "openai", "account@example.com", store.EgressModeSticky, []int64{id}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	rec := f.do(t, http.MethodPut, fmt.Sprintf("/api/upstreams/%d", id), cookie, map[string]any{
		"name": "bound", "platform": "resin", "base_url": "socks5://P:t@resin:2260", "enabled": true,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("platform change with binding: HTTP %d %s, want 409", rec.Code, rec.Body.String())
	}

	rows, err := f.st.ListUpstreams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Platform != "plain" {
		t.Fatalf("platform change must be rejected, rows=%+v", rows)
	}
	bindings, err := f.st.ListAcctEgressByAccount(context.Background(), "openai", "account@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].EgressID != id {
		t.Fatalf("binding changed unexpectedly: %+v", bindings)
	}
}

func TestUpstreamPlatformChangeKeepsUnboundEntryEditable(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)
	id := createUpstreamViaAPI(t, f, cookie, "unbound", "plain", "http://proxy.example:8080", nil)

	rec := f.do(t, http.MethodPut, fmt.Sprintf("/api/upstreams/%d", id), cookie, map[string]any{
		"name": "unbound", "platform": "resin", "base_url": "socks5://P:t@resin:2260", "enabled": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("unbound platform change: HTTP %d %s", rec.Code, rec.Body.String())
	}
	rows, err := f.st.ListUpstreams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Platform != "resin" {
		t.Fatalf("unbound platform change not applied: %+v", rows)
	}
}
