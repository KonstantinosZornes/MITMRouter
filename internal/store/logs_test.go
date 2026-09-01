package store

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"
)

func mkEntry(ts int64, method, host, path string, status int, hasMarker bool, acct, upstream, internalError string) LogEntry {
	return LogEntry{
		Ts: ts, ReqID: "reqid-test-" + method, Method: method, Host: host, Path: path, Status: status,
		DurMS: 12, BytesOut: 345, HasMarker: hasMarker, AccountFP: acct, Upstream: upstream, InternalError: internalError,
	}
}

func seedLogs(t *testing.T, st *Store, entries []LogEntry) {
	t.Helper()
	if err := st.insertLogs(context.Background(), entries); err != nil {
		t.Fatalf("insertLogs: %v", err)
	}
}

func sampleEntries(now int64) []LogEntry {
	return []LogEntry{
		mkEntry(now-1000, "POST", "api.openai.com", "/v1/chat/completions", 200, true, "aaaa1111aaaa1111", "homeus", ""),
		mkEntry(now-900, "POST", "api.openai.com", "/v1/embeddings", 401, true, "bbbb2222bbbb2222", "homeus", ""),
		// 目标站或上游真实返回的 5xx 保持为 HTTP 状态。
		mkEntry(now-800, "GET", "gw.dataimpulse.com", "/status", 502, false, "default", "direct", ""),
		// MITMRouter 自身产生的传输失败使用状态 0 加安全分类。
		mkEntry(now-700, "POST", "100%.com", "/a_b/c", 0, false, "-", "blind", "dial"),
		mkEntry(now-600, "POST", "api.anthropic.com", "/v1/messages", 204, true, "cccc3333cccc3333", "gen", ""),
		mkEntry(now+600000, "POST", "future.host", "/x", 200, false, "default", "homeus", ""),
	}
}

func TestListLogsNoFilter(t *testing.T) {
	st := openTest(t)
	now := time.Now().UnixMilli()
	seedLogs(t, st, sampleEntries(now))

	items, total, err := st.ListLogs(context.Background(), LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 6 || len(items) != 6 {
		t.Fatalf("total=%d len=%d want 6/6", total, len(items))
	}
	if items[0].ID <= items[1].ID {
		t.Error("results must be ordered by id DESC")
	}
	if items[0].Host != "future.host" {
		t.Errorf("newest row must come first: %+v", items[0])
	}
	zero := items[2]
	if zero.HasMarker || zero.Status != 0 || zero.AccountFP != "-" {
		t.Errorf("bool/null scan mismatch: %+v", zero)
	}
	if zero.InternalError != "dial" {
		t.Errorf("internal error = %q, want dial", zero.InternalError)
	}
	if items[0].ReqID != "reqid-test-POST" {
		t.Errorf("req_id did not round-trip: %q", items[0].ReqID)
	}
}

func TestAuditTTFBRoundTripPreservesUnknownAndZero(t *testing.T) {
	st := openTest(t)
	now := time.Now().UnixMilli()
	zero, measured := int64(0), int64(27)
	entries := []LogEntry{
		mkEntry(now, "GET", "unknown.example", "/", 200, false, "default", "direct", ""),
		mkEntry(now+1, "GET", "zero.example", "/", 200, false, "default", "direct", ""),
		mkEntry(now+2, "GET", "measured.example", "/", 200, false, "default", "direct", ""),
	}
	entries[1].TTFBMS = &zero
	entries[2].TTFBMS = &measured
	seedLogs(t, st, entries)

	items, total, err := st.ListLogs(context.Background(), LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(items) != 3 {
		t.Fatalf("total=%d len=%d, want 3/3", total, len(items))
	}
	if items[0].TTFBMS == nil || *items[0].TTFBMS != measured {
		t.Fatalf("measured TTFB=%v, want %d", items[0].TTFBMS, measured)
	}
	if items[1].TTFBMS == nil || *items[1].TTFBMS != zero {
		t.Fatalf("zero TTFB=%v, want pointer to 0", items[1].TTFBMS)
	}
	if items[2].TTFBMS != nil {
		t.Fatalf("unknown TTFB=%v, want nil", items[2].TTFBMS)
	}
}

func TestListLogsSubstringQueryWithEscaping(t *testing.T) {
	st := openTest(t)
	now := time.Now().UnixMilli()
	seedLogs(t, st, sampleEntries(now))

	_, n, err := st.ListLogs(context.Background(), LogFilter{Q: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("q=openai matched %d want 2 (host or path)", n)
	}

	// % 与 _ 是 LIKE 通配符，必须按字面量匹配
	_, n, _ = st.ListLogs(context.Background(), LogFilter{Q: "100%"})
	if n != 1 {
		t.Errorf("q=100%% matched %d want exactly 1", n)
	}
	_, n, _ = st.ListLogs(context.Background(), LogFilter{Q: "a_b"})
	if n != 1 {
		t.Errorf("q=a_b must match literally (underscore not wildcard), got %d", n)
	}
}

func TestListLogsStructuredFilters(t *testing.T) {
	st := openTest(t)
	now := time.Now().UnixMilli()
	seedLogs(t, st, sampleEntries(now))
	ctx := context.Background()

	cases := []struct {
		name string
		f    LogFilter
		want int64
	}{
		{"account exact", LogFilter{Account: "aaaa1111aaaa1111"}, 1},
		{"account no hit", LogFilter{Account: "zzzz9999zzzz9999"}, 0},
		{"upstream exact", LogFilter{Upstream: "direct"}, 1},
		{"class 2xx", LogFilter{StatusClass: "2xx"}, 3},
		{"class 4xx", LogFilter{StatusClass: "4xx"}, 1},
		{"class 5xx", LogFilter{StatusClass: "5xx"}, 1},
		{"class err includes internal error", LogFilter{StatusClass: "err"}, 1},
		{"from excludes older", LogFilter{FromMs: now - 650}, 2},
		{"to excludes newer", LogFilter{ToMs: now - 550}, 5},
		{"range window", LogFilter{FromMs: now - 950, ToMs: now - 750}, 2},
		{"combined", LogFilter{Upstream: "homeus", StatusClass: "2xx"}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, got, err := st.ListLogs(ctx, c.f)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("total=%d want %d", got, c.want)
			}
		})
	}
}

func TestListLogsPagination(t *testing.T) {
	st := openTest(t)
	var entries []LogEntry
	base := time.Now().UnixMilli()
	for i := 0; i < 7; i++ {
		entries = append(entries, mkEntry(base-int64(i), "GET", fmt.Sprintf("h%d.t", i), "/", 200, false, "default", "u", ""))
	}
	seedLogs(t, st, entries)

	items, total, err := st.ListLogs(context.Background(), LogFilter{Page: 2, PageSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	if total != 7 {
		t.Fatalf("total=%d want 7", total)
	}
	if len(items) != 3 {
		t.Fatalf("page2 len=%d want 3", len(items))
	}
	for i := 0; i < 2; i++ {
		if items[i].ID <= items[i+1].ID {
			t.Error("pagination must keep id DESC order")
		}
	}
	_, total, _ = st.ListLogs(context.Background(), LogFilter{Page: 10, PageSize: 3})
	if total != 7 {
		t.Error("total must be global, not per-page")
	}
	outOfRange, _, _ := st.ListLogs(context.Background(), LogFilter{Page: 99, PageSize: 3})
	if len(outOfRange) != 0 {
		t.Errorf("out-of-range page must be empty, got %d", len(outOfRange))
	}
	big, _, _ := st.ListLogs(context.Background(), LogFilter{PageSize: 500})
	if len(big) != 7 { // 全部 7 条，且未越界（clamp 后仍有效）
		t.Errorf("clamped pagesize query returned %d", len(big))
	}
}

// P2-4: 审计分页的极大页码不能让 SQL OFFSET 计算溢出。
func TestListLogsHandlesHugePage(t *testing.T) {
	st := openTest(t)
	seedLogs(t, st, []LogEntry{mkEntry(time.Now().UnixMilli(), "GET", "example.test", "/", 200, false, "default", "direct", "")})

	items, total, err := st.ListLogs(context.Background(), LogFilter{Page: math.MaxInt, PageSize: 200})
	if err != nil {
		t.Fatalf("huge page query: %v", err)
	}
	if total != 1 || len(items) != 0 {
		t.Fatalf("huge page result items=%d total=%d, want empty items and total=1", len(items), total)
	}
}

func TestClearLogsAndDeleteOlderThan(t *testing.T) {
	st := openTest(t)
	now := time.Now().UnixMilli()
	seedLogs(t, st, sampleEntries(now))

	n, err := st.DeleteOlderThan(context.Background(), now-650)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("deleted=%d want 4", n)
	}
	_, total, _ := st.ListLogs(context.Background(), LogFilter{})
	if total != 2 {
		t.Fatalf("after delete total=%d want 2", total)
	}
	if err := st.ClearLogs(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, total, _ = st.ListLogs(context.Background(), LogFilter{})
	if total != 0 {
		t.Errorf("after clear total=%d want 0", total)
	}
}

func TestRunRetentionImmediateCleanup(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	st.SetSetting(ctx, "log_retention_days", `1`)

	now := time.Now().UnixMilli()
	old := now - 3*86400_000
	seedLogs(t, st, []LogEntry{
		mkEntry(old, "GET", "old.host", "/", 200, false, "default", "u", ""),
		mkEntry(now-1000, "GET", "new.host", "/", 200, false, "default", "u", ""),
	})

	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { st.RunRetention(runCtx); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, total, _ := st.ListLogs(ctx, LogFilter{})
		if total == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	<-done

	_, total, _ := st.ListLogs(ctx, LogFilter{})
	if total != 1 {
		t.Fatalf("retention must delete expired rows, remaining=%d want 1", total)
	}
	items, _, _ := st.ListLogs(ctx, LogFilter{})
	if items[0].Host != "new.host" {
		t.Errorf("wrong survivor: %+v", items[0])
	}
}

func TestAuditMappedAccountRoundTripAndFilter(t *testing.T) {
	st := openTest(t)
	entry := mkEntry(time.Now().UnixMilli(), "POST", "api.x.ai", "/v1/responses", 200, true, "sid-123", "homeus", "")
	entry.Account = "grok-account@example.com"
	seedLogs(t, st, []LogEntry{entry})

	items, total, err := st.ListLogs(context.Background(), LogFilter{Account: entry.Account})
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("filter real account: items=%d total=%d err=%v", len(items), total, err)
	}
	if items[0].Account != entry.Account || items[0].AccountFP != entry.AccountFP {
		t.Fatalf("account round trip: %+v", items[0])
	}

	_, total, err = st.ListLogs(context.Background(), LogFilter{Account: entry.AccountFP})
	if err != nil || total != 1 {
		t.Fatalf("legacy session filter: total=%d err=%v", total, err)
	}
}

func TestRunLogWriterBatchesAndDrains(t *testing.T) {
	st := openTest(t)
	ch := make(chan LogEntry, 512)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { st.RunLogWriter(ctx, ch); close(done) }()

	for i := 0; i < 300; i++ { // 超过 256 触发中途批量落库，剩余靠排空
		ch <- mkEntry(int64(1000+i), "GET", "batch.host", "/p", 200, false, "default", "u", "")
	}
	close(ch)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not exit after channel close")
	}
	cancel()

	_, total, err := st.ListLogs(context.Background(), LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 300 {
		t.Fatalf("flushed=%d want 300", total)
	}
}

func TestRunLogWriterGracefulShutdownOnCtxCancel(t *testing.T) {
	st := openTest(t)
	ch := make(chan LogEntry, 16)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { st.RunLogWriter(ctx, ch); close(done) }()

	for i := 0; i < 5; i++ {
		ch <- mkEntry(int64(i), "GET", "drain.host", "/p", 200, false, "default", "u", "")
	}
	time.Sleep(80 * time.Millisecond) // 等写入器取走并暂存于批缓冲
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not exit on ctx cancel")
	}
	_, total, _ := st.ListLogs(context.Background(), LogFilter{})
	if total != 5 {
		t.Fatalf("graceful drain lost logs: %d want 5", total)
	}
}
