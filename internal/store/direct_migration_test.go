package store

import (
	"context"
	"testing"

	"mitmrouter/internal/acctmap"
)

func TestEnsureSchemaAddsDirectSourceColumns(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if _, err := st.db.ExecContext(ctx, `ALTER TABLE sync_sources DROP COLUMN mode`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `ALTER TABLE sync_sources DROP COLUMN direct_auth_dir`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `ALTER TABLE sync_sources DROP COLUMN direct_db_secret`); err != nil {
		t.Fatal(err)
	}
	if err := st.ensureSchema(); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	id, err := st.CreateSyncSource(context.Background(), acctmap.SourceKindCLIProxyAPI, "legacy-source", "http://example.test", "key", 60, true)
	if err != nil {
		t.Fatal(err)
	}
	row, ok, err := st.GetSyncSource(ctx, id)
	if err != nil || !ok {
		t.Fatalf("get source: ok=%v err=%v", ok, err)
	}
	if row.BaseURL != "http://example.test" || row.DirectAuthDir != "" || row.DirectDBSecret != "" {
		t.Fatalf("migrated source=%+v", row)
	}
}
