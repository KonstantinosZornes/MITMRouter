package store

// 账号映射更新记录（docs/013-update-log-design.md）：acct_map 每次因同步、
// 文件扫描、手动推送或删除而变化时记一条事件。写入走缓冲 channel + 批量
// 落库 goroutine，与 access_logs 的 RunLogWriter 同构。

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// UpdateEvent 是一条映射变更记录。
type UpdateEvent struct {
	ID      int64  `json:"id"`
	Ts      int64  `json:"ts"`     // unix ms
	Kind    string `json:"kind"`   // direct_file | direct_incremental | api_sync | push | delete
	Source  string `json:"source"` // 'src:<id>' / 'api'，与 acct_map.source 同口径
	Status  string `json:"status"` // ok | error
	Summary string `json:"summary"`
	Detail  string `json:"detail"` // 可选补充：文件绝对路径等
}

// 事件类型与状态取值（docs/013-update-log-design.md §3/§4）。
const (
	UpdateKindDirectFile        = "direct_file"
	UpdateKindDirectIncremental = "direct_incremental"
	UpdateKindAPISync           = "api_sync"
	UpdateKindPush              = "push"
	UpdateKindDelete            = "delete"

	UpdateStatusOK    = "ok"
	UpdateStatusError = "error"
)

// NewUpdateEvent 构造带当前时间戳的更新事件；配合 SendUpdateEvent 使用。
func NewUpdateEvent(kind, source, status, summary, detail string) UpdateEvent {
	return UpdateEvent{
		Ts: time.Now().UnixMilli(), Kind: kind, Source: source,
		Status: status, Summary: summary, Detail: detail,
	}
}

// SendUpdateEvent 把事件放入缓冲 channel；channel 为 nil 或已满时丢弃，
// 绝不阻塞同步与请求路径。
func SendUpdateEvent(ch chan<- UpdateEvent, ev UpdateEvent) {
	if ch == nil {
		return
	}
	select {
	case ch <- ev:
	default:
	}
}

// UpdateFilter 更新记录查询条件。
type UpdateFilter struct {
	FromMs, ToMs   int64  // 时间范围（unix ms），0=不限
	Kind           string // 精确匹配
	Source         string // 精确匹配
	Status         string // ok | error，空=不限
	Page, PageSize int    // Page 从 1 起；PageSize≤200 默认 50
}

const updateEventColumns = `id,ts,kind,source,status,summary,detail`

// ListUpdateEvents 分页查询更新记录，返回 (条目, 总数)。
func (s *Store) ListUpdateEvents(ctx context.Context, f UpdateFilter) ([]UpdateEvent, int64, error) {
	where, args := buildUpdateWhere(f)
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize <= 0 || f.PageSize > 200 {
		f.PageSize = 50
	}

	var total int64
	if err := s.ro.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sync_events `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	pageOffset := int64(f.Page) - 1
	if total == 0 || pageOffset > total/int64(f.PageSize) {
		return []UpdateEvent{}, total, nil
	}
	offset := pageOffset * int64(f.PageSize)
	q := `SELECT ` + updateEventColumns + ` FROM sync_events ` + where +
		fmt.Sprintf(` ORDER BY id DESC LIMIT %d OFFSET %d`, f.PageSize, offset)
	rows, err := s.ro.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []UpdateEvent
	for rows.Next() {
		var e UpdateEvent
		if err := rows.Scan(&e.ID, &e.Ts, &e.Kind, &e.Source, &e.Status, &e.Summary, &e.Detail); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

func buildUpdateWhere(f UpdateFilter) (string, []any) {
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
	if f.Kind != "" {
		conds = append(conds, "kind=?")
		args = append(args, f.Kind)
	}
	if f.Source != "" {
		conds = append(conds, "source=?")
		args = append(args, f.Source)
	}
	if f.Status != "" {
		conds = append(conds, "status=?")
		args = append(args, f.Status)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

// ClearUpdateEvents 清空全部更新记录。
func (s *Store) ClearUpdateEvents(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sync_events`)
	return err
}

// RunUpdateEventWriter 批量落库更新记录。模式与 RunLogWriter 相同：
// 落库脱离调用方生命周期（优雅退出排空时 ctx 已取消），退出时带超时排空缓冲。
func (s *Store) RunUpdateEventWriter(ctx context.Context, ch <-chan UpdateEvent) {
	const batchLimit = 256
	wctx := context.WithoutCancel(ctx)
	buf := make([]UpdateEvent, 0, batchLimit)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	flush := func() {
		if len(buf) == 0 {
			return
		}
		fctx, cancel := context.WithTimeout(wctx, 10*time.Second)
		defer cancel()
		if err := s.insertUpdateEvents(fctx, buf); err != nil {
			slog.Error("updates: batch flush failed", "count", len(buf), "err", err)
		}
		buf = buf[:0]
	}
	for {
		select {
		case <-ctx.Done():
			// 排空剩余（带超时保护）
			deadline := time.After(5 * time.Second)
			for {
				select {
				case e, ok := <-ch:
					if !ok {
						flush()
						return
					}
					buf = append(buf, e)
					if len(buf) >= batchLimit {
						flush()
					}
				case <-deadline:
					flush()
					return
				default:
					flush()
					return
				}
			}
		case e, ok := <-ch:
			if !ok {
				flush()
				return
			}
			buf = append(buf, e)
			if len(buf) >= batchLimit {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (s *Store) insertUpdateEvents(ctx context.Context, events []UpdateEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO sync_events(ts,kind,source,status,summary,detail) VALUES(?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range events {
		if _, err := stmt.ExecContext(ctx, e.Ts,
			truncate(e.Kind, 64, 64), truncate(e.Source, 64, 64), truncate(e.Status, 16, 16),
			truncate(e.Summary, 512, 256), truncate(e.Detail, 2048, 256)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
