# 设计：账号粘滞标识稳定化（拉取 + 推送接口版）

> 状态：v7 · 2026-08-26（数据模型改为"来源实例行"形态）
> 关联代码：`internal/acctmap/`、`internal/store/acctmap.go`、`internal/syncer/`、
> `internal/api/acctmap.go`、`internal/server/ingress.go`、`web/src/views/AcctMap.vue`

---

## 0. 模型

账号只有两种类型：

| 类型 | 凭据形态 | 是否入映射表 | 粘滞来源 |
|---|---|---|---|
| **订阅号**（账号ID/RT/AT） | Bearer 里的 AT/RT | **是**：fp(凭据) → 账号ID | 账号ID |
| **API Key 号** | sk-… / xai-… / AIza… | **否**（无归属账号，不记录） | key 自身哈希（marker 现状） |

```
请求 -> 提取凭据(Bearer / x-api-key / api-key / x-goog-api-key / ?key=)
         ├─ 映射表命中(fp) -> SHA256(salt "#a" platform "/" 账号ID)[:16]
         └─ 未命中          -> SHA256(salt marker)[:16]        // marker 现状
```

不做的事：不解 JWT、不读 Chatgpt-Account-Id 头、不解析 body/URL——唯一提取物是凭据本身。

**映射表两个写入来源**（并存）：
1. **拉取**：内置同步器定时从 **CLIProxyAPI** 与 **Sub2API** 拉取（页面配置源与间隔；
   同类源可配置多个实例）；
2. **推送**：对外提供 upsert 接口，外部系统直接登记任意账号（标识不限邮箱）的 AT/RT，
   并指定来源类型（内置两型或自定义）。

---

## 1. 数据模型

一行 = "某平台、某来源实例下的一个账号 + 一套凭据（以 RT 为身份成分）"。
唯一键五列：`(platform, source, account, rt_fp, source_type)`。**同键即同一行**：

- AT 更新 → 原地覆盖该行（RT 不变则键不变）；
- 该来源快照中消失 → 删除；
- 新出现 → 插入；
- 同账号不同 `source` 实例或不同 `source_type` → 各占一行，互不影响。

```sql
CREATE TABLE IF NOT EXISTS acct_map (
  platform    TEXT NOT NULL,
  source      TEXT NOT NULL,             -- 来源实例：'src:<id>'（拉取源）/ 'api'（推送通道）
  source_type TEXT NOT NULL,             -- 来源类型全名：'CLIProxyAPI' | 'Sub2API' | 自定义
  account     TEXT NOT NULL,             -- 账号标识：邮箱、uuid……调用方自定义（统一小写）
  at_fp       TEXT NOT NULL DEFAULT '',  -- access_token 指纹 sha256(platform ':' norm_cred)
  rt_fp       TEXT NOT NULL DEFAULT '',  -- refresh_token 指纹（主键成分）
  at_hint     TEXT NOT NULL DEFAULT '',  -- 脱敏尾缀 "…3f"
  rt_hint     TEXT NOT NULL DEFAULT '',
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY(platform, source, account, rt_fp, source_type)
);
CREATE INDEX IF NOT EXISTS idx_acct_map_source ON acct_map(source);

-- 拉取源配置（页面维护）；api_key 明文存 secrets 表（键 source_key_<id>），绝不入本表
CREATE TABLE IF NOT EXISTS sync_sources (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  kind         TEXT NOT NULL,        -- 存储值 'cpa' | 'sub2api'
  name         TEXT NOT NULL UNIQUE,
  base_url     TEXT NOT NULL,
  interval_s   INTEGER NOT NULL DEFAULT 600,
  enabled      INTEGER NOT NULL DEFAULT 1,
  last_sync_at INTEGER,
  last_status  TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);
```

要点：

- **`source` × `source_type` 双列分工**：`source` 标识"谁写的这行"（决定快照对齐与级联删除
  的范围；同类多实例天然隔离，拉取永不误删他源/推送的条目）；`source_type` 是给人看的
  类型全名——拉取源由 `kind` 映射（cpa→CLIProxyAPI、sub2api→Sub2API），推送可自定义
  （≤64 字符非空任意值），便于将来接入新上游而不改 schema。
- **明文凭据永不落盘**；查表键是指纹；指纹含 platform（跨平台隔离同一 token 串）。
- 归一化：trim+去成对引号；`Bearer `/`Token ` 前缀剥离大小写不敏感；account 统一小写。
- 内存镜像（`acctmap.Registry`）：按五元组行键存全量行，另建 AT/RT 两张 fp→行 索引，
  写入走全量重载，热路径零 SQL。同一凭据被多个来源收录时 Lookup 返回任一命中行
  （账号身份一致，热路径等价）。
- hash 只依赖 `(platform, account)`：凭据轮换后重新登记/拉取，账号不变则 hash 不变。
- Schema 版本 v5；开发期 owners 集合形态在迁移中直接重建（内容可由源重拉恢复）。

## 2. 热路径决策

```go
cred := acctmap.ExtractCred(r)                 // 已知 AI 平台专用载体提取
if pf := acctmap.PlatformForHost(host); pf != "" {
    if e, ok := acctMap.Lookup(acctmap.Fingerprint(pf, cred)); ok {
        ident = Derive(salt+"#a", e.Platform+"/"+e.Account, sidLen)
    }
}
// 未命中回落：ident = sticky.Derive(salt, cred, sidLen)
```

映射表为空或全局开关关闭时，与纯 marker 公式逐字节一致（天然降级）。

**身份与 URL 无关**：hash 只由"账号/凭据"决定。通用 marker 通道的可选路径过滤
（`marker_path_parts`，包含语义、两侧段边界）默认留空——即对所有 URL 同一规则，
同一凭据无论访问哪个路径都得到同一粘滞身份；该配置属高级限流选项，不推荐填写。

## 3. 写入通道一：内置同步器

### 3.1 上游对接端点（已实测）

| 源类型（kind） | 展示名 | 拉取调用 | 认证 |
|---|---|---|---|
| cpa | CLIProxyAPI | `GET {base}/v0/management/auth-files` 列表 → `/auth-files/download?name=…` 逐文件 | Management Key（Bearer） |
| sub2api | Sub2API | `GET {base}/api/v1/admin/accounts/data`（备份导出，唯一返回凭据原文的端点） | Admin API Key（x-api-key 头） |

解析规则：

- **CLIProxyAPI auth 文件**：文件内 email/email_address（回退列表项 email/account/label）
  → 账号ID；access_token/refresh_token 指纹化；provider→platform 白名单映射
  （codex/openai→openai、claude→anthropic、gemini/antigravity→gemini、xai→grok、
  kimi、qwen、iflow 等）；disabled 文件照常同步（停用是上游路由决策，不影响归属语义）；
  无法解析或无凭据的文件跳过并计数。
- **Sub2API 导出**：仅 `type ∈ {oauth, setup-token}` 入表；platform 白名单映射；
  credentials 里取 access_token/refresh_token；账号取 credentials.email 回退 name；
  apikey/upstream 等类型跳过。

### 3.2 调度与快照对齐

- 单调度循环 30s 一 tick，每源按各自 `interval_s`（下限 60s）到期即拉；启动立即跑一轮；
  页面「立即同步」经 wake 通道插队触发单源；状态（ok: N accounts, M tokens / error: …）
  回写 sync_sources 并回显页面。
- **快照对齐按来源实例全删重插**（`ReplaceSourceSnapshot`，单事务）：
  `DELETE WHERE source='src:<id>'` 后插入本次全集——同键自然覆盖、旧有新无消失、
  新出现插入；其他实例的行不受影响，故重拉不会误删他源或推送的数据。
  源被删除时调用方级联 `DeleteSourceRows(src)` 清掉其名下全部行。

### 3.3 时效窗口

AT 约 1 小时轮换，新 AT 在下次同步前不在表中 → 短暂回落 key 级哈希。缓解：
interval 可调小（下限 60s）；或外部系统改用 §4 推送接口在刷新后即时登记。

## 4. 写入通道二：对外 upsert 接口

挂在管理端口，复用管理员认证。供无法被拉取覆盖的场景使用（自建池、临时手工登记等），
账号标识不限邮箱。

| 方法 | 路径 | 说明 |
|---|---|---|
| PUT | `/api/acctmap/{platform}/{account}` | **upsert 账号的凭据集**（主入口）：body 见下；幂等 |
| DELETE | `/api/acctmap/{platform}/{account}?source=` | 删账号；`source` 空=全来源，否则只删该实例名下 |
| DELETE | `/api/acctmap/{platform}/{account}/tokens/{fp}` | 清除匹配指纹的那列（先 AT 后 RT），两列皆空删行 |

PUT 请求体：

```json
{
  "access_token":  "eyJhbGciOi...",
  "refresh_token": "rt_...",
  "source_type":   "CLIProxyAPI"          // 必填非空；可用内置值或任意自定义类型
}
```

语义（`ReplaceAccountSnapshot`，source 固定 `'api'`，单事务）：

- 至少给一个 token；服务端归一化+指纹化后 upsert 到 `(platform,'api',account,rt_fp,source_type)` 行；
- **同类型内快照对齐**：同账号同 `source_type` 下与本 RT 不同的旧行自动清除——换 RT 直接推新值即可，无需手动清理；AT 更新原地覆盖；
- 不同 `source_type` 的行互不影响：同一账号可同时有 CLIProxyAPI / Sub2API / 自定义类型的推送行并存；
- 与拉取通道互不干扰（`source` 不同）；回执 `{ok: true}`；请求/响应日志整体脱敏。

## 5. 管理台页面：账号映射

三个区块：

1. **同步源列表**：名称/类型（显示 CLIProxyAPI / Sub2API）/地址/间隔/启用/最近同步状态；
   新增编辑表单（kind、name、base_url、api_key 密码框、interval_s）；「测试连接」「立即同步」。
2. **当前映射预览**（acct_map 视图）：**平台 / 账号 / AT 尾缀 / RT 尾缀 / 来源 / 更新时间** 六列——
   来源列为 `source_type` 全名标签 + 实例副标（源名或「手动登记」）；支持按平台、账号、
   来源类型筛选分页；顶部统计条（映射行数、去重账号数、各来源类型计数）。修改走源重拉或 §4 接口。
3. **手动登记弹窗**：platform（可自建）、account、source_type（内置两项可选、可输入自定义）、
   access_token / refresh_token 双输入框（至少一项）。
4. **全局开关** `identity.acctmap_enabled`（settings 键 `acctmap_enabled`，关闭即全部回落 marker 公式）。

管理 API：`GET/POST /api/sources`、`PUT/DELETE /api/sources/{id}`（删源级联清映射）、
`POST /api/sources/{id}/test|sync`、`GET /api/acctmap?platform&account&source&source_type&page`、
`GET /api/acctmap/stats`。

## 6. 轮换语义

复用 marker_salts，键推广：

```
rotate_key = Fingerprint(platform + "/" + account)   // 命中映射表
           = Fingerprint(marker)                     // 未命中，现状
```

机制不变；换出口 IP 变成账号级动作，同账号的第二把凭据共享轮换后的身份。

## 7. 安全与隐私

- 凭据明文边界瞬态：客户端请求头、拉取响应体、upsert 请求体；落库一律指纹化。
- 源 api_key 存 secrets 表，页面打码回显（占位符保持不变语义）；审计日志维持短指纹。
- 审计日志不保存请求头。需要排障时，可在启动时传入 `-trace-file <path>`：它将 URL、完整请求/响应 headers 和 body 流式写入独立文本文件；该输出不脱敏、默认关闭，且绝不进入 SQLite 审计表。

## 8. 验收清单（已实现）

| # | 场景 | 预期 |
|---|---|---|
| 1 | 双源首轮同步 | 各源独立成行（`src:*` + 类型全名），统计/预览正确 |
| 2 | 同账号双源并集 | 同账号在两源下并存两行，任一凭据命中同账号身份 |
| 3 | 单源重拉 | 全删重插仅限该实例；他源与推送行原样保留（不误删） |
| 4 | 推送 AT 更新 | 同键原地覆盖，不新增行 |
| 5 | 推送 RT 轮换 | 新行替换旧行（同类型内对齐）；不同 source_type 并存 |
| 6 | 删账号（带/不带 source） | 只删指定范围；他源行保留 |
| 7 | 删源级联 | 该实例行全清，其余不动；sync_sources 与 secrets 键一并移除 |
| 8 | 空表/开关关闭 | 与纯 marker 公式逐字节一致 |
| 9 | AT 多次轮换、进程重启 | 账号 hash 恒等 |

回归：`go test ./...`（含 store 快照语义、server 热路径隔离/轮换、acctmap 双索引）；
真实验证：`go run ./tools/e2elive`（凭据经 E2E_* 环境变量注入）。
