package syncer

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type directDBTestState struct {
	mu      sync.Mutex
	queries []string
}

var (
	directDBTestRegister sync.Once
	directDBTestStatePtr *directDBTestState
)

type directDBTestDriver struct{}

type directDBTestConn struct {
	state *directDBTestState
}

type directDBTestRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (directDBTestDriver) Open(string) (driver.Conn, error) {
	return directDBTestConn{state: directDBTestStatePtr}, nil
}

func (c directDBTestConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c directDBTestConn) Close() error                        { return nil }
func (c directDBTestConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (c directDBTestConn) Ping(context.Context) error { return nil }

func (c directDBTestConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.state.mu.Lock()
	c.state.queries = append(c.state.queries, query)
	c.state.mu.Unlock()

	switch {
	case strings.Contains(query, "SELECT id, updated_at"):
		return &directDBTestRows{
			columns: []string{"id", "updated_at"},
			values:  [][]driver.Value{{int64(7), time.Now()}},
		}, nil
	case strings.Contains(query, "WHERE id IN"):
		return &directDBTestRows{
			columns: []string{"id", "name", "platform", "type", "deleted_at", "credential_email", "access_token", "refresh_token"},
			values:  [][]driver.Value{{int64(7), "db-name", "openai", "oauth", nil, "db@example.com", "db-at", "db-rt"}},
		}, nil
	default:
		return nil, nil
	}
}

func (c directDBTestConn) CheckNamedValue(*driver.NamedValue) error { return nil }

func (r *directDBTestRows) Columns() []string { return r.columns }
func (r *directDBTestRows) Close() error      { return nil }
func (r *directDBTestRows) Next(dest []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

func openDirectDBTest(t *testing.T, state *directDBTestState) *sql.DB {
	t.Helper()
	directDBTestRegister.Do(func() {
		sql.Register("mitmrouter-direct-test", directDBTestDriver{})
	})
	directDBTestStatePtr = state
	db, err := sql.Open("mitmrouter-direct-test", "")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSub2APIDirectReaderIncremental(t *testing.T) {
	state := &directDBTestState{}
	db := openDirectDBTest(t, state)
	defer db.Close()
	reader := &Sub2APIDirectReader{db: db}

	accounts, err := reader.Incremental(context.Background())
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	if len(accounts) != 1 || accounts[0].Entry == nil {
		t.Fatalf("accounts=%+v", accounts)
	}
	if got := accounts[0].Entry; got.Platform != "openai" || got.Account != "db@example.com" || got.AtToken != "db-at" || got.RtToken != "db-rt" {
		t.Fatalf("entry=%+v", got)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.queries) != 2 {
		t.Fatalf("queries=%d, want update + candidate", len(state.queries))
	}
	if !strings.Contains(state.queries[1], "WHERE id IN ($1)") {
		t.Fatalf("candidate query=%q", state.queries[1])
	}
}
