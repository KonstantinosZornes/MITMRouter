# benchmark

这是给 MITMRouter 做性能测试用的本地“假上游”。

它看起来像一个标准的上游出口：能接收普通 HTTP 转发请求，也能接收 `CONNECT`。区别是它**永远不会连接目标地址或公网**。收到请求后，它直接检查请求内容，并返回固定的测试响应。

## 它测到什么

```text
客户端 → MITMRouter → benchmark → 客户端
```

- HTTP：MITMRouter 必须发出真实的绝对式上游请求；
- HTTPS：MITMRouter 必须发出真实 `CONNECT`，然后在隧道中完成 TLS 和 HTTP/2；
- 上游认证：server 从实际收到的 `Proxy-Authorization` 重建粘滞会话 URL，再按平台格式解析出 sticky ID；
- 数据完整性：请求和响应都是可重复的 64 KiB 数据，会检查完整 body hash。

它不拨号任何目标地址，因此不会把公网、住宅出口或模型服务的波动算进性能结果。

## 运行

HTTP 裸连 benchmark 只需要启动：

```bash
go run ./benchmark \
  -addr 127.0.0.1:18080 \
  -platform dataimpulse
```

支持的平台：

```text
dataimpulse | decodo | 1024proxy | resin | generic
```

`generic` 必须明确告诉 server 怎么从用户名反解析 sticky ID，例如：

```bash
go run ./benchmark \
  -addr 127.0.0.1:18080 \
  -platform generic \
  -generic-sticky-prefix 'user-sessid-'
```

## HTTPS CONNECT 场景

HTTPS 场景需要给 `benchmark.test` 一张测试证书。server 在收到 CONNECT 后回 `200`，随后在同一条隧道中用该证书完成 TLS 服务端握手。

```bash
go run ./benchmark \
  -addr 127.0.0.1:18080 \
  -platform dataimpulse \
  -tls-cert ./benchmark.test.pem \
  -tls-key ./benchmark.test-key.pem
```

证书的 SAN 必须包含 `benchmark.test`。这张证书只应在本机 benchmark 中使用；客户端和 MITMRouter 的测试 transport 都应显式信任它。不要使用 `InsecureSkipVerify`。

## 返回什么

每个通过检查的请求都返回原始 `application/octet-stream` 响应体，大小固定为 65536 字节，并带这些响应头：

| 响应头 | 说明 |
|---|---|
| `X-Bench-Request-Bytes` | 实际收到的请求体大小，必须为 65536 |
| `X-Bench-Request-SHA256` | 完整请求体 hash |
| `X-Bench-Response-Body-SHA256` | 完整 64 KiB 响应体 hash |
| `X-Bench-Sticky-ID` | 从真实收到的粘滞会话 URL 解析的 session/sid |
| `X-Bench-Received-At` | server 收到请求的时间 |
| `X-Bench-Run` / `X-Bench-Seq` | 原样回显，用于逐请求对应 |

请求格式不对、body 不对、粘滞 URL 无法解析时返回 `400`。这不是普通性能错误，而是数据一致性失败。

## 测试

```bash
# server 自己的协议测试
GOCACHE="$PWD/.gocache" go test ./benchmark

# 真的穿过 MITMRouter：HTTP 裸连 + HTTPS CONNECT/HTTP2
MITMROUTER_RUN_BENCHMARK=1 GOCACHE="$PWD/.gocache" \
  go test ./internal/server \
  -run '^TestIndependentBenchmarkThroughMITMRouter$' -count=1 -v

# 压测：server 会被自动编译并启动，不需要手工启动它
MITMROUTER_RUN_BENCHMARK=1 GOCACHE="$PWD/.gocache" \
  go test ./internal/server -run '^$' \
  -bench '^BenchmarkIndependentBenchmark$' -benchtime 5s -count=3 -v
```

测试覆盖：平台 sticky ID URL 解析、固定 64 KiB 数据、HTTP 绝对式请求、HTTPS CONNECT + HTTP/2、完整响应体 hash，以及请求通过真实 MITMRouter 后是否仍完整。

## 不会记录什么

server 不写完整 Bearer、密码或完整 `Proxy-Authorization`。benchmark 结果只应保存解析出的测试 sticky ID 和聚合指标。
