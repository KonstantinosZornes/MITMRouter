# MITMRouter 详细设计（当前实现）

> 本文描述当前仓库 HEAD 的真实实现，不是未来版本的计划书。
>
> MITMRouter 是一个本地 HTTP MITM 流量路由器：客户端只需要把网络出口指向本机，服务就能在需要时解密 HTTPS，请求中识别 API Key/Token，按「订阅账号」或 Marker 推导粘滞身份，再经配置的上游出口访问目标站。
>
> 当前实现还支持：账号映射同步、账号到普通代理的绑定、SQLite 单文件存储、Vue3 管理台、审计和更新记录。

**最重要的约束：** MITMRouter 只改变“从哪条出口连接出去”。对目标站的业务请求和响应，不主动修改 URL、请求头、请求体、响应头或响应体；请求 ID、路由信息和审计信息只存在于服务端内部。HTTP/1.1、HTTP/2 及隧道协议自身的 framing 差异除外。

---

## 1. 先看整体：请求到底怎么走

### 1.1 两类身份

系统里的“账号”不是只有一种含义：

- **Marker**：请求中临时可见的唯一凭据，例如 `Authorization: Bearer ...`、`x-api-key` 或 `api-key`。找不到账号映射时，就用 Marker 自身计算粘滞身份。
- **订阅账号**：由 CLIProxyAPI、Sub2API 或手动登记得到的逻辑账号，例如邮箱、UUID 或调用方自定义的账号名。请求里的 AT/RT 只用于查表，命中后按逻辑账号路由，所以 AT 轮换不会改变出口身份。

核心决策可以压缩成下面几步：

```text
收到请求
  │
  ├─ 目标是否被 ACL 放行？
  │    ├─ 否：本地返回 403，不连接目标
  │    └─ 是：继续解析身份
  │
  ├─ 按目标 Host 识别内置 AI 平台
  ├─ 从 header/query，必要时从特定 body 提取凭据
  ├─ 用 sha256(platform + ":" + normalized credential) 查 acct_map
  │    ├─ 命中：得到 platform/account，账号级粘滞
  │    └─ 未命中：使用凭据本身作为 Marker，兼容旧版哈希行为
  │
  ├─ 命中账号出站绑定？
  │    ├─ 是：从绑定的 plain 出站中按 sticky/random 选择
  │    └─ 否：选择 default_upstream，并注入平台会话参数
  │
  └─ 经直连或上游出口转发，原样流式回传响应
```

### 1.2 为什么需要 MITM

普通 HTTP 代理只看得到 CONNECT 隧道，隧道里的 TLS 内容是密文，因此看不到目标请求的 `Authorization` 或其他凭据。只有本地终结 TLS，才能先识别账号，再为不同账号选择不同的粘滞会话。

这与 Resin 等上游平台的关系是：

- MITMRouter 负责本地解密、身份识别和路由；
- Resin、DataImpulse、Decodo、1024proxy 等负责真正的住宅/机房出口和平台侧粘滞；
- MITMRouter 把自己计算出的短小会话身份写入上游凭据的用户名部分。

### 1.3 当前能力

- HTTP 代理接入：绝对式明文请求和 HTTPS `CONNECT`；
- CONNECT 后按 SNI 现场签发叶子证书并 MITM，或作为盲隧道透明转发；
- 上游支持 `http://`、`https://`、`socks5://`、`socks5h://`；
- 内置 DataImpulse、Decodo、1024proxy、Resin、Generic 和 Plain 六种上游平台；
- Marker 粘滞、订阅账号映射、账号到普通代理的 sticky/random 绑定；
- CLIProxyAPI/Sub2API API 全量同步；CPA 文件直读、Sub2API PostgreSQL 增量直读；
- SSE、1xx、Trailer、HTTP/1.1 `101 Switching Protocols` 流式透传；
- SQLite 审计、同步更新记录、Prometheus 文本指标；
- Vue3 管理台：基础设置、上游出口、账号管理、访问审计、更新记录。

---

## 2. 范围与非目标

### 2.1 功能范围

1. 接入口默认监听 `127.0.0.1:55666`，管理台默认独立监听 `127.0.0.1:55667`。
2. 接入口可配置入站 Basic 认证；认证失败返回标准 `407 Proxy Authentication Required`。
3. HTTPS 目标默认进入 MITM 解析；ACL 先决定目标是否允许访问，放行后再按流量形态选择 MITM 或盲隧道。
4. 对已知 AI Host，按 AT/RT/API Key 指纹查找订阅账号；对未知或未命中的凭据保留 Marker 哈希回退。
5. 通过上游平台注入器把粘滞身份写入出口凭据；`plain` 上游不注入任何会话参数。
6. 所有需要持久化的配置、凭据材料、账号映射和审计记录保存在数据目录中的 SQLite 数据库；运行期快照、失败计数和事件队列只在内存中，不使用 YAML、`.env` 或 JSON 配置文件。
7. 管理台修改设置、上游、同步源、账号映射和绑定后，运行面通过快照或重载即时看到变化；TLS 路径变更需要重启。

### 2.2 非目标

- 不提供 SOCKS5 入站；本地接入语义是 HTTP 代理。
- 不转发 UDP、QUIC 或 HTTP/3；客户端若走 UDP 443，会绕过本服务。
- 不对抗证书固定（certificate pinning）。
- 不自动重试业务请求；LLM 生成请求可能非幂等，重试交给客户端 SDK。
- 不做跨多个默认上游的自动故障切换；上游不可用时通过受控失败和身份盐轮换处理。
- 不做多管理员、RBAC 或复杂权限模型。
- 不修改 CPA、Sub2API 源码；direct 监控只读文件目录或 PostgreSQL。
- 不把 Marker、AT、RT、上游密码或完整 DSN 写进审计、指标或普通日志。

---

## 3. 总体架构

### 3.1 流量面

```text
                         MITMRouter 进程
┌────────────┐       ┌─────────────────────────────────────────────┐
│ curl / SDK │ HTTP  │ ingress listener :55666                     │
│ AI 客户端  ├──────►│                                               │
└────────────┘       │  CONNECT                                      │
                      │    ├─ 入站认证                               │
                      │    ├─ ACL 拒绝 → 本地 403                    │
                      │    ├─ ACL 放行 → 200 后劫持连接              │
                      │    ├─ TLS ClientHello → TLS MITM             │
                      │    └─ 其他 → 盲隧道                           │
                      │                                               │
                      │  绝对式明文请求                               │
                      │    └─ 认证 → forward                         │
                      │                                               │
                      │  MITM/明文 forward                            │
                      │    ├─ identity.Resolver                       │
                      │    ├─ acctmap.Registry 查账号                 │
                      │    ├─ acctegress.Table 查账户绑定             │
                      │    ├─ upstream.Table 选上游                   │
                      │    ├─ SessionInjector 生成出口 URL            │
                      │    └─ http.Transport.RoundTrip + 流式回传      │
                      └───────────────────────┬─────────────────────┘
                                              │ HTTP CONNECT / SOCKS5 CONNECT
                                              ▼
                                    上游出口或目标站
```

普通 HTTP 转发使用共享 `http.Transport`。每个请求在 context 中携带选出的出口 URL，`Transport.Proxy` 从 context 读取它；没有出口 URL 时直连。

盲隧道不经过 HTTP Handler：HTTP 上游发送 CONNECT，SOCKS5 上游发送 SOCKS5 CONNECT，隧道建立后用两个 `io.Copy` 双向转发字节，并保留 TCP 半关闭语义。

### 3.2 管理面

```text
浏览器
  │  http(s)://<admin-addr>/ui/
  ▼
admin listener :55667
  ├─ /ui*       → embed.FS 中的 Vue SPA
  ├─ /api/*     → 管理 REST（除 login 外要求会话）
  └─ /metrics   → Prometheus 文本（开启且要求会话）
                         │
                         ▼
              settings / upstreams / marker_salts / acct_map / sync_sources
              acct_egress / access_logs / sync_events / secrets
```

接入口和管理台是两个独立的 `http.Server`、两个独立监听地址：

- 接入口只接受 CONNECT 和绝对式请求；浏览器直接访问 origin-form 时只返回静态提示页，不暴露 `/api`、`/ui` 或 `/metrics`。
- 管理台只接受 origin-form；收到 CONNECT 或绝对式代理请求时返回 `404 admin_no_ingress`。

### 3.3 数据与控制流

配置写入大致遵循：

```text
管理 API 校验
  → SQLite 写事务
  → 替换 settings / upstream / binding 快照
  → 后续请求无须重启即可读取新快照
```

账号映射写入遵循：

```text
同步器或管理 API
  → acct_map 单事务写入，并清理孤儿 acct_egress
  → 全量 Reload acctmap.Registry
  → OnMapChange 重建 acctegress.Table
```

同一 source 的全量同步和 direct 增量操作由 source 级锁串行；不同 source 可以并行。所有内存 Registry 重载和绑定快照重建再经过进程级 map-change 锁，避免旧快照晚到时覆盖新状态。

---

## 4. 启动、存储与默认值

### 4.1 启动参数

启动形态：

```bash
mitmrouter \
  -data ./data \
  -addr 127.0.0.1:55666 \
  -admin-addr 127.0.0.1:55667 \
  -trace-file ./debug.trace \
  -log-level info
```

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-data` | `./data` | 数据目录，内含 `router.db`；相对路径相对进程当前工作目录 |
| `-addr` | `127.0.0.1:55666` | 客户端 HTTP 接入口；每次启动生效，不入库 |
| `-admin-addr` | `127.0.0.1:55667` | 管理台入口；每次启动生效，不入库 |
| `-trace-file` | 空 | 开启本地明文 trace；包含敏感信息，默认关闭 |
| `-log-level` | `info` | `debug` / `info` / `warn` / `error` |

两个监听参数必须是合法的 `host:port`，端口在 `1–65535`，且不能完全相同。监听地址不再由管理台设置；旧数据库中的 `listen_addr`、`admin_addr` 键不会被读取。

首次启动：

1. 创建并收紧数据目录到 `0700`；
2. 创建 SQLite schema，开启 WAL，写连接串行化；
3. 写入默认设置和随机 `hash_salt`；
4. 生成或加载 ECDSA P-256 根 CA，存入 `secrets`；
5. 生成随机管理员口令，库中只存 bcrypt 哈希，并在控制台打印一次；
6. 恢复 Marker 动态盐、账号映射、账号出站绑定；
7. 启动审计、更新记录、同步器和两个 HTTP 监听。

接入端口和管理台端口可分别配置 TLS 证书/私钥路径。路径对成对填写后，该端口强制 HTTPS-only；证书文件同一路径下按 mtime/大小每 60 秒检测，续期后新握手使用新证书，坏文件只告警并保留旧证书。路径本身变化需要重启。

### 4.2 SQLite 运行方式

使用纯 Go 驱动 `modernc.org/sqlite`，不依赖 CGO：

- 主写连接 `MaxOpenConns=1`，串行化事务；
- 只读连接池最多 4 个连接，并设置 `query_only`；
- `journal_mode=WAL`；
- `busy_timeout=5s`；
- 数据目录 `0700`，`router.db`、WAL 和 SHM 文件 `0600`。

数据库是唯一的持久化状态载体，包含配置、出口凭据、管理会话密钥、CA 私钥、账号映射和审计信息；运行期快照由进程内结构维护。

### 4.3 实际表结构

当前实现有九张主要表。`store.ensureSchema` 以幂等方式建表并为旧库补列。

#### settings

```sql
CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,       -- JSON 文本
  updated_at INTEGER NOT NULL
);
```

#### upstreams

```sql
CREATE TABLE upstreams (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT NOT NULL UNIQUE,
  platform   TEXT NOT NULL,       -- dataimpulse/decodo/1024proxy/resin/generic/plain
  base_url   TEXT NOT NULL,       -- 含真实出口凭据；管理 API 回显时打码
  inject     TEXT,                -- 仅 generic 使用的 JSON
  enabled    INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
```

`plain` 也是 `upstreams` 的一行，不另建出口表。它的 `inject` 必须为空。

#### access_logs

```sql
CREATE TABLE access_logs (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  ts             INTEGER NOT NULL,       -- unix ms
  req_id         TEXT NOT NULL DEFAULT '',
  method         TEXT NOT NULL,
  host           TEXT NOT NULL,
  path           TEXT NOT NULL,
  status         INTEGER NOT NULL,
  dur_ms         INTEGER NOT NULL,
  ttfb_ms        INTEGER,
  bytes_out      INTEGER NOT NULL DEFAULT 0,
  has_marker     INTEGER NOT NULL,
  account        TEXT NOT NULL DEFAULT '',
  account_fp     TEXT NOT NULL,
  upstream       TEXT NOT NULL,
  err            TEXT,
  internal_error TEXT
);
```

- `account` 是命中 `acct_map` 后的真实逻辑账号，不是 AT/RT；未命中为空；
- `account_fp` 是本次请求计算出的派生身份；粘滞上游或 sticky 绑定会使用它；
- `err` 是旧版本遗留列，新请求写 `internal_error`；
- `ttfb_ms` 可为空，表示尚未提交任何响应头或历史记录没有该指标。

索引为 `idx_logs_ts(ts)`、`idx_logs_account(account_fp, ts)`。

#### marker_salts

```sql
CREATE TABLE marker_salts (
  marker_fp  TEXT PRIMARY KEY,   -- 完整 sha256 指纹，不是 Marker 明文
  salt       INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
```

它保存动态盐值，不保存连续失败计数；失败计数只在进程内 LRU 中存在。

#### acct_map

```sql
CREATE TABLE acct_map (
  platform    TEXT NOT NULL,
  source      TEXT NOT NULL,       -- src:<id> / api
  source_type TEXT NOT NULL,       -- CLIProxyAPI / Sub2API / 自定义
  account     TEXT NOT NULL,       -- 账号标识，统一小写
  at_fp       TEXT NOT NULL DEFAULT '',
  rt_fp       TEXT NOT NULL DEFAULT '',
  at_hint     TEXT NOT NULL DEFAULT '',
  rt_hint     TEXT NOT NULL DEFAULT '',
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY(platform, source, account, rt_fp, source_type)
);
CREATE INDEX idx_acct_map_source ON acct_map(source);
```

AT/RT 明文不落库。`at_fp`、`rt_fp` 是带平台命名空间的凭据指纹，`*_hint` 只显示最后几个字符。

#### sync_sources

```sql
CREATE TABLE sync_sources (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  kind              TEXT NOT NULL,       -- cpa / sub2api
  name              TEXT NOT NULL UNIQUE,
  mode              TEXT NOT NULL DEFAULT 'api', -- 旧字段，当前不再使用
  base_url          TEXT NOT NULL,
  direct_auth_dir   TEXT NOT NULL DEFAULT '',
  direct_db_secret  TEXT NOT NULL DEFAULT '',
  interval_s        INTEGER NOT NULL DEFAULT 600,
  enabled           INTEGER NOT NULL DEFAULT 1,
  last_sync_at      INTEGER,
  last_status       TEXT NOT NULL DEFAULT '',
  empty_streak      INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL
);
```

`base_url` 和 API Key 供定时 API 全量同步；`direct_auth_dir` 或 `direct_db_secret` 供可选增量直读。API Key 存为 `secrets[source_key_<id>]`，PostgreSQL DSN 存为 `secrets[source_direct_db_<id>]`，表中只保存 secret key。

#### acct_egress

```sql
CREATE TABLE acct_egress (
  platform   TEXT NOT NULL,
  account    TEXT NOT NULL,
  egress_id  INTEGER NOT NULL,       -- upstreams.id，必须是 plain
  mode       TEXT NOT NULL DEFAULT 'sticky', -- sticky / random
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(platform, account, egress_id)
);
CREATE INDEX idx_acct_egress_egress ON acct_egress(egress_id);
```

绑定不带 `source` 维度：同一个 `(platform, account)` 从多个来源出现时共享同一份绑定。

#### sync_events

```sql
CREATE TABLE sync_events (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  ts      INTEGER NOT NULL,
  kind    TEXT NOT NULL,           -- direct_file/direct_incremental/api_sync/push/delete
  source  TEXT NOT NULL DEFAULT '',
  status  TEXT NOT NULL DEFAULT 'ok', -- ok / error
  summary TEXT NOT NULL DEFAULT '',
  detail  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_sync_events_ts ON sync_events(ts);
```

它记录账号映射变化，不与 `access_logs` 混用。以上九表为 `settings`、`upstreams`、`access_logs`、`marker_salts`、`acct_map`、`sync_sources`、`acct_egress`、`sync_events` 和 `secrets`；`secrets` 是第九张表。

#### secrets

```sql
CREATE TABLE secrets (
  key   TEXT PRIMARY KEY,
  value BLOB NOT NULL
);
```

主要键包括：

- `admin_password_bcrypt`、`session_hmac_key`；
- `ca_cert_pem`、`ca_key_pem`；
- `source_key_<id>`：同步源 API Key；
- `source_direct_db_<id>`：Sub2API PostgreSQL DSN。

### 4.4 默认设置与热更新

首次启动的有效默认值：

| 设置键 | 默认值 | 当前语义 |
|---|---:|---|
| `listen_auth` | `""` | 空=关闭接入口 Basic 认证 |
| `default_upstream` | `""` | 空=不配置上游，允许直连；不是“自动选首个” |
| `no_marker_policy` | `default_session` | 无 Marker 使用固定身份 `default` |
| `marker_path_parts` | `[]` | 空=所有路径都尝试 Marker 规则 |
| `marker_headers` | `Authorization`, `x-api-key`, `api-key`, `x-goog-api-key` | 按顺序提取 |
| `hash_salt` | 随机 32 字节 hex | 改变后全体身份重新计算 |
| `sid_len` | `16` | 后端允许 `4–64` |
| `session_ttl_min` | `0` | `0=不干预平台 TTL` |
| `salt_rotate_failure_threshold` | `2` | 连续可轮换失败次数，允许 `1–100` |
| `block_private_targets` | `true` | 拒绝内网、本机和云元数据目标 |
| `acctmap_enabled` | `true` | 关闭时跳过账号映射，回退 Marker 公式 |
| `acl_whitelist` / `acl_blacklist` | `[]` | 默认全部目标允许 MITM 解析 |
| `log_retention_days` | `30` | 审计和更新记录保留天数 |
| `sync_empty_clear_threshold` | `3` | 成功但空结果连续 N 次后才清源映射 |
| `metrics_enabled` | `false` | `/metrics` 默认 404 |
| 四个 TLS 路径 | `""` | 成对填写才启用对应监听 TLS |

转发路径使用 `settings.Holder` 中的不可变 `Snapshot`，通过 `atomic.Value` 无锁读取。`log_retention_days`、`metrics_enabled`、`sync_empty_clear_threshold` 是运维设置，由 store/API 按需读取，不进入转发快照。

保存设置时会严格校验：TLS 路径成对且可加载、策略和数字范围合法、Marker header 非空、ACL 条目合法；`default_upstream` 非空时必须对应一个已启用的上游，空值则表示明确直连。路由快照保存后立即替换；TLS 路径变更的响应带 `restart_required=true`。`acctmap_enabled` 存在于设置表，但当前设置 API/页面不暴露它，PUT 设置时沿用旧值，避免旧客户端提交时误关闭账号映射。

---

## 5. 模块划分与关键接口

### 5.1 目录结构

```text
MITMRouter/
├── cmd/mitmrouter/main.go          # 启动引导、双监听、优雅退出
├── internal/
│   ├── acl/                        # 目标 ACL 编译与匹配
│   ├── acctegress/                 # 账户↔普通代理绑定不可变快照
│   ├── acctmap/                    # 账号映射注册表、指纹和 Host→平台
│   ├── api/                        # 管理 REST、会话和 CRUD
│   ├── certca/                     # 根 CA、SNI 叶子证书、缓存
│   ├── httpnames/                  # 共享凭据请求头常量
│   ├── identity/                   # header/query/body 身份解析
│   ├── marker/                     # Marker 规则与动态盐 LRU
│   ├── metrics/                    # Prometheus 文本指标
│   ├── reqid/                      # 服务端内部请求 ID
│   ├── server/                     # 接入面、MITM、盲隧道和转发
│   ├── settings/                   # 设置快照、校验和热更新
│   ├── sticky/                     # 纯函数哈希推导
│   ├── store/                      # SQLite schema、事务和异步写入
│   ├── syncer/                     # 全量同步和 direct 增量 reader
│   ├── tlsreload/                  # 外部监听证书 mtime 热重载
│   ├── trace/                      # 显式开启的明文排障追踪
│   ├── upstream/                   # 上游条目和会话注入器
│   └── webui/                      # //go:embed 的 SPA 文件服务
├── web/                            # Vue3 + Vite + Element Plus 前端
│   └── src/views/{Login,Settings,Upstreams,AcctMap,Audit,Updates}.vue
├── DEPLOY.md
├── PARAMETERS.md
├── DESIGN.md
└── docs/
    ├── 001-function-test-plan.md
    ├── 002-transparent-forwarding-design.md
    ├── 003-identity-resolution-design.md
    ├── 004-stable-account-hash-design.md
    ├── 005-upstream-endpoints-and-auth-analysis.md
    ├── 006-sticky-session-credentials.md
    ├── 007-security-public-deployment-assessment.md
    ├── 008-benchmark-system.md
    ├── 009-performance-test-report.md
    ├── 010-bug-revalidation-report.md
    ├── 011-plain-binding-design.md
    ├── 012-credential-refresh-monitoring-design.md
    └── 013-update-log-design.md
```

### 5.2 上游模块

```go
type Upstream struct {
    ID       int64
    Name     string
    Platform string
    BaseURL  *url.URL       // 原始出口 URL，含凭据
    Enabled  bool

    // generic 专用
    UsernameTemplate string
    StaticPassword   string
}

type InjectParams struct {
    Account string // 派生的会话身份或 default 等兜底身份
    TTLMin  int    // session_ttl_min；0=不干预
    Country string // 预留，当前为空
}

type SessionInjector interface {
    Inject(base *url.URL, p InjectParams) (*url.URL, error)
}
```

注入器通过 `upstream.Register` 注册，`InjectorFor(platform, upstream)` 按条目取得。注入器无共享可变状态，返回的是 URL 副本。

### 5.3 身份解析模块

```go
type Resolution struct {
    Credential string // 仅存在于本次请求内存，禁止记录
    Platform   string
    Account    string // acct_map 命中的真实账号；未命中为空
    Mapped     bool
    RuleID     string // 非敏感规则名
}

type Resolver struct{}
func (r *Resolver) ResolveWithBody(
    req *http.Request,
    targetHost string,
    opts Options,
) (Resolution, io.ReadCloser)
```

`ResolveWithBody` 的接口还返回准确的转发 body 流：特殊 body 解析可能提前读过原流，但返回值会把已读字节回放给出站副本。它不会把解析用的 body 替换到原始请求上。

### 5.4 账号映射和绑定快照

```go
type Registry struct { /* rows + AT/RT fingerprint indexes */ }
func (r *Registry) Lookup(fp string) (Entry, bool)
func (r *Registry) Reload(entries []Entry)

type Table struct { /* acctegress: immutable map[(platform,account)] */ }
func (t *Table) Lookup(platform, account string) (Binding, bool)
```

`acctmap.Registry` 以读锁保护内存索引，按 AT/RT fingerprint O(1) 查找；写入不做单行修改，而是全量重载。`acctegress.Table` 是不可变快照，写库后整体构建并由 `Server.SwapAcctEgress` 原子替换。

### 5.5 设置与路由服务

```go
type Holder struct{ /* atomic.Value */ }
func (h *Holder) Current() Snapshot
func (h *Holder) Set(snap Snapshot)

func (s *Server) SwapUpstreams(*upstream.Table)
func (s *Server) SwapAcctEgress(*acctegress.Table)
func (s *Server) AttachAcctMap(*acctmap.Registry)
```

`Server` 持有共享 `http.Transport`、CA、设置快照、上游表、账号映射、绑定表、Marker 盐 LRU、异步审计通道和可选 trace writer。

---

## 6. 身份识别与粘滞算法

### 6.1 Host 到平台

账号映射不是对所有域名盲查。`acctmap.PlatformForHost` 对目标 Host 做小写和端口归一化，再按域名后缀映射：

| 平台 | Host 后缀 |
|---|---|
| `openai` | `chatgpt.com`、`openai.com` |
| `anthropic` | `anthropic.com`、`claude.ai`、`claude.com` |
| `gemini` | `googleapis.com`、`ai.google.dev` |
| `grok` | `x.ai`、`grok.com` |
| `kimi` | `api.kimi.com`、`moonshot.cn` |
| `deepseek` | `deepseek.com` |
| `glm` | `bigmodel.cn`、`z.ai` |
| `qwen` | `dashscope.aliyuncs.com` |
| `iflow` | `apis.iflow.cn` |
| `ollama` | `ollama.com` |

未知 Host 返回空平台，不会命中账号映射；手动登记的自定义平台可以存储，但当前自动路由只有上述 Host 映射能触发。

### 6.2 凭据归一化与提取顺序

`NormalizeCred` 的规则：

1. 去首尾空白；
2. 去除成对单引号或双引号；
3. 如果开头是大小写不敏感的 `Bearer ` 或 `Token `，去掉 scheme 前缀；
4. 保留凭据剩余部分的大小写。

已知 AI Host 的通用提取顺序为：

```text
Authorization
→ x-api-key
→ api-key
→ x-goog-api-key
→ URL query 的 key
```

命中已知 AI Host 时，这条通道不受 `marker_path_parts` 限制，保证 AI 平台的凭据在任意 API 路径上都可用于账号归属。没有通用凭据时才尝试 `marker.Extract`：它按设置中的 `marker_headers` 顺序提取，`Authorization` 只接受 `Bearer`；非空 `marker_path_parts` 时，URL path 必须包含任一片段。路径片段是普通子串匹配，不解析 query。

解析顺序固定为：

```text
① 已知平台 header/query
② 通用 MarkerRules header
③ 命中精确 body rule 时读 body
④ 没有身份，使用 no_marker_policy
```

### 6.3 特殊 body 解析

当前只有一条 body 规则：

```text
Host:        auth.x.ai
Method:      POST
Path:        包含 /oauth2/token
Content-Type: application/x-www-form-urlencoded
字段:         refresh_token
```

body 解析的原则：

- header/query 已找到凭据时绝不读 body；
- 只读取最多 `64 KiB`；
- 解析用副本，不改变发给目标站的 body 字节；
- body 超限、格式错误、字段缺失时继续转发并使用回退策略；
- 同一 Host 下若有多条规则，选择命中的最长 `PathKey`；Host 必须精确匹配；
- 当前只读取 Grok OAuth refresh body，不做通用 JSON/body 扫描。

### 6.4 acct_map 命中与回退公式

凭据查表键为：

```text
credential' = NormalizeCred(credential)
fp          = lowercase_hex(SHA256(platform + ":" + credential'))
```

若 `acctmap_enabled=true` 且 `fp` 命中注册表，得到：

```text
identity.key = platform + "/" + account
```

同账号的 AT 和 RT、同账号来自不同 source 的凭据，都会得到同一个 `identity.key`。`account` 已统一小写；来源实例不参与路由身份。

路由器给上游的会话身份（也会写入审计的 `account_fp`）按下面公式计算：

```text
k          = markerSalts.Get(identity.key 或 credential 或兜底键)
base_salt  = sticky.CombineSalt(hash_salt, k)

命中 acct_map：
account_fp = Derive(base_salt + "#a", platform + "/" + account, sid_len)

未命中映射：
account_fp = Derive(base_salt, normalized credential, sid_len)
```

`Derive` 是：

```text
lowercase_hex(SHA256(salt + marker))[:sid_len]
```

这是字符串拼接的哈希，不是 HMAC。`sid_len` 后端范围为 `4–64`，默认 16。映射命中额外使用 `#a` 命名空间，确保映射身份不会偶然等于旧版 Marker 哈希。

### 6.5 无 Marker 兜底

| `no_marker_policy` | `account_fp` | 出站行为 |
|---|---|---|
| `default_session` | 未轮换时固定为 `default`；轮换后 `Derive(base_salt+"#default", "default")` | 经默认上游或直连 |
| `client_ip_session` | `Derive(base_salt+"#ip", clientIP)` | 经默认上游或直连 |
| `direct` | `-` | 强制直连，不使用上游 |

`default_session` 和 `client_ip_session` 仍有各自的动态盐键；`direct` 不创建动态盐键。

### 6.6 上游不可用时的盐轮换

这是一个“换身份逃生”机制，不是业务重试：

- 只对经上游出口的请求生效；直连不轮换；
- 有 Marker 时按 Marker 或映射账号轮换；无 Marker 时按固定兜底身份或来源 IP 轮换；
- TLS/证书失败、TLS alert、非法 TLS record、握手期 EOF、上游 proxy CONNECT 错误等被视为可轮换错误；目标真实 HTTP 5xx 不触发；普通拨号拒绝也不触发；
- 连续可轮换错误达到 `salt_rotate_failure_threshold`（默认 2）才将该身份的整数盐加一；非可轮换错误或收到任何 HTTP 响应都会清零连续失败计数；
- 下一次推导使用 `hash_salt + "#k" + N`，从而得到新的 `account_fp`；粘滞平台通常会因此分配新出口；
- 进程内使用容量 10000 的线程安全 LRU，键是完整 `sha256(identity key)`，不保存明文；
- 轮换事件通过容量 256 的异步队列写入 `marker_salts`；启动按最近更新时间恢复最多 10000 条；队列满或写库失败只影响持久化，不影响当前内存路由；
- 指标为 `marker_salt_rotations_total` 和 `marker_salt_persist_dropped_total`。

因此，设计保证的是“身份推导稳定”，不是永久锁死具体 IP。平台会话到期、节点耗尽或供应商回收租约后，具体出口 IP 仍可能变化。

---

## 7. 上游适配与路由选择

### 7.1 六种上游平台

| `platform` | 用途 | 注入行为 |
|---|---|---|
| `dataimpulse` | DataImpulse 粘滞出口 | 用户名参数区删除旧 `sessid.*`，追加 `sessid.<account>` |
| `decodo` | Decodo/Smartproxy 粘滞出口 | 要求 `user-` 前缀，替换/插入 `session-<account>`；可替换 `sessionduration` |
| `1024proxy` | 1024proxy 粘滞出口 | 扁平参数中替换 `sid-<account>`；可替换 `t-<分钟>` |
| `resin` | Resin 粘滞出口 | 保留第一个 `.` 前的 Platform，生成 `Platform.<account>` |
| `generic` | 用户自定义模板 | 替换 `{user}`、`{sid}`、`{ttl_min}`、`{country}` |
| `plain` | 普通代理 | 恒等返回 `base_url`，凭据原样使用，不注入会话参数 |

所有注入器只修改**发给上游代理的出口凭据 URL 副本**的 userinfo；目标站的业务 URL、请求头、请求体和响应内容不因此改变。

### 7.2 专用注入器规则

#### DataImpulse

```text
原始：login__cr.us
结果：login__cr.us;sessid.<account>
```

- 参数区由 `;` 分隔、键值由 `.` 分隔；
- 既有 `sessid.*` 删除后重新追加；
- 正确键名是 `sessid`，不是社区流传的 `sid`；
- 当前 `session_ttl_min` 不作用于这条 `sessid` 机制。

#### 1024proxy

```text
<apikey>-region-US-sid-old-t-5
→ <apikey>-region-US-sid-<account>-t-5
```

- 已知键为 `region`、`st`、`city`、`asn`、`sid`、`t`；
- 只把已知键后的下一个 token 当成值，因此 apikey 中含 `-` 仍能保留；
- `session_ttl_min>0` 时把 `t` 替换为 `1–120` 范围内的值；
- base URL 建议自带 `-t-N`。

#### Decodo

```text
user-alice-country-us-session-old-sessionduration-90
→ user-alice-country-us-session-<account>-sessionduration-90
```

- 用户名必须以 `user-` 开头；
- 已知键包括 `country`、`city`、`st`、`state`、`asn`、`session`、`sessionduration`、`session_iplock`；
- `session_ttl_min>0` 时 `sessionduration` 按 `1–1440` clamp；
- Decodo 的 session 默认有空闲过期语义，具体 IP 仍是尽力而为。

#### Resin

```text
Default:<token>
→ Default.<account>:<token>
```

- 用户名必须非空；
- 第一个 `.` 之前是 Resin Platform，旧 Account 整体丢弃；
- Resin 接收的 Account 是原样字符串，Resin 自己负责租约、健康切换和过期。

#### Generic

`inject` 保存为 JSON，例如：

```json
{
  "username_template": "{user}-sessid-{sid}",
  "password": "static-password"
}
```

占位符只有 `{user}`、`{sid}`、`{ttl_min}`、`{country}`。模板保存和运行时都会严格检查未闭合花括号、未知占位符；`{ttl_min}` 只有在 `session_ttl_min>0` 时才允许。`password` 非空时覆盖 base URL 密码，缺省沿用 base URL 密码。

### 7.3 协议和默认上游

| base URL scheme | 普通 HTTP 转发 | 盲隧道 |
|---|---|---|
| `http://` | `http.Transport` 代理 | TCP + HTTP CONNECT |
| `https://` | `http.Transport` 代理 | TCP + TLS 后 HTTP CONNECT |
| `socks5://` | SOCKS5 出站 | 自实现 SOCKS5 CONNECT |
| `socks5h://` | 按 SOCKS5 处理，目标域名交给上游解析 | 按 SOCKS5 处理 |

默认上游决策：

1. 没有凭据且 `no_marker_policy=direct` 时直连；
2. 映射账号命中绑定时，优先使用绑定的 `plain` 出站；
3. 否则读取 `default_upstream`；
4. `default_upstream=""` 表示明确没有默认上游，直连；
5. `default_upstream` 非空但条目不存在、停用、注入器缺失或注入失败时，受控失败，不能悄悄回退成直连；
6. `plain` 可以作为默认上游，此时所有未绑定请求经该普通代理出去，不注入会话参数。

---

## 8. 接入、ACL 与透明转发

### 8.1 接入口与管理台分流

接入口 `Server.Handler` 的行为：

```text
CONNECT       → 入站认证 → 劫持 → TLS MITM 或盲隧道
绝对式 URL    → 入站认证 → forward
origin-form   → 静态 Ingress Port 提示页
```

管理台 `AdminHandler` 的行为：

```text
/ui*、/api/*、/metrics  → 管理处理器
CONNECT、绝对式 URL      → 404 admin_no_ingress
其他 origin-form         → 管理台占位/入口页
```

### 8.2 入站 Basic 认证

设置 `listen_auth` 为 `user:pass` 才启用。CONNECT 和绝对式请求都检查：

```text
Proxy-Authorization: Basic base64(user:pass)
```

缺失、格式错误或凭据错误时：

```text
407 Proxy Authentication Required
Proxy-Authenticate: Basic realm="sticky-mitm"
```

认证比较使用 `subtle.ConstantTimeCompare`。CONNECT 只认证一次；隧道内已经解密的业务请求不重复认证。认证失败不消耗上游资源。

管理台的会话口令与 `listen_auth` 完全是两套凭据。当前 `/api/settings` 已认证，因此会原样返回 `listen_auth`，页面允许直接编辑或复制；带凭据接入地址也会返回给已登录管理员，请勿外传。

### 8.3 CONNECT、TLS MITM 与盲隧道

CONNECT 流程：

```text
CONNECT host:port
  → listen_auth 校验
  → ACL 目标检查
       ├─ 拒绝 → 本地 403，不劫持、不连接目标
       └─ 放行 → 继续
  → block_private_targets 目标预检
  → 劫持 TCP 连接
  → 先返回 200 Connection Established
  → 首字节 peek（握手前空闲 30s）
       ├─ 首字节 0x16 且 ACLIntercept(host)=true
       │    → tls.Server
       │    → 按 SNI 取/签发叶子证书
       │    → ALPN h2 或 http/1.1
       │    → 每个内层请求生成新的 req_id
       │    → forward
       └─ 其他情况
            → blindTunnel
```

根 CA 和叶子证书：

- 根 CA：ECDSA P-256，有效期 10 年，保存于 `secrets`；
- 叶子证书：ECDSA P-256，SAN 为目标域名或 IP，有效期 7 天；
- 叶子缓存 LRU 容量 4096，剩余不足 24 小时会重新签发；
- 同一 SNI 的并发签发由 singleflight 合并；
- 客户端必须信任管理台下载的 `ca.pem` 或 `ca.crt`；
- 管理台监听 TLS 使用外部证书，与 MITM 根 CA 不是同一用途。

盲隧道建立后只搬运字节，不读取 Marker；因此使用无 Marker 兜底策略。HTTP 上游和 SOCKS5 上游的握手阶段超时 15 秒，MITM TLS 握手超时 10 秒；转发阶段不设置响应头上限，以免切断长时间推理的 LLM 响应。

### 8.4 ACL：先决定是否允许访问，再决定是否 MITM

ACL 条目支持：

- 单 IP；
- CIDR；
- 精确域名；
- `*.example.com` 通配符（匹配子域，不匹配主域本身）。

每个名单最多 500 条，大小写不敏感，自动清理空白、端口、IPv6 括号和结尾根点。判定顺序：

```text
命中黑名单                    → 本地 403，拒绝访问
白名单非空且未命中            → 本地 403，拒绝访问
其他                          → 放行；HTTPS 再进入 MITM 解析
```

ACL 是目标访问控制，不会改写被放行的业务请求或响应。拒绝目标只收到本地
403，不会发生 DNS、身份解析、上游选择或目标连接。具体位置：

- CONNECT 在劫持连接前判断一次，拒绝时不会返回 `200 Connection Established`；
- `forward` 在身份解析前再判断一次，覆盖绝对式明文请求和内层 Host 变化；
- 放行的 CONNECT 才在首字节判定后选择 TLS 终结或盲隧道。

拒绝事件写入 `acl_blocked` 审计记录，`status=0` 表示没有收到目标响应。

ACL 只匹配目标字面主机名/IP，不做 DNS 解析。运行快照会预编译 ACL；管理 API 保存时遇到非法条目整体拒绝，运行期加载旧库时则跳过非法项并告警。若白名单原始配置过但有效条目为零，仍按拒绝所有目标处理，避免容错逻辑意外放开访问。

### 8.5 私网目标保护

`block_private_targets` 默认开启，独立于 ACL。它拒绝：

- `localhost`、loopback；
- RFC1918 私网、link-local、未指定地址、多播；
- CGNAT `100.64.0.0/10`；
- DNS 解析结果中包含上述任一地址的域名；
- 被目标地址校验规则判为非公网的其他特殊目标（包括常见云元数据地址）。

域名会在路由前解析一次，所有结果都必须是公网地址；直连时用已检查的 IP 拨号，减少 DNS rebinding 绕过。私网目标返回本地 403，审计 `status=0`、`internal_error=private_target_blocked`。关闭保护后，字面私网目标会按兼容语义直接连接，不经过配置的上游；这是高风险设置。

### 8.6 普通请求转发与“不改请求”

转发只为 `http.Transport` 创建内部副本：

```go
func cloneForwardRequest(r *http.Request, scheme, host string, ctx context.Context) *http.Request {
    out := r.Clone(ctx)
    out.URL.Scheme = scheme // 仅供出站 transport 使用
    out.URL.Host = host     // 仅供出站 transport 使用
    out.RequestURI = ""
    return out
}
```

原始业务请求保持不变。内部副本保留：

- method；
- path、RawPath、RawQuery、ForceQuery；
- 客户端提供的请求头；
- body 和 trailers。

body 解析器读过 body 时，传给出站副本的是“已读前缀 + 剩余原流”的回放 reader，因此目标站收到的请求体字节不丢失、不重复。没有 User-Agent 时，副本显式阻止 Go 自动补默认 User-Agent。

响应处理：

1. 通过 `RoundTrip` 获取响应；
2. 目标真实状态码和多值响应头逐项回传；
3. response body 用 32 KiB 缓冲逐块写出，每块 Flush；
4. SSE 不整包缓存；
5. 1xx（包括 `103 Early Hints`）先于最终响应回传；
6. Trailer 先声明，body 结束后再填入；
7. `101 Switching Protocols` 劫持连接并双向复制升级后的字节；
8. trace 和响应诊断 wrapper 只观察字节，不改变字节。

request ID 只存在 context、slog、`access_logs.req_id` 和 trace 的内部关联中。不会由 MITMRouter 新增到目标请求头，也不会由 MITMRouter 新增到客户端响应头；客户端/目标站自己提供的同名头不被专门删除。

---

## 9. 账号映射与同步

### 9.1 `acct_map` 的行语义

一行代表：

```text
某平台 + 某来源实例 + 某来源类型 + 一个账号 + 一套当前凭据
```

主键是 `(platform, source, account, rt_fp, source_type)`：

- AT 改变、RT 不变：同键覆盖原行；
- RT 改变：新 RT 行替换同来源/同类型旧行；
- 全量快照中消失：删除该 source 名下的行；
- 同账号来自不同 source 或 source_type：各占一行；
- 同账号只要还有任意 source 的行，账号级出站绑定仍保留。

`source` 的约定：

- `src:<id>`：某个 `sync_sources` 实例；
- `api`：管理 API 手动登记/推送。

`source_type` 是展示和扩展用的全名：内置 `CLIProxyAPI`、`Sub2API`，手动推送可以使用任意不超过 64 字符的非空自定义值。

### 9.2 API 全量同步

同步源有两种 `kind`，两者都支持多个独立实例：

| kind | 展示名 | 调用 |
|---|---|---|
| `cpa` | CLIProxyAPI | `GET {base}/v0/management/auth-files`，再并发下载 `/v0/management/auth-files/download?name=...`；认证为 `Authorization: Bearer <management-key>` |
| `sub2api` | Sub2API | `GET {base}/api/v1/admin/accounts/data`；认证为 `x-api-key: <admin-api-key>` |

调度器行为：

- 一个 30 秒 tick 循环；
- 每个 source 使用自己的 `interval_s`，下限 60 秒；
- 进程启动立即尝试一轮；
- 管理台“立即同步”通过 wake 通道触发指定 source；
- API 拉取失败时保留旧映射，不执行空快照清除；
- 成功后更新 `last_sync_at`、`last_status`，并产生 `api_sync` 更新事件。

CPA 解析：

- 列表中的 provider 必须在白名单内；
- auth JSON 使用 `type`/`provider`、`email`/`email_address`、`access_token`、`refresh_token`；
- 兼容顶层 token 和 `tokens` 嵌套 token；
- `codex/openai` 等映射到 `openai`，`claude` 到 `anthropic`，`gemini/antigravity` 到 `gemini`，`xai/grok` 到 `grok`，并支持 kimi、qwen、iflow；
- disabled 文件仍会同步，因为停用不等于凭据不属于该账号；
- 无法识别、无账号或无 AT/RT 的文件跳过；下载/解析失败会使本轮全量失败并保留旧快照。

Sub2API 解析：

- 只接收 `type=oauth` 或 `type=setup-token`；
- 从 `credentials.email` 取账号，缺失时回退 `name`；
- 读取 `access_token`、`refresh_token`；
- `apikey`、`upstream`、Bedrock 等不含可用 AT/RT 的类型跳过；
- platform 只接收白名单映射。

### 9.3 空快照保护

全量请求成功但解析出 0 个账号时，不立即清空该 source 的旧映射：

```text
keep 非空                       → 立即替换快照，empty_streak=0
keep 为空且旧映射也为空         → 不清映射，计数归 0
keep 为空且旧映射非空            → empty_streak + 1
                                  未达阈值：保留旧映射
                                  达到阈值：清空该 source 并 GC 绑定
```

阈值由 `sync_empty_clear_threshold` 控制，默认 3，范围 1–100；1 等价于立即清空。拉取或解析失败不会进入这个逻辑，因此失败不计数、不清空。计数存放在 `sync_sources.empty_streak`，重启不丢。

达到阈值后会清空该 source 的映射，并保留 `empty_streak` 在阈值处；后续空快照无需重复执行清理，下一次非空快照才把计数归零。该保护只针对整份快照为空。非空但漏掉部分账号的快照仍按全量结果立即对齐。

### 9.4 Direct 增量：Sub2API PostgreSQL

在某个 `sub2api` source 的 API 字段 `direct_db_dsn` 填入 DSN 即启用。数据库列名是 `direct_db_secret`，其中只保存 secret key；DSN 明文写入 `secrets[source_direct_db_<id>]`。它与 API 全量同步叠加，不是二选一：

- reader 启动先执行一轮，之后固定每 3 秒执行一次；
- 先查最近 30 秒内更新的 OAuth/setup-token 账号：

```sql
SELECT id, updated_at
FROM accounts
WHERE updated_at >= clock_timestamp() - interval '30 seconds'
  AND type IN ('oauth', 'setup-token')
ORDER BY updated_at, id;
```

- 再按这些 id 直接读取 `accounts.credentials` 中的 email、AT、RT；
- 不保存 cursor、旧 token hash 或额外快照；30 秒窗口允许重复处理；
- 每个账号调用 `ApplyAccountDelta`，只影响当前 source + platform + account：先删该账号旧映射，有 AT 或 RT 就插入新指纹；两者都没有则不插入；
- 增量不负责发现硬删除、改名、平台变化，完整性由 API 全量负责；
- 增量成功不覆盖 `last_sync_at`/`last_status`，有实际应用时记 `direct_incremental` 事件；失败更新 `last_status` 为 error 并记失败事件。

数据库安全约束：

- 同机 localhost、loopback 或 Unix socket 可不启用 TLS；
- 远程 PostgreSQL 必须是 `sslmode=verify-full`，不能跳过证书校验；
- 使用只读数据库账号；
- DSN 只在本次建立连接时读取，不回显到管理 API、日志或更新记录。

### 9.5 Direct 增量：CPA 认证文件目录

在某个 `cpa` source 填绝对的 `direct_auth_dir` 即启用：

- 根目录本身不能是 symlink；
- 启动时不做全目录初始导入，已有映射由 API 全量同步建立；
- 递归添加 fsnotify watch；新建子目录也会补 watch；
- 只处理普通 `.json` 文件；跳过 symlink、非普通文件和超过 2 MiB 的文件；
- `Create`、`Write`、可见的 atomic rename 进入有界 pending 队列；同一路径去抖 200ms；
- 读取前后比较文件大小和 mtime，防止半写入；
- 解析 `type/provider`、email/email_address、顶层或 `tokens` 中的 AT/RT；
- 有效变化调用 `ApplyAccountDelta`；
- 空文件、坏 JSON、未知 provider/type、无账号或无凭据不提交更新，也不把旧映射当作删除；
- `Remove` 和删除/改名后的清理交给下一次 API 全量，direct reader 不保存 file→account 关系；
- watcher 出错会尝试重建，漏掉的变化由 API 全量收敛；
- 文件队列和 watcher 故障只保留旧映射，不影响全量同步。

### 9.6 source 生命周期与并发

只有下面条件同时满足时才建立 direct reader：

```text
source.enabled = true
且 cpa 有 direct_auth_dir / sub2api 有 direct_db_secret
```

没有全局增量开关，也没有 `api/direct` 模式互斥。

- 填路径：启动对应 source 的 reader；
- 清空路径：停止 reader，关闭 watcher/数据库连接，不改变已有映射；
- 禁用 source：停止 reader，保留已有映射；
- 修改路径或 kind：先停止旧 reader，提交配置，再按新配置启动；
- 删除 source：先停止 reader、等待在途操作结束，再在同一事务中删除 source、其映射和 secrets；
- 同一 source 的 API 全量和 direct 增量共用 source 锁，不能互相覆盖；
- 映射写入完成后始终 `ReloadFromStore`，并通过 `OnMapChange` 重建绑定快照。

### 9.7 手动登记与凭据编辑

管理台或外部管理调用方可以使用：

```text
PUT /api/acctmap/{platform}/{account}
```

请求体：

```json
{
  "access_token": "...",
  "refresh_token": "...",
  "source_type": "CLIProxyAPI"
}
```

语义：

- `source` 固定为 `api`；
- `source_type` 必填且不超过 64 字符；
- AT/RT 至少提供一项；
- 只提交 AT 时保留当前 RT，只提交 RT 时保留当前 AT；
- RT 变化会替换同账号、同 source_type 的旧推送行；
- 不同 source_type 和拉取 source 互不影响；
- 服务端只保存 fingerprint 和尾缀 hint；
- 写入后重载 Registry、绑定快照并产生 `push` 事件。

删除接口：

```text
DELETE /api/acctmap/{platform}/{account}?source=
DELETE /api/acctmap/{platform}/{account}/tokens/{fp}
```

账号删除可限定 source；token 删除按 AT 优先、再 RT 匹配，两个字段都空时删除整行。所有删除路径都会在同一事务中清理孤儿 `acct_egress`。

---

## 10. 账号到普通代理的绑定

### 10.1 设计语义

`plain` 是普通代理条目：

```text
platform = plain
base_url = http(s)://user:pass@host:port 或 socks5://...
inject   = 空
```

绑定对象是 `(platform, account)`，不是某个 Marker，也不是某个 source。只有 `acct_map` 命中的逻辑账号会命中绑定；未映射的裸 Marker、无 Marker 和盲隧道继续走原有默认路由。`no_marker_policy=direct` 只对没有凭据的请求生效；它一旦命中就直接返回直连决策，不再检查账号绑定。

优先级：

```text
私网目标拒绝
  → 无凭据且 no_marker_policy=direct：直连
  → 已映射账号有绑定：选择绑定的 plain 出站
  → default_upstream + 会话注入
  → 明确没有 default_upstream 时直连
```

绑定存在但所有绑定出站都不存在或停用时，返回受控 502，`internal_error=upstream_config`，绝不悄悄回落默认粘滞出口或直连。绑定候选最终通过 `plain` 的恒等注入器返回一个 URL 副本，不向其凭据中加入会话参数。明确绑定一个账号的出口，相当于明确了它的出口语义。

### 10.2 sticky 与 random

绑定表中同一账号的全部行共享同一个 `mode`：

- `sticky`：对每个候选 plain 出站计算 Rendezvous/HRW 分数，取最高者；同一盐、账号和出站集合下确定性不变；重启不漂；
- `random`：每次请求使用 `math/rand/v2` 在候选集合中均匀随机选择，不保存选择状态。

HRW 分数：

```text
score = BigEndianUint64(
          SHA256(CombineSalt(hash_salt, dynamic_salt) + "#a"
                 + "\x00" + platform/account
                 + "\x00" + strconv(egress_id))[:8]
        )
```

候选集合增删时，HRW 只会影响原本命中被删/新增出站的账号，不会像取模那样整体重洗。账号动态盐轮换后，sticky 账号会自动重新选择出站。

`plain` 出站的账号绑定忽略 `session_ttl_min`，也不向其 URL 添加任何 query 或用户名参数。

### 10.3 级联清理

两条规则覆盖全部删除通道：

1. 删除 `upstreams` 行时，在同一事务删除 `acct_egress.egress_id` 引用；
2. 任意 `acct_map` 写路径结束时执行：

```sql
DELETE FROM acct_egress
WHERE NOT EXISTS (
  SELECT 1 FROM acct_map a
  WHERE a.platform = acct_egress.platform
    AND a.account  = acct_egress.account
);
```

所以：

- 账号仍在任一来源中，绑定保留；
- 账号在所有来源都消失，绑定同事务清掉；
- 空快照保护会同时保护账号和绑定；
- 绑定表快照在 GC 后重建，不会继续使用已删除的绑定。

### 10.4 绑定写入

账户方向：

```text
PUT /api/acctegress/{platform}/{account}
{"mode":"sticky", "egress_ids":[3,7]}
```

先删除该账号全部绑定，再插入新全集；`egress_ids=[]` 等价于清除。账号必须存在于 `acct_map`，所有 ID 必须是 `plain` 条目，停用与否不影响保存。

出站方向：

```text
PUT /api/acctegress/egress/{id}
{
  "accounts":[{"platform":"anthropic","account":"a@example.com"}],
  "mode":"random"
}
```

整体替换该出站的账号集合：列表外的账号解除该出站，列表内的账号关联该出站。`mode` 只作用于本次新关联的账号；已有账号保留自己的 mode。列表中存在未知账号时整单返回 404，不做部分写入。

---

## 11. 管理 API 与 Web 管理台

### 11.1 管理会话

- Cookie 名：`sticky_session`；
- payload 只有 `{exp}`，使用 base64url 编码；
- 签名为 HMAC-SHA256，密钥存 `secrets.session_hmac_key`；
- 有效期 7 天；剩余少于一半时滑动续期；
- `HttpOnly`、`SameSite=Lax`；通过 TLS 访问管理台时设置 `Secure`；
- 修改管理员口令会生成新的 HMAC 密钥，所有旧会话立即失效；
- 登录失败按来源 IP 做指数退避，最多 15 分钟，内存表最多 10000 条；
- 新口令至少 6 字符，数据库只保存 bcrypt 哈希。

除 `POST /api/auth/login` 外，所有管理 API（包括 `POST /api/auth/logout`）都要求有效会话。错误统一为：

```json
{"error":{"code":"...","message":"..."}}
```

常见状态码：400 参数错误、401 未登录、404 资源不存在、409 冲突、429 登录退避、500 内部错误、503 功能未装配。

### 11.2 REST 路由

#### 认证、设置和证书

| 方法 | 路径 | 作用 |
|---|---|---|
| POST | `/api/auth/login` | `{password}` 登录并签发 Cookie |
| POST | `/api/auth/logout` | 删除浏览器 Cookie |
| GET | `/api/auth/me` | 会话自检 |
| POST | `/api/auth/password` | 修改管理员口令并吊销旧会话 |
| GET/PUT | `/api/settings` | 读取/保存基础设置 |
| POST | `/api/settings/reset-salt` | 生成新 `hash_salt`，全体身份重新计算 |
| GET | `/api/ca.pem` | 下载 PEM 根证书 |
| GET | `/api/ca.crt` | 下载 DER 根证书 |
| GET | `/metrics` | 开启后返回 Prometheus 文本 |

`GET /api/settings` 还返回只读的：

- `ingress_url`：按本次请求 Host、接入端口和接入 TLS 状态拼出；
- `ingress_url_auth`：启用 `listen_auth` 时，在地址中带上 URL 编码后的 `user:pass`。

监听 IP/端口仍由启动参数决定，不能通过 PUT 设置。

#### 上游出口

| 方法 | 路径 | 作用 |
|---|---|---|
| GET | `/api/upstreams` | 列表；`base_url` 密码替换为 `____`，generic 密码同样打码 |
| POST | `/api/upstreams` | 新增上游 |
| PUT | `/api/upstreams/{id}` | 编辑上游；密码为 `____` 或 `__unchanged__` 时沿用旧密码 |
| DELETE | `/api/upstreams/{id}` | 删除；默认条目拒绝删除，并级联清理绑定 |
| POST | `/api/upstreams/{id}/default` | 设为默认 |
| POST | `/api/upstreams/{id}/test` | 用 `account=healthcheck` 测出口 IP/地理位置 |

保存校验 scheme、host、平台专用用户名和 Generic 模板。`plain` 不能带 `inject`。

#### 同步源和账号映射

| 方法 | 路径 | 作用 |
|---|---|---|
| GET/POST | `/api/sources` | 查看/新增同步源 |
| PUT/DELETE | `/api/sources/{id}` | 编辑/删除同步源；删除级联清理映射和 secrets |
| POST | `/api/sources/{id}/test` | 默认测试 API 全量配置 |
| POST | `/api/sources/{id}/test?target=incremental` | 测 direct PostgreSQL/CPA watcher |
| POST | `/api/sources/{id}/sync` | 立即触发 API 全量同步 |
| GET | `/api/acctmap` | 映射预览，支持 platform/account/source/source_type/binding 筛选和分页 |
| GET | `/api/acctmap/stats` | 映射行数、去重账号数、平台/来源统计 |
| PUT | `/api/acctmap/{platform}/{account}` | 手动登记或编辑 AT/RT |
| DELETE | `/api/acctmap/{platform}/{account}` | 删除账号映射，可用 `source=` 限定来源 |
| DELETE | `/api/acctmap/{platform}/{account}/tokens/{fp}` | 删除命中的 AT 或 RT |

新增同步源时，API 全量配置 `base_url` 和 `api_key` 必填；增量配置是可选的。编辑时 API Key/DSN 留空表示沿用，`direct_db_clear=true` 清除 Sub2API DSN 并停止增量。正常 source 列表响应从不返回 API Key 或完整 DSN，仅返回 `direct_db_configured`；增量测试失败时返回 reader 的运维错误摘要，生产 reader 会对 DSN 解析、TLS 和 ping 错误做安全归一化，不返回 DSN 内容。

#### 账号出站绑定

| 方法 | 路径 | 作用 |
|---|---|---|
| GET | `/api/acctegress` | 返回按账号聚合的绑定和各出站关联计数 |
| PUT | `/api/acctegress/{platform}/{account}` | 整体替换某账号的绑定 |
| DELETE | `/api/acctegress/{platform}/{account}` | 清除某账号绑定 |
| DELETE | `/api/acctegress` | 清除全部绑定 |
| PUT | `/api/acctegress/egress/{id}` | 整体替换某 plain 出站关联的账号集合 |

分页响应通常是：

```json
{"items":[],"total":0,"page":1,"page_size":50}
```

日志和更新记录列表使用后端最大 200 条/页；账号映射页面的内存筛选最多使用 2000 条/页，出站关联弹窗按服务端分页读取，避免一次拉取整个账号库。

### 11.3 实际页面

登录后左侧菜单有五个业务页面：

1. **基础设置 `Settings.vue`**
   - 顶部显示只读接入地址，启用入站认证时优先显示可直接复制的带认证地址；
   - 接入端口：入站认证、接入 TLS 路径；
   - 管理台：说明 `-addr/-admin-addr` 由启动参数指定、管理台 TLS 路径；
   - Marker：路径片段和请求头；
   - 粘滞：盐、SID 长度、TTL、盐轮换阈值；
   - 默认路由：默认上游、无 Marker 策略、私网目标开关；
   - ACL：黑白名单；
   - 维护：审计保留天数、空快照阈值、metrics；
   - 下载 `ca.pem/ca.crt`、修改管理员口令。

2. **上游出口 `Upstreams.vue`**
   - 上游 CRUD、平台动态提示、密码打码、启停、设为默认、出口连通性测试；
   - 支持 `plain` 普通代理；
   - `plain` 行可打开“关联账户”抽屉，服务端分页搜索账号，跨页保留勾选，批量设置新关联账号的模式。

3. **账号管理 `AcctMap.vue`**
   - 同步源列表：API 全量、可选 direct 增量路径、全量间隔、最近状态；
   - 测试全量/增量、立即同步、新增/编辑/删除 source；
   - 当前映射：平台、账号、AT/RT 尾缀、来源、绑定模式和出站数量、更新时间；
   - 手动登记/编辑 AT/RT；
   - 按平台、来源类型、账号和绑定状态查询；
   - 从账号方向绑定多个 plain 出站，模式为粘滞或随机；
   - 一键清空全部出站绑定。

4. **访问审计 `Audit.vue`**
   - 时间、host/path 关键词、账号或会话、上游、2xx/4xx/5xx/错误筛选；
   - 展示 request ID、方法、目标、状态、总耗时、TTFB、出站字节、Marker、账号/会话、内部错误和上游；
   - 分页、5 秒自动刷新、手动清空。

5. **更新记录 `Updates.vue`**
   - 展示同步源、direct 文件、手动推送、删除等映射变化；
   - 按时间、kind、source、成功/失败筛选；
   - 展示人话摘要和可选 detail（例如文件绝对路径）；
   - 分页、5 秒自动刷新、手动清空。

---

## 12. 审计、更新记录、指标与安全

### 12.1 访问审计

`forward` 中会为每个 HTTP 请求（包括本地生成失败响应）构造一条 `access_logs` 事件并尝试投递。事件进入异步 channel 后仍可能因队列满而丢弃；普通 CONNECT 盲隧道不建立 access_logs，连接级事件通常只记结构化日志；ACL 或私网策略在隧道建立前拒绝 CONNECT 时会额外尝试写一条 `status=0` 的审计记录。

审计字段口径：

| 字段 | 含义 |
|---|---|
| `req_id` | 32 位随机小写 hex，只在服务端内部使用 |
| `host/path` | 目标主机和 path；不保存 query 参数 |
| `status` | 目标/上游真实 HTTP 状态；本服务在收到响应前失败时为 0 |
| `dur_ms` | 从开始处理到响应完成/失败的时长 |
| `ttfb_ms` | 从开始处理到首次提交客户端响应头的时长；无法提交时为空 |
| `bytes_out` | 实际写给客户端的响应字节数 |
| `has_marker` | 是否解析出凭据/Marker |
| `account` | 命中映射时的真实逻辑账号；未映射为空 |
| `account_fp` | 派生的粘滞会话 ID；无 Marker 可能为 `default`、哈希值或 `-` |
| `upstream` | 选中的上游名，直连为 `direct` |
| `internal_error` | MITMRouter 自身的安全错误分类 |

`status` 与 `internal_error` 的组合：

| 场景 | status | internal_error |
|---|---:|---|
| 目标真实返回 2xx/4xx/5xx | 真实状态 | 空 |
| 响应头前 DNS/拨号/超时/TLS/EOF | 0 | `dns`/`dial`/`timeout`/`tls`/`eof` |
| 默认或绑定上游配置错误 | 0 | `upstream_config` |
| 上游 HTTP CONNECT 明确拒绝 | 0 | `upstream_connect_rejected` 或 `upstream_connect` |
| 私网目标被本地策略拒绝 | 0 | `private_target_blocked` |
| 收到响应后响应体意外中断 | 真实状态 | `upstream_response_eof` 或 `upstream_response_read` |
| 客户端断开导致写响应失败 | 真实状态 | `downstream_write` |
| 请求上下文取消 | 0 或已提交状态 | `canceled` |
| 其他转发错误 | 0 | `transport` |

本地失败给客户端的响应是固定文案，不带上游 URL、用户名、密码、账号或原始错误：

```json
{"error":{"code":"bad_gateway","message":"upstream unavailable"}}
```

真实上游返回的 407、4xx、5xx 则原样透传，不被转成内部错误。

审计写入使用容量 4096 的 channel；writer 每 200ms 或累计 256 条批量写入。队列满时丢弃并告警，不阻塞转发。优雅退出会排空队列，最多等待 5 秒。

### 12.2 账号映射更新记录

`sync_events` 记录以下事件：

| kind | 触发 |
|---|---|
| `direct_file` | CPA 文件变化处理成功；读取/解析失败（包括未知 type/provider）记失败事件；已识别但无账号或无凭据的文件只告警 |
| `direct_incremental` | Sub2API 增量应用账号或查询失败 |
| `api_sync` | API 全量成功、空快照保护或失败 |
| `push` | 手动登记/编辑账号 |
| `delete` | 删除账号或 token |

`summary` 是一句人话，`detail` 是补充信息；不会写 AT/RT 明文或完整凭据指纹。更新事件有独立的容量 4096 channel、200ms/256 条批量 writer，与访问审计互不影响；事件满时调用方非阻塞丢弃。两张表共享 `log_retention_days`，每日启动时和之后每 24 小时清理一次。

### 12.3 Prometheus 指标

`metrics_enabled=false` 时 `/metrics` 返回 404；开启后仍要求管理会话。当前指标由零依赖注册表输出：

- `requests_total{upstream,has_marker}`（`upstream` 为配置的条目名，`has_marker` 为布尔值）；
- `upstream_errors_total{upstream}`（当前只有 `upstream` 标签）；
- `auth_failures_total`：管理台登录失败；
- `ingress_auth_failures_total`：接入口 Basic 认证失败；
- `active_connections`：活动隧道数；
- `marker_salt_rotations_total`；
- `marker_salt_persist_dropped_total`。

不把账号、Marker、token、完整 URL 或请求参数作为指标标签。

### 12.4 日志与明文 trace

stdout 使用 `log/slog` JSON handler：

- 正常请求主要在 debug 级别记录；
- info/warn/error 记录启动、配置、同步和故障；
- 结构化日志可带 `req_id`、目标 host、上游名、状态和安全错误分类；
- 不记录 Marker、AT/RT、上游 URL userinfo、API Key 或 PostgreSQL 密码。

只有显式传入 `-trace-file <path>` 才开启明文 trace：

- 追加记录请求/响应 URL、完整 header 和 body；
- 按流式块记录，不等待完整 body；
- 文件以 `0600` 创建/打开；
- 默认关闭，trace 文件包含秘密，必须短期使用并妥善删除；
- trace 失败只记住错误，不中断业务转发，也不写入 SQLite 审计。

### 12.5 密钥保护

- `router.db` 和 WAL/SHM 权限为 `0600`，数据目录为 `0700`；
- CA 私钥、上游凭据、管理员哈希、会话密钥都在该数据库中；
- 拥有数据目录的人可以解密本服务处理的 HTTPS 流量，应像保护私钥一样保护备份；
- 管理 API 对上游密码、Generic 密码、同步源 API Key/DSN 做掩码或只返回“已配置”；
- 当前入站认证密码因为设置页支持“编辑/复制”而由已认证的管理员原样看到，带认证接入地址不得外传；
- 私网目标保护默认开启，非回环监听若没有入站认证会告警，非回环管理监听若没有 TLS 也会告警。

---

## 13. 测试、限制与当前状态

### 13.1 已覆盖的测试类型

运行后端测试：

```bash
go test ./...
```

当前测试覆盖：

- Marker/账号指纹、凭据归一化、路径和 header 提取；
- Host→平台映射、AT/RT 双索引、映射命中与 Marker 回退；
- body parser 读取上限、body 回放、Grok refresh token；
- `Derive`、盐组合、连续失败和动态盐持久化/重启恢复；
- DataImpulse、Decodo、1024proxy、Resin、Generic、Plain 注入器 golden 用例；
- 入站认证、双监听面隔离、CONNECT/MITM/盲隧道、半关闭；
- ACL 放行/拒绝、私网 DNS 防绕过和本地拒绝；
- URL/header/body/response/trailer/1xx/101/SSE 透明转发；
- 真实 HTTP 4xx/5xx 与本地转发失败的审计区分、TTFB 和 request ID；
- acct_map 全量快照、推送 AT/RT 单独更新、来源隔离、删除和 GC；
- CPA direct watcher、Sub2API direct 增量查询、远程 PostgreSQL TLS 校验；
- 空快照阈值、source 生命周期、同步更新事件和异步 writer；
- plain 账号绑定的 sticky HRW、random、池变化稳定性、全停用受控失败和级联清理。

完整功能测试方案见 [docs/001-function-test-plan.md](docs/001-function-test-plan.md)。

### 13.2 已知限制

1. **平台粘滞是尽力而为。** Decodo 默认会话有空闲过期，DataImpulse `sessid` 平均约半小时，1024proxy 受 `t` 和套餐范围限制，Resin 也有租约到期和节点耗尽情况。同一 `account_fp` 不等于永久固定某个 IP。
2. **全量同步最短 60 秒。** Direct Sub2API 用最近 30 秒重叠窗口、固定每 3 秒轮询；如果数据库时钟偏差大于窗口，最终仍由 API 全量兜底。
3. **Direct 只保证新鲜度，不保证完整性。** Direct reader 不负责硬删除、改名或平台变化；这些由 API 全量收敛。CPA direct 不保存文件到账号关系，因此删除文件不会立即删除映射。
4. **CPA 只支持已验证的最小 JSON 格式。** 插件自定义格式、一个文件展开多个虚拟账号等情况会跳过，下一次 API 全量负责收敛。
5. **同步源必须注意权限。** CPA 目录需要服务进程可读，Sub2API DSN 需要只读 PostgreSQL 账号；远程数据库必须 `verify-full`。
6. **ACL 是目标访问控制。** 命中黑名单或白名单非空但未命中的目标会收到本地 403；`block_private_targets` 是独立的私网安全检查，放行 ACL 也不能绕过它。
7. **账号映射只对内置 Host 平台自动生效。** 自定义平台可以登记和保存，但当前代码没有管理台配置的任意 Host→平台规则。
8. **事件队列是尽力而为。** 审计和更新记录都使用有界异步队列，队列满会丢弃记录并告警；不会阻塞请求或同步热路径。
9. **没有业务自动重试。** 本地失败返回 502/403，由 SDK 自己决定是否重试。
10. **HTTP/3/UDP 不在路径内。** 需要在客户端或系统层禁用 UDP 443，才能保证流量经过本服务。
11. **证书固定客户端不能 MITM。** 客户端必须信任本服务 CA，且不能 pin 目标站证书。
12. **关闭私网保护有明显 SSRF 风险。** 非回环部署必须同时配置防火墙、TLS 和入站认证。

### 13.3 当前实现的完成状态

当前实现已经不是原先的“只有 Marker、四张表、四个页面”的 M1–M4 计划形态，而是包含：

- 双监听面和可选监听 TLS；
- 透明流式转发和内部 request ID；
- Marker 级和账号级粘滞；
- CPA/Sub2API API 全量同步；
- CPA 文件和 Sub2API PostgreSQL direct 增量；
- `plain` 出站及账号绑定；
- 空快照保护、更新记录、审计和指标；
- 已认证管理员可编辑并复制已保存的入站凭据。

后续新增功能必须继续遵守两个现有原则：

1. **深模块原则：** 让调用方只了解小接口，把解析、快照、并发和失败语义留在模块内部；
2. **透明转发原则：** 不为路由、审计、trace 或 request ID 修改外部业务请求和响应。

相关专题设计：

- [透明转发](docs/002-transparent-forwarding-design.md)
- [特殊 body 身份识别](docs/003-identity-resolution-design.md)
- [账号映射稳定化](docs/004-stable-account-hash-design.md)
- [普通代理与账号绑定](docs/011-plain-binding-design.md)
- [AT/RT 增量监控](docs/012-credential-refresh-monitoring-design.md)
- [账号映射更新记录](docs/013-update-log-design.md)
- [参数填写指南](PARAMETERS.md)
- [生产部署手册](DEPLOY.md)

---

## 附录：当前请求的最短决策伪代码

下面是流程示意，不是可直接编译的代码。真实实现位于 `internal/server/ingress.go`、`internal/identity/resolver.go` 和相关快照模块；`resolveOutboundDetailed` 会在函数内部计算并返回 `account_fp`，调用方不会把它作为参数再次传入：

```text
forward(request):
  snap = settings.Current()
  body = request.Body

  if not snap.ACLAllowed(normalize(host)):
      writeLocal403(response, "acl_forbidden")
      emitAudit(status=0, internal_error="acl_blocked")
      return

  if snap.ACLIntercept(normalize(host)):
      resolved, body = Resolver.ResolveWithBody(request, host, {
          MarkerRules, AcctMapEnabled, AcctMap,
      })
      ident = mappedIdentity(resolved)  // 只有命中 acct_map 才有 platform/account
  else:
      resolved = emptyResolution()     // 预留给不需要 MITM 的允许流量
      ident = emptyIdentity()

  proxyURL, accountFP, upstreamName, reason, err =
      resolveOutboundDetailed(request.Context(), snap,
                              resolved.Credential, ident, clientIP, host)

  # resolveOutboundDetailed 内部顺序：
  # 1. 校验私网目标；失败立即返回本地 403；
  # 2. 没有凭据且 no_marker_policy=direct → 直连，短路账号绑定；
  # 3. mapped identity 有绑定 → 从启用的 plain 出站中 sticky/random 选择，
  #    再经 plain 恒等注入器返回 URL 副本；
  # 4. 否则选择 default_upstream；空值直连，非空失效则受控失败；
  # 5. 其余平台通过 SessionInjector 注入 accountFP。

  if err != nil:
      writeForwardFailure(responseWriter, err)
      emitAudit(status=0, internal_error=classify(err))
      return

  out = cloneForwardRequest(request, scheme, host,
                            context.WithValue(request.Context(), proxyURL))
  out.Body = body  # 需要 body 解析时使用回放流；业务字节不变
  response, err = transport.RoundTrip(out)
  if err != nil:
      writeForwardFailure(responseWriter, err)
      emitAudit(status=0, internal_error=classify(err))
      return

  relayResponseStream(responseWriter, response)
  emitAudit(accountFP, upstreamName, reason,
            response.status, response.headers, response.body_bytes)
```

实际实现中，审计和响应 relay 还会处理 1xx、Trailer、SSE、101 升级、响应体读取错误和下游写错误；账号绑定在 `resolveOutboundDetailed` 内优先于默认上游，但 `no_marker_policy=direct` 对无凭据请求会先短路绑定。
