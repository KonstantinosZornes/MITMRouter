package store

import (
	"context"
	"testing"
)

func TestListLogsDoesNotPanicOnNonStringInternalError(t *testing.T) {
	st := openTest(t)
	seedLogs(t, st, []LogEntry{{
		Ts: 1, ReqID: "probe", Method: "GET", Host: "example.com", Path: "/",
		Status: 0, DurMS: 1, AccountFP: "default", Upstream: "direct",
	}})
	if _, err := st.db.Exec(`UPDATE access_logs SET internal_error=CAST(123 AS BLOB) WHERE req_id='probe'`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ListLogs panicked on non-string internal_error: %v", r)
		}
	}()
	if _, _, err := st.ListLogs(context.Background(), LogFilter{}); err != nil {
		t.Fatal(err)
	}
}
