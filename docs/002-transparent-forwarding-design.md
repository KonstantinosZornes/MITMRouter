# 透明转发重构设计

## 先说结论

这次重构只解决一个问题：**MITMRouter 做内部路由和审计，但不把自己的东西带进外部 HTTP 请求，也不改外部 HTTP 响应。**

请求 ID 只存在于服务端内部，用来串起日志、审计和 trace；它不会变成 HTTP 头。

对客户端和目标站可见的业务内容，按下面的规则处理：

- 不添加、删除或改写请求 URL；
- 不添加、删除或改写请求头；
- 不添加、删除或改写响应头；
- 不改请求体和响应体；
- 不因为审计、trace 或 request ID 而修改请求/响应。

这里的“透明”指 HTTP 语义透明。HTTP/1.1 和 HTTP/2 之间不可避免会有协议栈自己的 framing 差异，不能承诺 TCP 字节级完全相同。

## 1. 为什么要改

当前使用 `net/http/httputil.ReverseProxy` 转发。即使给它一个空的 `Rewrite`，标准库仍会替转发器做一些事情：

- 删除连接级和 hop-by-hop 请求头；
- 处理 `X-Forwarded-*`；
- 清理部分 query 参数；
- 没有 `User-Agent` 时可能补上 Go 默认值；
- 处理响应头、Trailer 和响应流的方式不完全由本项目控制。

这些默认行为对普通反代通常合理，但不符合本项目现在的要求：本服务只负责 TLS 终结、身份识别、选上游和转发，不能擅自改变外部请求/响应。

另外，曾经把 `X-Request-Id` 写入响应头，造成了内部审计信息泄露到客户端；内层 HTTP 请求还曾经继承 CONNECT 的 context，导致同一条长连接内多个请求复用同一个 ID。

## 2. 重构后的模块与接口

新增一个私有的“转发模块”作为清晰的 seam。调用方只需要提供已经完成路由选择的请求，模块负责一次转发和流式回传。

### 2.1 请求副本

```go
func cloneForwardRequest(r *http.Request, scheme, host string, ctx context.Context) *http.Request
```

规则：

- 原始 `r` 永远不改；
- 通过 `r.Clone(ctx)` 创建私有副本；
- 保留 method、path、RawPath、query、headers、body 和 trailers；
- 只在副本上设置 `URL.Scheme`、`URL.Host` 和空的 `RequestURI`，满足 `http.Transport` 的拨号要求；
- 如果客户端没有 `User-Agent`，在副本中设置空值，阻止 Go 自动发送 `Go-http-client/1.1`；外部仍然看不到新增的 User-Agent。

这三个 URL 字段是 Transport 的内部路由字段，不会改变客户端请求的 path/query，也不会改变审计里记录的目标。

### 2.2 一次 RoundTrip

```go
func (s *Server) roundTripAndRelay(w *respRec, out *http.Request)
```

流程：

1. 调用现有共享 `*http.Transport.RoundTrip(out)`；
2. 出错且没有响应时，走统一的本地错误处理；
3. 成功拿到响应后，原样复制状态和响应头；
4. 用现有诊断 reader 和 trace reader 包住响应体，但只观察字节，不改字节；
5. 逐块写回客户端，SSE/流式响应每块立即 flush；
6. 响应结束后传递 Trailer；
7. 上游的 `103 Early Hints` 等 1xx 响应先于最终响应原样回传；
8. 上游 `101 Switching Protocols` 时劫持客户端连接并双向复制升级后的字节流；
9. 响应体读取失败或客户端写失败，只记录 `internal_error`，不把已经提交的目标响应改成 502。

### 2.3 本地失败

```go
func (s *Server) writeForwardFailure(w *respRec, r *http.Request, err error)
```

只有在目标站/上游还没有返回 HTTP 响应时，才生成本地响应：

- 上游配置错误；
- DNS、拨号、超时、TLS、EOF；
- 上游出口 CONNECT 被拒绝；
- 私网目标被本地安全策略拒绝。

本地失败仍可以返回标准 `502` 或 `403`，因为此时没有目标站响应可透传。审计使用 `status=0` 加 `internal_error` 区分它们。

## 3. request_id 规则

### 3.1 只存在于服务端

删除所有 `X-Request-Id` 的 HTTP 读写：

- 不写入客户端响应头；
- 不写入目标站请求头；
- 不删除客户端原本带的同名请求头；
- 不删除目标站原本返回的同名响应头。

内部 ID 通过 Go context 传递：

```text
context
  ├── slog req_id
  ├── access_logs.req_id
  └── trace 的服务端关联信息
```

### 3.2 每个内层请求独立生成

CONNECT 只代表一条隧道，不能代表隧道里的每个 HTTP 请求。

TLS 解密后的 handler 必须保留 `net/http` 为当前 HTTP/2 stream 或 HTTP/1.1 请求创建的原始 context，尤其不能丢掉它的取消信号。

如果需要传递 CONNECT 阶段已经完成的私网/DNS 校验结果，只复制那个内部值：

```go
innerCtx := r.Context()
innerCtx = context.WithValue(innerCtx, publicTargetResolutionKey{}, resolution)
r = r.WithContext(innerCtx)
r = withFreshInternalRequestID(r)
```

不能使用：

```go
r = r.WithContext(connectCtx)
```

否则会把 CONNECT 的 request ID 和取消语义带进所有内层请求。

## 4. 请求体身份解析

Grok OAuth refresh 请求需要从 body 读取身份。读取 body 本身不可避免，但不能改变发给目标站的字节。

当前 `snapshotBody` 会读取后替换 `r.Body`。重构后改成下面的接口：

```text
原始 body
  → 读取最多 64 KiB 的副本用于解析
  → 返回 credential 和一个“前缀 + 剩余原流”的回放 reader
  → 只把回放 reader 放到私有 outbound request 副本
```

要求：

- 原始 body 字节顺序不变；
- body 不重复、不丢失；
- 超过读取上限或解析失败也必须完整回放；
- body 的 Close 责任明确，不让上游失败导致客户端 body 被提前关闭；
- trace 仍然逐块记录，不把整个 body 额外复制到内存。

## 5. Header、URL、响应的具体规则

### 5.1 请求 Header

转发模块不主动增删业务请求头，也不清理客户端请求头。

`Proxy-Authorization` 不在本次重构中重新解释或改写。它属于代理认证层：

- 客户端给 MITMRouter 的认证按既有逻辑处理；
- MITMRouter 到上游出口的 CONNECT/SOCKS5 认证按既有上游配置处理；
- 不因为 request ID、审计或 trace 去复制、删除、替换它。

### 5.2 请求 URL

原始请求对象保持不变。Transport 副本只设置它建立连接所需的 scheme/host，并清空 `RequestURI`。

path、RawPath、RawQuery、ForceQuery 等业务 URL 信息不重写、不清理。

### 5.3 响应 Header

目标站响应头逐项复制，不因为 request ID 或审计删除任何头。

目标站返回的同名 `X-Request-Id` 也不处理；本项目不使用它。

### 5.4 请求/响应 Body

- 请求 body 使用原始字节回放；
- 响应 body 逐块直传；
- trace/诊断 wrapper 只读观察，不替换内容；
- 不做完整响应缓存；
- SSE 保持渐进返回。

## 6. HTTP 协议的客观限制

“透明”不等于原始 TCP 数据逐字节复制。`http.Transport` 仍会根据实际连接协议执行标准操作：

- HTTP/2 不允许部分 HTTP/1.1 连接级字段；
- `RequestURI` 不会作为 origin server 请求行直接发送；
- chunked、Content-Length、Trailer 和 HTTP/2 framing 由协议栈管理；
- header 名称可能被标准库规范化；
- 经上游出口的 CONNECT 本身必须使用隧道协议的控制报文。

本项目保证的是：合法的端到端业务 URL、业务 headers、body、响应状态/headers/body 的语义不被 MITMRouter 主动改写。

## 7. 审计与错误

审计规则保持不变，但和 HTTP 响应严格分开：

| 情况 | status | internal_error |
|---|---:|---|
| 目标站真实返回 200/401/503 | 真实状态 | 空 |
| 响应头前拨号失败 | 0 | `dial` / `timeout` / `dns` 等 |
| 响应头前 EOF | 0 | `eof` |
| 已收到响应后 body EOF | 真实状态 | `upstream_response_eof` |
| 已收到响应后 body 读取失败 | 真实状态 | `upstream_response_read` |
| 客户端断开导致写失败 | 真实状态 | `downstream_write` |

审计和日志使用内部 request ID 关联，但不把 ID 放进 HTTP。

## 8. 测试要求

### 请求透明性

- 原始 request 的 URL、headers、body 在调用转发后保持不变；
- 目标站收到原始 method/path/query；
- 目标站收到客户端提供的普通 header；
- 没有 User-Agent 时不出现 Go 默认 User-Agent；
- 请求 body 字节完全一致。

### 响应透明性

- 目标站 status 原样返回；
- 普通 response headers 原样返回；
- 多值 header 原样返回；
- response body 原样返回；
- Trailer 原样返回；
- SSE 第一块及时到达。

### 内部 request_id

- 客户端响应中没有 `X-Request-Id`；
- 目标站请求中没有 MITMRouter 生成的 request ID；
- 客户端自行提供同名 header 时不被 MITMRouter 代码删除；
- 目标站自行返回同名 header 时不被 MITMRouter 代码删除；
- 同一 CONNECT 隧道内多个 HTTP 请求 ID 不同；
- 每条 HTTP/2 stream 的取消信号仍然有效。

### 错误

- 上游真实 4xx/5xx 不被改写；
- 本地转发失败仍返回标准本地错误，但审计记录 `status=0`；
- response body EOF/读取错误不改变已提交状态。

## 9. 修改顺序

1. 先删除 `X-Request-Id` 的响应头写入和相关测试；
2. 修正 TLS 内层请求的 context 继承和 request ID 生成；
3. 把 `Server` 的 `ReverseProxy` 替换为共享 `http.Transport`；
4. 抽出统一的本地错误处理和透明响应 relay；
5. 重构 body 身份解析的回放方式；
6. 补齐 URL/header/body/response/trailer/SSE/cancellation 测试；
7. 跑全量测试和前端构建；
8. 只提交本次相关文件，直接提交到 `main`。

## 10. 不在本次范围内

- 不改变上游平台凭据注入规则；
- 不改变入站认证用户名密码规则；
- 不改变私网目标安全策略；
- 不改变审计字段含义；
- 不改变客户端看到的目标站业务响应；
- 不引入 8xx/9xx 非标准 HTTP 状态码；
- 不修改与本重构无关的并行工作区文件。

## 11. 影响文件

预计涉及：

- `internal/server/ingress.go`：转发模块、内部 request ID、响应 relay；
- `internal/identity/body.go` / `internal/identity/resolver.go`：body 回放；
- `internal/server/*_test.go`：透明性、取消、流式和审计测试；
- `internal/api/api.go` 与其测试：删除管理接口返回 request ID；
- `internal/trace/trace.go`：仅在确认需要时调整，保持 trace 内容语义；
- 本文档：记录最终设计与测试约束。

## 12. 完成标准

满足以下条件才算完成：

- 代码中不再使用 `X-Request-Id`；
- 不再依赖 `httputil.ReverseProxy`；
- 内层请求每条生成独立服务端 request ID；
- 原始 request 不被改写；
- 目标站和客户端之间的合法业务内容保持透明；
- 全量 Go 测试通过；
- 前端构建通过；
- 只提交本次相关修改，且提交在 `main` 分支。

> 这份文档用来约束实现，不是说 HTTP 协议可以绕过它自己的版本和 framing 规则。
