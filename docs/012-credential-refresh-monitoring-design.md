# AT/RT 刷新增量监控设计（极简版）

> 状态：按审查批注更新后的方案，只修改 MITMRouter；不修改 CPA 和 Sub2API 源码。
> 2026-08-29 修订：去掉全局增量开关和 api/direct 模式二选一；增量直读改为叠加在现有 API 定时全量之上的可选项，填了增量路径就启用，清空就停。
>
> 核心原则：Sub2API 直接读 PostgreSQL，CPA 直接读认证文件目录。发现变化后直接读取当前 AT/RT，计算现有 fingerprint，更新对应账号映射。不做新旧 Token hash 比较，增量直读不调用两个项目的管理 API（定时全量仍走现有 API），不保存额外监控快照。

## 0. 两种同步方式（为什么引入增量）

| | API 全量同步 | 增量直读 |
|---|---|---|
| 触发 | 定时任务，间隔 `interval_s`（≥60 秒） | Sub2API 每 3 秒轮询；CPA 文件事件即时处理 |
| 干什么 | 全量快照整体替换，管**完整性**：新增、删除、改名、换类型 | 只读有变化的账号/文件，更新 AT/RT 指纹，管**新鲜度** |
| 通道 | 上游管理 HTTP API（现状不变） | Sub2API 直接读 PostgreSQL；CPA 直接读认证文件目录 |

为什么要引入增量：全量是定时的，上游在两次同步之间刷新了 AT/RT，本地指纹就是旧的，最长要滞后一个 `interval_s`，期间按该账号路由的请求会失败。增量直读补的就是这个时间差。

两者**不是二选一**：增量是叠加在全量之上的可选项。全量保证账本"全、对"，增量保证账本"新"；删除和清理永远由全量负责，增量只写不删。

## 1. 范围和约束

```text
Sub2API：PostgreSQL → 变化账号 AT/RT → acct_map
CPA：    auth 文件目录 → 变化文件 AT/RT → acct_map
```

必须满足：

1. 不修改 CPA/Sub2API 应用代码；
2. 增量模式不访问 CPA 管理 API；
3. 增量模式不访问 Sub2API 管理 API；
4. 不持久化旧 AT/RT 的 hash，不保存监控快照；fingerprint 只用于现有 `acct_map` 索引，不做新旧 Token 判重；
5. 不保存 per-account hash、per-file hash 或额外源快照；
6. 不抓取 OAuth 请求/响应，不修改任何外部请求；
7. 增量监控是可选功能，没有全局开关：source 填了增量路径才启用，清空即停；未配置的 source 不建立数据库连接、不启动文件 watcher；
8. 增量直读与 API 定时全量并存，写同一个 source：全量照常定时跑，增量共用 source 锁串行写入，只更新 AT/RT 指纹，不做删除清理。

“不保存额外数据”指不持久化监控状态。运行期间允许有短暂的数据库连接、文件事件队列、去抖计时器和一次性扫描结果；进程退出后全部丢弃。

这里的“更新 marker”统一指：

```text
更新 acct_map 中该账号的 AT/RT fingerprint，
使后续请求继续按该账号身份路由。
```

客户端请求里的 Marker 本身不会被 MITMRouter 修改。

---

# 第一章 Sub2API：直接读取 PostgreSQL

## 1.1 配置和 source 绑定

增量监控没有全局开关：source 填了增量路径就启用，清空就停。直读配置必须挂在具体的同步源上，不能用一个全局地址绑定一个 source，这样可以同时配置多个独立 CPA/Sub2API 实例：

```text
同步源 A：Sub2API / 增量 / PostgreSQL-A
同步源 B：Sub2API / 增量 / PostgreSQL-B
同步源 C：CPA      / 增量 / /opt/cpa-a/auths
同步源 D：CPA      / 增量 / /opt/cpa-b/auths
```

源编辑表单只有一页，全量字段和增量字段全部摆开，没有 mode 单选：

```text
全量同步（必填）：base_url、api_key、interval_s
增量同步（选填）：Sub2API 数据库地址 或 CPA 认证文件目录；填了即启用，清空即停
```

`interval_s` 只表示全量同步间隔（≥60 秒）。增量频率用固定默认值，不做配置：
Sub2API 每 3 秒轮询一次；CPA 文件事件即时处理。

绑定关系持久化在 `sync_sources` 自身，而不是全局设置表。每个 source 独立配置自己的增量路径，进程重启后按 source 配置恢复增量 reader：

```text
sync_sources.id=1  kind=sub2api  direct_db_secret=...
sync_sources.id=2  kind=sub2api  direct_db_secret=...
sync_sources.id=3  kind=cpa      direct_auth_dir=/opt/cpa-a/auths
sync_sources.id=4  kind=cpa      direct_auth_dir=/opt/cpa-b/auths
```

V1 配置约束：

- 同一种 `kind` 可以配置多个 source；
- 每个 source 必填全量同步配置（base_url/api_key/interval_s），增量路径选填；
- 填了 Sub2API 增量路径的必须配置自己的 PostgreSQL DSN；
- 填了 CPA 增量路径的必须配置自己的认证文件目录；
- 配置了增量路径后，该 source 的 API 定时全量照常运行；「立即同步」按钮仍是触发一次 API 全量；
- 每个 source 独立启动 reader、独立加锁、独立更新 `src:<id>` 映射。

数据库地址如果包含用户名、密码或完整 DSN，必须作为对应 source 的敏感配置保存：

- 每个配置了增量路径的 Sub2API source 使用独立的 DSN secret，密钥名包含 source ID；
- 设置接口只回显“已配置”以及必要的非敏感地址信息；
- 不在日志、错误、审计或前端响应中输出密码和完整 DSN；
- 使用只读数据库账号；
- 同节点 PostgreSQL 可使用 `localhost`、回环地址或 Unix socket，不强制 TLS；
- 远程 PostgreSQL 必须使用 TLS 并校验证书（`sslmode=verify-full`）。

CPA 增量 source 的认证文件目录路径保存在对应 source 配置中；多个 source 可以使用不同目录。

## 1.2 两步增量

### 第一步：找最近更新的账号

不保存 cursor，直接使用数据库时间的短暂重叠窗口。默认每 3 秒执行一次，读取最近 30 秒有更新的 OAuth 账号：

```sql
SELECT id, updated_at
FROM accounts
WHERE updated_at >= clock_timestamp() - interval '30 seconds'
  AND type IN ('oauth', 'setup-token')
ORDER BY updated_at, id;
```

这一层只做一件事：

```text
拿到 account_id
```

不保存 `updated_at`，也不读取 `_token_version`。`updated_at` 变化可能只是普通账号编辑，允许误触发。

使用固定重叠窗口而不是单调 cursor，避免应用进程和数据库时钟不一致、同一时间戳并发更新等问题。重复处理同一账号是允许的。

Sub2API 的 `accounts` 表没有 `updated_at` 索引（ent schema 和 SQL migration 里都没有），这条查询是顺序扫描。几百上千个账号时每 3 秒扫一次没有压力；账号上万的部署需要运维侧加索引——只读监控账号无权建索引，这属于部署前提。时钟偏差超过 30 秒窗口时增量会漏，由定时 API 全量兜底。

### 第二步：直接读取候选账号的 AT/RT

只查询第一步拿到的账号：

```sql
SELECT
    id,
    name,
    platform,
    type,
    deleted_at,
    credentials->>'email' AS credential_email,
    credentials->>'access_token' AS access_token,
    credentials->>'refresh_token' AS refresh_token
FROM accounts
WHERE id = ANY($1);
```

该查询直接读取 Sub2API 的 `accounts.credentials`，不调用 Sub2API HTTP 接口。

处理规则：

```text
account = credentials.email；为空时使用 accounts.name

access_token / refresh_token
  → NormalizeCred
  → Fingerprint(platform, credential)
  → 更新该 source 下的账号映射
```

状态规则：

```text
oauth/setup-token 且有 AT 或 RT                  → 写入/替换映射
deleted_at 非空 / 变成其他账号类型 / 查询不到账号 → 增量跳过，由定时 API 全量清理
```

本流程不比较旧 Token。即使 AT/RT 没变，也可以重复覆盖，结果必须幂等。

## 1.3 增量写入

当前 `ReplaceSourceSnapshot` 是完整 source 快照语义，不能拿单个账号结果直接调用，否则会删除同 source 下其他账号。

增加一个简单的账号增量写入语义，例如：

```text
ApplyAccountDelta(sourceID, platform, account, accessToken, refreshToken)
```

同一个 source 的增量更新和 API 全量同步（syncOne）共用一把 source 级锁；锁覆盖“读取数据到提交映射”的全过程，不能只锁数据库写入。这样增量和全量不会互相覆盖。

一次处理一个 Sub2API 账号：

```text
获取 source 锁
  → 开始事务
  → 删除该 source/platform/account 下旧映射行
  → 当前有 AT 或 RT 时写入新的 fingerprint/hint
  → 两者都为空时只保留删除结果
  → 清理孤儿 acct_egress
  → 提交事务
  → ReloadFromStore 全量重载 Registry
  → 触发 OnMapChange 重建绑定快照
  → 释放 source 锁
```

API 全量同步本来就在同一把锁内完成“读取 → ReplaceSourceSnapshotGuarded → ReloadFromStore → OnMapChange”，增量与它天然互斥；全量提交之后的下一轮增量会补回扫描期间刚刚发生的刷新。

最后两步不能省：

- `Registry` 只有全量 `Reload` 一个写入口（acctmap 包注释明确选择了"全量换表最简单且无竞态"），本设计沿用，不新做单账号更新接口；
- 清理孤儿 `acct_egress` 可能删掉出站绑定，不触发 `OnMapChange` 重建绑定快照的话，程序会继续用旧清单派流量，路由到错误账号——现有 API 同步每次写完都会回调（见 syncer.go），direct 路径必须同样处理。

不做新旧 Token 比较，也不维护 per-account hash。3 秒轮询窗口内重复写入是允许的，写入必须幂等。

该操作只影响：

```text
当前 source + 当前 platform + 当前 account
```

不会影响 CPA、手工登记、其他 source 或其他账号。

V1 假设一个 Sub2API 账号对应一套当前凭据。当前 `acct_map` 主键允许同账号多套 RT，但直接增量路径不维护这种额外关系；发现同账号多套凭据时，以定时 API 全量结果为准。

Token 只作为该次调用的内存参数使用，不得进入日志、错误文本、指标、事件或新增持久化字段。

增量成功不覆盖 `TouchSyncSource` 的 last_sync_at/last_status（只记更新记录事件，docs/013）；增量失败把 last_status 置为 error。last_sync_at/last_status 只反映全量同步。

## 1.4 完整性由 API 全量负责

直读不做自己的完整扫描。增量窗口只能发现有 `updated_at` 的记录，发现不了硬删除、改名或平台变化——这些全部由现有 API 定时全量同步处理：快照整体替换，配 `ReplaceSourceSnapshotGuarded` 空快照保护（连接正常但解析出 0 个账号时，按 `sync_empty_clear_threshold` 连续空 N 次才清空，docs/011 §2.3）。增量漏掉的、上游删掉的，下一轮全量都会收敛。

## 1.5 Sub2API 失败行为

| 情况 | 行为 |
|---|---|
| 窄查询失败 | 不更新映射，下一轮重试 |
| 候选账号查询失败 | 不更新该账号，定时全量继续兜底 |
| 单账号写入失败 | 旧映射保留，重复处理 |
| 数据库断开 | 自动重连，期间不清空映射 |
| 增量 reader 停止 | 关闭连接，不改变已有映射 |

---

# 第二章 CPA：直接读取认证文件目录

## 2.1 配置

CPA 增量同步只需要一个认证文件目录：

```text
cpa_auth_dir = /path/to/CLIProxyAPI/auths
```

配置增量路径后：

- MITMRouter 直接读取该目录中的认证 JSON 处理文件事件；
- 增量路径不调用 `/v0/management/auth-files` 和 `/v0/management/auth-files/download`（定时全量仍走这两个管理 API）；
- 目录不可访问时报告配置错误。

CPA 当前认证文件加载支持递归目录（`filepath.WalkDir`），因此 watcher 也按递归目录处理。只处理普通 `.json` 文件；跳过符号链接，拒绝非普通文件和超过 2 MB 的文件（CPA 文件带 quota/model_states，几十 KB 很正常，上限放宽到 MB 级才不会误拒合法文件）。单个 symlink 不得阻塞整个 source。

## 2.2 文件变化处理

使用 `fsnotify` 监听认证文件目录及其子目录：

```text
Create
Write
Rename
Remove
```

fsnotify 不递归：启动时对每个子目录逐个 `Add`；运行期收到目录本身的 `Create` 事件时对该新目录补 `Add`。漏掉这步的话，新建子目录里的文件要等下一次 API 全量才能发现。目录被删除或重命名时对应 watch 自动失效，由定时全量兜底。

收到 `Create/Write`：

```text
事件进入有界内存队列
  → 同一路径短暂去抖
  → 确认文件可读且 JSON 完整
  → 直接读取 auth JSON
  → 解析 AT/RT
  → 计算 fingerprint
  → ApplyAccountDelta
```

文件正在被原地 truncate/write 时，可能暂时为空或 JSON 不完整。此时：

```text
不删除旧映射
本次事件稍后重试或等待下一次文件事件
```

事件队列溢出或 watcher 报错时记录脱敏告警；watcher 自动重建，漏掉的变化由下一次 API 定时全量收敛。

## 2.3 CPA 文件解析

直接读取沿用 MITMRouter 当前已验证的最小格式：

```text
账号：email / email_address
AT：access_token
RT：refresh_token
```

同时支持：

```text
顶层 access_token / refresh_token
tokens.access_token / tokens.refresh_token
```

平台映射使用现有 `cpaPlatformMap` 作为唯一规范。直读模式下优先读取 auth JSON 顶层 `type` 字段；如果实际文件提供 `provider`，可作为兼容回退。API 模式还有列表接口的元数据兜底，直读没有这条兜底。文件里 `disabled: true` 的同样入表，和现有 API 同步语义一致——停用是上游的路由决策，不影响指纹归属。未知 provider/type、symlink、无账号或无 AT/RT 的文件不生成映射；未知 provider/type 不得阻塞同一目录中其他有效文件。

V1 不承诺兼容 CPA 插件自定义解析器、一个文件展开多个虚拟账号等格式；遇到未知格式时跳过该文件、不生成新映射并记录脱敏错误，其他有效文件继续处理。由于 V1 不保存文件到账号的关系，无法单独保证该未知文件对应的旧映射保留；由定时 API 全量按可识别文件结果收敛。

## 2.4 删除、重命名和账号标识变化

由于 `acct_map` 当前不保存 CPA 文件名到账号的关系，增量不做删除和清理：

- 普通 `Write` 且文件内账号标识稳定时，只处理变化文件；
- `Remove` 事件忽略；`Rename` 后新路径可见时按新文件覆盖（覆盖 atomic-replace 写法）；
- 删除账号、改名、email/platform 变化、同账号多文件取哪份——全部由定时 API 全量按现有快照语义收敛，增量不做这些清理，也不保存 per-file 关系。

## 2.5 CPA 启动和失败行为

启用增量或进程启动时直接建立递归 fsnotify；初始导入和已有映射由定时 API 全量负责，不依赖增量首扫。

watcher 启动失败时不清空已有映射、报告错误，全量同步不受影响。

| 情况 | 行为 |
|---|---|
| 单文件半写入 | 保留旧映射，稍后重试 |
| 单文件解析失败 | 不更新该文件，保留旧映射 |
| 未知 provider/type | 跳过该文件，记录脱敏告警，不阻塞其他文件 |
| symlink | 跳过该条目，不阻塞 source |
| 删除/重命名 | 增量忽略或按新文件覆盖，清理由定时全量负责 |
| watcher 出错 | 自动重建，漏掉的事件由定时全量收敛 |
| 认证文件目录消失 | 停止 watcher，保留旧映射并报告错误 |
| 清空增量路径 | 关闭 watcher，不改变已有映射 |

---

# 第三章 统一运行规则

## 3.1 启用方式：填路径即启用，没有全局开关

增量同步没有全局开关，也没有 api/direct 模式二选一。启用条件只有一条：

```text
source.enabled = true 且 增量路径非空
  （Sub2API：DSN 已配置；CPA：认证文件目录非空）
```

- 填了路径 → 启动该 source 的增量 reader；清空路径 → 停止 reader、关闭数据库连接/watcher；
- 未配置增量路径的 source 不建立数据库连接、不启动文件 watcher；
- 配置了增量路径的 source，API 定时全量照常调度，不跳过；增量和全量共用同一把
  source 级锁，串行写入，不会互相覆盖；
- 每个 source 独立拥有 reader 和资源生命周期；不同 source 可以并行运行。

不同 source 可以并行读取和写入数据库，但 `ReloadFromStore` 与 `OnMapChange` 的收尾必须经过进程级 map-change 锁，避免较早读取的全量快照晚于较新快照完成而覆盖内存状态。管理台手动映射和绑定写入也使用同一把锁。

Source 生命周期：

- 删除 source：先停止 reader，等待当前任务退出，关闭数据库连接/watcher，再删除该 source 的映射和配置；
- 禁用 source：停止该 source reader，保留已有映射（API 全量同步同样停止，现有语义）；
- 修改增量 DSN 或认证文件目录：先停止并关闭旧 reader，再校验和启动新 reader，启动失败时保留旧映射，API 全量不受影响；
- 修改 source kind：按同样顺序停止旧 reader，再启动新 kind 的 reader。

## 3.2 映射更新后的行为

AT/RT 读取后统一经过：

```text
NormalizeCred
  → Fingerprint(platform, token)
  → acct_map 写入（含孤儿 acct_egress 清理）
  → ReloadFromStore 全量重载 Registry
  → OnMapChange 重建绑定快照
```

后两步是 direct 路径和现有 API 同步共用的收尾，缺一不可：`Registry` 只有全量重载一个写入口；清理孤儿绑定后不重建快照会用旧清单派流量。

所有 source 的全量 Registry reload 和绑定快照重建都必须串行完成；source 级锁只保护同一 source，不能替代进程级 map-change 锁。

`acct_map` 中与 Token 相关的字段只有：

```text
at_fp / rt_fp / at_hint / rt_hint
```

平台、账号、source 等映射定位字段仍按现有表结构保存。AT/RT 明文不写入：

- MITMRouter 数据库；
- 监控状态；
- 文件；
- 日志；
- 指标；
- HTTP 响应。

## 3.3 不改变外部请求

本设计只读取本地数据库和本地认证文件，不参与代理转发链路。必须保持：

- 外部请求 URL 不变；
- 外部请求头不变；
- 外部请求体不变；
- 外部响应头不变；
- 外部响应体不变。

## 3.4 最小实现顺序

1. 源编辑表单一页展示全量必填（base_url/api_key/interval_s）和增量选填（Sub2API DSN、CPA 认证文件目录），支持多个独立 source；填了增量路径即启用；
2. 增量 reader 与 API 全量同步共用 source 锁，API 同步照常调度；
3. 实现 Sub2API 最近更新时间窗口查询；
4. 实现 Sub2API 候选账号 AT/RT 直读；
5. 实现 `ApplyAccountDelta`（幂等覆盖、source 锁、`ReloadFromStore` 和 `OnMapChange` 收尾）；
6. 实现 Sub2API 固定 3 秒增量轮询，与全量共用 source 锁；不做直读完整扫描；
7. 实现 CPA 递归目录 watcher 和变化文件直读，单个 symlink 跳过；
8. CPA 删除/重命名交给定时全量收敛，增量只处理写入事件；
9. 实现增量 reader 的启停（由增量路径字段驱动）、删除、配置修改和资源关闭；
10. 增加失败保护、未知文件跳过、资源关闭、map-change 串行化和 Token 不入日志测试。

## 3.5 验收重点

### Sub2API

- 多个独立 Sub2API 增量 source 可以同时运行，分别读取各自 PostgreSQL；
- 增量轮询只访问对应 PostgreSQL；
- `updated_at` 窗口命中后直接读取对应 AT/RT；
- 不做 Token hash 比较；
- AT/RT 重复写入结果幂等；
- 软删除、改名、硬删除最终由定时 API 全量清理；
- 增量和 API 全量共用 source 锁；
- 配置了增量路径后，API 定时全量照常运行；
- 数据库失败不清空旧映射；
- `interval_s` 只控制全量同步间隔（≥60 秒），增量固定 3 秒轮询；
- 增量路径不发起 Sub2API HTTP 请求（全量同步仍走现有 API）。

### CPA

- 多个独立 CPA 增量 source 可以同时运行，分别读取各自认证文件目录；
- auth 文件写入后直接读取变化文件；
- 原地写入和 atomic rename 都能处理；
- 半写入不会清除旧映射；
- 删除、改名和账号标识变化由定时 API 全量处理；
- 递归目录文件可被监听；
- symlink 和未知 provider/type 文件被跳过，不阻塞其他有效文件；
- 增量和 API 全量共用 source 锁；
- 增量路径不发起 CPA HTTP 请求（全量同步仍走管理 API）。

### 共同

- 没有全局开关；未配置增量路径的 source 没有数据库连接和 watcher；
- 增量和 API 全量不会并发覆盖同一 source（共用 source 锁）；
- 配置了增量路径的 source，其删除、禁用、配置修改会停止旧 reader 并关闭资源；
- 重启后每个 source 的增量配置仍生效；
- 增量写入后绑定快照重建（OnMapChange 被触发）；
- 不同 source 并行更新时，Registry reload 和绑定快照重建按全局顺序完成；
- 全量快照空结果不立即清空，走 Guarded 连续计数；
- 指纹未变化的重复轮次可以重复幂等写入，不产生错误；
- 只更新对应 source；
- acct_map 的 Token 字段只保存 fingerprint 和 hint；
- 日志和接口响应中没有 Token 原文；
- 现有代理转发请求和响应完全不变。

## 3.6 本轮决定

- CPA 认证文件目录中的 symlink 跳过，不阻塞 watcher；配置的根目录本身仍不能是 symlink。
- 未知或不支持的 CPA `type/provider` 文件跳过并记录脱敏告警；JSON 损坏或读取失败时，不提交该文件并保留旧映射。
- `interval_s` 只表示全量同步间隔（≥60 秒）；增量频率固定：Sub2API 3 秒轮询，CPA 文件事件即时处理。
- 没有全局增量开关；清空某 source 的增量路径即停止该 source 的增量 reader。
- `gemini-cli` 共享平台映射行为本轮不再拆分或调整，接受其对 API 模式的既有影响。
- 同节点 PostgreSQL 使用 `localhost`、回环地址或 Unix socket 时不强制 TLS；远程 PostgreSQL 继续要求 `sslmode=verify-full`。
- 2026-08-29 决定：增量直读与 API 定时全量并存、不是二选一，填了增量路径就启用；完整性（删除/改名/类型变化）全部由全量负责，直读不做完整扫描；last_status/last_sync_at 只反映全量，增量成功只记更新记录（docs/013），失败置 error。

## 参考代码位置

MITMRouter：

- `internal/syncer/syncer.go`：现有 source 调度、快照同步和 OnMapChange 回调；
- `internal/syncer/cpa.go`：当前 CPA 字段解析规则；
- `internal/syncer/sub2api.go`：当前 Sub2API 字段解析规则；
- `internal/store/acctmap.go`：来源快照写入与账号级写入；
- `internal/store/acctegress.go`：`ReplaceSourceSnapshotGuarded` 空快照保护和孤儿绑定清理；
- `internal/acctmap/acctmap.go`：AT/RT fingerprint 索引（只有全量 Reload）；
- `internal/settings/settings.go`：设置快照；
- `internal/api/api.go`：设置接口；
- `web/src/views/Settings.vue`：设置页。

Sub2API：

- `backend/ent/schema/account.go`：`accounts.credentials` 和账号字段；
- `backend/internal/service/oauth_refresh_api.go`：OAuth 刷新和 `_token_version`；
- `backend/internal/repository/account_repo.go`：凭据写回。

CPA：

- `sdk/auth/filestore.go`：auth 文件遍历和读写；
- `internal/watcher/events.go`：CPA 自身文件 watcher；
- `internal/watcher/synthesizer/file.go`：auth JSON 解析。
