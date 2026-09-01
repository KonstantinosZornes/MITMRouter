package syncer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mitmrouter/internal/acctmap"
	"mitmrouter/internal/store"
)

// P0-3: CPA 某个认证文件临时下载失败时，不得用不完整结果覆盖旧快照。
// P2-5: 部分 CPA 认证文件把凭据嵌在 tokens 对象里，必须被解析。
func TestFetchCPAFileReadsNestedTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/auth-files/download" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"email":"nested@example.com","tokens":{"access_token":"at-nested","refresh_token":"rt-nested"}}`)
	}))
	defer server.Close()

	m := &Manager{hc: server.Client(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	entry, err := m.fetchCPAFile(context.Background(), server.URL, "management-key", cpaFileMeta{
		Name: "nested.json", Provider: "openai",
	}, "openai")
	if err != nil {
		t.Fatalf("fetch nested tokens: %v", err)
	}
	if entry == nil || entry.Account != "nested@example.com" || entry.AtToken != "at-nested" || entry.RtToken != "rt-nested" {
		t.Fatalf("entry=%+v, want nested tokens", entry)
	}
}

func TestSyncOneKeepsSnapshotWhenCPAFileFails(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer management-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v0/management/auth-files":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"files":[
				{"name":"good.json","provider":"openai","email":"new@example.com"},
				{"name":"temporary-failure.json","provider":"openai","email":"old@example.com"}
			]}`)
		case "/v0/management/auth-files/download":
			if r.URL.Query().Get("name") == "temporary-failure.json" {
				http.Error(w, "temporary failure", http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"email":"new@example.com","access_token":"at-new","refresh_token":"rt-new"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	id, err := st.CreateSyncSource(ctx, acctmap.SourceKindCLIProxyAPI, "test-cpa", server.URL, "management-key", MinIntervalSec, true)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	source := fmt.Sprintf(acctmap.SourceInstancePrefix+"%d", id)
	oldRT := acctmap.Fingerprint("openai", acctmap.NormalizeCred("rt-old"))
	if err := st.ReplaceSourceSnapshot(ctx, source, acctmap.SourceTypeForKind(acctmap.SourceKindCLIProxyAPI), []store.AcctUpsert{{
		Platform: "openai",
		Account:  "old@example.com",
		RtFP:     oldRT,
		RtHint:   "…-old",
	}}); err != nil {
		t.Fatalf("seed old snapshot: %v", err)
	}

	m := New(st, acctmap.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.syncOne(ctx, id)

	rows, err := st.LoadAcctMapAll(ctx)
	if err != nil {
		t.Fatalf("load account map: %v", err)
	}
	if len(rows) != 1 || rows[0].Account != "old@example.com" || rows[0].RtFP != oldRT {
		t.Fatalf("partial CPA sync replaced old snapshot: rows=%+v", rows)
	}

	src, ok, err := st.GetSyncSource(ctx, id)
	if err != nil || !ok {
		t.Fatalf("get source: ok=%v err=%v", ok, err)
	}
	if len(src.LastStatus) < len("error:") || src.LastStatus[:len("error:")] != "error:" {
		t.Fatalf("last status=%q, want error", src.LastStatus)
	}
}

func TestGetJSONRejectsTrailingData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"files":[]}garbage`)
	}))
	defer server.Close()

	m := &Manager{hc: server.Client()}
	var dst cpaListResp
	if err := m.getJSON(context.Background(), server.URL, nil, &dst); err == nil {
		t.Fatal("getJSON accepted trailing data after a valid JSON value")
	}
}

func TestGetJSONRejectsOversizedBody(t *testing.T) {
	const oversizedSyncBodySize = 16<<20 + 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, oversizedSyncBodySize))
	}))
	defer server.Close()

	m := &Manager{hc: server.Client()}
	var dst cpaListResp
	err := m.getJSON(context.Background(), server.URL, nil, &dst)
	if err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("oversized response error=%v, want response size error", err)
	}
}

func TestFetchCPARejectsMissingFilesEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	m := &Manager{hc: server.Client()}
	if _, err := m.fetchCPA(context.Background(), server.URL, "<REDACTED>"); err == nil {
		t.Fatal("fetchCPA accepted a response without the files field")
	}
}

func TestFetchSub2APIRejectsMissingDataEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	m := &Manager{hc: server.Client()}
	if _, err := m.fetchSub2API(context.Background(), server.URL, "<REDACTED>"); err == nil {
		t.Fatal("fetchSub2API accepted a response without the data envelope")
	}
}
