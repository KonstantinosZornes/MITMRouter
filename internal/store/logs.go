package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// LogFilter 审计查询条件。
type LogFilter struct {
	FromMs, ToMs   int64  // 时间范围（unix ms），0=不限
	Q              string // host/path 子串匹配
	Account        string // 真实账号或 account_fp 精确匹配
	Upstream       string // 条目名精确匹配
	StatusClass    string // "" | 2xx | 4xx | 5xx | err (internal_error is set)
	Page, PageSize int    // Page 从 1 起；PageSize≤200 默认 50
}

const logColumns = `id,ts,req_id,method,host,path,status,dur_ms,ttfb_ms,bytes_out,has_marker,account,account_fp,upstream,internal_error`

// ListLogs 分页查询审计记录，返回 (条目, 总数)。
func (s *Store) ListLogs(ctx context.Context, f LogFilter) ([]LogEntry, int64, error) {
	where, args := buildLogWhere(f)
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize <= 0 || f.PageSize > 200 {
		f.PageSize = 50
	}

	var total int64
	if err := s.ro.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM access_logs `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	pageOffset := int64(f.Page) - 1
	if total == 0 || pageOffset > total/int64(f.PageSize) {
		return []LogEntry{}, total, nil
	}
	offset := pageOffset * int64(f.PageSize) // 上面已证明 offset <= total，不会溢出。
	q := `SELECT ` + logColumns + ` FROM access_logs ` + where +
		fmt.Sprintf(` ORDER BY id DESC LIMIT %d OFFSET %d`, f.PageSize, offset)
	rows, err := s.ro.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []LogEntry
	for rows.Next() {
		var e LogEntry
		var hasAK int
		var internalError any
		if err := rows.Scan(&e.ID, &e.Ts, &e.ReqID, &e.Method, &e.Host, &e.Path, &e.Status,
			&e.DurMS, &e.TTFBMS, &e.BytesOut, &hasAK, &e.Account, &e.AccountFP, &e.Upstream, &internalError); err != nil {
			return nil, 0, err
		}
		e.HasMarker = hasAK != 0
		if internalError != nil {
			switch v := internalError.(type) {
			case string:
				e.InternalError = v
			case []byte:
				e.InternalError = string(v)
			default:
				e.InternalError = fmt.Sprint(v)
			}
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

func buildLogWhere(f LogFilter) (string, []any) {
	var conds []string
	var args []any
	if f.FromMs > 0 {
		conds = append(conds, "ts>=?")
		args = append(args, f.FromMs)
	}
	if f.ToMs > 0 {
		conds = append(conds, "ts<=?")
		args = append(args, f.ToMs)
	}
	if f.Q != "" {
		esc := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_")
		pat := "%" + esc.Replace(f.Q) + "%"
		conds = append(conds, "(host LIKE ? ESCAPE '\\' OR path LIKE ? ESCAPE '\\')")
		args = append(args, pat, pat)
	}
	if f.Account != "" {
		// 兼容已有调用方按会话 ID 筛选，同时允许按真实账号筛选。
		conds = append(conds, "(account=? OR account_fp=?)")
		args = append(args, f.Account, f.Account)
	}
	if f.Upstream != "" {
		conds = append(conds, "upstream=?")
		args = append(args, f.Upstream)
	}
	switch f.StatusClass {
	case "2xx":
		conds = append(conds, "status BETWEEN 200 AND 299")
	case "4xx":
		conds = append(conds, "status BETWEEN 400 AND 499")
	case "5xx":
		conds = append(conds, "status>=500")
	case "err":
		conds = append(conds, "internal_error IS NOT NULL")
	}
	if len(conds) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

// ClearLogs 清空全部审计记录。
func (s *Store) ClearLogs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM access_logs`)
	return err
}

// DeleteOlderThan 删除指定时间戳之前的记录，返回删除行数。
func (s *Store) DeleteOlderThan(ctx context.Context, cutoffMs int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM access_logs WHERE ts<?`, cutoffMs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RunRetention 每日按设置项 log_retention_days 清理过期审计记录。
func (s *Store) RunRetention(ctx context.Context) {
	run := func() {
		days := 30
		if m, err := s.AllSettings(ctx); err == nil {
			if v, ok := m["log_retention_days"]; ok {
				if json.Unmarshal([]byte(v), &days) != nil || days < 1 {
					days = 30
				}
			}
		}
		cutoff := time.Now().UnixMilli() - int64(days)*86400_000
		if n, err := s.DeleteOlderThan(ctx, cutoff); err != nil {
			slog.Error("audit: retention cleanup failed", "err", err)
		} else if n > 0 {
			slog.Info("audit: retention cleanup done", "deleted", n, "retention_days", days)
		}
		// 更新记录（docs/013）与审计同保留期，不单独设设置项。
		if _, err := s.db.ExecContext(ctx, `DELETE FROM sync_events WHERE ts<?`, cutoff); err != nil {
			slog.Error("updates: retention cleanup failed", "err", err)
		}
	}
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	run() // 启动即清一次
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
