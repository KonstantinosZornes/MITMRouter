package store

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestOpenCreatesUsableDB(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if _, err := st.AllSettings(context.Background()); err != nil {
		t.Errorf("query on fresh DB failed: %v", err)
	}
	if _, err := Open(dir); err != nil {
		t.Errorf("reopen must be idempotent: %v", err)
	}
}

func TestSQLiteDSNForWindowsKeepsNativePath(t *testing.T) {
	dbPath := `C:\Users\wangm\Downloads\data\router.db`
	got := sqliteDSNForOS(dbPath, "windows")
	want := dbPath + "?" + sqlitePragmas
	if got != want {
		t.Fatalf("Windows DSN=%q, want %q", got, want)
	}
}

// P1-4: SQLite file URI 中的 ?、#、% 是合法目录名字符，不能被误当作
// query、fragment 或 percent escape。
func TestOpenSupportsSpecialCharactersInDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data?hash#percent%dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create data dir: %v", err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open special-character path: %v", err)
	}
	defer st.Close()
	if _, err := os.Stat(filepath.Join(dir, "router.db")); err != nil {
		t.Fatalf("router.db was not created in requested directory: %v", err)
	}
}

func TestFreshSchemaComplete(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// 全部表就位（含 marker_salts）。
	for _, table := range []string{
		"settings", "upstreams", "access_logs", "secrets",
		"marker_salts", "acct_map", "sync_sources",
	} {
		var name string
		err := st.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing on fresh DB: %v", table, err)
		}
	}
	// 幂等：对已有库重复执行建表必须无副作用
	if err := st.ensureSchema(); err != nil {
		t.Errorf("re-run ensureSchema must be idempotent: %v", err)
	}
}

func TestEnsureSchemaAddsAuditAccountColumn(t *testing.T) {
	st := openTest(t)
	if _, err := st.db.Exec(`ALTER TABLE access_logs DROP COLUMN account`); err != nil {
		t.Fatal(err)
	}
	if err := st.ensureSchema(); err != nil {
		t.Fatal(err)
	}
	var found int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('access_logs') WHERE name='account'`).Scan(&found); err != nil {
		t.Fatal(err)
	}
	if found != 1 {
		t.Fatalf("account column count=%d, want 1", found)
	}
}

func TestEnsureSchemaAddsAuditReqIDColumn(t *testing.T) {
	st := openTest(t)
	if _, err := st.db.Exec(`ALTER TABLE access_logs DROP COLUMN req_id`); err != nil {
		t.Fatal(err)
	}
	if err := st.ensureSchema(); err != nil {
		t.Fatal(err)
	}
	var found int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('access_logs') WHERE name='req_id'`).Scan(&found); err != nil {
		t.Fatal(err)
	}
	if found != 1 {
		t.Fatalf("req_id column count=%d, want 1", found)
	}
}

func TestEnsureSchemaAddsNullableAuditTTFBColumn(t *testing.T) {
	st := openTest(t)
	if _, err := st.db.Exec(`INSERT INTO access_logs(ts,req_id,method,host,path,status,dur_ms,bytes_out,has_marker,account,account_fp,upstream)
		VALUES(1,'legacy-ttfb','GET','api.example','/',200,1,0,0,'','default','direct')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`ALTER TABLE access_logs DROP COLUMN ttfb_ms`); err != nil {
		t.Fatal(err)
	}
	if err := st.ensureSchema(); err != nil {
		t.Fatal(err)
	}
	var found int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('access_logs') WHERE name='ttfb_ms'`).Scan(&found); err != nil {
		t.Fatal(err)
	}
	if found != 1 {
		t.Fatalf("ttfb_ms column count=%d, want 1", found)
	}
	var ttfb any
	if err := st.db.QueryRow(`SELECT ttfb_ms FROM access_logs WHERE req_id='legacy-ttfb'`).Scan(&ttfb); err != nil {
		t.Fatal(err)
	}
	if ttfb != nil {
		t.Fatalf("historical ttfb_ms=%v, want NULL", ttfb)
	}
}

func TestEnsureSchemaAddsAndBackfillsAuditInternalErrorColumn(t *testing.T) {
	st := openTest(t)
	if _, err := st.db.Exec(`ALTER TABLE access_logs DROP COLUMN internal_error`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO access_logs(ts,req_id,method,host,path,status,dur_ms,bytes_out,has_marker,account,account_fp,upstream,err)
		VALUES
		(1,'legacy','GET','api.example','/',502,1,0,0,'','default','legacy','eof'),
		(2,'legacy-raw','GET','api.example','/',502,1,0,0,'','default','legacy','dial tcp: 10.0.0.1: refused')`); err != nil {
		t.Fatal(err)
	}
	if err := st.ensureSchema(); err != nil {
		t.Fatal(err)
	}
	var internalError string
	if err := st.db.QueryRow(`SELECT internal_error FROM access_logs WHERE req_id='legacy'`).Scan(&internalError); err != nil {
		t.Fatal(err)
	}
	if internalError != "eof" {
		t.Fatalf("backfilled internal_error = %q, want eof", internalError)
	}
	var status int
	if err := st.db.QueryRow(`SELECT status FROM access_logs WHERE req_id='legacy'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != 0 {
		t.Fatalf("backfilled internal-error status = %d, want 0", status)
	}
	var rawInternalError any
	if err := st.db.QueryRow(`SELECT internal_error FROM access_logs WHERE req_id='legacy-raw'`).Scan(&rawInternalError); err != nil {
		t.Fatal(err)
	}
	if rawInternalError != nil {
		t.Fatalf("raw legacy error was copied into internal_error: %v", rawInternalError)
	}
	if err := st.db.QueryRow(`SELECT status FROM access_logs WHERE req_id='legacy-raw'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusBadGateway {
		t.Fatalf("unclassified legacy status = %d, want %d", status, http.StatusBadGateway)
	}
}

func TestEnsureSchemaRemovesLegacyHeaderAuditData(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if _, err := st.db.Exec(`ALTER TABLE access_logs ADD COLUMN headers TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "audit_header_names", "true"); err != nil {
		t.Fatal(err)
	}

	if err := st.ensureSchema(); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	rows, err := st.db.Query(`PRAGMA table_info(access_logs)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "headers" {
			t.Fatal("legacy access_logs.headers column was not removed")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	settings, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := settings["audit_header_names"]; ok {
		t.Fatal("legacy audit_header_names setting was not removed")
	}
}

func TestBootstrapFreshInstall(t *testing.T) {
	dir := t.TempDir()
	st, info, err := Bootstrap(dir)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	defer st.Close()

	if !info.FreshInstall {
		t.Error("first bootstrap must report FreshInstall")
	}
	if info.AdminPassword == "" {
		t.Fatal("fresh install must generate an admin password")
	}
	hash, err := st.GetSecret(context.Background(), "admin_password_bcrypt")
	if err != nil {
		t.Fatalf("admin hash missing: %v", err)
	}
	if !CheckPassword(string(hash), info.AdminPassword) {
		t.Error("generated password must verify against stored bcrypt hash")
	}
	if len(hash) < 59 || !strings.HasPrefix(string(hash), "$2") {
		t.Error("stored secret must look like a bcrypt hash")
	}

	m, err := st.AllSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"listen_auth", "default_upstream", "no_marker_policy",
		"marker_path_parts", "marker_headers", "hash_salt", "sid_len",
		"session_ttl_min", "salt_rotate_failure_threshold", "block_private_targets", "acl_whitelist", "acl_blacklist",
		"log_retention_days",
	} {
		if _, ok := m[k]; !ok {
			t.Errorf("default setting %q missing", k)
		}
	}
	if got := m["hash_salt"]; len(got) != 66 { // 32 bytes hex = 64 chars + 2 JSON quotes
		t.Errorf("hash_salt=%q want 64 hex chars", got)
	}
	if m["sid_len"] != "16" {
		t.Errorf("sid_len=%q want \"16\"", m["sid_len"])
	}
	if m["no_marker_policy"] != "\"default_session\"" {
		t.Errorf("no_marker_policy=%q", m["no_marker_policy"])
	}
	if m["block_private_targets"] != "true" {
		t.Errorf("block_private_targets=%q, want true", m["block_private_targets"])
	}
	if _, ok := m["private_target_direct"]; ok {
		t.Error("obsolete private_target_direct must not be seeded")
	}
	if _, ok := m["session_hmac_key"]; ok {
		t.Error("session_hmac_key is a secret, not a setting")
	}
	if _, err := st.GetSecret(context.Background(), "session_hmac_key"); err != nil {
		t.Errorf("session_hmac_key secret missing: %v", err)
	}
}

func TestBootstrapRemovesObsoletePrivateTargetDirect(t *testing.T) {
	dir := t.TempDir()
	st, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(context.Background(), "private_target_direct", "true"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, _, err = Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	settings, err := st.AllSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := settings["private_target_direct"]; exists {
		t.Fatal("bootstrap must remove obsolete private_target_direct")
	}
	if settings["block_private_targets"] != "true" {
		t.Fatalf("block_private_targets=%q, want true", settings["block_private_targets"])
	}
}

func TestBootstrapReopenPreservesState(t *testing.T) {
	dir := t.TempDir()
	st, _, err := Bootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	oldSalt, _ := st.GetSecret(context.Background(), "session_hmac_key")
	st.Close()

	st2, info2, err := Bootstrap(dir)
	if err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	defer st2.Close()

	if info2.FreshInstall {
		t.Error("reopen must not be FreshInstall")
	}
	if info2.AdminPassword != "" {
		t.Error("existing admin password must not regenerate/print again")
	}
	newSalt, _ := st2.GetSecret(context.Background(), "session_hmac_key")
	if string(oldSalt) != string(newSalt) {
		t.Error("ensureSecret must not overwrite existing key material")
	}
	m, _ := st2.AllSettings(context.Background())
	if _, ok := m["listen_addr"]; ok {
		t.Errorf("listen_addr must not be persisted anymore, got %q", m["listen_addr"])
	}
}

func TestSettingsCRUD(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)

	if err := st.SetSetting(ctx, "k1", `"v1"`); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "k1", `"v2"`); err != nil {
		t.Fatal(err)
	}
	m, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m["k1"] != `"v2"` {
		t.Errorf("upsert failed: %q", m["k1"])
	}
	if err := st.DeleteSetting(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	m, _ = st.AllSettings(ctx)
	if _, ok := m["k1"]; ok {
		t.Error("DeleteSetting did not delete")
	}
}

func TestSetSettingsTxAtomicUpsert(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	st.SetSetting(ctx, "a", `"old"`)

	err := st.SetSettingsTx(ctx, map[string]string{"a": `"new"`, "b": `"created"`})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := st.AllSettings(ctx)
	if m["a"] != `"new"` || m["b"] != `"created"` {
		t.Fatalf("tx upsert result: %v", m)
	}
}

func TestSecrets(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)

	if _, err := st.GetSecret(ctx, "nope"); err != ErrNotFound {
		t.Errorf("missing secret must return ErrNotFound, got %v", err)
	}
	if err := st.SetSecret(ctx, "sk", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	b, err := st.GetSecret(ctx, "sk")
	if err != nil || string(b) != "v1" {
		t.Fatalf("GetSecret=%q,%v", b, err)
	}
	if err := st.SetSecret(ctx, "sk", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	b, _ = st.GetSecret(ctx, "sk")
	if string(b) != "v2" {
		t.Errorf("secret overwrite failed: %q", b)
	}
}

func TestPasswordHashAndCheck(t *testing.T) {
	h, err := HashPassword("s3cret-pw")
	if err != nil {
		t.Fatal(err)
	}
	if CheckPassword(h, "wrong") {
		t.Error("wrong password must not verify")
	}
	if !CheckPassword(h, "s3cret-pw") {
		t.Error("correct password must verify")
	}
	if h2, _ := HashPassword("s3cret-pw"); h == h2 {
		t.Error("bcrypt must salt, identical hashes are suspicious")
	}
}

func TestUpstreamsCRUDAndUniqueName(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)

	id1, err := st.CreateUpstream(ctx, "homeus", "resin", "socks5://Default:tok@r:2260", "", true)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := st.CreateUpstream(ctx, "gen", "generic", "http://u:p@g:1", `{"username_template":"{user}-{sid}"}`, false)
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Fatal("ids must differ")
	}

	if _, err := st.CreateUpstream(ctx, "homeus", "resin", "socks5://x@y:1", "", true); err == nil {
		t.Fatal("duplicate name must violate UNIQUE constraint")
	} else if !strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
		t.Errorf("error must mention UNIQUE, got %v", err)
	}

	rows, err := st.ListUpstreams(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows)=%d want 2", len(rows))
	}
	r0 := rows[0]
	if r0.ID != id1 || r0.Name != "homeus" || r0.Platform != "resin" ||
		r0.BaseURL != "socks5://Default:tok@r:2260" || !r0.Enabled {
		t.Errorf("row0 mismatch: %+v", r0)
	}
	if r0.Inject.Valid {
		t.Error("empty inject must be NULL")
	}
	if rows[1].Inject.String != `{"username_template":"{user}-{sid}"}` || rows[1].Enabled {
		t.Errorf("row1 mismatch: %+v", rows[1])
	}
	n, _ := st.CountUpstreams(ctx)
	if n != 2 {
		t.Errorf("CountUpstreams=%d want 2", n)
	}

	r0.Enabled = false
	r0.BaseURL = "http://moved:8080"
	if err := st.UpdateUpstream(ctx, r0); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.ListUpstreams(ctx)
	if rows[0].Enabled || rows[0].BaseURL != "http://moved:8080" {
		t.Errorf("update failed: %+v", rows[0])
	}
	if rows[0].CreatedAt != r0.CreatedAt {
		t.Error("update must preserve created_at")
	}

	if err := st.DeleteUpstream(ctx, id1); err != nil {
		t.Fatal(err)
	}
	n, _ = st.CountUpstreams(ctx)
	if n != 1 {
		t.Errorf("after delete CountUpstreams=%d want 1", n)
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in                     string
		maxBytes, maxRunes     int
		wantLenBytes, wantLenR int
	}{
		{"short", 10, 10, 5, 5},
		{strings.Repeat("a", 100), 10, 100, 10, 10},
		{"日本語テキスト", 7, 50, 6, 2}, // 撕裂的字节边界被丢弃而非产出 U+FFFD
		{strings.Repeat("日", 300), 4096, 3, 9, 3},
	}
	for i, c := range cases {
		got := truncate(c.in, c.maxBytes, c.maxRunes)
		if len(got) != c.wantLenBytes || len([]rune(got)) != c.wantLenR {
			t.Errorf("case %d: truncate(%d bytes,%d runes)=len(%d),runes(%d)", i, c.maxBytes, c.maxRunes, len(got), len([]rune(got)))
		}
		if strings.ContainsRune(got, '�') {
			t.Errorf("case %d: must not produce U+FFFD", i)
		}
	}
}
