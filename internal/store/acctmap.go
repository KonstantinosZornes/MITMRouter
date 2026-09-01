package store

// acct_map 与 sync_sources 两表的读写方法。
// 设计：docs/004-stable-account-hash-design.md。
// 模型：唯一键 (platform, source, account, rt_fp, source_type)，同键即同一行：
// AT 更新原地覆盖、快照消失删除、新出现插入。source 为来源实例
// （'src:<id>'，同类源可并存多个；'api'=推送/手动通道），source_type 为
// 来源类型全名（拉取源按实例映射，推送可自定义）。
// 明文凭据绝不入库；sync_sources 的 api_key 存于 secrets 表（键 source_key_<id>）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mitmrouter/internal/acctmap"
)

// SourcePush 保留 store 包的兼容入口，实际定义位于 acctmap 包。
const SourcePush = acctmap.SourcePush

// AcctRow 是 acct_map 的一行。
type AcctRow struct {
	Platform   string
	Account    string
	AtFP       string
	RtFP       string
	AtHint     string
	RtHint     string
	Source     string // 'src:<id>' / 'api'
	SourceType string // 'CLIProxyAPI' / 'Sub2API' / 自定义全名
	UpdatedAt  int64
}

// LoadAcctMapAll 返回全表。
func (s *Store) LoadAcctMapAll(ctx context.Context) ([]AcctRow, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT platform,account,at_fp,rt_fp,at_hint,rt_hint,source,source_type,updated_at FROM acct_map
		 ORDER BY platform,source_type,source,account`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AcctRow
	for rows.Next() {
		var r AcctRow
		if err := rows.Scan(&r.Platform, &r.Account, &r.AtFP, &r.RtFP,
			&r.AtHint, &r.RtHint, &r.Source, &r.SourceType, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AcctUpsert 是一次快照写入中的单账号凭据集。空串字段表示"本来源未提供"，
// 保留现值。
type AcctUpsert struct {
	Platform string
	Account  string
	AtFP     string
	RtFP     string
	AtHint   string
	RtHint   string
}

// ReplaceSourceSnapshot 一个来源实例的全量快照对齐：单事务内先删该实例全部
// 旧行、再插入 keep 集合——同键自然覆盖、"旧有新无"即消失、新出现即插入；
// 其他来源实例的行不受影响，故重拉不会误删别源数据。
func (s *Store) ReplaceSourceSnapshot(ctx context.Context, source, sourceType string, keep []AcctUpsert) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM sync_sources WHERE 'src:' || id = ?)`, source).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("sync source %q no longer exists", source)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM acct_map WHERE source=?`, source); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for _, k := range keep {
		// OR REPLACE：同一快照内意外重复的同键行取最后一条
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO acct_map(platform,account,at_fp,rt_fp,at_hint,rt_hint,source,source_type,updated_at)
			 VALUES(?,?,?,?,?,?,?,?,?)`,
			k.Platform, k.Account, k.AtFP, k.RtFP, k.AtHint, k.RtHint, source, sourceType, now); err != nil {
			return err
		}
	}
	if err := gcAcctEgress(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ApplyAccountDelta 对一个 direct source 的单账号映射做精确替换。
// 与 ReplaceSourceSnapshot 不同，up 只代表当前账号，不会触碰同一 source 下的其他账号；
// 空的 AT/RT 表示清除该账号的 direct 映射，不表示沿用旧值。
func (s *Store) ApplyAccountDelta(ctx context.Context, source, sourceType string, up AcctUpsert) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM sync_sources WHERE 'src:' || id = ?)`, source).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("sync source %q no longer exists", source)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM acct_map WHERE source=? AND platform=? AND account=?`,
		source, up.Platform, up.Account); err != nil {
		return err
	}
	if up.AtFP != "" || up.RtFP != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO acct_map(platform,account,at_fp,rt_fp,at_hint,rt_hint,source,source_type,updated_at)
			 VALUES(?,?,?,?,?,?,?,?,?)`,
			up.Platform, up.Account, up.AtFP, up.RtFP, up.AtHint, up.RtHint,
			source, sourceType, time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	if err := gcAcctEgress(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceAccountSnapshot 推送/手动通道（source='api'）的单账号快照对齐：
// 空字段表示本次未提供，沿用当前快照中的另一类凭据；随后原子替换该账号/类型
// 的当前行。这样单独轮换 AT 或 RT 都不会丢掉另一类凭据。
func (s *Store) ReplaceAccountSnapshot(ctx context.Context, platform, account, sourceType string, up AcctUpsert) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if up.AtFP == "" && up.RtFP == "" {
		// API 层会拒绝空提交；存储层也不要把它解释成清空凭据。
		return tx.Commit()
	}

	var current struct {
		atFP, atHint string
		rtFP, rtHint string
	}
	// 遗留数据可能同账号/类型下并存多行（旧 bug 产物、手工改库）。按
	// updated_at 倒序扫描全部行，每个字段取最近一次非空值，避免只看
	// 最新一行时把其他行仍有效的凭据一并删掉。
	scanRows, err := tx.QueryContext(ctx,
		`SELECT at_fp,at_hint,rt_fp,rt_hint FROM acct_map
		 WHERE platform=? AND account=? AND source=? AND source_type=?
		 ORDER BY updated_at DESC`,
		platform, account, SourcePush, sourceType)
	if err != nil {
		return err
	}
	for scanRows.Next() {
		var atFP, atHint, rtFP, rtHint string
		if err := scanRows.Scan(&atFP, &atHint, &rtFP, &rtHint); err != nil {
			scanRows.Close()
			return err
		}
		if current.atFP == "" && atFP != "" {
			current.atFP, current.atHint = atFP, atHint
		}
		if current.rtFP == "" && rtFP != "" {
			current.rtFP, current.rtHint = rtFP, rtHint
		}
	}
	if err := scanRows.Err(); err != nil {
		scanRows.Close()
		return err
	}
	scanRows.Close()

	atFP, atHint := up.AtFP, up.AtHint
	if atFP == "" {
		atFP, atHint = current.atFP, current.atHint
	}
	rtFP, rtHint := up.RtFP, up.RtHint
	if rtFP == "" {
		rtFP, rtHint = current.rtFP, current.rtHint
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM acct_map WHERE platform=? AND account=? AND source=? AND source_type=?`,
		platform, account, SourcePush, sourceType); err != nil {
		return err
	}
	if atFP == "" && rtFP == "" {
		if err := gcAcctEgress(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO acct_map(platform,account,at_fp,rt_fp,at_hint,rt_hint,source,source_type,updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		platform, account, atFP, rtFP, atHint, rtHint,
		SourcePush, sourceType, time.Now().UnixMilli()); err != nil {
		return err
	}
	if err := gcAcctEgress(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteAcctMapAccount 删除某账号的映射行：source 非空时只删该来源实例名下
// 的行，空串时删全部来源。绑定（acct_egress）的孤儿清理在删除同一事务内完成，
// 保证「账号消失」与「绑定消失」同生共死。返回移除的 acct_map 行数。
func (s *Store) DeleteAcctMapAccount(ctx context.Context, platform, account, source string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	q := `DELETE FROM acct_map WHERE platform=? AND account=?`
	args := []any{platform, account}
	if source != "" {
		q += ` AND source=?`
		args = append(args, source)
	}
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := gcAcctEgress(ctx, tx); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// acctMapKey 定位 acct_map 一行的五列主键。
type acctMapKey struct {
	platform, account  string
	source, sourceType string
	rtFP               string // 主键成分：行定位必带
}

// delete 删除该主键对应的行。
func (k acctMapKey) delete(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM acct_map WHERE platform=? AND account=? AND source=? AND source_type=? AND rt_fp=?`,
		k.platform, k.account, k.source, k.sourceType, k.rtFP)
	return err
}

// ClearAcctMapFp 清除账号下与 fp 匹配的 AT 或 RT 列（两列都可能命中多行）。
// 清除 RT 会把主键成分 rt_fp 置空：若同键已存在空 RT 行，合并为一行
// （非空 AT 信息优先），避免主键约束失败。
// 返回命中的字段（"at"/"rt"/"at+rt"）与涉及的来源实例（唯一时给出，多个为空串），
// 供更新记录标注摘要；无命中时 ok=false。
func (s *Store) ClearAcctMapFp(ctx context.Context, platform, account, fp string) (field, source string, ok bool, err error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT source,source_type,rt_fp,at_fp,at_hint FROM acct_map
		 WHERE platform=? AND account=? AND (at_fp=? OR rt_fp=?)`,
		platform, account, fp, fp)
	if err != nil {
		return "", "", false, err
	}
	type hit struct {
		key          acctMapKey
		atFP, atHint string
	}
	var hits []hit
	for rows.Next() {
		var h hit
		h.key.platform, h.key.account = platform, account
		if err := rows.Scan(&h.key.source, &h.key.sourceType, &h.key.rtFP, &h.atFP, &h.atHint); err != nil {
			rows.Close()
			return "", "", false, err
		}
		hits = append(hits, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", "", false, err
	}
	if len(hits) == 0 {
		return "", "", false, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", false, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	for _, h := range hits {
		at := h.atFP
		rt := h.key.rtFP
		if at == fp {
			at = "" // 先 AT 后 RT：双列同指纹时本轮只清 AT 列
		} else {
			rt = ""
		}
		switch {
		case at == "" && rt == "":
			if err := h.key.delete(ctx, tx); err != nil {
				return "", "", false, err
			}
		case rt == "":
			// 清 RT：主键成分变化。先移除原行，再以空 RT 键合并写入；
			// 与既有空 RT 行同键时非空 AT 信息优先，杜绝唯一键冲突。
			// rt_hint 一并置空（与"只清 AT 时清 at_hint"对称）。
			if err := h.key.delete(ctx, tx); err != nil {
				return "", "", false, err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO acct_map(platform,account,at_fp,rt_fp,at_hint,rt_hint,source,source_type,updated_at)
				 VALUES(?,?,?,'',?,'',?,?,?)
				 ON CONFLICT(platform,source,account,rt_fp,source_type) DO UPDATE SET
				   at_fp    = CASE WHEN excluded.at_fp    <> '' THEN excluded.at_fp    ELSE acct_map.at_fp    END,
				   at_hint  = CASE WHEN excluded.at_hint  <> '' THEN excluded.at_hint  ELSE acct_map.at_hint  END,
				   rt_hint  = '',
				   updated_at = excluded.updated_at`,
				platform, account, at, h.atHint, h.key.source, h.key.sourceType, now); err != nil {
				return "", "", false, err
			}
		default:
			// 只清 AT：rt_fp 未动、主键不变，原地更新（提示尾缀一并清除）
			if _, err := tx.ExecContext(ctx,
				`UPDATE acct_map SET at_fp='',at_hint='',updated_at=?
				 WHERE platform=? AND account=? AND source=? AND source_type=? AND rt_fp=?`,
				now, platform, account, h.key.source, h.key.sourceType, h.key.rtFP); err != nil {
				return "", "", false, err
			}
		}
	}
	if err := gcAcctEgress(ctx, tx); err != nil {
		return "", "", false, err
	}
	// 聚合命中信息供更新记录使用：命中的字段（at/rt/both）与来源实例（唯一时给出）。
	seenField := map[string]bool{}
	seenSource := map[string]bool{}
	for _, h := range hits {
		if h.atFP == fp {
			seenField["at"] = true
		} else {
			seenField["rt"] = true
		}
		seenSource[h.key.source] = true
	}
	if seenField["at"] && seenField["rt"] {
		field = "at+rt"
	} else if seenField["at"] {
		field = "at"
	} else {
		field = "rt"
	}
	if len(seenSource) == 1 {
		for src := range seenSource {
			source = src
		}
	}
	return field, source, true, tx.Commit()
}

// DeleteSourceRows 级联清理：删除指定来源实例的全部映射行。返回删除行数。
func (s *Store) DeleteSourceRows(ctx context.Context, source string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM acct_map WHERE source=?`, source)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ---------- sync_sources ----------

// SyncSourceRow 是 sync_sources 表的一行（不含 api_key/DSN 明文，见 secrets 表）。
// API 全量同步配置（base_url + api_key secret）必填；增量路径
// （direct_auth_dir / direct_db_secret）选填，填了即启用增量直读（docs/012 v2）。
type SyncSourceRow struct {
	ID             int64
	Kind           string // cpa | sub2api
	Name           string
	BaseURL        string
	DirectAuthDir  string
	DirectDBSecret string // key in secrets; never the DSN itself
	IntervalS      int
	Enabled        bool
	LastSyncAt     int64 // 0 = 从未同步
	LastStatus     string
	CreatedAt      int64
	UpdatedAt      int64
}

const (
	sourceKeyPrefix      = "source_key_"
	sourceDirectDBPrefix = "source_direct_db_"
)

// SourceKeySecretKey 返回源 api_key 在 secrets 表中的键名。
func SourceKeySecretKey(id int64) string { return sourceKeyPrefix + strconv.FormatInt(id, 10) }

// SourceDirectDBSecretKey 返回 Sub2API 增量 DSN 的 secrets 键名。
func SourceDirectDBSecretKey(id int64) string {
	return sourceDirectDBPrefix + strconv.FormatInt(id, 10)
}

// SyncSourceConfig 是创建同步源时使用的配置。DirectDBDSN 只在调用期间传入，
// 实际值保存在 secrets，不写入 sync_sources。
type SyncSourceConfig struct {
	Kind          string
	Name          string
	BaseURL       string
	DirectAuthDir string
	DirectDBDSN   string
	IntervalS     int
	Enabled       bool
}

// ListSyncSources 返回全部拉取源。
func (s *Store) ListSyncSources(ctx context.Context) ([]SyncSourceRow, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT id,kind,name,base_url,direct_auth_dir,direct_db_secret,interval_s,enabled,
			COALESCE(last_sync_at,0),last_status,created_at,updated_at
		 FROM sync_sources ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SyncSourceRow
	for rows.Next() {
		r, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetSyncSource 返回单个源。
func (s *Store) GetSyncSource(ctx context.Context, id int64) (SyncSourceRow, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id,kind,name,base_url,direct_auth_dir,direct_db_secret,interval_s,enabled,
			COALESCE(last_sync_at,0),last_status,created_at,updated_at
		 FROM sync_sources WHERE id=?`, id)
	r, err := scanSource(row)
	if err != nil {
		return SyncSourceRow{}, false, err
	}
	return r, true, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanSource(rs rowScanner) (SyncSourceRow, error) {
	var r SyncSourceRow
	var en int
	if err := rs.Scan(&r.ID, &r.Kind, &r.Name, &r.BaseURL,
		&r.DirectAuthDir, &r.DirectDBSecret, &r.IntervalS, &en,
		&r.LastSyncAt, &r.LastStatus, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return SyncSourceRow{}, err
	}
	r.Enabled = en != 0
	return r, nil
}

// CreateSyncSource 新增 API 全量同步源，返回新 ID。
func (s *Store) CreateSyncSource(ctx context.Context, kind, name, baseURL, apiKey string, intervalS int, enabled bool) (int64, error) {
	return s.CreateSyncSourceConfig(ctx, SyncSourceConfig{
		Kind: kind, Name: name, BaseURL: baseURL,
		IntervalS: intervalS, Enabled: enabled,
	}, apiKey)
}

// CreateSyncSourceConfig 新增一个同步源。API 密钥与增量 DSN 各自独立存
// secrets：DSN 只写入 secrets，sync_sources 仅保存其 key。
func (s *Store) CreateSyncSourceConfig(ctx context.Context, cfg SyncSourceConfig, apiKey string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().UnixMilli()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO sync_sources(kind,name,base_url,direct_auth_dir,direct_db_secret,interval_s,enabled,created_at,updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		cfg.Kind, cfg.Name, cfg.BaseURL, cfg.DirectAuthDir, "",
		cfg.IntervalS, boolToInt(cfg.Enabled), now, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	if apiKey != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO secrets(key,value) VALUES(?,?)`, SourceKeySecretKey(id), []byte(apiKey)); err != nil {
			return 0, err
		}
	}
	if cfg.Kind == acctmap.SourceKindSub2API && strings.TrimSpace(cfg.DirectDBDSN) != "" {
		secretKey := SourceDirectDBSecretKey(id)
		if _, err := tx.ExecContext(ctx,
			`UPDATE sync_sources SET direct_db_secret=? WHERE id=?`, secretKey, id); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO secrets(key,value) VALUES(?,?)`, secretKey, []byte(cfg.DirectDBDSN)); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// UpdateSyncSource 更新同步源；apiKey 非空时一并轮换密钥。
func (s *Store) UpdateSyncSource(ctx context.Context, r SyncSourceRow, apiKey string) error {
	return s.UpdateSyncSourceConfig(ctx, r, apiKey, "", false, false)
}

// UpdateSyncSourceConfig 更新同步源。API 密钥和增量 DSN 是两组独立配置，
// 可以并存（docs/012 v2）：apiKey 非空时轮换密钥，永远不会被删除；
// directDBSet 表示本次提交了新 DSN，directDBClear 表示清除 DSN（停增量），
// 两者都为 false 时沿用旧值。
func (s *Store) UpdateSyncSourceConfig(ctx context.Context, r SyncSourceRow, apiKey, directDBDSN string, directDBSet, directDBClear bool) error {
	if directDBSet && directDBClear {
		return errors.New("direct_db_set and direct_db_clear are mutually exclusive")
	}
	if directDBSet && strings.TrimSpace(directDBDSN) == "" {
		return errors.New("direct database DSN is empty")
	}
	oldDirectSecret := strings.TrimSpace(r.DirectDBSecret)
	canonicalSecret := SourceDirectDBSecretKey(r.ID)

	// 列里保存的 secret key：清除或非 sub2api 时清空；提交新 DSN 时统一用
	// 规范键名；否则沿用旧值不动。
	directSecret := ""
	switch {
	case directDBClear || r.Kind != acctmap.SourceKindSub2API:
		directSecret = ""
	case directDBSet:
		directSecret = canonicalSecret
	default:
		directSecret = r.DirectDBSecret
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`UPDATE sync_sources SET kind=?,name=?,base_url=?,direct_auth_dir=?,direct_db_secret=?,interval_s=?,enabled=?,updated_at=? WHERE id=?`,
		r.Kind, r.Name, r.BaseURL, r.DirectAuthDir, directSecret,
		r.IntervalS, boolToInt(r.Enabled), time.Now().UnixMilli(), r.ID); err != nil {
		return err
	}

	// DSN secret 清理：清除、换库或换成 cpa 时，删掉旧键，防止残留明文。
	if directDBClear || r.Kind != acctmap.SourceKindSub2API {
		if _, err := tx.ExecContext(ctx, `DELETE FROM secrets WHERE key IN (?,?)`,
			canonicalSecret, oldDirectSecret); err != nil {
			return err
		}
	} else if directDBSet && oldDirectSecret != "" && oldDirectSecret != canonicalSecret {
		if _, err := tx.ExecContext(ctx, `DELETE FROM secrets WHERE key=?`, oldDirectSecret); err != nil {
			return err
		}
	}
	if directDBSet {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO secrets(key,value) VALUES(?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			canonicalSecret, []byte(directDBDSN)); err != nil {
			return err
		}
	}
	if apiKey != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO secrets(key,value) VALUES(?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			SourceKeySecretKey(r.ID), []byte(apiKey)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteSyncSource 在同一事务内删除源、其 acct_map 映射和 secrets 键。
// 这样后台同步要么完整看到旧源，要么在快照写入时发现源已不存在，不能复活映射。
func (s *Store) DeleteSyncSource(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	owner := acctmap.SourceInstancePrefix + strconv.FormatInt(id, 10)
	var directSecret string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(direct_db_secret,'') FROM sync_sources WHERE id=?`, id).Scan(&directSecret); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM acct_map WHERE source=?`, owner); err != nil {
		return err
	}
	if err := gcAcctEgress(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sync_sources WHERE id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM secrets WHERE key IN (?,?,?)`, SourceKeySecretKey(id), SourceDirectDBSecretKey(id), directSecret); err != nil {
		return err
	}
	return tx.Commit()
}

// TouchSyncSource 更新最近同步时间与状态摘要。
func (s *Store) TouchSyncSource(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sync_sources SET last_sync_at=?,last_status=?,updated_at=? WHERE id=?`,
		time.Now().UnixMilli(), truncate(status, 1024, 256), time.Now().UnixMilli(), id)
	return err
}

// TouchSyncSourceStatus 只更新 last_status，不动 last_sync_at。last_sync_at
// 表示"最近一次全量同步时间"，增量失败提示专用（docs/012 v2 §7）。
func (s *Store) TouchSyncSourceStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sync_sources SET last_status=?,updated_at=? WHERE id=?`,
		truncate(status, 1024, 256), time.Now().UnixMilli(), id)
	return err
}

// GetSourceAPIKey 读取源的 api_key 明文。
func (s *Store) GetSourceAPIKey(ctx context.Context, id int64) (string, error) {
	b, err := s.GetSecret(ctx, SourceKeySecretKey(id))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// GetSourceDirectDB 读取 Sub2API direct 源的 PostgreSQL DSN。
func (s *Store) GetSourceDirectDB(ctx context.Context, id int64) (string, error) {
	row, ok, err := s.GetSyncSource(ctx, id)
	if err != nil {
		return "", err
	}
	if !ok || row.DirectDBSecret == "" {
		return "", ErrNotFound
	}
	b, err := s.GetSecret(ctx, row.DirectDBSecret)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
