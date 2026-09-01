package settings

import (
	"context"
	"reflect"
	"testing"

	"mitmrouter/internal/store"
)

func TestDefaultSnapshot(t *testing.T) {
	s := DefaultSnapshot()
	if s.ListenAuth != "" {
		t.Errorf("listen fields: %+v", s)
	}
	if s.NoMarkerPolicy != PolicyDefaultSession {
		t.Errorf("policy=%q", s.NoMarkerPolicy)
	}
	if len(s.MarkerRules.PathParts) != 0 {
		t.Errorf("path parts default must be empty (all URLs), got %v", s.MarkerRules.PathParts)
	}
	wantHeaders := []string{"Authorization", "x-api-key", "api-key", "x-goog-api-key"}
	if !reflect.DeepEqual(s.MarkerRules.Headers, wantHeaders) {
		t.Errorf("marker headers: %+v", s.MarkerRules.Headers)
	}
	if s.SIDLen != 16 || s.SaltRotateFailureThreshold != 2 || !s.BlockPrivateTargets {
		t.Errorf("defaults: sid=%d rotate_threshold=%d block_private_targets=%v", s.SIDLen, s.SaltRotateFailureThreshold, s.BlockPrivateTargets)
	}
}

func TestHolderAtomicSwapRecompilesACL(t *testing.T) {
	h := NewHolder(DefaultSnapshot())

	s := h.Current()
	s.ACLWhitelist = []string{"api.openai.com"}
	h.Set(s)
	cur := h.Current()
	if !cur.ACLAllowed("api.openai.com") || cur.ACLAllowed("evil.com") || !cur.ACLIntercept("api.openai.com") || cur.ACLIntercept("evil.com") {
		t.Error("whitelist must restrict access to listed targets")
	}

	s = h.Current()
	s.ACLBlacklist = []string{"10.0.0.0/8"}
	h.Set(s)
	cur = h.Current()
	if cur.ACLAllowed("10.1.2.3") || cur.ACLIntercept("10.1.2.3") {
		t.Error("blacklist entry must be rejected")
	}

	var zero Snapshot
	if !zero.ACLIntercept("anything.example") {
		t.Error("uncompiled snapshot must default to intercept-everything")
	}
}

func TestHolderInvalidWhitelistFailsClosed(t *testing.T) {
	h := NewHolder(DefaultSnapshot())
	snap := h.Current()
	snap.ACLWhitelist = []string{"not a valid target"}
	h.Set(snap)

	cur := h.Current()
	if cur.ACLAllowed("api.openai.com") || cur.ACLIntercept("api.openai.com") {
		t.Fatal("invalid configured whitelist must not fall back to allow-all")
	}
}

func TestHolderCurrentBeforeSet(t *testing.T) {
	h := &Holder{}
	got := h.Current()
	if got.Salt != "" || got.SIDLen != 0 || len(got.MarkerRules.PathParts) != 0 {
		t.Errorf("empty holder must return zero snapshot, got %+v", got)
	}
}

func TestLoadFromStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	snap := DefaultSnapshot()
	snap.Salt = "abcdef"
	snap.ListenAuth = "u:p"
	snap.SessionTTLMin = 45
	snap.SaltRotateFailureThreshold = 7
	snap.ACLWhitelist = []string{"*.example.com"}
	snap.ACLBlacklist = []string{"bad.example.com"}
	if err := SaveSnapshot(ctx, st, snap); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	got, err := LoadFromStore(ctx, st, DefaultSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	got.acl = nil // 非序列化编译字段不参与比对
	want := snap
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestLoadFromStoreClampsAndDefaults(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	set := func(k, v string) {
		if err := st.SetSetting(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	set("sid_len", "2")
	set("salt_rotate_failure_threshold", "0")
	set("no_marker_policy", `"bogus"`)
	set("hash_salt", `"s"`)
	fb := DefaultSnapshot()
	fb.SIDLen = 99
	got, err := LoadFromStore(ctx, st, fb)
	if err != nil {
		t.Fatal(err)
	}
	if got.SIDLen != 4 {
		t.Errorf("sid_len below floor must clamp to 4, got %d", got.SIDLen)
	}
	if got.SaltRotateFailureThreshold != 2 {
		t.Errorf("zero threshold must default to 2, got %d", got.SaltRotateFailureThreshold)
	}
	if got.NoMarkerPolicy != PolicyDefaultSession {
		t.Errorf("unknown policy must fall back to %q, got %q", PolicyDefaultSession, got.NoMarkerPolicy)
	}
	if got.Salt != "s" {
		t.Errorf("salt from store: %q", got.Salt)
	}

	set("sid_len", "500")
	got, _ = LoadFromStore(ctx, st, fb)
	if got.SIDLen != 64 {
		t.Errorf("sid_len above ceiling must clamp to 64, got %d", got.SIDLen)
	}
}

func TestLoadFromStoreIgnoresObsoletePrivateTargetDirect(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSetting(ctx, "private_target_direct", "true"); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFromStore(ctx, st, DefaultSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if !got.BlockPrivateTargets {
		t.Fatal("obsolete private_target_direct must not disable private-target blocking")
	}
}

func TestLoadFromStoreFallsBackWhenKeysMissing(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fb := DefaultSnapshot()
	fb.Salt = "fallback-salt"
	got, err := LoadFromStore(ctx, st, fb)
	if err != nil {
		t.Fatal(err)
	}
	if got.Salt != "fallback-salt" {
		t.Errorf("missing keys must inherit fallback: salt=%q", got.Salt)
	}
}

func TestSaveSnapshotOverwrites(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	a := DefaultSnapshot()
	a.Salt = "first"
	if err := SaveSnapshot(ctx, st, a); err != nil {
		t.Fatal(err)
	}
	b := DefaultSnapshot()
	b.Salt = "second"
	if err := SaveSnapshot(ctx, st, b); err != nil {
		t.Fatal(err)
	}
	got, _ := LoadFromStore(ctx, st, a)
	if got.Salt != "second" {
		t.Errorf("save must overwrite previous values: %+v", got)
	}
	m, _ := st.AllSettings(ctx)
	if _, ok := m["metrics_enabled"]; ok {
		t.Error("SaveSnapshot must not touch ops-only settings keys")
	}
}

// 旧库遗留的 listen_addr/admin_addr 键不再被读取（地址已改由启动参数指定），
// 加载时不得报错；TLS 路径键缺失时为空 = 不启用。
func TestLoadFromStoreLegacyAddrKeysIgnored(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 模拟旧库：直接写入历史键
	for k, v := range map[string]string{"listen_addr": `"127.0.0.1:8080"`, "admin_addr": `"127.0.0.1:55667"`} {
		if err := st.SetSetting(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	got, err := LoadFromStore(ctx, st, DefaultSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{got.ListenTLSCert, got.ListenTLSKey, got.AdminTLSCert, got.AdminTLSKey} {
		if p != "" {
			t.Errorf("missing tls keys must default to empty path, got %q", p)
		}
	}
}

// 新键往返：保存后重载必须原样恢复（四个 TLS 路径）。
func TestSaveLoadRoundTripTLSKeys(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	snap := DefaultSnapshot()
	snap.ListenTLSCert = "/etc/letsencrypt/live/dm/fullchain.pem"
	snap.ListenTLSKey = "/etc/letsencrypt/live/dm/privkey.pem"
	snap.AdminTLSCert = "/etc/letsencrypt/live/dm/cert.pem"
	snap.AdminTLSKey = "/etc/letsencrypt/live/dm/key.pem"
	if err := SaveSnapshot(ctx, st, snap); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFromStore(ctx, st, DefaultSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if got.ListenTLSCert != snap.ListenTLSCert || got.ListenTLSKey != snap.ListenTLSKey ||
		got.AdminTLSCert != snap.AdminTLSCert || got.AdminTLSKey != snap.AdminTLSKey {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, snap)
	}
}
