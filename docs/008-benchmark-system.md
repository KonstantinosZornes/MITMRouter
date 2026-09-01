# MITMRouter 性能测试怎么测

## 先说结论

这套 benchmark 只回答两件事：

1. MITMRouter 转发请求时有多快；
2. 请求和响应经过 MITMRouter 转发后，有没有被改坏、丢掉、串给别的请求。

测试不访问公网，也不连接真实住宅出口。上游只启动一个本地的虚拟上游出口，但它使用真实的 HTTP、HTTPS、CONNECT、TLS 和 HTTP/2 协议。

**上游是 HTTP 还是 HTTPS，不是本次 benchmark 要比较的内容。**选一种固定配置跑即可，建议用 HTTP 上游；HTTPS 上游另做连通性 smoke test，不混进性能结果。

## 1. 测试到底怎么走

```text
测试客户端
    │
    │ ① HTTP 裸请求
    │ 或
    │ ② HTTPS + CONNECT
    ▼
MITMRouter（被测）
    │
    │ 发给本地虚拟上游出口
    ▼
benchmark（测试上游）
    │
    │ 不再连接任何目标站
    │ 直接返回固定格式的测试响应
    ▼
测试客户端
```

`benchmark` 不是一个普通的 `httptest` handler。它要真正监听 TCP 端口，并按真实上游出口的方式接收请求：

- HTTP 裸请求：接收 MITMRouter 发来的绝对式 HTTP 请求；
- HTTPS 请求：接收 MITMRouter 发来的 `CONNECT`，回 `200`，然后在隧道里完成 TLS + HTTP/2；
- 收到请求后，不再拨号目标站，不访问公网；
- 直接读取请求，检查内容，然后返回测试响应。

这样能测到真实的上游转发链路和粘滞凭据处理，同时不会把公网延迟、住宅出口质量、目标站响应速度混进结果。

## 2. 只测两个场景

| 场景 | 客户端怎么访问 | 测什么 |
|---|---|---|
| HTTP 裸连 | 向接入口发送 HTTP 绝对式请求 | 明文请求转发速度和数据完整性 |
| HTTPS 连接 | 先发 CONNECT，再走 MITM TLS + HTTP/2 | CONNECT、TLS 解密、重加密、HTTP/2、数据完整性 |

两种场景都使用同一个 `benchmark` 测试上游。不要再把“HTTP 上游”和“HTTPS 上游”拆成两套性能结论。

建议每种场景都跑以下三类测试：

- **正确性小测**：少量请求，逐条检查所有内容；适合 CI；
- **性能测试**：固定 64 KiB 请求体和响应体，测吞吐和延迟；
- **粘滞测试**：大量不同 Marker，统计同 Marker 是否稳定、不同 Marker 是否撞 ID。

## 3. 每个请求和响应是什么样

### 请求

每个请求都使用固定的测试地址和测试头：

```text
POST /v1/responses?run=<本次运行ID>&seq=<序号>
Host: benchmark.test
Authorization: Bearer bench-marker-<Marker>
Content-Type: application/octet-stream
Content-Length: 65536
X-Bench-Run: <本次运行ID>
X-Bench-Seq: <序号>
X-Bench-Contract: no-rewrite-v1
```

请求体固定为 **64 KiB（65536 字节）**。它看起来像随机数据，但实际上是根据 `runID + seq` 生成的可重复伪随机数据：

```text
request_body = deterministicBytes("request" + runID + seq, 65536)
```

不用 `crypto/rand`，这样同一个请求每次都能生成完全一样的 body，出了问题可以重现。

### 响应

`benchmark` 收到请求后：

1. 先把请求体完整读完；
2. 检查 URL、Host、业务 headers 和请求体；
3. 生成对应的 64 KiB 响应体；
4. 返回响应及校验信息。

```text
response_body = deterministicBytes("response" + runID + seq, 65536)
```

响应体是原始二进制，不套 JSON、不 Base64、不 gzip。响应头至少包括：

| 响应头 | 含义 |
|---|---|
| `Content-Type` | 固定为 `application/octet-stream` |
| `Content-Length` | 固定为 `65536` |
| `X-Bench-Request-Bytes` | 实际收到的请求体大小 |
| `X-Bench-Request-SHA256` | 实际收到的完整请求体 hash |
| `X-Bench-Response-Body-SHA256` | 完整 64 KiB 响应体 hash |
| `X-Bench-Sticky-ID` | 从 MITMRouter 上游粘滞 URL 中解析出的 session/sid |
| `X-Bench-Received-At` | 测试上游收到请求的时间戳 |
| `X-Bench-Run` / `X-Bench-Seq` | 用来把响应和请求对应起来 |

这些是 `benchmark` 自己生成的测试响应字段，不是修改真实外部服务的响应。生产代码不应为了 benchmark 改写外部请求或响应。

## 4. 粘滞 ID 怎么来

这是本测试最重要的一点：

> **粘滞 ID 必须从 MITMRouter 实际发给上游的粘滞会话 URL 里解析出来，不能由 benchmark 另造一个 ID。**

流程如下：

```text
Marker
  ↓
MITMRouter 生产代码推导 account
  ↓
生产代码把 account 写进上游 URL 的用户名
  ↓
benchmark 收到真实 Proxy-Authorization
  ↓
按平台真实 URL 语法解析 session/sid
  ↓
返回 X-Bench-Sticky-ID
```

`benchmark` 要做的不是看测试参数，而是看它实际收到的认证信息：

1. 解码 `Proxy-Authorization: Basic ...`；
2. 取出真实用户名和密码；
3. 3. 按当前测试上游的 scheme 和地址重建收到的粘滞会话 URL；
4. 用平台对应的规则解析 session ID；
5. 将解析出的 ID 放入 `X-Bench-Sticky-ID`。

格式不对、字段不存在或字段出现多次，都必须报错，不能猜一个 ID，也不能回退成随机值。

### 各平台解析规则

| 平台 | 解析哪里 |
|---|---|
| DataImpulse | 用户名 `__` 后的参数区，必须有且只能有一个 `sessid.<id>` |
| Decodo | 用户名扁平参数里的 `session-<id>`，必须唯一 |
| 1024proxy | 用户名扁平参数里的 `sid-<id>`，必须唯一 |
| Resin | 用户名 `Platform.<id>` 中第一个 `.` 后面的 `<id>` |
| Generic | 必须给出明确的反解析规则；不能猜任意模板 |

解析出来的 ID 必须同时出现在：

- `benchmark` 的内存事件记录；
- 返回给客户端的 `X-Bench-Sticky-ID`；
- 最终 benchmark 结果中的统计数据。

日志和结果不能保存密码、Bearer 或完整 `Proxy-Authorization`。

## 5. 怎么检查数据没有被改坏

### 请求检查

`benchmark` 和客户端都要检查：

- URL、path、query、Host 完全一致；
- 业务 headers 完全一致；
- `Proxy-Authorization` 只能出现在 MITMRouter 与上游出口之间这段转发链路上，不能跑进业务请求；
- 请求体必须逐字节一致；
- 请求体大小必须是 `65536`；
- 请求体 SHA-256 必须一致。

HTTP 裸连在线路上是绝对式 URL，HTTPS 隧道内是 origin-form，这是两种隧道形态在协议上本来就不同，不算改写业务 URL。

### 响应检查

客户端收到响应后，必须：

1. 检查 status；
2. 检查所有约定响应头；
3. 把响应体从头到尾读完；
4. 按收到的完整业务 body 重新计算 SHA-256；
5. 和 `X-Bench-Response-Body-SHA256` 比较；
6. 再和根据 `runID + seq` 生成的期望 64 KiB body 逐字节比较。

这里检查的是**整个响应体的 hash**，不是前几个字节，也不是每个分块的 hash。如果底层使用 chunked transfer，hash 只计算拼出来的业务 body，不包含 chunk size 或其他传输编码。

同时要求：

- 每个 seq 恰好收到一次响应；
- 没有丢请求、重复请求或响应串给别的请求；
- 客户端完成数 = `benchmark` 收到的业务请求数；
- 正常场景下，成功审计记录数也必须相等。

任意一项不一致，都算 `integrity_failure`，不能只算作普通网络错误。

## 6. 怎么检查粘滞是否正常

分两种情况看重复，不能混在一起：

### 同一个 Marker

同一个 Marker 多次请求，解析出的 sticky ID 应该一直相同：

```text
same_marker_stability_rate = 保持相同 ID 的请求数 / 同 Marker 请求总数
```

期望值：**100%**。

### 不同 Marker

不同 Marker 得到同一个 ID，才算碰撞：

```text
cross_marker_collision_rate = 发生相同 ID 的不同 Marker 组 / Marker 组总数
```

建议使用至少 10,000 个不同 Marker。默认 `sid_len=16` 时，ID 是 16 位十六进制数，也就是 64 bit；随机碰撞的理论概率约为 `2.7e-12`，正常期望碰撞数为 0。

如果把 `sid_len` 降到 8，ID 只有 32 bit，10,000 个 Marker 的碰撞概率约 1.16%，这时不能再把“碰撞很低”当作验收标准。因此基准测试固定使用 `sid_len >= 16`。

## 7. 性能看哪些数字

固定 64 KiB 请求和响应后，每个请求都先校验数据，再计入性能结果。主要看四个数字：

- **首字节时延（TTFB）**：从客户端开始发请求，到 `client.Do` 收到响应头的时间。它反映整条转发链路多久开始回数据；
- **平均时延**：从开始发请求，到完整读完并校验 64 KiB 响应体的平均时间；
- **最大时延**：同一组请求里，完整读完并校验响应体的最慢一次；
- **IOPS**：每秒完成且校验通过的请求数。这里 1 个 I/O 是“写入 64 KiB 请求体 + 读回 64 KiB 响应体”的完整操作，不是磁盘 I/O。

同时记录：

- TTFB 平均值和最大值；
- 请求/响应字节数，确认确实传输了完整 64 KiB；
- 网络错误、协议错误和数据完整性错误；
- 上游 CONNECT 数，观察连接池是否复用；
- 可选的 CPU、内存、goroutine、FD、GC 和审计队列。

并发固定跑四档：

```text
1 → 8 → 64 → 256
```

每档先用一个请求预热，再连续测 2 秒；整个命令重复 3 次。输出示例：

```text
BenchmarkIndependentBenchmark/http/conc-64-8
  64  31.2 ms/op  2.10 avg-ttfb-ms  2.86 avg-lat-ms
      4.91 max-ttfb-ms  7.34 max-lat-ms  350.2 iops  0 errors
```

实际以 `avg-ttfb-ms`、`avg-lat-ms`、`max-ttfb-ms`、`max-lat-ms` 和 `iops` 为准；`ns/op` 是 Go benchmark 的内部平均值，不能代替上面四项。结果按同一机器、同一 Go 版本比较：

- IOPS 下降超过 10%，报警；
- 平均或最大时延明显上升时，先看 TTFB 是否一起上升；
- 数据完整性失败，直接失败。

这些阈值用于发现回归，不代表所有机器都必须达到某个固定 IOPS。

## 8. 测试代码怎么放

测试上游是仓库根目录的独立程序，不放在 `internal`：

```text
benchmark/
  main.go              # 独立的本地上游：HTTP 裸连、CONNECT、TLS、HTTP/2
  main_test.go         # URL 解析、64 KiB、完整 hash、HTTP/HTTPS 协议测试
  README.md            # 单独启动和证书说明

internal/server/
  benchmark_server_integration_test.go
                       # 明确启动 benchmark，穿过真实 MITMRouter
  benchmark_server_bench_test.go
                       # 多并发档位，输出 TTFB、平均/最大时延、IOPS
```

这样分工很直接：`benchmark` 负责“像上游一样收请求、校验、回响应”；`internal/server` 负责证明 MITMRouter 能把 HTTP 和 HTTPS 请求正确送到它那里，并测量客户端看到的结果。性能 benchmark 已经以这个独立 server 为目标，不再新建 `internal/benchkit`。

## 9. 怎么运行

日常测试不自动跑长 benchmark，必须显式打开。独立的 `benchmark` 会由测试自动编译并启动，所以不需要手工再开一个窗口：

```bash
# 现有 HTTP 微基准
go test ./internal/server -run '^$' \
  -bench BenchmarkForwardPipeline -benchtime 2s -count=3

# 两个客户端场景的小规模正确性检查：HTTP 裸连 + HTTPS CONNECT/HTTP2
MITMROUTER_RUN_BENCHMARK=1 GOCACHE="$PWD/.gocache" \
  go test ./internal/server \
  -run '^TestIndependentBenchmarkThroughMITMRouter$' -count=1 -v

# 真正的性能 benchmark：两个场景、四档并发，各测 2 秒，重复 3 次
MITMROUTER_RUN_BENCHMARK=1 GOCACHE="$PWD/.gocache" \
  go test ./internal/server -run '^$' \
  -bench '^BenchmarkIndependentBenchmark$' -benchtime 2s -count=3 -v
```

这里不按上游是 HTTP 还是 HTTPS 拆性能结果；当前 fixture 固定用 HTTP 上游。HTTPS 上游如果需要验证，只做单独 smoke test。

性能输出由 Go benchmark 直接给出 `ns/op`，并额外给出 `avg-ttfb-ms`、`max-ttfb-ms`、`avg-lat-ms`、`max-lat-ms`、`iops` 和错误数。`iops` 指每秒完成的完整请求/响应操作，不是磁盘 I/O。每条请求仍会完整校验 URL、headers、64 KiB body、完整响应体 hash 和实际解析出的 sticky ID；任何完整性错误都会让 benchmark 失败。

`artifacts/bench/` 是后续扩展 JSON 输出时的本机原始结果目录，不提交到 Git。

## 10. 安全边界

- 产品默认的 `BlockPrivateTargets=true` 不得修改；benchmark 只在自己的测试快照里显式允许 loopback；
- 同时保留默认安全配置下访问私网目标应返回 `403` 的对照测试；
- 所有监听使用 `127.0.0.1:0`，避免固定端口冲突；
- HTTPS 测试使用临时 CA 和测试证书；禁止 `InsecureSkipVerify`；
- `benchmark` 不拨号任何目标地址；
- 请求和响应只用确定性合成数据，不读真随机；
- benchmark 只检查并验证生产请求/响应，不修改生产代码对外转发的 URL、headers 或 body。

一句话总结：**用真实的 HTTP/HTTPS 上游转发协议跑两条客户端链路，用固定 64 KiB 请求/响应和完整 body hash 检查数据，用真实粘滞 URL 解析出的 session/sid 检查粘滞系统；全程不访问真实上游。**
