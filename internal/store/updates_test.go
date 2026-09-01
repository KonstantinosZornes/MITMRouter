package store

import (
	"context"
	"testing"
	"time"
)

func TestUpdateEventsListFilterClear(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UnixMilli()
	events := []UpdateEvent{
		{Ts: now, Kind: "direct_file", Source: "src:1", Status: "ok", Summary: "a.json → openai/x@e.com", Detail: "/auth/a.json"},
		{Ts: now - time.Minute.Milliseconds(), Kind: "api_sync", Source: "src:2", Status: "error", Summary: "upstream HTTP 502"},
		{Ts: now - 2*time.Minute.Milliseconds(), Kind: "push", Source: "api", Status: "ok", Summary: "openai/y@e.com"},
		{Ts: now - 3*time.Minute.Milliseconds(), Kind: "direct_incremental", Source: "src:1", Status: "ok", Summary: "applied 3 accounts"},
	}
	if err := st.insertUpdateEvents(ctx, events); err != nil {
		t.Fatal(err)
	}

	items, total, err := st.ListUpdateEvents(ctx, UpdateFilter{})
	if err != nil || total != 4 || len(items) != 4 {
		t.Fatalf("list all: total=%d len=%d err=%v", total, len(items), err)
	}
	if items[0].Kind != "direct_incremental" || items[0].Summary != "applied 3 accounts" {
		t.Fatalf("order: got %s %q", items[0].Kind, items[0].Summary)
	}
	if items[1].Summary != "openai/y@e.com" {
		t.Fatalf("summary roundtrip: %q", items[1].Summary)
	}
	if items[3].Kind != "direct_file" || items[3].Detail != "/auth/a.json" {
		t.Fatalf("detail roundtrip: %+v", items[3])
	}

	if items, total, _ = st.ListUpdateEvents(ctx, UpdateFilter{Kind: "direct_file"}); total != 1 || len(items) != 1 {
		t.Fatalf("filter kind: total=%d len=%d", total, len(items))
	}
	if items, total, _ = st.ListUpdateEvents(ctx, UpdateFilter{Kind: "direct_incremental"}); total != 1 || items[0].Summary != "applied 3 accounts" {
		t.Fatalf("filter long kind (truncation edge): total=%d items=%v", total, items)
	}
	if items, total, _ = st.ListUpdateEvents(ctx, UpdateFilter{Status: "error"}); total != 1 || items[0].Kind != "api_sync" {
		t.Fatalf("filter status: total=%d items=%v", total, items)
	}
	if _, total, _ = st.ListUpdateEvents(ctx, UpdateFilter{Source: "src:2"}); total != 1 {
		t.Fatalf("filter source: total=%d", total)
	}
	if _, total, _ = st.ListUpdateEvents(ctx, UpdateFilter{FromMs: now - 90_000, ToMs: now - 30_000}); total != 1 {
		t.Fatalf("filter window: total=%d", total)
	}

	page1, total, _ := st.ListUpdateEvents(ctx, UpdateFilter{Page: 1, PageSize: 2})
	page2, _, _ := st.ListUpdateEvents(ctx, UpdateFilter{Page: 2, PageSize: 2})
	if total != 4 || len(page1) != 2 || len(page2) != 2 {
		t.Fatalf("paging: total=%d page1=%d page2=%d", total, len(page1), len(page2))
	}
	if page1[0].ID == page2[0].ID {
		t.Fatal("paging overlap")
	}

	if err := st.ClearUpdateEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if _, total, _ = st.ListUpdateEvents(ctx, UpdateFilter{}); total != 0 {
		t.Fatalf("after clear: total=%d", total)
	}
}

// 更新记录与访问审计同保留期（docs/013 §9）：RunRetention 必须一并清理 sync_events。
func TestSyncEventsRetentionCleanup(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSetting(ctx, "log_retention_days", `1`); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UnixMilli()
	old := now - 3*86400_000
	if err := st.insertUpdateEvents(ctx, []UpdateEvent{
		{Ts: old, Kind: UpdateKindAPISync, Source: "src:1", Status: UpdateStatusOK, Summary: "expired"},
		{Ts: now - 1000, Kind: UpdateKindAPISync, Source: "src:1", Status: UpdateStatusOK, Summary: "fresh"},
	}); err != nil {
		t.Fatal(err)
	}

	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { st.RunRetention(runCtx); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, total, _ := st.ListUpdateEvents(ctx, UpdateFilter{}); total == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	<-done

	items, total, err := st.ListUpdateEvents(ctx, UpdateFilter{})
	if err != nil || total != 1 {
		t.Fatalf("retention must delete expired sync_events, remaining=%d err=%v", total, err)
	}
	if items[0].Summary != "fresh" {
		t.Fatalf("wrong row kept: %+v", items[0])
	}
}

func TestSendUpdateEventNilSafeAndDropsWhenFull(t *testing.T) {
	SendUpdateEvent(nil, UpdateEvent{}) // 不 panic
	ch := make(chan UpdateEvent, 1)
	SendUpdateEvent(ch, UpdateEvent{ID: 1})
	SendUpdateEvent(ch, UpdateEvent{ID: 2}) // 满则丢弃，不阻塞
	if len(ch) != 1 {
		t.Fatalf("len=%d", len(ch))
	}
}

func TestUpdateEventWriterDrainsOnCancel(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	wctx, cancel := context.WithCancel(ctx)
	cancel() // 立刻取消：writer 必须仍把缓冲里的事件排空落库
	ch := make(chan UpdateEvent, 1)
	ch <- UpdateEvent{Ts: time.Now().UnixMilli(), Kind: "api_sync", Source: "src:9", Status: "ok", Summary: "drain me"}
	done := make(chan struct{})
	go func() { st.RunUpdateEventWriter(wctx, ch); close(done) }()

	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("writer did not exit after ctx cancel")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, total, err := st.ListUpdateEvents(ctx, UpdateFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if total == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("event not persisted after drain: total=%d", total)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
