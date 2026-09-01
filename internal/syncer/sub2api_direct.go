package syncer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"mitmrouter/internal/acctmap"
)

const (
	directIncrementalInterval = 3 * time.Second
)

const (
	sub2APIIncrementalQuery = `
SELECT id, updated_at
FROM accounts
WHERE updated_at >= clock_timestamp() - interval '30 seconds'
  AND type IN ('` + accountTypeOAuth + `', '` + accountTypeSetupToken + `')
ORDER BY updated_at, id`

	sub2APICandidateQueryPrefix = `
SELECT id, name, platform, type, deleted_at,
       credentials->>'email' AS credential_email,
       credentials->>'access_token' AS access_token,
       credentials->>'refresh_token' AS refresh_token
FROM accounts
WHERE id IN (`
)

// Sub2APIDirectAccount is one database row observed by the direct reader.
// Entry is nil when the row is no longer usable for incremental mapping.
type Sub2APIDirectAccount struct {
	ID    int64
	Entry *Entry
	Valid bool
}

// Sub2APIDirectReader reads one Sub2API PostgreSQL database without using its HTTP API.
type Sub2APIDirectReader struct {
	db *sql.DB
}

// NewSub2APIDirectReader opens and verifies a direct PostgreSQL connection.
func NewSub2APIDirectReader(ctx context.Context, dsn string) (*Sub2APIDirectReader, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, errors.New("direct Sub2API database DSN is empty")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("direct Sub2API database DSN is invalid")
	}
	if !localPostgresHost(cfg.Host) {
		if cfg.TLSConfig == nil || cfg.TLSConfig.InsecureSkipVerify || strings.TrimSpace(cfg.TLSConfig.ServerName) == "" {
			return nil, errors.New("remote Sub2API PostgreSQL requires sslmode=verify-full")
		}
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(30 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, errors.New("direct Sub2API database ping failed")
	}
	return &Sub2APIDirectReader{db: db}, nil
}

func localPostgresHost(host string) bool {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" || strings.HasPrefix(host, "/") || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Close releases the direct database pool.
func (r *Sub2APIDirectReader) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
}

// Incremental returns accounts updated in the recent overlap window.
func (r *Sub2APIDirectReader) Incremental(ctx context.Context) ([]Sub2APIDirectAccount, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("direct Sub2API reader is closed")
	}
	rows, err := r.db.QueryContext(ctx, sub2APIIncrementalQuery)
	if err != nil {
		return nil, fmt.Errorf("query Sub2API account updates: %w", err)
	}
	ids := make([]int64, 0)
	seen := make(map[int64]struct{})
	for rows.Next() {
		var id int64
		var updatedAt time.Time
		if err := rows.Scan(&id, &updatedAt); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan Sub2API account update: %w", err)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate Sub2API account updates: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close Sub2API account updates: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	accounts, err := r.queryAccounts(ctx, ids)
	if err != nil {
		return nil, err
	}
	return accounts, nil
}

func (r *Sub2APIDirectReader) queryAccounts(ctx context.Context, ids []int64) ([]Sub2APIDirectAccount, error) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	query := sub2APICandidateQueryPrefix + strings.Join(placeholders, ",") + ")"
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query Sub2API candidate accounts: %w", err)
	}
	defer rows.Close()
	out := make([]Sub2APIDirectAccount, 0, len(ids))
	for rows.Next() {
		account, err := scanSub2APIAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("scan Sub2API candidate account: %w", err)
		}
		out = append(out, account)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Sub2API candidate accounts: %w", err)
	}
	return out, nil
}

func scanSub2APIAccount(rows *sql.Rows) (Sub2APIDirectAccount, error) {
	var (
		account                      Sub2APIDirectAccount
		name, platform, accountType  string
		deletedAt                    sql.NullTime
		credentialEmail, accessToken sql.NullString
		refreshToken                 sql.NullString
	)
	if err := rows.Scan(&account.ID, &name, &platform, &accountType, &deletedAt,
		&credentialEmail, &accessToken, &refreshToken); err != nil {
		return Sub2APIDirectAccount{}, err
	}
	if deletedAt.Valid {
		return account, nil
	}
	if accountType != accountTypeOAuth && accountType != accountTypeSetupToken {
		return account, nil
	}
	accountName := acctmap.NormalizeAccount(credentialEmail.String)
	if accountName == "" {
		accountName = acctmap.NormalizeAccount(name)
	}
	at := strings.TrimSpace(accessToken.String)
	rt := strings.TrimSpace(refreshToken.String)
	if accountName == "" || (at == "" && rt == "") {
		return account, nil
	}
	pf, ok := sub2apiPlatformMap[strings.ToLower(strings.TrimSpace(platform))]
	if !ok {
		return account, nil
	}
	account.Valid = true
	account.Entry = &Entry{
		Platform: pf,
		Account:  accountName,
		AtToken:  at,
		RtToken:  rt,
	}
	return account, nil
}
