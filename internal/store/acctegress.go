// acct_egress 绑定表（账户 ↔ 出站）与同步空快照守卫。
// 设计：docs/011-plain-binding-design.md。
//
// 模型：mode 是账户属性——同一账户的全部绑定行 mode 相同，表内冗余存储，
// 由「按账户整体替换」的单事务写路径保证一致。级联规则两条：
//  1. 删出站 → DeleteUpstream 同事务清引用行；
//  2. 账号在 acct_map 消失 → gcAcctEgress 清孤儿绑定；所有会改动 acct_map
//     的写入路径都在其事务末尾调用 GC，覆盖手动删、快照消失、清凭据、删源
//     全部删除通道。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AcctEgressRow 是 acct_egress 的一行。
type AcctEgressRow struct {
	Platform  string
	Account   string
	EgressID  int64
	Mode      string // 'sticky' | 'random'
	CreatedAt int64
	UpdatedAt int64
}

// 合法的绑定模式。暴露为常量供 API/前端契约共用。
const (
	EgressModeSticky = "sticky"
	EgressModeRandom = "random"
)

// ValidEgressMode 报告 mode 是否合法。
func ValidEgressMode(m string) bool { return m == EgressModeSticky || m == EgressModeRandom }

const acctEgressCols = `platform,account,egress_id,mode,created_at,updated_at`

func scanAcctEgressRows(rows *sql.Rows) ([]AcctEgressRow, error) {
	defer rows.Close()
	var out []AcctEgressRow
	for rows.Next() {
		var r AcctEgressRow
		if err := rows.Scan(&r.Platform, &r.Account, &r.EgressID, &r.Mode, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListAcctEgress 返回全部绑定行。
func (s *Store) ListAcctEgress(ctx context.Context) ([]AcctEgressRow, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT `+acctEgressCols+` FROM acct_egress ORDER BY platform,account,egress_id`)
	if err != nil {
		return nil, err
	}
	return scanAcctEgressRows(rows)
}

// ListAcctEgressByAccount 返回某账户的全部绑定行。
func (s *Store) ListAcctEgressByAccount(ctx context.Context, platform, account string) ([]AcctEgressRow, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT `+acctEgressCols+` FROM acct_egress WHERE platform=? AND account=? ORDER BY egress_id`,
		platform, account)
	if err != nil {
		return nil, err
	}
	return scanAcctEgressRows(rows)
}

// AcctExists 报告 (platform, account) 是否在 acct_map 中存在（任一来源任一行）。
func (s *Store) AcctExists(ctx context.Context, platform, account string) (bool, error) {
	var exists bool
	err := s.ro.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM acct_map WHERE platform=? AND account=?)`,
		platform, account).Scan(&exists)
	return exists, err
}

// ReplaceAccountBinding 整体替换某账户的绑定：先删该账户全部行，再按 mode 插入
// 新全集（egressIDs 去重由调用方负责）。绑定指向的账号若已不在 acct_map 中，
// GC 会立即清掉——本方法不做账号存在性校验，那是 API 层的职责。
func (s *Store) ReplaceAccountBinding(ctx context.Context, platform, account, mode string, egressIDs []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM acct_egress WHERE platform=? AND account=?`, platform, account); err != nil {
		return err
	}
	for _, id := range egressIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO acct_egress(platform,account,egress_id,mode,created_at,updated_at)
			 VALUES(?,?,?,?,?,?)`, platform, account, id, mode, now, now); err != nil {
			return err
		}
	}
	if err := gcAcctEgress(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearAcctEgress 删除全部出站绑定，返回删除的绑定行数。无需 GC：
// 绑定行本身就是删除目标，acct_map 不受影响。
func (s *Store) ClearAcctEgress(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM acct_egress`)
	if err != nil {
		return 0, fmt.Errorf("clear acct_egress: %w", err)
	}
	return res.RowsAffected()
}

// EgressAssign 是出站方向批量关联中的一个账户条目。
type EgressAssign struct {
	Platform string
	Account  string
	Mode     string // 仅对此前未绑定该出站的账户生效；已绑定的保留原 mode
}

// ReplaceEgressAccounts 整体替换某出站关联的账户集合：不在 assign 中的账户移除
// 该出站（若因此该账户一条不剩，GC 连带清掉整份绑定）；assign 内的账户保留已有
// mode，新加入的使用各自给定的 Mode。单事务原子生效。
func (s *Store) ReplaceEgressAccounts(ctx context.Context, egressID int64, assign []EgressAssign) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	prevModes := map[string]string{} // "platform\x00account" -> mode
	rows, err := tx.QueryContext(ctx,
		`SELECT platform,account,mode FROM acct_egress WHERE egress_id=?`, egressID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var pf, acct, m string
		if err := rows.Scan(&pf, &acct, &m); err != nil {
			rows.Close()
			return err
		}
		prevModes[pf+"\x00"+acct] = m
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM acct_egress WHERE egress_id=?`, egressID); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for _, a := range assign {
		mode := a.Mode
		if om, ok := prevModes[a.Platform+"\x00"+a.Account]; ok && om != "" {
			mode = om // 批量操作不改既有账户的路由语义
		}
		if !ValidEgressMode(mode) {
			mode = EgressModeSticky
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO acct_egress(platform,account,egress_id,mode,created_at,updated_at)
			 VALUES(?,?,?,?,?,?)
			 ON CONFLICT(platform,account,egress_id) DO UPDATE SET mode=excluded.mode, updated_at=excluded.updated_at`,
			a.Platform, a.Account, egressID, mode, now, now); err != nil {
			return err
		}
	}
	if err := gcAcctEgress(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// gcAcctEgress 清理孤儿绑定：账号在 acct_map 中一行都不剩时，其绑定一并删除。
// 必须在与 acct_map 写入相同的事务中调用，保证"删账号"与"删绑定"同生共死。
func gcAcctEgress(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM acct_egress WHERE NOT EXISTS (
		SELECT 1 FROM acct_map a
		WHERE a.platform = acct_egress.platform AND a.account = acct_egress.account)`)
	if err != nil {
		return fmt.Errorf("gc acct_egress: %w", err)
	}
	return nil
}

// ---------- 空快照保护下的同步对齐 ----------

// SyncEmptyClearThreshold 返回设置项 sync_empty_clear_threshold：
// 同一来源连续 N 次「连接正常但拉回 0 个账号」才允许按空快照清空该源名下映射行。
// 默认 3，钳位 1–100；1 等价于无保护立即清空。低频读取（每次源同步一次），不走快照。
func (s *Store) SyncEmptyClearThreshold(ctx context.Context) int {
	n := 3
	if m, err := s.AllSettings(ctx); err == nil {
		if v, ok := m["sync_empty_clear_threshold"]; ok {
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 1 {
				n = 3
			}
		}
	}
	if n > 100 {
		n = 100
	}
	return n
}

// ReplaceSourceSnapshotGuarded 带「空快照保护」的来源快照对齐（替代裸 ReplaceSourceSnapshot）：
//   - keep 非空：正常全删重插，并把连续空快照计数清零；
//   - keep 为空且该源名下没有映射行：无事可做，计数归零；
//   - keep 为空且该源名下仍有映射行：计数 +1；计数达到 threshold 前只记账不动
//     映射（skipped=true），达到后执行真正的清空并保持计数（后续空快照无需重复清）。
//
// 取回/解析失败不会进入本方法（syncer 提前返回），因此计数只反映「连接成功但为空」。
// 返回 skipped 表示本次请求被保护拦截。
func (s *Store) ReplaceSourceSnapshotGuarded(ctx context.Context, source, sourceType string, keep []AcctUpsert, threshold int) (skipped bool, streak int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM sync_sources WHERE 'src:' || id = ?)`, source).Scan(&exists); err != nil {
		return false, 0, err
	}
	if !exists {
		return false, 0, fmt.Errorf("sync source %q no longer exists", source)
	}

	streak = 0
	if len(keep) == 0 {
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(empty_streak,0) FROM sync_sources WHERE 'src:' || id = ?`, source).Scan(&streak); err != nil {
			return false, 0, err
		}
	}
	setStreak := func(v int64) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sync_sources SET empty_streak=?, updated_at=? WHERE 'src:' || id = ?`,
			v, time.Now().UnixMilli(), source)
		return err
	}

	if len(keep) == 0 {
		var haveRows bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM acct_map WHERE source=?)`, source).Scan(&haveRows); err != nil {
			return false, 0, err
		}
		if !haveRows {
			// 本来就空：无东西可清，计数归零即可。
			if err := setStreak(0); err != nil {
				return false, 0, err
			}
			return false, 0, tx.Commit()
		}
		next := streak + 1
		if threshold < 1 {
			threshold = 1
		}
		if next < int64(threshold) {
			// 保护期内：只累计计数，不动映射行。
			if err := setStreak(next); err != nil {
				return false, 0, err
			}
			return true, next, tx.Commit()
		}
		// 达到阈值：真正执行清空；计数保持在阈值处（表已空，再次空快照自然幂等）。
		if _, err := tx.ExecContext(ctx, `DELETE FROM acct_map WHERE source=?`, source); err != nil {
			return false, 0, err
		}
		if err := gcAcctEgress(ctx, tx); err != nil {
			return false, 0, err
		}
		if err := setStreak(next); err != nil {
			return false, 0, err
		}
		return false, next, tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM acct_map WHERE source=?`, source); err != nil {
		return false, 0, err
	}
	now := time.Now().UnixMilli()
	for _, k := range keep {
		// OR REPLACE：同一快照内意外重复的同键行取最后一条
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO acct_map(platform,account,at_fp,rt_fp,at_hint,rt_hint,source,source_type,updated_at)
			 VALUES(?,?,?,?,?,?,?,?,?)`,
			k.Platform, k.Account, k.AtFP, k.RtFP, k.AtHint, k.RtHint, source, sourceType, now); err != nil {
			return false, 0, err
		}
	}
	if err := gcAcctEgress(ctx, tx); err != nil {
		return false, 0, err
	}
	if err := setStreak(0); err != nil {
		return false, 0, err
	}
	return false, 0, tx.Commit()
}
