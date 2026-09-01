# 账号映射更新记录设计（管理台「更新记录」页）

> 状态：已实现（2026-08-29）。真实验证：direct 文件事件带文件名可见、API 拉取/推送/删除事件、筛选与清空、退出排空均通过；store 单测覆盖分页/过滤/保留/截断边界。
>
> 一句话：在管理台「访问审计」下面加一个「更新记录」页。acct_map 每次因为同步、
> 文件扫描、手动推送或删除而变化时记一条事件，页面上能看到**哪个来源、用什么方式、
> 动了哪个文件/哪些账号、成功还是失败**。

---

## 1. 要解决的问题

1. direct 模式增量扫到的文件目前**无处可查**：acct_map 只存解析结果（账号/指纹），
   `sync_sources.last_status` 只存最近一条状态，成功时还是固定文案
   `ok: direct file update`，不带文件名。用户问了"增量扫到的文件在哪里看"，答案是
   现在看不到。
2. API 拉取、手动推送、删除映射也一样：只有 last_status 一条最新状态，没有历史。
3. 「访问审计」记录的是代理转发的流量，和映射变更无关，回答不了这类问题。

## 2. 范围和非目标

记录：acct_map 的全部变更事件（成功与失败）+ direct 文件扫描事件（含文件路径）。

非目标：

- 不修改代理转发语义，不修改任何对外请求的 URL/头/体（AGENTS.md 第 3 条）；
- 不做实时推送（WebSocket/SSE），沿用审计页的"手动刷新 + 5 秒自动轮询"；
- 不记录每行 acct_map 的改前/改后镜像（量大且没必要），只记事件 + 账号摘要；
- 不新增设置项：保留期直接复用访问审计的 `log_retention_days`。

## 3. 在哪里埋点（事件来源）

| 事件类型 kind | 触发位置 | 记录内容 |
|---|---|---|
| `direct_file` | `syncer.runDirectCPAFile` 处理一个文件后 | 文件名 + 平台/账号；读取/解析失败时记失败原因 |
| `direct_incremental` | `syncer.runDirectIncremental` 一轮结束 | 本轮应用账号数；查询失败时记失败原因 |
| `api_sync` | `syncer.syncOne` 一次拉取结束 | 账号数/token 数、空快照保护触发、失败原因 |
| `push` | `api.putAcctMapAccount`（手动推送） | 平台/账号，source='api' |
| `delete` | `api.deleteAcctMapAccount` / `deleteAcctMapToken` | 删除的平台/账号/指纹 |

说明：

- **失败也记**（status=error）。这样这一页同时就是"同步失败历史"，排查拉取源问题
  不用再翻进程日志。
- 埋点都在已有的成功/失败分支上，不新增并发路径。direct 路径的写入发生在
  sourceLock 之内，单行插入对锁持有时间的增加可以忽略（审计已确认这些锁本就覆盖
  数据库事务）。

## 4. 表结构

```sql
CREATE TABLE IF NOT EXISTS sync_events (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  ts      INTEGER NOT NULL,
  kind    TEXT NOT NULL,              -- direct_file | direct_incremental | api_sync | push | delete
  source  TEXT NOT NULL DEFAULT '',   -- 'src:<id>' / 'api'，与 acct_map.source 同口径
  status  TEXT NOT NULL DEFAULT 'ok', -- ok | error
  summary TEXT NOT NULL DEFAULT '',   -- 一句话人话摘要，见第 5 节
  detail  TEXT NOT NULL DEFAULT ''    -- 可选补充：文件绝对路径等
);
CREATE INDEX IF NOT EXISTS idx_sync_events_ts ON sync_events(ts);
```

与 `access_logs` 分表，不混用：访问审计是流量，这边是映射变更，保留和清空互不影响。

## 5. summary / detail 的内容约定

原则：**summary 是人话，一眼看懂；detail 放路径等补充信息；绝不写 token 明文或完整指纹**
（指纹等价于凭据，日志里最多留尾缀提示）。

```text
direct_file 成功:  summary='gem.json → gemini/api@example.com'  detail='/opt/cpa/auth/gem.json'
direct_file 失败:  summary='读取失败: CPA auth entry is not a regular file'
direct_incremental: summary='applied 3 accounts'
api_sync 成功:      summary='416 accounts, 832 tokens'
api_sync 空快照保护: summary='empty snapshot deferred 2/5'
api_sync 失败:      summary='upstream http://x: HTTP 502'
push:              summary='openai/a@example.com'
delete:            summary='openai/a@example.com (rt …xx)'
```

## 6. 写入路径：复制 access_logs 的异步批量模式

代理访问日志的现有写法是：业务方往 `auditCh`（缓冲 4096）里丢，`RunLogWriter`
goroutine 每 200ms 批量落库，退出时带超时排空。更新记录**完全复用这个模式**：

- store 新增 `UpdateEvent` 类型、`RunUpdateEventWriter(ctx, <-chan UpdateEvent)`
  （实现照抄 RunLogWriter：`context.WithoutCancel` + 批量 + 超时排空）；
- `Manager` 加字段 `Updates chan<- store.UpdateEvent`（可 nil，nil 时埋点直接跳过，
  测试不用接线）；`api.Deps` 同理；
- main.go 建一条 channel 接给两个生产方和一个 writer goroutine。

不用同步直写的原因：与 access_logs 同构最好维护，且埋点方永不阻塞；事件频率低
（分钟级同步 + 文件事件），缓冲 4096 足够，极端 burst（初扫两万文件）下 writer
也能在秒级追平。

## 7. API

```text
GET    /api/updates?from&to&kind&source&status&page&page_size
         → {"items":[{id,ts,kind,source,status,summary,detail}],"total":N}
         （from/to 为 unix ms；分页上限沿用 access_logs 的 200）
DELETE /api/updates   清空（复用审计页"清空"的确认交互）
```

store 层对应 `ListUpdateEvents(ctx, UpdateFilter) ([]UpdateEvent, int64, error)`、
`ClearUpdateEvents(ctx)`，过滤条件实现照抄 `ListLogs`。

## 8. 前端

- `web/src/router.ts` 加 `/updates` 路由；菜单项放在「访问审计」下面：
  i18n `menu.updates`，中文「更新记录」、英文「Updates」（zh.ts / en.ts 都加，
  遵循 e85d91d 的双语惯例）；
- 新增 `web/src/views/Updates.vue`，交互照抄 `Audit.vue`：
  - 筛选：时间范围（1h/24h/7d/全部）、类型下拉、状态下拉（成功/失败）、来源下拉
    （数据来自 `GET /api/sources` 的现值，纯文本兜底）；
  - 列表列：时间 | 类型 | 来源 | 结果 | 摘要（detail 放 tooltip 或展开行）；
  - 分页 + 5 秒自动刷新开关 + 清空按钮（带确认）。
- 已删除的 source 不影响历史事件展示：source 列存的是 `src:<id>`，名称解析不到时
  直接显示 `src:<id>`。

## 9. 保留与清理

- `RunRetention` 的每日清理扩为两张表：`access_logs` 和 `sync_events` 同用
  `log_retention_days`，不新增设置；
- `DELETE /api/updates` 提供手动清空，与 `DELETE /api/logs` 平级。

## 10. 测试要点

1. store 单测：写入/分页/过滤（kind、source、status、时间窗）/清空/保留清理；
2. 真实链路（沿用 014 审计时的本地真跑方法）：起服务 → direct 源目录投文件 →
   页面能看到 `direct_file` 事件且**文件名可见**（本设计要解决的核心问题）→
   API 源同步 → `api_sync` 事件 → 删除该 source → 其历史事件仍在；
3. `go test -race ./...` 回归：埋点在 sourceLock 内新增 channel 发送，确认无阻塞
   与竞态；
4. 优雅退出：进程收 SIGTERM 后缓冲中的事件落库（RunUpdateEventWriter 排空语义）。

## 11. 落地顺序

1. store：表结构 + `UpdateEvent` + writer/list/clear + retention 扩展；
2. syncer 与 api 的埋点 + main.go 接线；
3. `GET/DELETE /api/updates`；
4. 前端路由、菜单、`Updates.vue`、i18n；
5. 构建本地真实验证后发布（替换服务器二进制并重启）。

---

## 批注区

请把决定写在下面（例如：要不要记失败事件、来源筛选用下拉还是文本框、
保留期是否独立设置）：

```text
1.

2.
```
