// Package store 封装 SQLite 持久化：四表 schema、首次引导、
// secrets 读写、设置键值与审计日志的异步批量写入。
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mitmrouter/internal/httpnames"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，注册名 "sqlite"
)

// Store 持有唯一写连接池（MaxOpenConns=1 串行化写入，读多场景后续可拆只读池）。
type Store struct {
	db *sql.DB // 写连接（MaxOpenConns=1）
	ro *sql.DB // 只读连接池（WAL 并发读者，query_only）
}

// ErrUpstreamHasBindings 表示正在改平台的上游仍被账户绑定。
var ErrUpstreamHasBindings = errors.New("upstream has account bindings")

// schema 是当前完整建表语句（幂等，可重复执行）。
// 常规结构演进直接修改本定义；已发布过的敏感数据字段由 ensureSchema 中的
// 定向清理迁移移除，避免旧库继续保留不再需要的审计数据。
const schema = `
CREATE TABLE IF NOT EXISTS settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS upstreams (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT NOT NULL UNIQUE,
  platform   TEXT NOT NULL,
  base_url   TEXT NOT NULL,
  inject     TEXT,
  enabled    INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS access_logs (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  ts         INTEGER NOT NULL,
  req_id     TEXT NOT NULL DEFAULT '', -- 服务端为每个入站请求生成的关联 ID
  method     TEXT NOT NULL,
  host       TEXT NOT NULL,
  path       TEXT NOT NULL,
  status     INTEGER NOT NULL,
  dur_ms     INTEGER NOT NULL,
  ttfb_ms    INTEGER, -- 从路由开始处理到首次提交响应头的时延；未能提交响应时为空
  bytes_out  INTEGER NOT NULL DEFAULT 0,
  has_marker INTEGER NOT NULL,
  account    TEXT NOT NULL DEFAULT '', -- acct_map 命中时的真实账号；空=未映射
  account_fp TEXT NOT NULL,            -- 派生的粘滞会话 ID
  upstream       TEXT NOT NULL,
  err            TEXT, -- legacy generic error field; retained for existing databases
  internal_error TEXT  -- MITMRouter-generated failure class; NULL for HTTP responses
);
CREATE INDEX IF NOT EXISTS idx_logs_ts      ON access_logs(ts);
CREATE INDEX IF NOT EXISTS idx_logs_account ON access_logs(account_fp, ts);
CREATE TABLE IF NOT EXISTS secrets (
  key   TEXT PRIMARY KEY,
  value BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS marker_salts (
  marker_fp  TEXT PRIMARY KEY,
  salt       INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);`

// schemaAcctMap 账号映射表（订阅号 AT/RT → 平台/账号ID）与拉取源配置。
// 唯一键 (platform, source, account, rt_fp, source_type)：同键即同一行——
// AT 更新原地覆盖、快照消失删除、新出现插入。source 为来源实例
// （'src:<id>'，同类源可并存多个；'api' = 推送/手动）；source_type 为
// 来源类型全名（'CLIProxyAPI' | 'Sub2API'），拉取源按实例自动映射，
// 手动推送可指定任意非空自定义类型，便于扩展新上游。
const schemaAcctMap = `
CREATE TABLE IF NOT EXISTS acct_map (
  platform    TEXT NOT NULL,
  source      TEXT NOT NULL,             -- 来源实例：'src:<id>' / 'api'
  source_type TEXT NOT NULL,             -- 来源类型全名：'CLIProxyAPI' | 'Sub2API' | 自定义
  account     TEXT NOT NULL,             -- 账号标识：邮箱/uuid/任意调用方自定义串
  at_fp       TEXT NOT NULL DEFAULT '',  -- access_token 指纹
  rt_fp       TEXT NOT NULL DEFAULT '',  -- refresh_token 指纹（主键成分）
  at_hint     TEXT NOT NULL DEFAULT '',
  rt_hint     TEXT NOT NULL DEFAULT '',
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY(platform, source, account, rt_fp, source_type)
);
CREATE INDEX IF NOT EXISTS idx_acct_map_source ON acct_map(source);
CREATE TABLE IF NOT EXISTS sync_sources (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  kind              TEXT NOT NULL,        -- 'cpa' | 'sub2api'
  name              TEXT NOT NULL UNIQUE,
  mode              TEXT NOT NULL DEFAULT 'api', -- 已废弃：增量直读由 direct_auth_dir/direct_db_secret 是否配置决定，本列不再读写
  base_url          TEXT NOT NULL,
  direct_auth_dir   TEXT NOT NULL DEFAULT '',
  direct_db_secret  TEXT NOT NULL DEFAULT '', -- key in secrets; never the DSN itself
  interval_s        INTEGER NOT NULL DEFAULT 600,
  enabled           INTEGER NOT NULL DEFAULT 1,
  last_sync_at      INTEGER,
  last_status       TEXT NOT NULL DEFAULT '',
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL
);`

// schemaAcctEgress 账户 ↔ 出站 绑定表（docs/011-plain-binding-design.md）。
// mode 是账户属性：同一账户的全部行同值，由「按账户整体替换」的写路径单事务保证。
// egress_id 指向 upstreams 中 platform='plain' 的行；库不开外键，级联删除与
// 「账号消失即清绑定」（gcAcctEgress）均由本包事务保证。
const schemaAcctEgress = `
CREATE TABLE IF NOT EXISTS acct_egress (
  platform   TEXT NOT NULL,             -- 账号平台（acct_map.platform）
  account    TEXT NOT NULL,             -- 账号标识（acct_map.account，统一小写）
  egress_id  INTEGER NOT NULL,          -- upstreams.id（platform='plain' 行）
  mode       TEXT NOT NULL DEFAULT 'sticky', -- 'sticky' | 'random'
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(platform, account, egress_id)
);
CREATE INDEX IF NOT EXISTS idx_acct_egress_egress ON acct_egress(egress_id);`

// schemaSyncEvents 账号映射更新记录（docs/013-update-log-design.md）。
// 每次 acct_map 因同步/文件扫描/推送/删除而变化时记一条，与 access_logs
// 分表：那边是代理流量，这边是映射变更，保留与清空互不影响。
const schemaSyncEvents = `
CREATE TABLE IF NOT EXISTS sync_events (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  ts      INTEGER NOT NULL,
  kind    TEXT NOT NULL,              -- direct_file | direct_incremental | api_sync | push | delete
  source  TEXT NOT NULL DEFAULT '',   -- 'src:<id>' / 'api'，与 acct_map.source 同口径
  status  TEXT NOT NULL DEFAULT 'ok', -- ok | error
  summary TEXT NOT NULL DEFAULT '',   -- 一句话人话摘要；绝不存 token 明文或完整指纹
  detail  TEXT NOT NULL DEFAULT ''    -- 可选补充：文件绝对路径等
);
CREATE INDEX IF NOT EXISTS idx_sync_events_ts ON sync_events(ts);`

// BootInfo 携带引导阶段产生的一次性信息（如生成的管理员明文口令，仅打印一次）。
type BootInfo struct {
	DataDir       string
	AdminPassword string // 仅当新生成时非空；调用方打印后即丢弃
	FreshInstall  bool
}

// Open 打开（或创建）数据目录下的路由库并完成 schema 迁移。
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	// MkdirAll 不会收紧已存在但权限宽松的目录；这里显式收紧到 0700，
	// 否则 db 文件即便 0600，宽松目录仍可被同组/其他用户遍历读取。
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("tighten data directory: %w", err)
	}
	dbPath := filepath.Join(dataDir, "router.db")
	dsn := sqliteDSN(dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // 单写者串行化；WAL 下读写比例低，规模足够
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.ensureSchema(); err != nil {
		db.Close()
		return nil, err
	}
	// 路由库保存 CA 私钥、上游凭据、管理员口令哈希、会话签名密钥。
	// 驱动按 umask 创建文件（0022 下即 0644）；这里强制收紧到 0600，
	// 同时覆盖 WAL 伴随文件。数据目录已 0700，是更强的访问兜底。
	if err := tightenDBFilePerms(dbPath); err != nil {
		db.Close()
		return nil, err
	}
	ro, err := sql.Open("sqlite", dsn+"&_pragma=query_only(1)")
	if err != nil {
		db.Close()
		return nil, err
	}
	ro.SetMaxOpenConns(4)
	s.ro = ro
	return s, nil
}

// tightenDBFilePerms 把 SQLite 主库及 WAL/SHM 伴随文件收紧到 0600。
// 驱动按进程 umask 创建这些文件；不收紧则在曾宽松的数据目录下会泄露
// CA 私钥、上游凭据与管理员口令哈希。
func tightenDBFilePerms(dbPath string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := dbPath + suffix
		if err := os.Chmod(p, 0o600); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("tighten %s: %w", p, err)
		}
	}
	return nil
}

// ensureSchema 幂等创建全部表与索引，并清理已废弃的 header-name 审计数据。
func (s *Store) ensureSchema() error {
	if _, err := s.db.Exec(schema + schemaAcctMap + schemaAcctEgress + schemaSyncEvents); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	if err := s.removeLegacyHeaderAudit(); err != nil {
		return err
	}
	if err := s.ensureAuditAccountColumn(); err != nil {
		return err
	}
	if err := s.ensureAuditReqIDColumn(); err != nil {
		return err
	}
	if err := s.ensureAuditInternalErrorColumn(); err != nil {
		return err
	}
	if err := s.ensureAuditTTFBColumn(); err != nil {
		return err
	}
	if err := s.ensureColumn("sync_sources", "empty_streak", "empty_streak INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("sync_sources", "mode", "mode TEXT NOT NULL DEFAULT 'api'"); err != nil {
		return err
	}
	if err := s.ensureColumn("sync_sources", "direct_auth_dir", "direct_auth_dir TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("sync_sources", "direct_db_secret", "direct_db_secret TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return nil
}

// removeLegacyHeaderAudit 物理移除旧版 access_logs.headers 列及其数据，并删除
// audit_header_names 设置。列存在时才执行 ALTER，因而可安全地在每次启动重复执行。
func (s *Store) removeLegacyHeaderAudit() error {
	rows, err := s.db.Query(`PRAGMA table_info(access_logs)`)
	if err != nil {
		return fmt.Errorf("inspect access_logs columns: %w", err)
	}

	hasHeaders := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("scan access_logs columns: %w", err)
		}
		if name == "headers" {
			hasHeaders = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate access_logs columns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close access_logs column query: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if hasHeaders {
		if _, err := tx.Exec(`ALTER TABLE access_logs DROP COLUMN headers`); err != nil {
			return fmt.Errorf("drop legacy access_logs.headers: %w", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM settings WHERE key='audit_header_names'`); err != nil {
		return fmt.Errorf("delete legacy audit setting: %w", err)
	}
	return tx.Commit()
}

// ensureAuditAccountColumn 为现有审计库补上真实账号列。账号不是凭据：它只在
// acct_map 命中时写入，空串表示请求未能归属账号。ALTER 是幂等保护，仅缺列时执行。
func (s *Store) ensureAuditAccountColumn() error {
	return s.ensureAuditColumn("account", "account TEXT NOT NULL DEFAULT ''")
}

// ensureAuditReqIDColumn 为早于 request ID 功能创建的数据库补上请求关联 ID 列。
// 历史记录无法还原原始请求 ID，因此保持为空。
func (s *Store) ensureAuditReqIDColumn() error {
	return s.ensureAuditColumn("req_id", "req_id TEXT NOT NULL DEFAULT ''")
}

// ensureAuditInternalErrorColumn 为 MITMRouter 自身产生的转发失败补上独立内部错误分类列。
// 旧 err 列中已有的安全分类会被保留，方便继续筛选历史错误。
func (s *Store) ensureAuditInternalErrorColumn() error {
	if err := s.ensureAuditColumn("internal_error", "internal_error TEXT"); err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE access_logs
		SET internal_error=trim(err), status=0
		WHERE internal_error IS NULL
		  AND trim(err) IN ('upstream_config','dns','dial','timeout','eof','tls',
		                    'proxy_connect','proxy_connect_rejected','canceled','transport',
		                    'upstream_response_eof','upstream_response_read','downstream_write')`); err != nil {
		return fmt.Errorf("backfill access_logs internal_error: %w", err)
	}
	return nil
}

// ensureAuditTTFBColumn 补上可为空的首字节时延列。NULL 表示该历史请求早于此指标，
// 或当时未能提交任何响应。
func (s *Store) ensureAuditTTFBColumn() error {
	return s.ensureAuditColumn("ttfb_ms", "ttfb_ms INTEGER")
}

func (s *Store) ensureAuditColumn(name, definition string) error {
	return s.ensureColumn("access_logs", name, definition)
}

// ensureColumn 幂等补列：仅当表中缺该列时执行 ALTER。
func (s *Store) ensureColumn(table, name, definition string) error {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return fmt.Errorf("inspect %s %s column: %w", table, name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var columnName, typ string
		var dflt any
		if err := rows.Scan(&cid, &columnName, &typ, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("scan %s %s column: %w", table, name, err)
		}
		if columnName == name {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate %s %s column: %w", table, name, err)
	}
	if _, err := s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + definition); err != nil {
		return fmt.Errorf("add %s %s column: %w", table, name, err)
	}
	return nil
}

// Bootstrap 打开库并完成首次初始化：默认设置、随机盐、会话密钥、管理员口令。
// 返回的 BootInfo.AdminPassword 仅在新生成时非空——调用方必须打印一次后丢弃。
// 监听地址（接入口/管理台）不属于设置表：由启动参数指定，每次启动生效。
func Bootstrap(dataDir string) (*Store, BootInfo, error) {
	info := BootInfo{DataDir: dataDir}
	st, err := Open(dataDir)
	if err != nil {
		return nil, info, err
	}
	ctx := context.Background()

	// 首次安装判定改用必然播种且不会被删除的 hash_salt 键
	var fresh bool
	if _, ok, _ := st.getSetting(ctx, "hash_salt"); !ok {
		fresh = true
	}
	info.FreshInstall = fresh

	defaults := map[string]any{
		"listen_tls_cert":               "",
		"listen_tls_key":                "",
		"admin_tls_cert":                "",
		"admin_tls_key":                 "",
		"listen_auth":                   "",
		"default_upstream":              "",
		"no_marker_policy":              "default_session",
		"marker_path_parts":             []string{}, // 空 = 所有路径同一规则（推荐）
		"marker_headers":                []string{httpnames.HeaderAuthorization, httpnames.HeaderXAPIKey, httpnames.HeaderAPIKey, httpnames.HeaderXGoogAPIKey},
		"hash_salt":                     randHex(32),
		"sid_len":                       16,
		"session_ttl_min":               0,
		"salt_rotate_failure_threshold": 2,
		"block_private_targets":         true,
		"acctmap_enabled":               true,
		"acl_whitelist":                 []string{},
		"acl_blacklist":                 []string{},
		"log_retention_days":            30,
		"sync_empty_clear_threshold":    3,
	}
	for k, v := range defaults {
		if err := st.seedSetting(ctx, k, v); err != nil {
			return nil, info, err
		}
	}
	// private_target_direct 过去默认是 true。直接删除它，而不是猜测旧值 true
	// 是否由管理员有意设置；替代设置始终从上面的安全默认值开始。
	if err := st.DeleteSetting(ctx, "private_target_direct"); err != nil {
		return nil, info, err
	}
	// incremental_enabled 全局增量开关已废弃（docs/012 v2）：增量直读改由
	// 每个 source 的增量路径字段驱动，直接删掉旧键。
	if err := st.DeleteSetting(ctx, "incremental_enabled"); err != nil {
		return nil, info, err
	}
	// 旧 direct 源的 interval_s 曾被挪用为增量轮询间隔（3 秒）；现在 interval_s
	// 只表示全量同步间隔，统一抬到下限 60。
	if _, err := st.db.ExecContext(ctx, `UPDATE sync_sources SET interval_s=60 WHERE interval_s<60`); err != nil {
		return nil, info, fmt.Errorf("clamp sync source intervals: %w", err)
	}

	// 会话签名密钥
	if err := st.ensureSecret(ctx, "session_hmac_key", []byte(randHex(32))); err != nil {
		return nil, info, err
	}

	// 管理员口令：缺失则生成随机口令（bcrypt 入库，明文仅本次返回）
	if _, err := st.GetSecret(ctx, "admin_password_bcrypt"); errors.Is(err, ErrNotFound) {
		pw := randHex(12) // 96bit，控制台打印一次
		hash, err := bcryptHash(pw)
		if err != nil {
			return nil, info, err
		}
		if err := st.SetSecret(ctx, "admin_password_bcrypt", []byte(hash)); err != nil {
			return nil, info, err
		}
		info.AdminPassword = pw
	} else if err != nil {
		return nil, info, err
	}
	return st, info, nil
}

// ---------- settings ----------

func (s *Store) seedSetting(ctx context.Context, key string, val any) error {
	if _, ok, err := s.getSetting(ctx, key); err != nil {
		return err
	} else if ok {
		return nil
	}
	b, err := json.Marshal(val)
	if err != nil {
		return err
	}
	return s.SetSetting(ctx, key, string(b))
}

func (s *Store) getSetting(ctx context.Context, key string) (val string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return val, err == nil, err
}

// AllSettings 返回全部设置的原始 JSON 文本。
func (s *Store) AllSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.ro.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

// Close 关闭全部连接。
func (s *Store) Close() error {
	if s.ro != nil {
		s.ro.Close()
	}
	return s.db.Close()
}

// SetSettingsTx 单事务批量 UPSERT 设置键：要么全部生效要么全部不变。
func (s *Store) SetSettingsTx(ctx context.Context, kv map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().UnixMilli()
	for k, v := range kv {
		if _, err := stmt.ExecContext(ctx, k, v, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// truncate 字节上限粗截后按 rune 对齐：合法 UTF-8 且两个上限均不越界。
func truncate(s string, maxBytes, maxRunes int) string {
	if len(s) > maxBytes {
		s = strings.ToValidUTF8(s[:maxBytes], "") // 丢弃被字节边界撕裂的尾部 rune，而非产出 U+FFFD
	}
	rs := []rune(s)
	if len(rs) > maxRunes {
		rs = rs[:maxRunes]
	}
	return string(rs)
}

// SetSetting 写入单个设置键（value 为 JSON 文本）。
func (s *Store) SetSetting(ctx context.Context, key, valueJSON string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, valueJSON, time.Now().UnixMilli())
	return err
}

// DeleteSetting 删除设置键。
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, key)
	return err
}

// ---------- secrets ----------

// ErrNotFound 表示键不存在。
var ErrNotFound = errors.New("not found")

// GetSecret 读取密钥材料。
func (s *Store) GetSecret(ctx context.Context, key string) ([]byte, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM secrets WHERE key=?`, key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// SetSecret 写入/覆盖密钥材料。
func (s *Store) SetSecret(ctx context.Context, key string, val []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO secrets(key,value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, val)
	return err
}

// DeleteSecret 删除密钥材料；键不存在时静默成功。
func (s *Store) DeleteSecret(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE key=?`, key)
	return err
}

func (s *Store) ensureSecret(ctx context.Context, key string, gen []byte) error {
	if _, err := s.GetSecret(ctx, key); errors.Is(err, ErrNotFound) {
		return s.SetSecret(ctx, key, gen)
	} else if err != nil {
		return err
	}
	return nil
}

// ---------- upstreams ----------

// UpstreamRow 是 upstreams 表的原始行。
type UpstreamRow struct {
	ID        int64
	Name      string
	Platform  string
	BaseURL   string
	Inject    sql.NullString
	Enabled   bool
	CreatedAt int64
	UpdatedAt int64
}

// ListUpstreams 返回全部上游条目。
func (s *Store) ListUpstreams(ctx context.Context) ([]UpstreamRow, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT id,name,platform,base_url,inject,enabled,created_at,updated_at FROM upstreams ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpstreamRow
	for rows.Next() {
		var r UpstreamRow
		var en int
		if err := rows.Scan(&r.ID, &r.Name, &r.Platform, &r.BaseURL, &r.Inject, &en, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Enabled = en != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateUpstream 新增条目，返回新 ID。名称冲突返回错误（含 UNIQUE 关键字）。
func (s *Store) CreateUpstream(ctx context.Context, name, platform, baseURL, inject string, enabled bool) (int64, error) {
	now := time.Now().UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO upstreams(name,platform,base_url,inject,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
		name, platform, baseURL, nullIfEmpty(inject), boolToInt(enabled), now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateUpstream 全量更新条目字段。
func (s *Store) UpdateUpstream(ctx context.Context, r UpstreamRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := updateUpstreamTx(ctx, tx, r); err != nil {
		return err
	}
	return tx.Commit()
}

func updateUpstreamTx(ctx context.Context, tx *sql.Tx, r UpstreamRow) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE upstreams SET name=?,platform=?,base_url=?,inject=?,enabled=?,updated_at=?
		 WHERE id=? AND (platform=? OR NOT EXISTS (
			SELECT 1 FROM acct_egress WHERE egress_id=?
		 ))`,
		r.Name, r.Platform, r.BaseURL, r.Inject, boolToInt(r.Enabled), time.Now().UnixMilli(), r.ID,
		r.Platform, r.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	var currentPlatform string
	if err := tx.QueryRowContext(ctx, `SELECT platform FROM upstreams WHERE id=?`, r.ID).Scan(&currentPlatform); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // 保持旧 UpdateUpstream 对不存在 ID 的兼容行为
		}
		return err
	}
	if currentPlatform != r.Platform {
		return ErrUpstreamHasBindings
	}
	return nil
}

// UpdateUpstreamAndDefault 原子更新上游及其默认名称。默认上游改名时，
// 两张表必须同成同败，避免重启后加载到悬空的旧名称。
func (s *Store) UpdateUpstreamAndDefault(ctx context.Context, r UpstreamRow, defaultName string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := updateUpstreamTx(ctx, tx, r); err != nil {
		return err
	}
	b, err := json.Marshal(defaultName)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		"default_upstream", string(b), time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteUpstream 按 ID 删除，并在同一事务内级联清除 acct_egress 中引用它的绑定行
// （非 egress 上游删除时是空操作）。绑定随出站一同消失是设计语义（docs/011 §1.2）。
func (s *Store) DeleteUpstream(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM acct_egress WHERE egress_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM upstreams WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// CountUpstreams 返回条目数。
func (s *Store) CountUpstreams(ctx context.Context) (int, error) {
	var n int
	err := s.ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM upstreams`).Scan(&n)
	return n, err
}

// ---------- marker_salts：per-marker 动态盐值持久化 ----------

// UpsertMarkerSalt 落库一条盐值。marker_fp 为 Marker 的完整 SHA-256 十六进制指纹（不落明文）。
func (s *Store) UpsertMarkerSalt(ctx context.Context, akFp string, salt int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO marker_salts(marker_fp,salt,updated_at) VALUES(?,?,?)
		 ON CONFLICT(marker_fp) DO UPDATE SET salt=excluded.salt, updated_at=excluded.updated_at`,
		akFp, salt, time.Now().UnixMilli())
	return err
}

// MarkerSaltRow 是 marker_salts 表的原始行。
type MarkerSaltRow struct {
	FP   string
	Salt int64
}

// LoadMarkerSalts 按最近活跃倒序返回前 limit 条，供启动时灌回内存 LRU；
// 超出容量的最旧条目自然被舍弃（与内存淘汰语义一致）。
func (s *Store) LoadMarkerSalts(ctx context.Context, limit int) ([]MarkerSaltRow, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT marker_fp,salt FROM marker_salts ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MarkerSaltRow
	for rows.Next() {
		var r MarkerSaltRow
		if err := rows.Scan(&r.FP, &r.Salt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------- access_logs：异步批量写入 ----------

// LogEntry 对应 access_logs 一行。
type LogEntry struct {
	ID     int64  `json:"id"`
	Ts     int64  `json:"ts"`
	ReqID  string `json:"req_id"`
	Method string `json:"method"`
	Host   string `json:"host"`
	Path   string `json:"path"`
	// Status 是上游/目标返回的 HTTP 状态。0 表示 MITMRouter 自身产生的传输失败，
	// 具体分类见 InternalError。
	Status int   `json:"status"`
	DurMS  int64 `json:"dur_ms"`
	// TTFBMS 是从收到请求到首次把响应提交给客户端的处理时长。nil 表示无法提交响应
	// （或旧记录早于此指标）；0 则是有效测量值。
	TTFBMS        *int64 `json:"ttfb_ms,omitempty"`
	BytesOut      int64  `json:"bytes_out"`
	HasMarker     bool   `json:"has_marker"`
	Account       string `json:"account,omitempty"` // acct_map 命中时的真实账号；空=未映射
	AccountFP     string `json:"account_fp"`        // 派生的粘滞会话 ID
	Upstream      string `json:"upstream"`
	InternalError string `json:"internal_error,omitempty"`
}

// RunLogWriter 消费日志通道，约每 200ms 或攒满 256 条批量落库；
// ctx 结束后先排空通道内剩余条目再返回（配合优雅退出）。
func (s *Store) RunLogWriter(ctx context.Context, ch <-chan LogEntry) {
	const batchLimit = 256
	// 落库操作必须脱离调用方生命周期：优雅退出排空时 ctx 已取消，
	// 若直接使用会导致最后一批日志写入失败丢失。
	wctx := context.WithoutCancel(ctx)
	buf := make([]LogEntry, 0, batchLimit)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	flush := func() {
		if len(buf) == 0 {
			return
		}
		fctx, cancel := context.WithTimeout(wctx, 10*time.Second)
		defer cancel()
		if err := s.insertLogs(fctx, buf); err != nil {
			slog.Error("audit: batch flush failed", "count", len(buf), "err", err)
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

func (s *Store) insertLogs(ctx context.Context, entries []LogEntry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO access_logs(ts,req_id,method,host,path,status,dur_ms,ttfb_ms,bytes_out,has_marker,account,account_fp,upstream,internal_error)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range entries {
		path := truncate(e.Path, 2048, 256)
		host := truncate(e.Host, 512, 256)
		internalError := truncate(e.InternalError, 1024, 256)
		account := truncate(e.Account, 512, 256)
		reqID := truncate(e.ReqID, 64, 32)
		if _, err := stmt.ExecContext(ctx, e.Ts, reqID, e.Method, host, path, e.Status,
			e.DurMS, e.TTFBMS, e.BytesOut, boolToInt(e.HasMarker), account, e.AccountFP, e.Upstream, nullIfEmpty(internalError)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------- 小工具 ----------

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return driver.Value(s)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// randHex 返回 n 字节随机的 hex 编码（长度 2n）。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand 失败属系统级故障
	}
	return hex.EncodeToString(b)
}
