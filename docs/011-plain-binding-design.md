# 设计：出站（普通代理）与账户出站绑定

> 状态：v1 · 2026-08-27（已实现）
> 关联代码：`internal/upstream/`、`internal/store/`、`internal/server/ingress.go`、
> `internal/api/`、`web/src/views/Upstreams.vue`、`web/src/views/AcctMap.vue`

---

## 0. 一句话说清

现在的上游全是**粘滞平台**（DataImpulse/Decodo 这类，靠注入会话 ID 换出口）。
本设计新增一种上游类型——平台名为 **`plain`（普通代理）**：就是一条普通代理（http/https/socks5），
凭据原样使用，不做任何会话注入。

然后给**账户**和**出站**之间搭一座桥：

```
账户（账号管理页里的 platform/account）
   │  绑定：模式（粘滞 / 随机） + 出站列表（可多个）
   ▼
出站 A ── 出站 B ── 出站 C
```

- **粘滞**：这个账户的请求永远从同一个出站走（哪个出站由哈希定，重启不变）。
- **随机**：这个账户的每个请求在它的出站列表里随机挑一个。

用户可以从两个方向配：

| 方向 | 操作 |
|---|---|
| 选账户 → 加出站 | 勾多个出站 + 选模式（粘滞/随机），保存即整体替换该账户的绑定 |
| 选出站 → 加账户 | 勾多个账户批量关联；已有绑定的账户保持原模式，新账户用本次指定的模式 |

最终形态就一行话：**每个账户关联了哪些出站、是粘滞还是随机**。

## 1. 需求与边界

1. **优先级**：账户绑定命中 > 现有粘滞路由（默认上游 + 会话注入）。绑定存在即生效；
   绑定的出站全部停用或缺失时**受控失败**（502），绝不悄悄回落粘滞路由——
   换出口等于改变账户的出口 IP 语义。
2. **级联删除**：
   - 账户被删（任何通道：手动删、同步快照消失、清凭据清空、删源）→ 它的绑定一并删除；
   - 出站被删 → 引用它的绑定行一并删除。
3. **不改请求承诺**（仓库铁律）：出站只决定「流量从哪出去」。对目标站的请求
   URL、请求头、请求体、响应头、响应体一个字节都不改。发给出站代理本身的
   `Proxy-Authorization` 是隧道认证，与现有粘滞上游同一机制，不属于修改请求。
4. 只有**账号映射命中**的请求参与绑定（未映射的裸 Marker 维持现状：默认上游 +
   纯 Marker 哈希）。Marker 级绑定是未来扩展，本期不做。
5. 盲隧道、无 Marker 流量不受影响（它们没有映射身份，天然不命中绑定）。
6. 逃生门复用现有开关：`acctmap_enabled` 关闭后映射不生效 → 绑定自然不参与，
   全体回落纯 Marker 公式。绑定本身不设独立开关。

## 2. 数据模型

### 2.1 出站 = upstreams 表里的一行

不建新表。出站就是 `platform='plain'` 的上游条目：

| 字段 | 出站的取值 |
|---|---|
| platform | `plain` |
| base_url | `scheme://user:pass@host:port`，凭据写在 URL 里（与现有上游一致） |
| inject | 恒空（无会话注入） |

好处：

- 增删改查、启停、「测试」、「设为默认」全部复用现有上游页面与 API；
- **出站也可以被设为默认上游**：此时未绑定流量直接经它出去（无会话注入），
  「拿一条普通代理当整体出口」免费获得。`plain` 平台注册一个恒等注入器
  （`Inject` 原样返回 base_url），default_upstream 选择逻辑零改动。

校验（`ValidateForSave` 增加 plain 分支）：scheme ∈ http/https/socks5/socks5h、
host 非空、inject 必须为空；无平台特定用户名要求。

### 2.2 新表 acct_egress（绑定）

```sql
CREATE TABLE IF NOT EXISTS acct_egress (
  platform   TEXT NOT NULL,    -- 账号平台（acct_map.platform）
  account    TEXT NOT NULL,    -- 账号标识（acct_map.account，统一小写）
  egress_id  INTEGER NOT NULL, -- upstreams.id（platform='plain' 的行）
  mode       TEXT NOT NULL DEFAULT 'sticky',  -- 'sticky' | 'random'
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(platform, account, egress_id)
);
```

要点：

- **mode 是账户属性，不是出站属性**。同一账户的所有行 mode 相同——表里冗余存，
  由写路径保证：任何修改都按「先删该账户全部行、再插入新全集」的单事务执行，
  不存在半新半旧。
- 绑定键是 `(platform, account)`，与来源（source/source_type）无关：同账号多来源
  凭据都路由到同一份绑定。
- egress_id 不建外键（库未开 `foreign_keys`），级联由 store 层事务保证，与
  `DeleteSyncSource` 的既有做法一致。

### 2.3 级联与垃圾回收（两条规则，覆盖全部路径）

1. **删出站**：`DeleteUpstream` 改为单事务，附带
   `DELETE FROM acct_egress WHERE egress_id=?`（非 plain 上游删除时是空操作）。
2. **账号消失**：所有会改动 `acct_map` 的 store 方法
   （`ReplaceSourceSnapshot`、`ReplaceAccountSnapshot`、`DeleteAcctMapAccount`、
   `ClearAcctMapFp`、`DeleteSourceRows`/`DeleteSyncSource`）在**同一事务**末尾执行：

   ```sql
   DELETE FROM acct_egress
   WHERE NOT EXISTS (
     SELECT 1 FROM acct_map a
     WHERE a.platform = acct_egress.platform AND a.account = acct_egress.account
   );
   ```

   一条 GC 语句覆盖所有删除通道——账号在 acct_map 里一行都不剩，绑定才删；
   同账号只要还有任一来源的行，绑定保留。

   **空快照保护**：同步源连接正常但拉回 0 个账号（闪断、上游侧短暂异常）时，
   不能立刻按快照对齐清掉该源名下的行——那会让账号消失、绑定被 GC。
   规则：连续 N 次空快照（期间取回失败不计入也不清零）才执行清空；
   计数持久化在 `sync_sources.empty_streak`，重启不清零；任一非空快照归零；
   清空发生后源名下已无行，计数回到 0——账号若再出现、之后又闪断，会重新
   获得完整的保护期（宁可多保一次，不误删）。N 为设置项
   `sync_empty_clear_threshold`（默认 3，范围 1–100；1 = 立即清空，
   即旧行为）。取回/解析失败本来就不写快照，所以「连接正常」天然成立。
   边界说明：非空但缺了部分账号的快照（分页异常等）仍立即对齐——按账号级
   缺席计数保护的复杂度不值当，闪断的主要形态是全空。

## 3. 热路径决策

### 3.1 决策顺序（resolveOutboundDetailed 内，身份推导之后）

```
① 私网目标校验（不变，安全优先）
② no_marker_policy=direct 直连（不变；映射身份必有凭据，与此无交集）
③ 账户绑定命中且存在「启用」的出站 → 走出站（本设计新增）
④ 默认粘滞上游 + 会话注入（现状）
```

③ 的细节：

```
binding, ok := egressTable.Lookup(ident.Platform, ident.Account)
候选 = binding.EgressIDs 在上游表快照中存在、platform=plain 且 enabled 的条目（按 ID 排序去重）
候选为空 → 受控失败：502 + internal_error=upstream_config，绝不回落粘滞路由、更不直连
            （与「默认上游缺失必须受控失败」同一语义：用户明确绑定了出站，
             悄悄换出口等于改变账户的出口 IP 语义；resolution_reason=egress_none_enabled）
粘滞模式 → HRW 挑一个（见 3.2）
随机模式 → 均匀随机挑一个
返回该出站的 base_url（恒等注入，不注入任何会话参数；session_ttl_min 对出站无意义，忽略）
```

实现落点：

- `identity` 结构体增加独立的 `Platform`、`Account` 字段（现在只有拼接串
  `platform/account`，账号含 `/` 时拆分有歧义，不拆）。
- 新包 `internal/acctegress`：不可变快照 `Table`（`map[Key]Binding`，Key 为
  platform+account），启动时从库构建，管理台写入后整体原子替换——与
  `upstream.Table`、`settings.Holder` 同一套「零锁读、写时换表」模式。
- `Server` 持有 `atomic.Pointer[acctegress.Table]` + `SwapAcctEgress()`；
  API 结构体注入 `SwapAcctEgress func(*acctegress.Table)`（对齐现有 `SwapUpstreams`）。

### 3.2 粘滞挑选：Rendezvous 哈希（HRW）

对候选池里每个出站算分数，取最大者：

```
score = SHA256( 盐 ‖ account键 ‖ 出站ID )[:8] 按大端解释为 uint64
盐    = CombineSalt(hash_salt, markerSalts.Get(saltKey))   ← 与身份推导同源
```

性质：

- **确定性**：同盐同池同结果，重启、多副本都不漂；不持久化任何挑选状态。
- **增删出站影响面小**：池里加减一个出站，只有原本 hash 到它的账户换出口，
  其余账户不动（普通取模会大面积重洗，不用）。
- **逃生阀自动生效**：现有机制「连续可轮换错误达阈值 → 该身份动态盐 +1」原样
  复用——盐变了，HRW 分数变了，粘滞模式的账户自动换到另一个出站。失败计数
  键仍是 `ident.key`（platform/account），代码不改。
- 随机模式下轮换无感（下一请求本来就随机），失败计数照记，无害。

随机挑选用均匀随机（math/rand/v2 自动播种），每请求独立，无会话记忆。

### 3.3 可观测性

- 审计 `upstream` 列直接显示出站名；`account_fp` 仍记账户身份哈希（沿用现有
  推导，保证审计筛选连续性）。
- debug 日志带 `resolution_reason`：`egress_sticky` / `egress_random` /
  `egress_none_enabled`。
- metrics 复用 `requests_total{upstream=出站名}`，不加新指标。

## 4. 管理 REST

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/acctegress` | 全量绑定 + 每个出站的关联计数（一次喂饱双向两个视图） |
| PUT | `/api/acctegress/{platform}/{account}` | **账户方向**：整体替换该账户绑定，body 见下 |
| DELETE | `/api/acctegress/{platform}/{account}` | 清除该账户绑定 |
| PUT | `/api/acctegress/egress/{id}` | **出站方向**：整体替换该出站关联的账户集合，body 见下 |

账户方向 body：

```json
{ "mode": "sticky", "egress_ids": [3, 7] }
```

- 语义：先删该账户全部行，再按 mode 插入新全集（单事务）；`egress_ids` 为空
  等价于清除绑定。
- 校验：账号必须存在于 acct_map（404 `account_not_found`）；所有 id 必须是
  启用与否不限的 plain 条目（400 `not_plain`）；mode ∈ sticky/random（400）；
  id 去重。

出站方向 body：

```json
{ "accounts": [{"platform":"anthropic","account":"a@x.com"}, ...], "mode": "random" }
```

- 语义：整体替换该出站的账户集合——不在列表里的账户移除此出站（若因此
  出站清空，绑定整行消失）；列表里的账户追加此出站。
- **mode 只对「本次新绑定」的账户生效**（缺省 sticky）；已有绑定的账户保留
  自己的 mode——批量操作不悄悄改别人的路由语义。
- 校验：id 必须是 plain 条目；accounts 非空可（空=清空该出站全部关联）；
  每个账户须存在于 acct_map（整单 404，原子不落库）。

上游 CRUD 接口不变，`platform=plain` 自然走通；删除上游时 store 层级联清绑定，
随后 `rebuildTable` + 重建绑定快照一起换。

## 5. 管理台页面

### 5.1 上游出口列表页（Upstreams.vue）

- 平台下拉新增「plain（普通代理）」：选中后表单切简版——名称、代理地址
  （含可选 user:pass）、启用；无模板无提示语。
- 出站行同样支持「测试」（验证出口 IP/地理）与「设为默认」。
- 出站行新增「关联账户」按钮 → 弹窗：账户多选（按平台分组、可搜索）+ 新账户
  模式单选（粘滞/随机）+ 当前已关联账户预勾选；保存调出站方向 PUT。

### 5.2 账号管理页（AcctMap.vue）

- 映射预览按 `(platform, account)` 去重展示「出站」列：徽标显示模式 + 出站数
  （如 `粘滞 · 3` / `随机 · 1` / `—`）。
- 行操作新增「绑定出站」→ 弹窗：出站多选 + 模式单选，保存调账户方向 PUT。
- 同一账户多行（多来源/多凭据）共享同一绑定，徽标一致。

中英双语词条同步补（zh/en i18n 文件）。

## 6. 安全与隐私

- 出站凭据存 `upstreams.base_url`（与现有上游同位）；列表接口打码回显，
  日志与审计绝不落 URL/凭据。
- 绑定表只存平台名、账号标识、出站 ID、模式——无凭据无敏感串。
- 不改请求铁律再申明一次：出站路由对请求/响应内容零改动（见 §1.3）。

## 7. 兼容与迁移

- `ensureSchema` 追加 `acct_egress` 建表（幂等），无设置键变更，无数据回填。
- 旧版本二进制读新库：多一张表无副作用；`platform='plain'` 的行在旧代码里
  注入器缺失——仅当选为默认上游时报 `injector_missing` 受控失败（降级注意项）。

## 8. 测试与验收清单

| # | 场景 | 预期 |
|---|---|---|
| 1 | 账户绑定 2 个出站（粘滞） | 同账户多次请求恒走同一出站；重启后仍同一出站 |
| 2 | 粘滞池加/删一个出站 | 仅原本命中被删出站的账户换出站，其余不动 |
| 3 | 随机模式 | 多次请求命中全部候选出站（分布均匀性冒烟） |
| 4 | 绑定命中 | 绝不走默认粘滞上游；审计 upstream 列为出站名 |
| 5 | 无绑定 / 未映射 Marker | 与现状逐字节一致（默认上游 + 会话注入） |
| 6 | 绑定的出站全部停用 | 受控失败（502 / upstream_config），绝不回落粘滞路由或直连 |
| 7 | 删账户（手动/同步消失/清凭据/删源） | 绑定同事务清理；多来源账号只要剩一行则保留 |
| 8 | 删出站 | 引用行级联删除；快照热更新，在途请求不受影响 |
| 9 | 出站设为默认 | 未绑定/未映射流量经它直出，无注入 |
| 10 | 粘滞出站连续失败达阈值 | 盐轮换 → 自动换出站（逃生阀） |
| 11 | API 校验 | 未知账户 404；非 plain 的 id 400；坏 mode 400；两方向 PUT 均原子 |
| 12 | acctmap_enabled 关闭 | 绑定不参与，回落纯 Marker 公式 |

单测：store 级联/GC/替换语义、HRW 确定性与稳定性、acctegress 快照；
集成：`tools/mockexit` 起多条出站 + mock 粘滞上游，验证路由走向与审计；
回归：`go test ./...`。
