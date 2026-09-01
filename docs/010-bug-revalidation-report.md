# Bug 复核报告

> 复核对象：当前 `main` 工作树（基线提交 `97c9843`；逻辑在 `eebb35d` 仍成立）   
> 复核方式：不修改业务代码，只阅读当前实现并运行定向检查。  
> 结论：此前列出的 20 项问题中，**1 项已修复，19 项仍存在**。其中有 4 项高风险问题需要优先处理。

## 一眼看懂

| 结果 | 数量 | 含义 |
|---|---:|---|
| 已修复 | 1 | 代码和回归测试都能证明问题已消失 |
| 仍存在 | 19 | 当前实现仍可走到原来的错误路径，或已实际复现 |
| 未验证 | 0 | 本轮没有留下无法判断的项目 |

## 本轮检查结果

| 检查 | 结果 | 备注 |
|---|---|---|
| `go test ./...` | 通过 | 普通单次测试通过不能代表没有状态或并发问题 |
| `go vet ./...` | 通过 | 没有发现 vet 可报告的问题 |
| `git diff --check` | 通过 | 当前文本补丁没有空白错误 |
| `go test -shuffle=on -count=3 ./internal/api ./internal/server ./internal/store ./internal/syncer` | **失败** | `TestMetricsGate` 仍依赖全局指标状态，重复运行后断言失败 |
| 新建实例的数据库权限检查 | **失败** | `umask 0022` 下，`router.db` 实际权限仍为 `0644` |

## 优先级 P0：应先修

### P0-1 默认上游失效会静默直连

**状态：仍存在**

`internal/server/ingress.go` 的 `resolveOutboundDetailed` 中，当 `default_upstream` 已配置、但运行时表里找不到可用条目时，仍返回：

```go
return nil, account, directName, "no_upstream", nil
```

`nil` 上游 URL 表示直连。这会让本应经住宅出口的请求从 MITMRouter 所在机器直接出网，暴露真实出口 IP。

**应有行为：**

- `default_upstream` 为空：允许按“没有上游”语义直连；
- `default_upstream` 非空但条目缺失、禁用或解析失败：返回受控失败，不能直连。

---

### P0-2 数据库文件权限是 0644

**状态：仍存在，已实际复现**

启动新实例并设定 `umask 0022` 后：

```text
700 data/
644 data/router.db
```

`router.db` 保存 CA 私钥、上游出口凭据、管理员密码哈希、会话签名密钥等敏感数据。若数据目录本身曾以宽松权限存在，这就是实际本机信息泄露风险。

**应有行为：**

- 数据目录启动时收紧为 `0700`；
- `router.db` 启动时创建或收紧为 `0600`；
- 同时检查 SQLite 的 `-wal`、`-shm` 伴随文件权限。

---

### P0-3 CLIProxyAPI 只下载到部分文件时会覆盖完整账号快照

**状态：仍存在**

`internal/syncer/cpa.go` 中，部分 auth 文件下载失败只会增加 `skipped` 并打日志，函数最后仍返回已下载成功的条目：

```go
if skipped > 0 {
    m.log.Warn("syncer: cpa files skipped", "count", skipped)
}
return out, nil
```

之后 `internal/syncer/syncer.go` 会调用 `ReplaceSourceSnapshot`，该操作会先删掉来源的旧映射，再写入这份不完整的数据。

**影响：** 一次临时 502 或网络超时可能让未下载成功账号的映射消失，账号粘滞关系退化或出口 IP 改变。

**应有行为：** 只要本次完整文件列表中有应同步文件下载或解析失败，就保留旧快照，将同步标记为失败。

---

### P0-4 删除同步源时，后台同步可能把已删除映射写回来

**状态：仍存在**

当前删除流程分两步：先删 `acct_map` 中 `src:<id>` 的行，再删 `sync_sources`。后台同步则可在拉取完成后继续调用 `ReplaceSourceSnapshot`。

可能时序：

```text
同步协程读取 source -> 管理员删除 source 和映射 -> 同步协程写回旧 source 的映射
```

**影响：** UI 已删除 source，但数据库可能重新出现孤儿映射；运行时 Registry 重载后，这些已删除来源的凭据仍可参与账号归属。

**应有行为：**

- 删除 source、删除其 secret、删除其 mapping 必须同一事务；
- 写入同步快照时，必须在同一事务确认 source 仍存在。

## 已修复

### MITM 内层请求丢失单请求取消信号

**状态：已修复**

当前 `internal/server/ingress.go` 在 TLS 解密后，不再用 CONNECT 请求的 Context 覆盖内层 HTTP 请求 Context，而是只转移已完成的目标解析结果：

```go
r = withTunnelTargetResolution(r, ctx)
r = withFreshInternalRequestID(r)
```

`internal/server/logging_test.go` 已覆盖取消场景：取消内层请求后，处理后的 Context 会正常结束。

**效果：** HTTP/2 的单独 stream 取消、SSE 中断和客户端取消请求不再被 CONNECT 隧道 Context 吞掉。

## 其余仍存在的问题

| 编号 | 问题 | 说明 |
|---|---|---|
| P1-1 | 单独更新 AT 或 RT 会丢另一类凭据映射 | `ReplaceAccountSnapshot` 仍以 `rt_fp` 作为替换条件；AT-only 更新会删除已有 RT 行，反向同理。 |
| P1-2 | `bufConn` 包装后 TCP 半关闭丢失 | 盲隧道只断言顶层对象是否有 `CloseWrite`；HTTP CONNECT 返回的 `bufConn` 没有该方法，底层 TCP 连接收不到半关闭。 |
| P1-3 | 修改 TTL 可让已有 Generic 上游运行时报 502 | 设置保存没有按新 `session_ttl_min` 重验 `{ttl_min}` 模板。 |
| P1-4 | SQLite 路径含 `?`、`#`、`%` 时解析错误 | DSN 仍直接拼接：`"file:" + dbPath + "?..."`，合法目录名会被当作 URI 控制字符。 |
| P1-5 | source 与 API Key secret 的 CRUD 非原子 | 创建、更新、删除同步源仍分多次数据库操作；中间失败会留下半成品状态。 |
| P1-6 | source 测试接口处理不完整 | 不存在 source、打码 key、chunked body、临时 URL/kind 的处理仍不完整。 |
| P1-7 | source URL 不严格校验，URL 凭据可能回显 | 当前只 trim 末尾 `/`，可接受带 userinfo、query、fragment 或非 HTTP(S) URL。 |
| P2-1 | 登出不清 Cookie | 前端只跳转登录页并刷新；没有 `/api/auth/logout`，有效 session cookie 仍存在。 |
| P2-2 | Vite 开发服务器的 API 转发端口错误 | `web/vite.config.ts` 仍把 `/api` 指向 `127.0.0.1:55666`；管理 API 实际在 `55667`。 |
| P2-3 | 带认证接入地址未做 URL 编码 | `ingressURLWithAuth` 仍字符串拼接；密码含 `@:/?#` 时生成的 URL 可能无法使用。 |
| P2-4 | account map 和审计分页有整数溢出风险 | 两处仍直接计算 `(page - 1) * pageSize`。 |
| P2-5 | CPA 嵌套 `tokens` 未解析 | 虽定义 `Tokens json.RawMessage`，实际仍只读取顶层 AT/RT。 |
| P2-6 | Qwen / iFlow Host 映射缺失 | 同步和 UI 支持这两个 platform，但 `hostPlatforms` 不包含其 API host。 |
| P2-7 | `TestMetricsGate` 使用全局指标 | 重复、随机顺序执行时 `probe_total` 累加，测试仍硬编码期待值为 1。 |
| P2-8 | JSON 请求体允许尾随 JSON | `readJSON` 只 Decode 第一个值，不检查第二个 JSON 值或尾随垃圾。 |

## 建议修复顺序

1. 先处理 P0-1 到 P0-4：防止真实出口泄露、敏感文件泄露、账号映射误删与删除后复活。
2. 再处理 P1-1 到 P1-7：保证 token 更新、隧道传输、Generic 配置与同步源生命周期正确。
3. 最后处理 P2：完善退出登录、开发环境、URL 编码、分页、平台映射与测试稳定性。

## 备注

- `go test ./...` 和 `go vet ./...` 通过，只说明普通路径大体可编译和运行；不能覆盖配置失效、并发删除、失败同步和重复运行污染等问题。
- 本报告只描述复核时已经确认的问题，不代表代码库不存在其他问题。
- 若开始修复，应先为每个 P0/P1 项补最小回归测试，再改实现，避免修复后再次回归。
