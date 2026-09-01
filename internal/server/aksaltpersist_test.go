package server

// per-marker 盐值持久化：轮换落库、重启恢复的往返一致性测试。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"mitmrouter/internal/marker"
	"mitmrouter/internal/store"
)

var tlsHandshakeErr = errors.New("remote error: tls: handshake failure")

func TestMarkerSaltPersistRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "router.db")
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	s := newSaltTestServer()
	if err := s.AttachMarkerSaltStore(st); err != nil {
		t.Fatalf("attach: %v", err)
	}
	wctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.RunMarkerSaltWriter(wctx) }()

	const mk = "sk-persist-mk-0001"
	req := func() *http.Request {
		return httptest.NewRequest("GET", "http://t.example.com/v1/x", nil).
			Clone(context.WithValue(context.Background(), fwdMetaKey{},
				&fwdMeta{marker: mk, viaUpstream: true}))
	}
	s.rotateOnUnusable(req(), tlsHandshakeErr)
	s.rotateOnUnusable(req(), fmt.Errorf("wrapped: %w", tlsHandshakeErr))
	if got := s.markerSalts.Get(mk); got != 1 {
		t.Fatalf("two failures at default threshold should rotate to 1, got %d", got)
	}
	if fp := marker.Fingerprint(mk); len(fp) != 64 {
		t.Fatalf("fingerprint must be full sha256 hex, got %d chars", len(fp))
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("salt writer did not drain in time")
	}

	rows, err := st.LoadMarkerSalts(context.Background(), 100)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 1 || rows[0].FP != marker.Fingerprint(mk) || rows[0].Salt != 1 {
		t.Fatalf("persisted rows mismatch: %+v (want fp=%s salt=1)", rows, marker.Fingerprint(mk))
	}
	st.Close()

	// 重启路径：新进程、新 LRU，从库恢复后盐值保持
	st2, err := store.Open(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st2.Close()
	s2 := newSaltTestServer()
	if err := s2.AttachMarkerSaltStore(st2); err != nil {
		t.Fatalf("reattach: %v", err)
	}
	if got := s2.markerSalts.Get(mk); got != 1 {
		t.Fatalf("restored salt should survive restart, want 1 got %d", got)
	}
}
