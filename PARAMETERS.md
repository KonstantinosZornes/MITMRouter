[English](PARAMETERS.en.md)

# MITMRouter 参数填写完全指南

> 这是一份面向实际部署者的参数说明。目标不是解释代码，而是把每个输入框、每个启动参数、每个取值怎么填、填错会发生什么都说清楚。
>
> 适用范围：当前仓库的 `main` 分支实现。

---

## 0. 先看结论：到底在哪里填参数

MITMRouter **没有 YAML、`.env`、JSON 配置文件**。运行期配置最终保存在数据目录里的 SQLite 文件：

```text
<data 目录>/router.db
```

参数分成五类：

| 参数位置 | 典型参数 | 是否保存到数据库 | 什么时候生效 |
|---|---|---:|---|
| 启动命令 | `-data`、`-addr`、`-admin-addr`、`-trace-file`、`-log-level` | 否 | 启动进程时；改完要重启 |
| 管理台「基础设置」 | TLS 路径、入站认证、Marker 规则、粘滞、ACL、维护设置 | 是 | 大多数保存后立即生效；TLS 路径改动要重启 |
| 管理台「上游出口」 | 名称、平台、`base_url`、Generic 模板、启用状态 | 是 | 保存后热生效 |
| 管理台「账号管理」 | 同步源、账号、AT/RT、账号出站绑定 | 是 | 保存后热生效；同步源按间隔运行 |
| 管理台「访问审计 / 更新记录」 | 时间范围、关键词、分页、筛选条件 | 否（只是查询条件） | 点击查询或自动刷新时生效 |

最重要的区别是：

- **监听 IP 和端口只能通过启动参数改**，不能在管理台里改；
- **上游凭据、管理员口令哈希、MITM 根 CA 私钥都在 `router.db` 里**，这个文件必须按密钥材料保护；
- 管理台显示的上游密码、同步源 API Key、Generic 密码是掩码，不要把 `____` 当成真实密码填写；
- 默认白名单为空时，HTTPS 目标默认会被 MITM 解密，所以客户端通常必须安装管理台下载的根 CA；
- 本项目的配置文档只描述配置，不改变外部业务请求的 URL、请求头、响应头或响应体。

## 目录

- [1. 推荐的填写顺序](#1-推荐的填写顺序)
- [2. 运行命令行参数](#2-运行命令行参数)
- [3. 首次启动后必须知道的三个东西](#3-首次启动后必须知道的三个东西)
- [4. 基础设置逐项说明](#4-管理台基础设置逐项说明)
- [5. 上游出口参数](#5-上游出口参数nameplatformbase_urlinjectenabled)
- [6. 六种上游平台怎么填](#6-六种上游平台怎么填)
- [7. 账号管理：同步源参数](#7-账号管理同步源参数)
- [8. 账号管理：手动登记参数](#8-账号管理手动登记参数)
- [9. 账号 ↔ 出站绑定参数](#9-账号--出站绑定参数)
- [10. 账号平台与目标主机的对应关系](#10-账号平台与目标主机的对应关系)
- [11. 访问审计页面的查询参数](#11-访问审计页面的查询参数)
- [12. 更新记录页面的查询参数](#12-更新记录页面的查询参数)
- [13. 管理 API 自动化填写参考](#13-管理-api-自动化填写参考)
- [14. 构建脚本参数](#14-构建脚本参数不是运行配置)
- [15. 测试工具环境变量](#15-测试工具环境变量不是生产服务配置)
- [16. 常见配置组合](#16-常见配置组合)
- [17. 常见错误和排查顺序](#17-常见错误和排查顺序)
- [18. 不要忽略的运行限制](#18-不要忽略的运行限制)
- [19. 参数来源索引](#19-参数来源索引)
- [20. 最终检查清单](#20-一份可以照着做的最终检查清单)

---

## 1. 推荐的填写顺序

不要一开始就把所有开关都改一遍。推荐按下面顺序配置。

### 1.1 本机使用的最小配置

```text
1. 启动程序
2. 记下首次启动时只打印一次的管理员口令
3. 打开管理台
4. 下载 ca.pem，并安装到客户端信任区
5. 新增一个上游出口
6. 将它设为默认上游
7. 将客户端代理指向接入地址
8. 用“测试上游”和实际请求验证出口 IP
```

最小启动命令：

```bash
./mitmrouter -data ./data
```

默认地址：

```text
客户端接入口：127.0.0.1:55666
管理台：      127.0.0.1:55667
```

### 1.2 公网或局域网部署的推荐顺序

如果接入口或管理台要监听非回环地址，不要只改成 `0.0.0.0` 就结束。推荐顺序是：

```text
1. 先准备防火墙规则
2. 为接入口配置 listen_tls_cert / listen_tls_key
3. 为管理台配置 admin_tls_cert / admin_tls_key
4. 配置 listen_auth（接入口 Basic 认证）
5. 限制 admin-addr 的来源，只允许运维网络访问
6. 重启，确认两个端口都进入预期的 HTTP/HTTPS 模式
7. 再把客户端或容器接入
```

管理台使用自己的会话 Cookie 认证；`listen_auth` 是**客户端接入口**的认证，不等于管理台登录口令。公网部署时两者都要考虑。

---

## 2. 运行命令行参数

运行参数来自 `cmd/mitmrouter`。完整形态：

```bash
./mitmrouter \
  -data ./data \
  -addr 127.0.0.1:55666 \
  -admin-addr 127.0.0.1:55667 \
  -log-level info
```

`-trace-file` 只建议排障时临时加上，详见后文。

### 2.1 `-data`：数据目录

| 项目 | 说明 |
|---|---|
| 参数名 | `-data` |
| 默认值 | `./data` |
| 类型 | 目录路径字符串 |
| 是否必填 | 否 |
| 生效时间 | 每次启动时 |

**应该填什么：**

填一个程序可以创建、读写，并且由本服务独占或主要使用的目录。例如：

```bash
-data ./data
-data /opt/mitmrouter/data
-data /var/lib/mitmrouter
```

相对路径是相对于进程当前工作目录，不是相对于二进制文件所在目录。systemd 中建议使用绝对路径。

**目录里会有什么：**

```text
/opt/mitmrouter/data/
└── router.db       # SQLite 主库，包含配置、密钥、CA、审计和账号映射
```

SQLite 运行期间还可能有：

```text
router.db-wal
router.db-shm
```

程序启动时会自动：

- 创建不存在的数据目录；
- 把数据目录权限收紧到 `0700`；
- 把 `router.db` 及 WAL/SHM 文件权限收紧到 `0600`；
- 创建表并执行幂等结构补全。

**不要这样填：**

```bash
-data ./router.db       # 不要把文件名当成目录
-data /tmp               # 不建议把生产密钥放在临时目录
```

换一个全新的 `-data` 目录等价于创建一个新实例：管理员口令、根 CA、盐和全部数据库状态都会重新生成。旧数据目录不会自动合并进新目录。

**备份建议：**

`router.db` 包含 CA 私钥、上游真实凭据和账号映射，不能当普通日志复制到公开位置。备份前最好停止服务，或者使用 SQLite 感知的备份方式；不要只随意复制正在写入的 WAL 数据库。

### 2.2 `-addr`：客户端接入口监听地址

| 项目 | 说明 |
|---|---|
| 参数名 | `-addr` |
| 默认值 | `127.0.0.1:55666` |
| 类型 | `host:port` |
| 是否写入数据库 | 否 |
| 生效时间 | 每次启动时 |

这个端口给 curl、SDK、容器或其他客户端当 HTTP 代理使用。它承载：

- HTTP 绝对式请求；
- HTTPS `CONNECT`；
- CONNECT 后的本地 MITM 解密；
- 入站 `Proxy-Authorization: Basic ...` 认证（如果配置了 `listen_auth`）。

**常见填写：**

```bash
# 只允许本机访问，最安全的默认方式
-addr 127.0.0.1:55666

# 监听所有 IPv4 网卡；只有在配好防火墙、TLS 和认证后才建议使用
-addr 0.0.0.0:55666

# 等价于监听所有本地地址之一，适合简单场景，但建议显式写清楚
-addr :55666

# IPv6 全地址监听，IPv6 地址要用方括号包住
-addr '[::]:55666'
```

**格式要求：**

- 必须能解析为 `host:port`；
- 端口必须是 `1` 到 `65535`；
- 不能和 `-admin-addr` 完全相同；
- 改完命令后必须重启进程。

**监听地址与协议的关系：**

`-addr` 只决定监听在哪里，不决定 HTTP 还是 HTTPS。接入口是否 TLS-only 由管理台的：

```text
listen_tls_cert
listen_tls_key
```

决定：

- 两个路径都为空：接入口接受明文 HTTP；客户端代理 URL 使用 `http://...`；
- 两个路径都填好：接入口强制 HTTPS-only；客户端代理 URL 使用 `https://...`；明文连接会在 TLS 握手阶段被拒绝。

**风险提醒：**

当 `-addr` 绑定非回环地址且 `listen_auth` 为空时，程序会记录风险警告。此时任何能访问端口的机器都可能把它当成代理使用。

### 2.3 `-admin-addr`：管理台监听地址

| 项目 | 说明 |
|---|---|
| 参数名 | `-admin-addr` |
| 默认值 | `127.0.0.1:55667` |
| 类型 | `host:port` |
| 是否写入数据库 | 否 |
| 生效时间 | 每次启动时 |

这个端口只承载：

- `/ui/` 管理台页面；
- `/api/*` 管理 API；
- `/metrics`（启用后，且仍需登录）。

客户端代理不能指向管理台端口。管理台端口收到 CONNECT 或绝对式代理请求会拒绝；接入口端口也不会把管理面能力当成代理请求提供。

**常见填写：**

```bash
# 只从本机浏览器访问
-admin-addr 127.0.0.1:55667

# 监听所有 IPv4 地址；必须配管理台 TLS，并用防火墙限制来源
-admin-addr 0.0.0.0:55667

# 仅监听某个内网地址
-admin-addr 192.168.1.10:55667
```

**与接入口的硬性要求：**

```text
-addr 不能和 -admin-addr 完全相同
```

如果管理台绑定非回环地址但没有配置 `admin_tls_cert` / `admin_tls_key`，程序会发出警告。管理台登录 Cookie 不应该通过不可信的明文网络传输。

管理台地址改动不会写入 `router.db`。如果 systemd 的 `ExecStart` 仍使用旧地址，网页里保存任何设置都不会改变监听位置，必须修改启动命令或 service 文件再重启。

### 2.4 `-trace-file`：明文请求/响应追踪文件

| 项目 | 说明 |
|---|---|
| 参数名 | `-trace-file` |
| 默认值 | 空字符串，关闭 |
| 类型 | 文件路径 |
| 生效时间 | 启动时打开文件 |
| 使用建议 | 只用于短期本地排障 |

例如：

```bash
-trace-file /tmp/mitmrouter-trace.log
```

启用后会追加记录：

- 请求方法和完整 URL；
- 全部请求头；
- 请求体；
- 响应状态；
- 全部响应头；
- 流式响应体的每个数据块。

它**不脱敏**，可能包含：

- API Key；
- Bearer Token；
- Refresh Token；
- 用户问题、模型输入和模型输出；
- Cookie；
- 其他业务秘密。

文件会以 `0600` 权限创建或收紧，默认追加而不是覆盖旧内容。排障结束后应：

```text
1. 停止或重启时去掉 -trace-file
2. 删除追踪文件
3. 根据组织安全要求清理备份、终端历史和日志采集副本
```

不要把它当作常规审计日志，也不要在生产环境长期打开。普通 `-log-level debug` 不等于开启明文追踪；只有明确填写 `-trace-file` 才会落请求体和响应体。

### 2.5 `-log-level`：运行日志级别

| 输入值 | 含义 |
|---|---|
| `debug` | 最详细，适合排障 |
| `info` | 默认，正常运行信息 |
| `warn` | 警告 |
| `warning` | `warn` 的兼容写法 |
| `error` | 只看错误 |

示例：

```bash
-log-level info
-log-level debug
```

比较时会忽略大小写并去掉两端空白。其他值会导致程序启动失败，例如：

```bash
-log-level verbose   # 不支持
```

程序日志会使用 Marker 的指纹而不是 Marker 明文，但不要因为这一点就把日志随便公开：日志仍可能包含目标主机、错误原因、上游名称、路径和拓扑信息。

运行程序时还可以使用标准帮助参数：

```bash
./mitmrouter -h
```

它只显示命令行帮助，不会修改任何配置。

---

## 3. 首次启动后必须知道的三个东西

### 3.1 一次性管理员口令

第一次使用一个全新的数据目录时，程序会生成随机管理员口令，并在控制台输出一次：

```text
Admin password: <随机字符串>
```

程序数据库只保存 bcrypt 哈希，明文不会再次从数据库恢复。请在第一次登录后立即修改口令。

管理员新口令的当前要求：

- 旧口令必须正确；
- 新口令至少 6 个字符；
- 修改成功后旧管理会话会失效，需要重新登录；
- 管理会话有效期是固定 7 天，目前不能在管理台调整。

### 3.2 MITM 根 CA 与监听 TLS 证书不是一回事

项目里有两类完全不同的证书：

| 证书 | 用途 | 在哪里配置 |
|---|---|---|
| MITM 根 CA（`ca.pem` / `ca.crt`） | 给客户端信任，用于本地签发目标站叶子证书 | 程序自动生成；管理台下载 |
| 接入口/管理台服务器证书 | 保护两个监听端口自身的 TLS | 基础设置页四个 TLS 路径 |

如果客户端没有安装 MITM 根 CA，访问被拦截解析的 HTTPS 目标时通常会报“自签证书”或“未知 CA”。

下载入口：

```text
GET /api/ca.pem   # PEM 文本格式
GET /api/ca.crt   # DER/CRT 格式，Windows 通常可直接导入
```

### 3.3 默认 ACL 会拦截所有 HTTPS 目标

默认值是：

```text
acl_whitelist = []
acl_blacklist = []
```

这表示不限制目标访问；对 HTTPS 目标仍是：

```text
白名单为空 + 黑名单未命中 = 允许访问并 MITM 解析
```

因此第一次验证前，通常要先下载并安装 `ca.pem`。如果只想允许少数域名访问，应填写白名单，见第 4.15 节。

---

## 4. 管理台「基础设置」逐项说明

管理台地址通常是：

```text
http://127.0.0.1:55667/ui/
```

如果管理台 TLS 已启用，则使用：

```text
https://<管理台主机>:<管理台端口>/ui/
```

基础设置中的 IP/端口提示是只读的：它们来自启动参数，不是可以填写的字段。

### 4.1 接入地址（`ingress_url`）——只读，不要填

管理台顶部显示的“接入地址”是根据以下信息动态生成的：

- `-addr` 的端口；
- 当前请求使用的主机名；
- 接入口 TLS 是否启用。

例如启动参数是：

```bash
-addr 0.0.0.0:55666
```

则页面可能显示：

```text
http://127.0.0.1:55666
```

或在 HTTPS 启用后显示：

```text
https://router.example.com:55666
```

它只是复制给客户端/SDK 使用的提示值，不能通过 `PUT /api/settings` 改变监听端口。

### 4.2 带认证地址（`ingress_url_auth`）——只读，不要填

当 `listen_auth` 已启用时，页面会显示一个把用户名和密码嵌入代理 URL 的地址，例如：

```text
http://proxy-user:proxy-pass@127.0.0.1:55666
```

它可以直接粘贴到支持代理 URL 的客户端配置中，但因为包含真实凭据：

- 不要截图外传；
- 不要提交到 Git；
- 不要贴到公开 issue；
- 不要放进不受控的命令历史或 CI 日志。

如果凭据中包含 URL 特殊字符，页面生成的地址会进行 URL 编码。更稳妥的做法是使用客户端单独的代理用户名/密码输入框。

### 4.3 `listen_auth`：客户端接入口 Basic 认证

#### 填写方式

页面把它拆成两个输入框：

```text
用户名：<user>
密码：  <pass>
```

底层保存形态是：

```text
user:pass
```

例如：

```text
用户名：router-client
密码：  一段随机长密码
```

等价于保存：

```text
router-client:<一段随机长密码>
```

#### 取值规则

| 输入 | 结果 |
|---|---|
| 用户名和密码都留空 | 关闭入站认证 |
| 用户名和密码都填写 | 开启入站认证 |
| 只填其中一个 | 页面阻止保存；API 返回校验错误 |
| 密码含冒号 | 协议上可作为密码的一部分；不要把冒号放在用户名里，避免阅读和 URL 编码混乱 |

空值适合纯本机回环监听：

```text
-addr 127.0.0.1:55666 + listen_auth 空
```

非回环监听建议一定开启：

```text
-addr 0.0.0.0:55666 + listen_auth 非空 + 防火墙 + 接入口 TLS
```

#### 请求如何认证

客户端必须在代理请求中发送：

```http
Proxy-Authorization: Basic base64("用户名:密码")
```

认证失败返回标准：

```text
407 Proxy Authentication Required
```

注意这是**接入口认证**。上游代理返回的 407 是另一件事，通常说明上游 `base_url` 的凭据错了。

#### 编辑时的显示

已保存的密码会在已登录的管理台中直接回显：

```text
用户名：router-client
密码：  <原来的密码>
```

可以直接修改密码，也可以复制到客户端的用户名/密码配置中。包含真实凭据的页面和接入地址不要截图外传、提交 Git 或贴到公开 issue。

### 4.4 `listen_tls_cert` / `listen_tls_key`：接入口 TLS 证书对

这是接入口服务器自身的 TLS 证书和私钥路径，不是 MITM 根 CA 路径。

建议填写绝对路径：

```text
证书 PEM：/opt/mitmrouter/certs/fullchain.pem
私钥 PEM：/opt/mitmrouter/certs/privkey.pem
```

也可以使用其他合法 PEM 证书，例如：

```text
/etc/letsencrypt/live/proxy.example.com/fullchain.pem
/etc/letsencrypt/live/proxy.example.com/privkey.pem
```

#### 必须成对

| 证书路径 | 私钥路径 | 结果 |
|---|---|---|
| 空 | 空 | 接入口明文 HTTP |
| 非空 | 非空 | 接入口 HTTPS-only |
| 非空 | 空 | 拒绝保存 |
| 空 | 非空 | 拒绝保存 |

保存时程序会尝试加载证书和私钥。以下问题会阻止保存：

- 文件不存在；
- 没有读取权限；
- PEM 格式不合法；
- 证书和私钥不匹配；
- 文件不是可解析的证书对。

证书已经过期或尚未到生效时间属于警告，不会因为时间状态阻止保存；但重启后客户端会遇到证书问题，应及时续期或修正。

#### 生效语义

- 改路径：保存成功后页面提示需要重启；重启前监听器仍使用旧路径；
- 路径不变、文件内容变化：程序会按文件修改时间自动热加载，通常约 60 秒内生效；
- 两项都填好后，该端口不再接受明文 HTTP。

#### 客户端地址变化

接入口 TLS 开启后，客户端代理配置从：

```text
http://127.0.0.1:55666
```

变为：

```text
https://127.0.0.1:55666
```

不要只在服务端填证书而继续让客户端使用 `http://`。

### 4.5 `admin_tls_cert` / `admin_tls_key`：管理台 TLS 证书对

填写方式与接入口证书对完全相同，但只作用于 `-admin-addr` 监听端口：

```text
管理台证书 PEM：/opt/mitmrouter/certs/fullchain.pem
管理台私钥 PEM：/opt/mitmrouter/certs/privkey.pem
```

可以和接入口共用一对证书，前提是证书的 SAN 覆盖实际访问的主机名；也可以使用两对不同证书。

#### 什么时候必须配置

- 管理台只在 `127.0.0.1` 使用：明文管理台可以接受，但仍建议通过 SSH 隧道或其他安全方式访问；
- 管理台绑定内网或公网地址：强烈建议配置；
- 管理员登录口令、会话 Cookie 需要跨机器传输：应使用 HTTPS。

改路径需要重启；同一路径下证书续期可热加载。管理台 TLS 和接入口 TLS 是两套独立开关，可以一个开启、另一个关闭，但生产环境通常应一起考虑。

### 4.6 `marker_path_parts`：Marker 路径片段，可选

#### 默认值

```json
[]
```

空数组表示：

```text
对所有请求路径应用 Marker 提取规则
```

这是推荐默认值，因为同一个账号访问不同 API 路径时仍使用同一个粘滞身份。

#### 怎么填

页面是可输入多个标签的选择框。每输入一个值后按回车添加，例如：

```text
/v1/
/chat/completions
/responses
```

每个片段必须以 `/` 开头：

```text
正确：/v1/
正确：/oauth/
错误：v1/
错误：chat
```

#### 匹配方式

这是**简单子串包含匹配**，不是正则表达式：

```text
只要 URL.Path 包含任意一个片段，就算命中
```

例如配置：

```text
/v1/
```

下面这些路径会命中：

```text
/v1/models
/v1/chat/completions
/proxy/v1/responses
```

下面这些不会命中：

```text
/models
/chat/completions
```

多个片段之间是 OR 关系，命中任意一个即可。查询字符串不属于 `URL.Path`，不要把 `?key=...` 写进路径片段。

#### 不建议过度限制

如果你填了：

```text
/chat/completions
```

同一个凭据访问 `/v1/models`、`/v1/embeddings` 或其他路径时可能无法提取到 Marker，继而落入无 Marker 兜底策略。除非你非常确定只需要某些路径，否则留空更不容易出问题。

#### 内置 AI 平台的例外

对代码能够识别的内置 AI 目标，系统会优先从固定的凭据载体读取平台账号映射；这条内置路径不依赖 `marker_path_parts`。自定义主机或未被内置平台识别的流量，才主要依赖这里的通用 Marker 规则。

### 4.7 `marker_headers`：Marker 请求头列表

#### 默认值

当前默认四个请求头：

```text
Authorization
x-api-key
api-key
x-goog-api-key
```

页面允许多选，也允许输入自定义头名。

#### 处理顺序

程序按列表顺序依次读取：

```text
第一个非空请求头的值 = Marker
```

因此如果一个请求同时有多个头，列表顺序会影响身份选择。一般把最稳定、最能代表账号的头放在前面。

#### Authorization 的特殊规则

只有下面这种形式会被当作 Marker：

```http
Authorization: Bearer sk-example
```

下面这些不会被通用 Marker 提取器使用：

```http
Authorization: Basic ...
Authorization: Token ...
Authorization: just-a-value
```

其他配置头（例如 `x-api-key`）读取其原值。

#### 自定义头示例

如果你的客户端使用：

```http
X-Workspace-Key: workspace-abc
```

可以在列表中添加：

```text
X-Workspace-Key
```

注意大小写通常不影响 HTTP 头查找，但建议按客户端实际名称填写，便于排查。

#### 不能留空

管理台保存时 Header 列表至少要有一项。若不希望某类默认头参与提取，应替换成自己的头，而不是把列表清空。

#### 内置 AI 载体说明

对于已识别的 AI 平台，账号映射还会检查：

```text
Authorization: Bearer ...
x-api-key: ...
api-key: ...
x-goog-api-key: ...
URL 查询参数 ?key=...
```

这是内置行为，不是通过 `marker_headers` 完全关闭的开关。不要误以为从页面删掉 `Authorization` 就能阻止已知平台使用它。

### 4.8 `hash_salt`：全局粘滞盐

#### 能不能直接填写

不能。页面只读显示，并提供：

```text
重置盐
```

首次启动时程序随机生成盐并保存。正常情况下不要改它。

#### 重置盐会发生什么

粘滞身份大致由下面的关系推导：

```text
account = SHA256(hash_salt + 身份输入) 的十六进制结果截断
```

按下“重置盐”并确认后：

- 所有 Marker 的派生会话身份改变；
- 所有映射账号的派生会话身份改变；
- 上游平台看到的 session/sid 会改变；
- 账号通常会重新分配出口 IP；
- 旧的粘滞关系不会继续保持。

这适合整体更换出口、处理供应商侧风控或迁移会话，不适合日常点击试验。页面会二次确认，就是因为它影响全体账号。

### 4.9 `sid_len`：派生会话 ID 长度

| 项目 | 值 |
|---|---|
| 默认值 | `16` |
| 当前有效范围 | `4` 到 `64` |
| 单位 | 十六进制字符数 |

例如 `sid_len=16` 时，派生值类似：

```text
8c12a0f4d7e91b20
```

它不是字节数，而是最终输出字符串长度。

**怎么选：**

- `16`：推荐默认值，足够区分常见规模的账号；
- `24` 或 `32`：账号规模较大，或供应商希望更长 session ID；
- `4`：只适合非常小的测试，不建议生产使用；
- `64`：使用完整 SHA-256 十六进制结果，通常没有必要。

**注意：**

当前代码实际按 `4–64` 校验。仓库中的部分旧设计文字曾出现 `8–32` 或 `8–64`，以当前页面和 API 的 `4–64` 为准。

改变 `sid_len` 会让身份字符串长度改变，因而可能让上游会话重新分桶；不要在高峰期频繁调节。

### 4.10 `session_ttl_min`：上游会话时长（分钟）

| 项目 | 值 |
|---|---|
| 默认值 | `0` |
| 页面可填范围 | `0–1440` |
| API 保存校验范围 | `0–10080` |
| 单位 | 分钟 |

`0` 的含义是：

```text
不主动干预上游 URL 中已有的 TTL 参数
```

大多数情况下先保持 `0`，确认上游能正常使用后，再根据供应商需求设置。

**重要：页面和 API 上限略有不同。**页面数字输入框当前上限是 `1440`，自动化 API 的后端校验允许到 `10080`。不建议为了填写更大的数绕过页面；各供应商自身的最大值更低，而且 Generic 模板可能会原样收到这个数。

#### 不同平台的实际行为

| 上游平台 | `session_ttl_min=0` | `session_ttl_min>0` |
|---|---|---|
| Decodo | 保留 `base_url` 现有的 `sessionduration` | 写入/替换 `sessionduration`，并限制到 `1–1440` |
| 1024proxy | 保留 `base_url` 现有的 `t` | 写入/替换 `t`，并限制到 `1–120` |
| DataImpulse | 不注入 TTL | 当前内置注入器仍忽略该设置；已有 `sessttl` 会保留 |
| Resin | 不涉及 TTL | 不涉及 TTL |
| Generic | 如果模板含 `{ttl_min}` 会因 0 被拒绝 | `{ttl_min}` 替换为当前整数 |
| plain | 无意义 | 忽略 |

例如填写 `30`：

- Decodo 会使用 `sessionduration-30`；
- 1024proxy 会使用 `t-30`；
- Generic 的 `{ttl_min}` 会替换成 `30`；
- DataImpulse 不会因为这个设置自动增加 `sessttl`。

#### 修改 TTL 的隐藏影响

如果已经保存了 Generic 模板：

```json
{"username_template":"{user}-session-{sid}-ttl-{ttl_min}"}
```

则不能把 `session_ttl_min` 改回 `0`，因为 `{ttl_min}` 在 0 时被禁止。程序会在保存设置时重新检查已有 Generic 条目，并拒绝产生不兼容的组合。

### 4.11 `salt_rotate_failure_threshold`：自动换出口失败次数

| 项目 | 值 |
|---|---|
| 默认值 | `2` |
| 有效范围 | `1–100` |
| 含义 | 同一身份连续遇到可轮换的上游不可用错误后，切换动态盐 |

它不是“任何 HTTP 错误出现几次就换出口”。只有这类情况才主要参与计数：

- 上游 TLS 握手或证书失败；
- 上游连接建立失败；
- 上游代理拒绝建立 CONNECT；
- 收到响应前对端 EOF / unexpected EOF；
- 其他被判定为当前出口不可用的传输层错误。

目标站正常返回的 `4xx` 或 `5xx` 是真实业务响应，不会被当作“换出口”依据；收到任何 HTTP 响应通常会清除此前连续失败计数。

#### 如何选

| 值 | 适合场景 | 代价 |
|---:|---|---|
| `1` | 供应商节点坏一个就立即逃生 | 短暂网络抖动也可能导致换出口 |
| `2` | 推荐默认，兼顾反应速度和稳定性 | 可能多失败一次 |
| `3–5` | 网络偶尔抖动、希望更稳 | 坏节点上会多等待几次 |
| 很大 | 仅适合明确希望保持会话的场景 | 真正坏节点恢复慢 |

达到阈值后，下一次请求使用新身份。换盐是按身份记录的，不是每次全局重置 `hash_salt`；不同 Marker/账号互不影响。

### 4.12 `default_upstream`：默认上游

#### 怎么填

从下拉框中选择一个已经新增的、**启用状态**的上游名称，例如：

```text
us-residential-1
```

也可以清空，表示没有默认上游。

#### 谁会使用它

默认上游用于：

- 有 Marker 但没有账号出站绑定的请求；
- 有 Marker，但账号映射未命中的请求；
- `no_marker_policy=default_session` 或 `client_ip_session` 时的无 Marker 请求；
- 没有更高优先级出站绑定的请求。

#### 校验与风险

如果填写了名称，保存时必须满足：

```text
该名称存在，并且对应条目 enabled=true
```

当前默认条目不能直接停用或删除，必须先切换默认值。若手工改库留下了悬空默认名称，程序不会偷偷直连，而会受控返回 502，避免意外暴露本机出口。

#### 清空后的语义

清空 `default_upstream` 后：

- 没有绑定、需要走默认路由的请求会按“无上游”处理，通常直连；
- `no_marker_policy=direct` 仍然直连；
- 已配置的账号出站绑定仍优先于默认值；
- 如果你的目标是“所有请求必须经过代理”，不要清空它。

### 4.13 `no_marker_policy`：没有 Marker 时怎么办

当前只有三个值：

| 值 | 页面名称 | 行为 |
|---|---|---|
| `default_session` | 固定身份 `default` | 所有没有 Marker 的请求共享一个默认会话身份 |
| `client_ip_session` | 按来源 IP | 使用客户端来源 IP 推导会话身份 |
| `direct` | 直连不走上游 | 不经过配置的上游代理，直接访问目标 |

#### `default_session`（推荐默认）

适合：

- 只有少数请求带 API Key；
- 希望无凭据请求仍经同一个上游会话；
- 本机代理只服务一个主要客户端。

缺点是所有无 Marker 请求共享同一个身份。如果无 Marker 流量来自多个账号，不能靠它区分账号。

#### `client_ip_session`

适合：

- 多台客户端接入；
- 客户端没有可用的 API Key，但不同来源 IP 能代表不同使用者。

注意：如果多个设备经同一个 NAT、Docker 网桥或反向代理接入，它们可能共享同一个来源 IP，也就共享一个会话身份。

#### `direct`

适合：

- 明确希望没有身份的普通流量不使用住宅出口；
- 只让带 Marker 的流量经过上游；
- 调试路由。

它只对“没有 Marker”的请求生效。即使选择 `direct`，`block_private_targets=true` 仍会阻止访问本机、私网和云元数据等危险目标。

### 4.14 `block_private_targets`：禁止访问本机和内网

默认值：

```text
true
```

建议生产环境保持开启。

开启时，程序会拒绝：

- `localhost`；
- `127.0.0.0/8` 等回环地址；
- RFC1918 私网地址；
- IPv6 私有/本地地址；
- link-local 地址；
- 未指定地址；
- 多播地址；
- `100.64.0.0/10` CGNAT 地址；
- 解析后包含私有地址的域名；
- 云厂商常见的 link-local 元数据地址。

它不仅检查用户填写的字面 IP，还会检查目标域名的 DNS 结果，并要求解析出的地址全部是公网地址，以减少用域名绕过检查的机会。

**这个开关与 ACL 是两层独立检查：**

- ACL 先决定目标是否允许访问；
- `block_private_targets` 再决定目标是否属于禁止访问的私网范围；
- ACL 放行不代表可以访问内网，黑名单也不能用来绕过私网保护；
- 即使请求会经过上游代理，目标本身仍会先经过本机安全检查。

关闭后的含义是：能够连接到 MITMRouter 的客户端可能访问 `localhost`、内网服务、Docker 网段或云元数据。只有在完全隔离、明确需要访问内网且有其他安全边界时才考虑关闭。

### 4.15 `acl_whitelist`：目标访问白名单

这是目标主机的**访问白名单**。它控制哪些目标可以进入转发路径。

#### 为空时

```text
[] = 不限制白名单，允许所有目标；HTTPS 目标默认做 MITM 解析
```

#### 非空时

只有命中白名单的目标才允许访问；未命中的目标直接返回本地 `403`，不会连接目标。
命中的 HTTPS 目标仍按正常流程做 MITM 解析；这条名单不是请求改写开关。

例如只允许访问并解析 OpenAI 和 Anthropic：

```text
api.openai.com
*.openai.com
api.anthropic.com
```

#### 支持的四种写法

| 形态 | 示例 | 含义 |
|---|---|---|
| 单个 IPv4/IPv6 | `1.2.3.4`、`::1` | 匹配这个 IP |
| CIDR 网段 | `10.0.0.0/8`、`2001:db8::/32` | 匹配这个地址段 |
| 精确域名 | `api.openai.com` | 只匹配该域名 |
| 通配符域名 | `*.openai.com` | 匹配任意层级子域，不匹配 `openai.com` 本身 |

#### 匹配细节

- 匹配不区分大小写；
- 两端空白会被去掉；
- 域名末尾的根点会被去掉；
- 不要写协议：`https://api.openai.com` 是错的；
- 不要写端口：`api.openai.com:443` 是错的；
- 不要写路径或查询字符串；
- 域名匹配只按目标字面主机名/IP，不会把域名条目和 IP 条目互相转换；
- 单个名单最多 500 条。

#### 示例

```text
正确：api.openai.com
正确：*.openai.com
正确：10.0.0.0/8
正确：[填写时不需要给 IPv6 加端口]
错误：https://api.openai.com
错误：api.openai.com:443
错误：/v1/chat/completions
```

### 4.16 `acl_blacklist`：目标访问黑名单

黑名单与白名单的优先级很简单：黑名单命中就拒绝访问，即使同时命中白名单：

```text
命中黑名单 → 本地 403，拒绝访问
```

整体判定顺序是：

```text
1. 命中黑名单         → 本地 403，拒绝访问
2. 白名单非空且未命中 → 本地 403，拒绝访问
3. 其他情况           → 放行；HTTPS 目标进入 MITM 解析
```

因此黑名单永远优先于白名单。例如：

```text
白名单：*.example.com
黑名单：private.example.com
```

访问 `private.example.com` 时会返回本地 403，不会连接目标，也不会被 MITM。

这适合以下场景：

- 在允许范围内排除某个不应访问的域名；
- 阻断误配置或不再需要的目标；
- 配合白名单收紧接入口的可访问范围。

再次强调：黑名单和白名单都属于访问控制；`block_private_targets` 是独立的私网安全检查。

### 4.17 `log_retention_days`：审计与更新记录保留天数

| 项目 | 值 |
|---|---|
| 默认值 | `30` |
| 有效范围 | `1–3650` |
| 单位 | 天 |

程序启动时清理一次，之后大约每天清理一次。清理范围包括：

- 访问审计记录；
- 账号映射更新记录。

它不会删除：

- 上游配置；
- 账号映射当前快照；
- Marker 动态盐；
- MITM CA；
- 管理员口令哈希。

#### 怎么选

| 天数 | 适合场景 |
|---:|---|
| `1–7` | 磁盘紧张、只需要短期排障 |
| `30` | 推荐默认，日常运维 |
| `90–180` | 需要较长的趋势与问题回溯 |
| `365` 以上 | 合规或长期审计；先确认数据库增长和访问控制 |

审计记录只保存元数据，不保存请求体和请求头；如果需要完整内容排障，应临时用 `-trace-file`，不要把保留天数当成明文流量记录开关。

### 4.18 `metrics_enabled`：Prometheus `/metrics`

默认关闭：

```text
false
```

开启后：

```text
GET /metrics
```

才会提供 Prometheus 文本格式指标，并且仍然需要管理台登录会话。关闭时访问返回 404，而不是把指标公开。

适合填写：

```text
开启：有 Prometheus/监控系统，并已限制管理台访问
关闭：不需要指标，或管理台暴露边界还没有做好
```

当前没有额外的端口、路径、用户名或标签配置项。指标走 `-admin-addr` 对应的管理面，不走客户端接入口。

### 4.19 `sync_empty_clear_threshold`：同步空快照保护阈值

| 项目 | 值 |
|---|---|
| 默认值 | `3` |
| 有效范围 | `1–100` |
| 作用范围 | API 全量同步返回空账号列表时 |

当某个同步源连接成功，但返回 0 个账号时，程序不会马上删除已有映射，而是累计连续空快照次数。

例如阈值为 `3`：

```text
第 1 次成功但返回 0 个账号 → 保留旧账号，记录 1/3
第 2 次成功但返回 0 个账号 → 保留旧账号，记录 2/3
第 3 次成功但返回 0 个账号 → 才清空该源的账号映射
```

如果一次同步是连接失败、HTTP 错误或解析失败，则不会按空快照计数，也不会因为这个失败清空账号。

#### 如何选

| 值 | 行为 |
|---:|---|
| `1` | 立即清空；适合源端空列表就代表确实没有账号的环境 |
| `3` | 推荐默认，能抵抗短暂闪断或上游异常 |
| `5–10` | 源端偶尔返回空列表，希望更保守 |
| 很大 | 保护强，但真实删除账号也会延迟 |

当账号真的从映射表消失时，与它关联的账号出站绑定也会被级联清理。这个阈值因此不仅保护账号映射，也保护其出站绑定。

### 4.20 `acctmap_enabled`：账号映射开关（当前不是页面输入项）

默认值：

```text
true
```

它是数据库兼容设置项，但当前基础设置页面没有暴露开关，设置保存 API 也会沿用当前值，不会因为旧客户端漏传字段而静默关闭它。

开启时：

```text
已同步/手动登记的 AT/RT 指纹命中后，按“平台 + 账号”推导粘滞身份
```

这样 AT 轮换后，只要仍属于同一个账号，出口身份可以保持不变。

关闭时：

```text
回到纯 Marker 哈希逻辑，token 本身变化可能导致出口变化
```

普通使用不要手工修改这个键，也不要直接编辑 `router.db`。如果你没有使用账号映射，保持默认值即可。

---

## 5. 上游出口参数：`name`、`platform`、`base_url`、`inject`、`enabled`

位置：管理台 → **上游出口** → 新增/编辑。

一个上游条目代表一个可用的 HTTP/HTTPS/SOCKS5 出口代理。业务请求的目标 URL、业务请求头和响应内容不会因为填写这里而被改写；这里决定的是路由器连接哪一个出口，以及是否在出口代理用户名中注入粘滞身份。

### 5.1 `name`：条目名称

**怎么填：**

填一个你自己看得懂、不会重复的名称。例如：

```text
us-residential-main
us-residential-backup
decodo-rotating
plain-office-proxy
```

**规则：**

- 新建时不能为空；
- 必须唯一；
- 建议只使用字母、数字、短横线和下划线；
- 不建议在名称前后加空格；
- 名称不是供应商用户名，不要把完整 `base_url` 填到这里；
- 这个名称会显示在默认路由、审计和更新记录中。

名称只是管理标识，不参与平台凭据注入。改名不会改变平台会话 ID，但如果它是当前默认上游，程序会同步更新默认指向。

### 5.2 `platform`：平台类型，必须选对

当前平台选项：

```text
dataimpulse
decodo
1024proxy
resin
generic
plain
```

这个字段不是装饰标签，而是决定“如何改写上游用户名”的程序分支。选错时，最麻烦的情况是：

```text
上游认证仍然成功，但会话参数没有按供应商语法注入
```

表面看起来“能用”，实际粘滞功能可能已经失效。

选择原则：

| 你的上游 | 应选平台 |
|---|---|
| DataImpulse | `dataimpulse` |
| Decodo（原 Smartproxy） | `decodo` |
| 1024proxy | `1024proxy` |
| Resin 网关 | `resin` |
| 其他用户名模板平台 | `generic` |
| 普通 HTTP/SOCKS5 代理，不需要会话注入 | `plain` |

### 5.3 `base_url`：上游代理地址和凭据

#### 基本格式

```text
<scheme>://[用户名:密码@]<代理主机>:<端口>
```

示例：

```text
http://user:pass@proxy.example.com:8080
https://user:pass@gateway.example.com:8443
socks5://user:pass@proxy.example.com:1080
socks5h://user:pass@gateway.example.com:1080
```

当前代码允许的 scheme：

```text
http
https
socks5
socks5h
```

必须有 host。程序保存时会检查 URL 能解析、host 非空、scheme 在允许列表内；不会替你检查供应商域名、端口、套餐或 API Key 是否真实有效，供应商相关部分必须按控制台生成值填写。

#### 用户名和密码的 URL 编码

用户名和密码处于 URL 的 userinfo 区域。如果真实凭据包含下面这些字符：

```text
@ : / ? # %
```

应按 URL userinfo 规则编码。例如不要直接把包含 `@` 的密码写成：

```text
http://user:p@ss@example.com:8080
```

应使用对应的百分号编码形式：

```text
http://user:p%40ss@example.com:8080
```

最稳妥的做法是复制供应商控制台生成的代理 URL，不要手工重新拼接。

#### scheme 的含义

- `http://`：通过 HTTP 代理协议连接上游；
- `https://`：通过 HTTPS 保护上游代理连接；
- `socks5://`：通过 SOCKS5 连接上游；
- `socks5h://`：允许填写，运行时会归一到 SOCKS5 传输，域名解析交给 SOCKS5 出口侧。

scheme 是“到上游代理的连接方式”，不是最终目标站的协议。客户端访问 HTTPS 目标时仍然会通过标准 CONNECT/流式转发处理。

#### 编辑已有条目时的掩码

列表中的密码会显示成：

```text
http://user:____@proxy.example.com:8080
```

编辑后原样保存这个掩码，程序会沿用旧密码。也支持程序化更新时使用：

```text
____
__unchanged__
```

它们表示保留旧密码，不是新的真实密码。新建条目时必须填真实凭据，不能指望掩码自动产生密码。

### 5.4 `inject`：Generic 模板

只有 `platform=generic` 使用 `inject`。页面输入的是 JSON 文本，例如：

```json
{"username_template":"{user}-session-{sid}","password":"<可选静态密码>"}
```

#### 字段

| JSON 字段 | 是否必填 | 说明 |
|---|---:|---|
| `username_template` | 是 | 最终写入上游用户名的模板 |
| `password` | 否 | 非空时覆盖 `base_url` 中的密码；空时沿用 `base_url` 密码 |

模板中只允许四个占位符：

| 占位符 | 替换内容 |
|---|---|
| `{user}` | `base_url` 原本的用户名 |
| `{sid}` | 当前派生的会话身份，通常是小写十六进制字符串 |
| `{ttl_min}` | 基础设置里的 `session_ttl_min` 整数 |
| `{country}` | 当前版本预留值，实际恒为空 |

模板是字符串替换，不是 Go/Python 表达式，也不能写任意变量或函数。

#### 合法示例

```json
{"username_template":"{user}-session-{sid}"}
```

```json
{"username_template":"{user}-session-{sid}-sessionduration-{ttl_min}"}
```

```json
{"username_template":"{user}__sessid.{sid}","password":"<静态密码>"}
```

第二个示例要求：

```text
session_ttl_min > 0
```

否则保存时会提示模板使用了被禁用的 `{ttl_min}`。

#### 不合法示例

```json
{"username_template":"{username}-{sid}"}
```

`{username}` 不是受支持的占位符。

```json
{"username_template":"{user}-{sid"}
```

花括号没有闭合。

```json
{"username_template":"{user}}-{sid}"}
```

花括号不匹配。

```json
{"username_template":"{user}-{ttl_min}"}
```

当 `session_ttl_min=0` 时不允许保存。

#### Generic 密码的优先级

```text
inject.password 非空 → 使用 inject.password
inject.password 为空/没有 → 沿用 base_url 密码
```

编辑列表中的 Generic 条目时，`inject.password` 也会回显成 `____`。保留它即可沿用旧密码；不要把掩码提交成真实新密码。

#### 其他 JSON 字段

当前只有 `username_template` 和 `password` 有明确语义。不要把供应商 API 文档中的其他字段直接塞进 `inject`，除非你确认 Generic 模板只依赖用户名和密码；未知字段不会自动产生额外行为。

---

## 6. 六种上游平台怎么填

下面的 `<...>` 都是占位符，落地时要替换成供应商控制台真实内容。

### 6.1 DataImpulse：`platform=dataimpulse`

#### 推荐填写形态

HTTP 示例：

```text
base_url = http://<login>__cr.us:<password>@gw.dataimpulse.com:823
```

SOCKS5 示例（端口以供应商控制台为准）：

```text
base_url = socks5://<login>__cr.us:<password>@gw.dataimpulse.com:824
```

如果控制台给了其他地区、端口或参数，以控制台为准。代码本身只检查通用 URL 格式，不会强制 host 必须是 `gw.dataimpulse.com`。

#### 用户名会怎样被改写

DataImpulse 用户名常见语法：

```text
<login>__<key>.<value>;<key>.<value>
```

程序会：

1. 找到用户名中的 `__`；
2. 把双下划线前的部分视为登录名；
3. 删除已有的 `sessid.*` 段；
4. 在末尾追加：

```text
sessid.<当前sid>
```

例如填写：

```text
<login>__cr.us
```

实际发送给上游的用户名逻辑上类似：

```text
<login>__cr.us;sessid.8c12a0f4d7e91b20
```

已有的其他参数会保留。

#### TTL 注意事项

当前内置 DataImpulse 注入器只注入 `sessid`，**忽略 `session_ttl_min`**。如果你的 `base_url` 原本带有 `sessttl.*`，程序不会删除它，但也不会根据全局 TTL 自动添加或修改它。

因此：

- 只想使用 `sessid`：全局 TTL 保持 `0` 最直观；
- 需要 DataImpulse 的 `sessttl`：按供应商语法直接把它写进 `base_url`，并确认对应端口/套餐支持；
- 不要以为把全局 TTL 填成 30 就会自动变成 `sessttl.30`。

#### 常见错误

```text
把 __sid.xxx 当成官方 sessid 语法
把 sessionduration 写进 DataImpulse 用户名
把地区代码或账号密码放到错误的分隔符后
```

平台选择错误时，DataImpulse 可能仍然认证成功，但会话参数不一定有效。

### 6.2 Decodo：`platform=decodo`

#### 推荐填写形态

```text
base_url = http://user-<login>-country-us:<password>@gate.decodo.com:7000
```

也可以保留控制台生成的更多位置参数：

```text
base_url = http://user-<login>-country-us-city-chicago:<password>@gate.decodo.com:7000
```

SOCKS5 可以使用供应商支持的写法，例如：

```text
base_url = socks5h://user-<login>-country-us:<password>@gate.decodo.com:7000
```

#### 最重要的用户名规则

用户名必须以：

```text
user-
```

开头。下面这个会被拒绝：

```text
<login>-country-us
```

程序会在用户名中注入或替换：

```text
-session-<当前sid>
```

如果设置了 `session_ttl_min>0`，还会注入或替换：

```text
-sessionduration-<分钟>
```

例如全局 TTL 为 `30`，最终逻辑形态类似：

```text
user-<login>-country-us-session-8c12a0f4d7e91b20-sessionduration-30
```

#### TTL 范围

Decodo 支持的内置注入范围是 `1–1440` 分钟。程序会把更大的值限制到 1440。

如果全局 TTL 为 `0`：

- 已有 `sessionduration-*` 会保留；
- 没有就使用供应商默认行为；
- `session` 仍会注入，因为它是粘滞身份的关键。

#### 供应商行为提醒

Decodo 会话有供应商侧的生命周期限制，常见资料包括默认约 10 分钟和 60 秒无活动过期。MITMRouter 的同一 sid 只能做到“尽力保持同一供应商会话”，不能让已经被供应商回收的住宅节点永久不变。

### 6.3 1024proxy：`platform=1024proxy`

#### 推荐填写形态

建议直接复制控制台生成的完整用户名，典型格式：

```text
base_url = socks5://<apikey>-region-US-sid-placeholder-t-5:<password>@us.1024proxy.io:3000
```

HTTP 也可能使用同一 host:port，只把 scheme 换成供应商要求的形式：

```text
base_url = http://<apikey>-region-US-sid-placeholder-t-5:<password>@us.1024proxy.io:3000
```

具体 host、端口、地区和套餐以控制台为准。

#### 用户名会怎样被改写

1024proxy 常见语法：

```text
<apikey>-region-<CC>-sid-<sessid>-t-<分钟>
```

程序会替换或补上：

```text
sid-<当前sid>
```

如果 `session_ttl_min>0`，还会替换或补上：

```text
t-<分钟>
```

当 TTL 为 `0` 时，程序保留 `base_url` 已有的 `t` 值；如果没有，最终行为由供应商决定，所以生产上建议保留控制台生成的完整用户名。

#### TTL 范围

内置注入器把 TTL 限制到 `1–120` 分钟。你的套餐可能更窄，例如某些端口套餐是 `3–30` 分钟。程序不会知道你的具体套餐，最终是否接受由供应商决定。

#### 不要删除的参数

如果控制台用户名中有：

```text
region-US
st-...
city-...

```

请不要为了“简化”随意删除位置/线路参数。程序只负责识别并改写已知会话键，其他部分通常会原样保留。

### 6.4 Resin：`platform=resin`

#### 推荐填写形态

```text
base_url = socks5://Default:<RESIN_TOKEN>@resin:2260
```

也可以使用你部署的 Resin 实际 host 和端口：

```text
base_url = http://<Platform>:<RESIN_TOKEN>@<resin-host>:<port>
```

#### 用户名规则

Resin 常见用户名：

```text
Platform
Platform.Account
```

MITMRouter 会取第一个 `.` 之前的部分作为平台名，再写入自己的身份：

```text
Platform.<当前sid>
```

例如：

```text
base_url 用户名：Default
```

会逻辑上变为：

```text
Default.8c12a0f4d7e91b20
```

如果原用户名是：

```text
Default.old-account
```

旧的 `old-account` 会被丢弃，换成当前 sid。

#### 必须有用户名

当前 Resin 注入器要求用户名非空，并且每次注入要求当前 account 非空。因此不要填写：

```text
socks5://:<RESIN_TOKEN>@resin:2260
```

如果你需要“凭据完全原样、不做会话注入”的普通 Resin 代理，请考虑使用 `plain`，但那将不提供本项目的 Resin 会话注入功能。

密码应填写 Resin token，程序不会把会话 ID放进密码。

### 6.5 Generic：`platform=generic`

当供应商不是内置四家，且它把 session 参数放在用户名中时使用 Generic。

需要填写两部分：

```text
base_url：供应商的原始代理 URL
inject：   描述如何由原用户名和 sid 生成新用户名的 JSON
```

例如供应商语法是：

```text
<user>-session-<session_id>
```

可以填写：

```text
base_url = http://<user>:<password>@gateway.example.com:8080
inject = {"username_template":"{user}-session-{sid}"}
```

如果供应商语法是：

```text
<user>-session-<session_id>-ttl-<minutes>
```

则填写：

```text
inject = {"username_template":"{user}-session-{sid}-ttl-{ttl_min}"}
```

并把全局 `session_ttl_min` 设置为大于 0。

Generic 不会：

- 自动识别你的供应商参数名；
- 自动检查地区代码；
- 自动检查端口是否匹配套餐；
- 自动把 `{country}` 变成某个国家；
- 自动修改业务请求内容。

这些都要由你根据供应商文档负责。

### 6.6 plain：`platform=plain`

`plain` 表示普通代理，不做会话注入。

#### 示例

```text
base_url = http://user:pass@proxy.example.com:8080
```

```text
base_url = socks5://user:pass@proxy.example.com:1080
```

#### 规则

- `inject` 必须为空；
- 凭据和 URL 按原样使用；
- 不会注入 `sid`、session、TTL；
- `session_ttl_min` 对它没有意义；
- 可用于普通固定代理、办公出口或 IP 白名单代理。

`plain` 的特殊用途是“账号 ↔ 多个普通出站”的绑定。只有 `plain` 条目可以出现在账号出站绑定选择器中，见第 8 节。

如果你把一个 `plain` 条目设为默认上游，所有没有更高优先级绑定的请求都会走这个普通代理；它本身不提供按 Marker 的供应商侧粘滞。

---

## 7. 账号管理：同步源参数

账号管理有两条用途不同的路径：

```text
全量同步：从 CLIProxyAPI / Sub2API 的管理接口定时拉取
增量同步：直接监听 CPA 认证文件目录，或直接读 Sub2API PostgreSQL
```

两者可以同时配置。全量同步负责定期完整对齐；增量路径负责更快感知 AT/RT 变化。

### 7.1 什么时候需要账号映射

如果客户端直接使用稳定 API Key，通常不需要账号管理：

```text
API Key → Marker 哈希 → 粘滞出口
```

如果客户端使用会轮换的 Bearer AT/RT，而你希望：

```text
同一个订阅号/账号的 token 轮换后，出口仍保持不变
```

就应该把 AT/RT 映射到真实账号：

```text
AT/RT 指纹 → 平台 + 账号 → 账号级粘滞出口
```

### 7.2 同步源 `kind`：源类型

当前只有两个值：

| 页面名称 | 保存值 | 用途 |
|---|---|---|
| CLIProxyAPI | `cpa` | 通过 CLIProxyAPI 管理接口或认证目录读取 |
| Sub2API | `sub2api` | 通过 Sub2API 管理接口或 PostgreSQL 读取 |

不要把页面显示名 `CLIProxyAPI` 填到 `kind` 字段；程序化 API 要填 `cpa`。

### 7.3 同步源 `name`：源实例名称

填一个唯一、好认的实例名称，例如：

```text
cpa-production
cpa-test
sub2api-main
sub2api-vps-1
```

**规则：**

- 不能为空；
- 必须唯一；
- 这是同步源实例标识，不是供应商账号名；
- 删除同步源时，该源名下的账号映射会级联删除；
- 账号管理的更新记录会显示这个名称。

### 7.4 同步源 `base_url`：管理 API 根地址

#### 通用规则

必须是干净的 HTTP 或 HTTPS 根地址：

```text
http://127.0.0.1:8317
https://sub2api.example.com
```

程序会：

- 去掉末尾 `/`；
- 要求 scheme 是 `http` 或 `https`；
- 要求有 host；
- 拒绝 URL 中的用户名/密码；
- 拒绝 query；
- 拒绝 fragment。

因此不要这样填：

```text
https://user:pass@sub2api.example.com       # 有 URL 凭据，拒绝
https://sub2api.example.com?key=secret       # 有 query，拒绝
https://sub2api.example.com#admin            # 有 fragment，拒绝
https://sub2api.example.com/api/v1/...       # 通常应填服务根，程序会自己拼路径
```

API Key 单独填到 `api_key`，不要塞进 URL。

### 7.5 同步源 `api_key`：管理接口密钥

#### 新建

新建源时必填。应填：

- CLIProxyAPI 的 management key；
- Sub2API 的 admin API key。

不要填写客户端业务 API Key，除非它确实是对应管理接口要求的密钥。

#### 编辑

编辑已有源时，输入框为空表示：

```text
沿用数据库中原来的 API Key
```

页面不会把真实 Key 回显出来。不要把“留空保持不变”误解成清空密钥。

#### 程序使用方式

CLIProxyAPI：

```http
Authorization: Bearer <management-key>
```

Sub2API：

```http
x-api-key: <admin-api-key>
```

密钥保存到 secrets 表，不应放入 `base_url`、源名称或查询参数。

### 7.6 CLIProxyAPI 的全量同步字段

选择：

```text
kind = cpa
```

填写：

```text
name      = cpa-production
base_url  = https://<CLIProxyAPI地址>
api_key   = <management-key>
```

程序会调用类似：

```text
GET <base_url>/v0/management/auth-files
GET <base_url>/v0/management/auth-files/download?name=<文件名>
```

它会识别的常见 provider/platform 包括：

| CLIProxyAPI provider/type | 归一后的平台 |
|---|---|
| `codex`、`openai` | `openai` |
| `claude`、`anthropic` | `anthropic` |
| `gemini`、`antigravity` | `gemini` |
| `xai`、`grok` | `grok` |
| `kimi` | `kimi` |
| `qwen` | `qwen` |
| `iflow` | `iflow` |

未知 provider 会跳过，不会进入账号映射。

认证文件需要能找到：

- 账号标识（通常是 email/email_address 等）；
- `access_token` 或 `refresh_token` 至少一项。

没有账号或没有任何可用凭据的文件不会创建映射行。

### 7.7 Sub2API 的全量同步字段

选择：

```text
kind = sub2api
```

填写：

```text
name      = sub2api-main
base_url  = https://<Sub2API地址>
api_key   = <admin-api-key>
```

程序会调用：

```text
GET <base_url>/api/v1/admin/accounts/data
```

认证头：

```http
x-api-key: <admin-api-key>
```

程序只把下面两种账号类型纳入 AT/RT 映射：

```text
oauth
setup-token
```

`apikey`、`upstream`、`bedrock` 等没有可用 AT/RT 映射的类型会跳过。

支持的主要平台归一规则：

| Sub2API platform | 归一后的平台 |
|---|---|
| `openai`、`codex` | `openai` |
| `anthropic`、`claude` | `anthropic` |
| `gemini` | `gemini` |
| `grok`、`xai` | `grok` |
| `kimi`、`moonshot` | `kimi` |
| `deepseek` | `deepseek` |
| `zhipu`、`glm` | `glm` |
| `ollama` | `ollama` |

账号标识优先取凭据中的 email，没有时使用账号 name。账号标识会统一转小写。

### 7.8 `interval_s`：全量同步间隔（秒）

| 项目 | 值 |
|---|---|
| 新建默认值 | `600` 秒，即 10 分钟 |
| 最小值 | `60` 秒 |
| 含义 | 全量 API 同步之间的最短间隔 |

推荐：

```text
600
```

如果上游凭据更新很频繁，可以考虑：

```text
120
300
```

不要设置成几秒。程序会把小于 60 的值抬到 60，避免频繁打爆源端。

这只是**全量同步**间隔，不是增量同步间隔：

- CPA 目录增量由文件事件触发；
- Sub2API PostgreSQL 增量当前按固定约 3 秒轮询；
- 页面里没有单独的增量间隔参数。

编辑已有源时，如果程序化请求不发送或发送 `0`，通常会沿用旧值；新建时非正值会回到默认 600。

### 7.9 同步源 `enabled`：启用/停用

启用时：

- 参与定时全量同步；
- 如果配置了增量路径，启动对应增量 reader；
- 同步结果更新账号映射。

停用时：

- 停止定时全量同步；
- 停止对应增量 reader；
- 已经写入的账号映射不会因为单纯停用而立即删除。

如果你希望完全清理某源的数据，应删除同步源；删除会连带清理其映射。

### 7.10 `direct_auth_dir`：CPA 认证文件目录（可选）

只有 `kind=cpa` 可以填写。

示例：

```text
/opt/cliproxyapi/auths
```

#### 必须满足

- 必须是绝对路径；
- 目录必须已经存在；
- 必须是目录，不是文件；
- 根目录不能是符号链接；
- MITMRouter 进程必须有读取和监听权限。

填写后即启用 CPA 增量同步：

```text
认证文件发生变化 → 读取 JSON → 提取账号和 AT/RT → 更新映射
```

程序会递归监听目录及子目录，重点处理 `.json` 文件。文件太大、空文件、符号链接、读取时发生变化或 JSON 不符合支持格式时，会跳过该文件并记录错误，不会把一次暂时读坏直接当成账号删除。

清空这个路径即停用 CPA 增量同步，但不影响全量 API 同步。

### 7.11 `direct_db_dsn`：Sub2API PostgreSQL 连接地址（可选）

只有 `kind=sub2api` 可以填写。

#### 同机/本机数据库示例

```text
postgres://readonly:<password>@127.0.0.1:5432/sub2api?sslmode=disable
```

也可以使用 `localhost` 或 Unix socket 形式，具体取决于 pgx/PostgreSQL 环境。

#### 远程数据库示例

```text
postgres://readonly:<password>@db.example.com:5432/sub2api?sslmode=verify-full
```

远程主机必须使用安全 TLS 配置。当前代码要求远程 DSN 的效果等同于：

```text
sslmode=verify-full
```

并且要有正确的证书校验和 server name。不要用：

```text
sslmode=disable
sslmode=require 但没有完整主机名校验
sslmode=verify-full 但证书/SAN/DNS 不匹配
```

#### 数据库账号权限

建议建立只读账号，只需要读取 Sub2API 的 `accounts` 表相关字段。增量读取会检查：

- `updated_at`；
- `type`；
- `platform`；
- `deleted_at`；
- `credentials` 中的 email、access_token、refresh_token。

DSN 会存入 secrets，不会在页面回显。

#### 编辑和清除

编辑已有 Sub2API 源时：

- DSN 输入框留空：沿用已保存 DSN；
- 勾选“清除已保存的连接地址”：删除 DSN，并停用增量读取；
- `direct_db_clear=true` 与非空 `direct_db_dsn` 不能同时提交。

### 7.12 增量路径不是全局开关

当前没有一个全局 `incremental_enabled` 输入框。增量 reader 是否运行由两项同时决定：

```text
同步源 enabled=true
并且该源填了对应增量路径
```

所以：

```text
cpa + direct_auth_dir 非空       → CPA 文件增量
sub2api + direct_db_dsn 非空     → Sub2API 数据库增量
路径为空                         → 不运行增量
```

### 7.13 空快照保护与同步源的关系

`sync_empty_clear_threshold` 只保护全量 API 同步返回空列表的情况。它不改变：

- 正常非空快照的完整替换；
- API 请求失败时的保留行为；
- 增量 reader 对单个账号更新的处理。

如果你看到状态类似：

```text
ok: empty snapshot deferred 1/3
```

说明连接成功但空列表被保护了，并不代表当前映射已经被删除。

---

## 8. 账号管理：手动登记参数

入口：管理台 → 账号管理 → **手动登记**。

手动登记适合：

- 没有可用同步源；
- 只需要登记少量账号；
- 想通过 API 推送账号凭据；
- 测试账号映射和出站绑定。

### 8.1 `platform`：账号所属平台

页面预置选项：

```text
openai
anthropic
gemini
grok
kimi
deepseek
glm
qwen
iflow
ollama
```

页面允许自定义输入，但生产上建议使用代码中的规范小写平台名。

平台必须和目标主机归类一致。例如：

```text
openai   → openai.com / chatgpt.com
anthropic → anthropic.com / claude.ai / claude.com
grok    → x.ai / grok.com
```

如果你填了自定义平台名，但目标域名没有被内置平台识别，程序通常无法把请求映射到这个自定义平台；自定义值主要是为扩展场景预留，不是任意填一个名字就能自动生效。

### 8.2 `account`：稳定账号标识

必填。推荐填：

```text
user@example.com
```

也可以是：

```text
订阅号 UUID
供应商账号 ID
你自己定义的稳定账号字符串
```

不要填：

```text
当前 access token
当前 refresh token
随机数
```

因为这个字段的目的就是在 token 轮换后仍然表示同一个账号。

程序会：

- 去掉两端空白；
- 转成小写；
- 以“平台 + 账号”作为账号级身份的一部分。

因此下面两个输入会被视为同一个账号：

```text
User@example.com
user@example.com
```

### 8.3 `source_type`：凭据来源类型

必填，最多 64 个字符。页面预置：

```text
CLIProxyAPI
Sub2API
```

也可以填自定义值，例如：

```text
manual-import
team-a
backup-export
```

它主要用于：

- 在账号管理页面分组展示；
- 统计不同来源的映射行；
- 追踪手动推送来源。

它不是上游代理平台名，也不是必须与 `kind` 相同的机器枚举。为了让运维人员看得懂，建议使用有意义的名称。

### 8.4 `access_token`：AT

可选，但 `access_token` 和 `refresh_token` 至少要有一项。

建议直接粘贴真实 token 原文，不要加引号，不要把它填到账号名中。程序会在保存前做必要的空白和常见 scheme 归一化，然后保存指纹而不是完整 token 明文。

页面不会回显完整 token，只显示尾缀提示，例如：

```text
…a1b2
```

这不是可用于重新登录的 token，只是帮助你确认是不是换了凭据。

### 8.5 `refresh_token`：RT

与 AT 相同：

- 可选；
- 可以只填 RT；
- 与 AT 至少有一项；
- 保存的是指纹和尾缀提示；
- 轮换 RT 后重新登记或通过同步源更新即可。

典型填写：

```text
platform   = openai
account    = user@example.com
source_type = manual-import
access_token = <AT>
refresh_token = <RT>
```

### 8.6 手动登记不会把 token 明文显示在列表

账号映射表展示的是：

- 平台；
- 账号；
- AT/RT 指纹尾部提示；
- 来源；
- 更新时间；
- 出站绑定。

完整 token 不应出现在管理台列表、审计记录或普通运行日志中。仍然要把“输入框、浏览器自动填充、反向代理访问日志、终端历史”当作敏感信息处理。

### 8.7 手动删除账号的影响

删除账号会删除匹配账号的映射行。默认从所有来源删除；如果通过 API 的 `source` 查询参数指定来源，则只删除该来源的行。

当该账号在任何来源都不再存在时：

```text
账号的 plain 出站绑定也会被垃圾回收
```

所以删除账号前要确认是否还要保留它的出站绑定语义。

---

## 9. 账号 ↔ 出站绑定参数

出站绑定使用 `plain` 类型的普通代理条目，把某个平台账号固定或随机分配到若干普通出口。

它的优先级高于 `default_upstream`：

```text
账号绑定命中 → 走绑定的 plain 出站
账号未绑定   → 按默认路由处理
```

### 9.1 为什么必须是 `plain`

绑定功能本身管理的是“选哪一条普通出口”：

- `plain` 保留每条代理的原始凭据；
- 绑定层负责在多个 `plain` 出站中选择；
- 不再由 DataImpulse/Decodo 等平台注入 session 参数。

其他平台条目不会出现在绑定选择器中。要使用供应商自带的用户名粘滞，请把它作为默认上游或普通默认路由，而不是作为 plain 账号绑定。

### 9.2 `mode`：绑定模式

只有两个值：

| 值 | 页面名称 | 行为 |
|---|---|---|
| `sticky` | 粘滞 | 在选中的出口集合中为账号稳定选择一条 |
| `random` | 随机 | 每次请求在选中的启用出口中随机选择一条 |

#### `sticky`

适合：

- 希望账号固定出口 IP；
- 希望重启后选择尽量不漂移；
- 出口数量较多，想用多条线路做稳定分配。

程序使用基于账号和出站 ID 的稳定评分来选择。盐值变化或绑定集合变化可能导致重新选择。

#### `random`

适合：

- 不要求同一账号固定 IP；
- 希望把请求分散到多个普通出口；
- 做简单的负载分摊。

每次请求都可能选到不同出口。如果只有一条启用出站，随机模式与粘滞模式没有实际区别。

### 9.3 账户方向绑定：`egress_ids`

在账号行点击“绑定出站”时：

1. 选择 `sticky` 或 `random`；
2. 勾选一个或多个 `plain` 出站；
3. 保存。

`egress_ids` 是出站条目的数据库 ID 数组，例如：

```json
{
  "mode": "sticky",
  "egress_ids": [3, 7, 9]
}
```

页面中的“清空已选”会清除该账号的全部绑定，使它回落默认路由。

### 9.4 出站方向绑定：`accounts`

从某条 plain 出站的“关联账户”入口批量选择账号时，API 形态是：

```json
{
  "mode": "sticky",
  "accounts": [
    {"platform":"openai","account":"user@example.com"},
    {"platform":"anthropic","account":"claude@example.com"}
  ]
}
```

当前页面支持：

- 按平台筛选；
- 按平台/账号模糊搜索；
- 分页；
- 只看已选；
- 全选当前页；
- 反选当前页；
- 跨页保留选择。

保存的语义是：

```text
该 plain 出站与本次提交的账号集合完全一致
```

本次没有提交的账号会从该出站解绑。

### 9.5 批量模式对已有绑定的影响

从出站方向批量关联时，页面上的 `mode`：

- 对新加入该出站的账号生效；
- 已经绑定该出站的账号保留自己的原有模式。

如果你想修改某个账号已经存在的模式，应该从账号方向打开“绑定出站”并保存。

### 9.6 停用、删除和绑定的关系

- 删除 plain 出站：引用它的账号绑定会级联删除；
- 停用 plain 出站：绑定行可能暂时保留，但运行时不会把它作为可用候选；
- 一个账号绑定的所有出站都缺失或停用：请求会受控失败，不会偷偷回落到默认上游；
- 账号在所有来源中消失：账号绑定会被垃圾回收；
- 删除某个来源的映射行不一定立即删除账号绑定，只要其他来源仍保留同一平台/账号。

因此，停用或删除出站前，最好先在账号管理页筛选受影响绑定。

---

## 10. 账号平台与目标主机的对应关系

账号映射要生效，目标主机识别出的平台必须和登记的 `platform` 一致。当前内置归类包括：

| 账号平台 | 内置目标后缀示例 |
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

内置平台载体读取顺序大致是：

```text
Authorization: Bearer ...
x-api-key: ...
api-key: ...
x-goog-api-key: ...
URL ?key=...
```

如果这些都没有，且目标 URL 符合内置 body 解析规则，程序还可能从特定请求体读取凭据；该规则不是管理台参数，当前主要覆盖 Grok OAuth refresh token 路径。

未知域名不会自动猜测平台。对于未知主机：

- 通用 `marker_headers` 仍可提供一个纯 Marker；
- 但通常没有可用于账号表匹配的内置平台；
- 请求仍可以按纯 Marker 哈希粘滞。

---

## 11. 访问审计页面的查询参数

这些不是持久化配置，而是查询条件。入口：管理台 → **访问审计**。

### 11.1 时间范围 `range`

页面选项：

| 值 | 含义 |
|---|---|
| `1h` | 最近 1 小时 |
| `24h` | 最近 24 小时，默认 |
| `7d` | 最近 7 天 |
| `all` | 全部时间 |

程序最终转换成 `from` / `to` 的 Unix 毫秒时间戳。

### 11.2 关键词 `q`

输入目标 host 或 path 的一部分，例如：

```text
openai.com
/v1/chat
```

它是 host/path 子串匹配，不是正则表达式。查询会同时检查：

```text
host LIKE %q%
path LIKE %q%
```

### 11.3 账号或会话 `account`

这里是精确匹配，可以填：

- 真实账号标识；
- 完整的 `account_fp`/派生会话 ID。

它不是模糊搜索。页面显示的短尾缀只是提示，排查时最好复制完整值或直接按真实账号筛选。

### 11.4 上游 `upstream`

按上游条目名称精确匹配，例如：

```text
us-residential-main
plain-office-proxy
```

也可能出现特殊值：

```text
direct
blind
```

### 11.5 状态 `class`

页面选项：

| 值 | 含义 |
|---|---|
| `2xx` | 真实目标/上游返回的 200–299 |
| `4xx` | 真实返回的 400–499 |
| `5xx` | 真实返回的 500 及以上 |
| `err` | MITMRouter 自身有内部错误分类 |

审计中 `status=0` 表示在收到真实 HTTP 响应前就失败了，例如上游连接失败、私网目标被本地阻止或配置错误。客户端可能收到 502，但审计不会把本机生成的失败伪装成上游 5xx。

### 11.6 分页

| 参数 | 默认 | 有效行为 |
|---|---:|---|
| `page` | `1` | 小于 1 会按 1 处理 |
| `page_size` | `50` | 小于等于 0 或大于 200 时按 50 处理 |

“自动刷新”是页面每 5 秒重新查询一次，不是服务端持久化设置。

---

## 12. 更新记录页面的查询参数

更新记录追踪账号映射发生了什么变化，与访问审计分开。

### 12.1 `kind`

当前值：

| 值 | 含义 |
|---|---|
| `direct_file` | CPA 认证文件变化 |
| `direct_incremental` | Sub2API PostgreSQL 增量读取 |
| `api_sync` | CLIProxyAPI/Sub2API 全量 API 同步 |
| `push` | 手动登记或程序推送 |
| `delete` | 删除账号/凭据 |

### 12.2 `status`

```text
ok
error
```

### 12.3 `source`

同步源会显示为：

```text
src:<数据库ID>
```

页面会把它映射成同步源名称。手动登记的来源是：

```text
api
```

### 12.4 分页和时间范围

与访问审计相同：

```text
range = 1h | 24h | 7d | all
page >= 1
page_size <= 200，默认 50
```

更新记录与访问审计共用 `log_retention_days` 保留周期，但清空其中一个页面不会清空另一个页面。

---

## 13. 管理 API 自动化填写参考

所有管理 API 都需要管理员登录后的会话 Cookie。前端默认发送：

```http
Content-Type: application/json
Cookie: sticky_session=<会话值>
```

请求体有两个通用限制：

- 最大约 1 MiB；
- 必须正好包含一个 JSON 值，尾部不能再拼第二个 JSON。

### 13.1 登录

```http
POST /api/auth/login
Content-Type: application/json

{"password":"<管理员口令>"}
```

成功后服务器设置会话 Cookie。连续错误登录会触发按来源 IP 的退避，不要用循环脚本反复猜口令。

### 13.2 修改管理员口令

```http
POST /api/auth/password
Content-Type: application/json

{
  "old_password": "<旧口令>",
  "new_password": "<新口令，至少6个字符>"
}
```

成功后旧会话会被吊销，需要重新登录。

### 13.3 读取基础设置

```http
GET /api/settings
```

返回中以下字段是只读提示：

```text
ingress_url
ingress_url_auth
hash_salt
```

以下字段是配置字段：

```text
listen_tls_cert
listen_tls_key
admin_tls_cert
admin_tls_key
listen_auth
default_upstream
no_marker_policy
marker_path_parts
marker_headers
sid_len
session_ttl_min
salt_rotate_failure_threshold
log_retention_days
metrics_enabled
sync_empty_clear_threshold
block_private_targets
acl_whitelist
acl_blacklist
```

### 13.4 保存基础设置

```http
PUT /api/settings
Content-Type: application/json

{
  "listen_tls_cert": "",
  "listen_tls_key": "",
  "admin_tls_cert": "",
  "admin_tls_key": "",
  "listen_auth": "",
  "default_upstream": "us-residential-main",
  "no_marker_policy": "default_session",
  "marker_path_parts": [],
  "marker_headers": [
    "Authorization",
    "x-api-key",
    "api-key",
    "x-goog-api-key"
  ],
  "hash_salt": "<从GET读取的非空盐，不能省略>",
  "sid_len": 16,
  "session_ttl_min": 0,
  "salt_rotate_failure_threshold": 2,
  "log_retention_days": 30,
  "metrics_enabled": false,
  "sync_empty_clear_threshold": 3,
  "block_private_targets": true,
  "acl_whitelist": [],
  "acl_blacklist": []
}
```

注意：

- 这是全量设置保存，不要把它当成任意字段 PATCH；
- `hash_salt` 必须非空，不能随便写新字符串；
- `marker_headers` 不能是空数组；
- `marker_path_parts` 的每一项必须以 `/` 开头；
- `default_upstream` 如果非空，必须指向已启用上游；
- `acctmap_enabled` 即使放进 JSON 也不会被这个保存接口作为新值使用；
- `block_private_targets` 在兼容旧客户端时可以省略，省略表示沿用当前值；
- `GET /api/settings` 会在已认证的管理台会话中原样返回 `listen_auth`；`PUT /api/settings` 仍兼容用 `____` 或 `__unchanged__` 表示沿用旧密码；
- TLS 路径有变化时响应中的 `restart_required` 会是 `true`。

### 13.5 重置盐

```http
POST /api/settings/reset-salt
```

不需要请求体。它会重新生成盐并导致全体身份重新分配出口。

### 13.6 新增上游

普通上游：

```http
POST /api/upstreams
Content-Type: application/json

{
  "name": "plain-office-proxy",
  "platform": "plain",
  "base_url": "http://user:pass@proxy.example.com:8080",
  "enabled": true
}
```

Decodo：

```json
{
  "name":"decodo-us",
  "platform":"decodo",
  "base_url":"http://user-login-country-us:password@gate.decodo.com:7000",
  "enabled":true
}
```

Generic：

```json
{
  "name":"generic-sticky",
  "platform":"generic",
  "base_url":"http://user:password@gateway.example.com:8080",
  "inject":{
    "username_template":"{user}-session-{sid}"
  },
  "enabled":true
}
```

创建时：

- `name` 和 `base_url` 必填；
- `platform` 必须是已注册平台；
- `plain` 的 `inject` 必须为空/省略；
- `generic` 必须有非空 `inject.username_template`；
- `enabled` 省略时默认为 `true`。

### 13.7 编辑上游

编辑时可以提交完整对象，也可以利用当前 API 的部分字段语义：

```json
{
  "name":"decodo-us-new",
  "base_url":"http://user-login-country-us:____@gate.decodo.com:7000",
  "enabled":true
}
```

其中 `____` 表示沿用旧密码。Generic 的 `inject.password` 同样支持掩码保留。

如果把平台改成 `generic`，要同时提供合法模板；如果把平台改成 `plain`，必须去掉 `inject`。

### 13.8 设为默认和测试

```http
POST /api/upstreams/<id>/default
```

```http
POST /api/upstreams/<id>/test
```

设为默认要求条目启用。测试会使用注入后的健康检查身份，并通过外部 IP/地理信息探测服务尝试显示出口 IP；测试失败不代表上游配置一定无法处理所有业务，但通常应先解决测试返回的连接、认证或协议错误。

### 13.9 新增同步源

CLIProxyAPI：

```json
{
  "kind":"cpa",
  "name":"cpa-production",
  "base_url":"https://cpa.example.com",
  "api_key":"<management-key>",
  "interval_s":600,
  "enabled":true,
  "direct_auth_dir":"/opt/cliproxyapi/auths"
}
```

Sub2API：

```json
{
  "kind":"sub2api",
  "name":"sub2api-production",
  "base_url":"https://sub2api.example.com",
  "api_key":"<admin-api-key>",
  "interval_s":600,
  "enabled":true,
  "direct_db_dsn":"postgres://readonly:password@127.0.0.1:5432/sub2api?sslmode=disable"
}
```

### 13.10 编辑同步源和清除 DSN

已有源的 API Key 留空表示沿用旧值：

```json
{
  "name":"sub2api-production",
  "api_key":"",
  "interval_s":600,
  "enabled":true
}
```

清除 Sub2API 增量 DSN：

```json
{
  "kind":"sub2api",
  "direct_db_clear":true
}
```

不能同时发送：

```json
{
  "direct_db_clear":true,
  "direct_db_dsn":"postgres://..."
}
```

### 13.11 手动登记账号

路径参数 `<platform>` 和 `<account>` 必须进行 URL 编码。请求体：

```http
PUT /api/acctmap/openai/user%40example.com
Content-Type: application/json

{
  "source_type":"manual-import",
  "access_token":"<AT>",
  "refresh_token":"<RT>"
}
```

要求：

- platform/account 非空；
- source_type 非空且不超过 64 字符；
- AT/RT 至少一项非空。

### 13.12 账户方向绑定

```http
PUT /api/acctegress/<platform>/<account>
Content-Type: application/json

{
  "mode":"sticky",
  "egress_ids":[3,7]
}
```

空数组等价于清除该账号绑定：

```json
{"mode":"sticky","egress_ids":[]}
```

### 13.13 出站方向批量绑定

只能对 `platform=plain` 的出站 ID 使用：

```http
PUT /api/acctegress/egress/<plain-upstream-id>
Content-Type: application/json

{
  "mode":"random",
  "accounts":[
    {"platform":"openai","account":"user@example.com"},
    {"platform":"grok","account":"grok@example.com"}
  ]
}
```

未知账号会导致整单失败，不会静默只保存一部分。

### 13.14 审计查询 API

```text
GET /api/logs
```

可用查询参数：

```text
from       Unix 毫秒起始时间
to         Unix 毫秒结束时间
q          host/path 子串
account    真实账号或 account_fp 精确值
upstream   上游名称精确值
class      2xx | 4xx | 5xx | err
page       页码
page_size  1–200，默认 50
```

### 13.15 更新记录查询 API

```text
GET /api/updates
```

可用查询参数：

```text
from
to
kind      direct_file | direct_incremental | api_sync | push | delete
source    src:<id> 或 api
status    ok | error
page
page_size
```

---

## 14. 构建脚本参数（不是运行配置）

下面这些参数属于 `scripts/build.sh`，只影响构建产物，不会写入 `router.db`，也不是运行时管理台设置。

```bash
./scripts/build.sh [options]
```

| 参数 | 默认值 | 用法 |
|---|---|---|
| `--os OS` | `linux` | 目标操作系统，例如 `linux` |
| `--arch ARCH` | `amd64` | 目标架构，例如 `amd64`、`arm64` |
| `-o PATH` / `--output PATH` | `bin/mitmrouter-OS-ARCH` | 输出二进制路径 |
| `--debug` | 关闭 | 保留 Go debug symbol；默认构建会去符号 |
| `--skip-web` | 不启用 | 复用已有 `internal/webui/dist`，不重新构建前端 |
| `-h` / `--help` | — | 显示帮助 |

示例：

```bash
# 默认 Linux amd64 静态二进制
./scripts/build.sh

# Linux arm64
./scripts/build.sh --os linux --arch arm64

# 自定义输出路径并保留调试符号
./scripts/build.sh --output ./mitmrouter --debug

# 复用已有前端构建产物
./scripts/build.sh --skip-web
```

`--skip-web` 要求 `internal/webui/dist/index.html` 已经存在，否则构建失败。

---

## 15. 测试工具环境变量（不是生产服务配置）

正式运行的 `mitmrouter` 不读取环境变量来代替上述配置。仓库中出现的以下环境变量只用于测试或实时 E2E 工具。

### 15.1 长耗时测试开关

```bash
MITMROUTER_RUN_BENCHMARK=1
MITMROUTER_RUN_LOAD=1
```

它们只决定是否运行特定 benchmark/load 测试，不会改变生产服务的监听、路由或上游配置。

### 15.2 `tools/e2elive` 的真实环境参数

运行真实双源 E2E 测试时需要：

```bash
E2E_SUB2API_URL=<Sub2API管理地址>
E2E_SUB2API_KEY=<Sub2API管理Key>
E2E_CPA_URL=<CLIProxyAPI管理地址>
E2E_CPA_KEY=<CLIProxyAPI管理Key>
```

例如：

```bash
E2E_SUB2API_URL=https://sub2api.example.com \
E2E_SUB2API_KEY='<secret>' \
E2E_CPA_URL=https://cpa.example.com \
E2E_CPA_KEY='<secret>' \
go run ./tools/e2elive
```

这些变量不会被 `mitmrouter` 主程序读取，也不要把它们误填到管理台的 `listen_auth` 或上游 `base_url` 中。

---

## 16. 常见配置组合

### 16.1 本机 + API Key + 一个住宅上游

```text
启动：
  -data ./data
  -addr 127.0.0.1:55666
  -admin-addr 127.0.0.1:55667

基础设置：
  listen_auth = 空（只限本机时可关闭）
  marker_path_parts = 空
  marker_headers = 默认四项
  default_upstream = 你新增的供应商条目
  no_marker_policy = default_session
  block_private_targets = true
  acl_whitelist = 空
  acl_blacklist = 空

客户端：
  http://127.0.0.1:55666
```

第一次使用前安装 `ca.pem`，因为默认会 MITM HTTPS。

### 16.2 订阅号 AT/RT + 账号级粘滞

```text
1. 新增 cpa 或 sub2api 同步源
2. 填服务根 base_url，不要把 API 路径和 key 放进 URL
3. 填 api_key
4. interval_s 先用 600
5. 立即测试，再立即同步
6. 在当前账号映射中确认 platform/account 出现
7. default_upstream 选择粘滞供应商条目
8. 用真实请求检查同账号 token 轮换后出口是否保持
```

如果使用 plain 出站绑定：

```text
1. 先新增多个 platform=plain 的出站
2. 在账号上选择 sticky 或 random
3. 选择 plain 出站集合
4. 保存后绑定优先于 default_upstream
```

### 16.3 公网接入口、内网管理台

推荐让接入口和管理台都监听内网/公网可达地址，但用网络策略隔离管理面：

```text
-addr       0.0.0.0:55666
-admin-addr 127.0.0.1:55667
```

外部管理通过 SSH 隧道访问管理台；接入口配置：

```text
listen_tls_cert/key = 已签发证书对
listen_auth         = 随机长用户名/密码
```

这是比直接把管理台 `0.0.0.0:55667` 暴露出来更简单的方案。

### 16.4 只对指定目标做 MITM

例如只允许访问 OpenAI 目标，其他目标直接拒绝：

```text
acl_whitelist =
  api.openai.com
  *.openai.com

acl_blacklist = 空
```

客户端只有访问白名单目标时需要信任 MITM CA。注意：未命中目标会收到本地 403，不会被转发。

### 16.5 不带身份的流量直连

```text
no_marker_policy = direct
```

这只改变没有 Marker 的请求；有 Marker 的请求仍按上游和账号绑定处理。`block_private_targets` 仍然建议保持开启。

---

## 17. 常见错误和排查顺序

### 17.1 启动后管理台打不开

按顺序检查：

```text
1. -admin-addr 是否真的是你访问的地址
2. 是否把客户端接入口端口误当成管理台端口
3. 是否启用了 admin TLS，却仍用 http://
4. 证书和私钥路径是否可读、是否匹配
5. 监听端口是否已被其他进程占用
6. 是否被防火墙拦截
```

### 17.2 客户端收到 407

如果是刚连到接入口：

```text
检查 listen_auth
检查客户端是否发送 Proxy-Authorization
检查代理 URL 中用户名/密码是否被 URL 编码
```

如果审计或日志表明是上游 CONNECT 407：

```text
检查上游 base_url 的用户名、密码和 platform
确认没有把管理 API Key 当成上游代理密码
```

### 17.3 客户端提示自签证书

通常是：

```text
客户端没有安装 MITMRouter 下载的 ca.pem
```

请确认：

- 客户端实际使用的是这台实例生成的 CA；
- 容器内的 CA bundle 已更新；
- 更新 bundle 后进程/容器已经重启；
- 你没有误把监听 TLS 证书当成 MITM 根 CA。

如果 ACL 白名单为空，几乎所有 HTTPS 目标都会遇到这个要求。

### 17.4 上游能认证，但出口没有粘滞

依次检查：

```text
1. platform 是否选对
2. DataImpulse 是否使用 sessid 语法
3. Decodo 用户名是否以 user- 开头
4. 1024proxy 是否保留控制台生成的 region/t 等参数
5. Resin 用户名是否非空
6. Generic 的 username_template 是否正确
7. session_ttl_min 是否与 Generic 的 {ttl_min} 兼容
8. 审计中的 account_fp 是否因凭据/账号变化而变化
```

平台选错是最常见原因：上游只校验 API Key 和密码时，错误 platform 也可能“看起来能用”。

### 17.5 保存基础设置时报字段错误

| 错误类型 | 常见原因 |
|---|---|
| `invalid_listen_auth` | 用户名/密码只有一项，或为空密码 |
| `invalid_tls_pair` | TLS 证书、私钥没有成对填写，或文件解析失败 |
| `invalid_policy` | `no_marker_policy` 不在三个枚举中 |
| `invalid_rules` | Header 列表为空，或路径片段不以 `/` 开头 |
| `invalid_salt` | `hash_salt` 被删空 |
| `invalid_sidlen` | `sid_len` 不在 `4–64` |
| `invalid_ttl` | `session_ttl_min` 小于 0 或超过 API 上限 |
| `invalid_acl` | IP/CIDR/域名/通配符格式不合法，或超过 500 条 |
| `invalid_retention` | 保留天数不在 `1–3650` |
| `invalid_sync_empty_clear_threshold` | 阈值不在 `1–100` |
| `unknown_upstream` | 默认上游不存在或已停用 |

### 17.6 Generic 保存失败

优先检查：

```text
1. inject 是否是合法 JSON 对象
2. username_template 是否非空
3. 是否误写了不支持的占位符
4. 是否有未配对的花括号
5. 模板是否含 {ttl_min}，但 session_ttl_min 仍是 0
6. platform 是否真的选了 generic
```

### 17.7 同步源一直没有账号

检查：

```text
1. base_url 是否是服务根地址
2. api_key 是否是管理 API Key，而不是业务 token
3. CLIProxyAPI 的 provider 是否在支持映射内
4. Sub2API 的 type 是否为 oauth/setup-token
5. Sub2API platform 是否在支持映射内
6. 账号是否真的包含 email/name 与 AT/RT
7. “最近同步”状态和更新记录中的 error 详情
8. 是否只是空快照保护尚未达到阈值
```

### 17.8 账号已经绑定，但请求仍然 502

重点看：

```text
绑定的 egress 是否都是 platform=plain
plain 条目是否被停用或删除
绑定账号是否仍存在于 acct_map
是否所有绑定出站都不可用
```

有明确绑定但候选全部不可用时，程序不会悄悄绕过绑定走默认上游；先恢复或调整绑定。

### 17.9 改了设置但像没生效

按参数类别判断：

| 改动 | 是否需要重启 |
|---|---:|
| `-addr` | 是，修改启动命令后重启 |
| `-admin-addr` | 是，修改启动命令后重启 |
| TLS 路径 | 是 |
| 同一路径下证书文件内容 | 通常不需要，约 60 秒热加载 |
| `listen_auth` | 否，保存后热生效 |
| 默认上游 | 否 |
| Marker 规则 | 否 |
| ACL | 否 |
| 上游条目 | 否 |
| 账号映射/绑定 | 否 |
| 日志保留、metrics、空快照阈值 | 否 |

不要直接改 `router.db` 期待运行中的内存快照自动发现变化。运行期配置应通过管理台/API 保存。

---

## 18. 不要忽略的运行限制

这些不是可填写参数，但会影响你对参数效果的预期：

1. **粘滞是尽力而为。** 供应商回收住宅节点、会话到期或节点离线后，同一 sid 仍可能得到新 IP。
2. **不支持 UDP/QUIC。** 客户端如果使用 HTTP/3/UDP 443，可能绕过本代理；需要时在系统层限制 UDP 443。
3. **证书固定客户端无法正常 MITM。** 即使系统信任了根 CA，应用自己的 certificate pinning 仍可能拒绝。
4. **默认全量 MITM 需要客户端信任根 CA。** 这不是上游配置错误。
5. **`router.db` 是高敏感文件。** 持有它的人可能恢复 CA 私钥、上游凭据哈希/材料和其他实例秘密。
6. **监听地址与上游地址是两件事。** `-addr`/`-admin-addr` 是本服务监听；`base_url` 是本服务连接的外部代理。
7. **管理台口令与入站认证不是一套凭据。** 修改一个不会自动修改另一个。

---

## 19. 参数来源索引

如果需要核对当前实现，主要代码位置如下：

| 内容 | 代码位置 |
|---|---|
| 运行参数 | `cmd/mitmrouter/main.go` |
| 基础设置结构、默认和加载 | `internal/settings/settings.go` |
| 默认数据库设置 | `internal/store/store.go` |
| 基础设置 API 与校验 | `internal/api/api.go` |
| Marker 提取 | `internal/marker/extract.go` |
| ACL 格式和判定 | `internal/acl/acl.go` |
| 上游通用校验与 Generic 模板 | `internal/upstream/upstream.go`、`internal/upstream/generic.go` |
| DataImpulse | `internal/upstream/dataimpulse.go` |
| Decodo | `internal/upstream/decodo.go` |
| 1024proxy | `internal/upstream/c1024.go` |
| Resin | `internal/upstream/resin.go` |
| plain | `internal/upstream/plain.go` |
| 同步源 API | `internal/api/acctmap.go` |
| CLIProxyAPI 同步 | `internal/syncer/cpa.go` |
| Sub2API 同步 | `internal/syncer/sub2api.go` |
| CPA 目录增量 | `internal/syncer/cpa_direct.go` |
| Sub2API PostgreSQL 增量 | `internal/syncer/sub2api_direct.go` |
| 账号映射 | `internal/acctmap/acctmap.go` |
| 账号出站绑定 API | `internal/api/acctegress.go` |
| 路由优先级和私网保护 | `internal/server/ingress.go` |
| 管理台字段和输入控件 | `web/src/views/Settings.vue`、`Upstreams.vue`、`AcctMap.vue` |
| 审计查询页面 | `web/src/views/Audit.vue` |
| 更新记录页面 | `web/src/views/Updates.vue` |
| 供应商官方语法调研 | `docs/006-sticky-session-credentials.md` |
| 生产部署步骤 | `DEPLOY.md` |

如果代码和旧文档之间出现差异，应优先以当前运行中的页面校验结果和当前源码为准；尤其注意：

- `sid_len` 当前保存范围是 `4–64`；
- 页面 `session_ttl_min` 上限是 `1440`，API 后端上限是 `10080`；
- ACL 黑名单命中或白名单未命中时会本地 403，不会连接目标；
- DataImpulse 当前内置注入器不会根据 `session_ttl_min` 自动注入 `sessttl`；
- `acctmap_enabled` 当前不是基础设置页面中的可编辑字段。

---

## 20. 一份可以照着做的最终检查清单

### 启动层

- [ ] `-data` 指向持久化目录，并且备份策略不会公开 `router.db`；
- [ ] `-addr` 与 `-admin-addr` 不同；
- [ ] 非回环接入口已配置入站认证和网络边界；
- [ ] 非回环管理台已配置 TLS 或被防火墙/SSH 隧道保护；
- [ ] 没有长期打开 `-trace-file`。

### 基础设置

- [ ] `listen_auth` 用户名和密码同时填写或同时留空；
- [ ] 两个接入口 TLS 路径成对；
- [ ] 两个管理台 TLS 路径成对；
- [ ] 已下载正确实例的 `ca.pem` 并安装到实际客户端；
- [ ] `marker_path_parts` 不必要时保持空；
- [ ] `marker_headers` 非空且顺序合理；
- [ ] `sid_len` 在 `4–64`，通常使用 `16`；
- [ ] `session_ttl_min` 与供应商套餐和 Generic 模板一致；
- [ ] `default_upstream` 指向启用中的条目；
- [ ] `no_marker_policy` 是预期的三个值之一；
- [ ] `block_private_targets` 保持开启，除非有明确的隔离方案；
- [ ] ACL 白名单/黑名单的访问拒绝语义已经确认；
- [ ] `log_retention_days` 与磁盘/审计要求匹配；
- [ ] 空快照保护阈值不是误填的 `1`。

### 上游

- [ ] `platform` 与实际供应商一致；
- [ ] `base_url` 是供应商控制台生成的代理 URL；
- [ ] 用户名/密码中的 URL 特殊字符已正确编码；
- [ ] Decodo 用户名以 `user-` 开头；
- [ ] 1024proxy 保留了控制台的 `region`、`sid`、`t` 等结构；
- [ ] Resin 用户名非空；
- [ ] Generic 模板 JSON 合法且占位符受支持；
- [ ] plain 条目没有填 `inject`；
- [ ] 至少一个上游已启用，并已测试出口 IP；
- [ ] 已把正确条目设为默认。

### 账号管理

- [ ] 同步源 `base_url` 不包含 userinfo、query、fragment；
- [ ] API Key 填的是管理接口密钥；
- [ ] `interval_s >= 60`；
- [ ] CPA 增量目录是绝对路径、真实目录、非符号链接；
- [ ] Sub2API 远程 DSN 使用 `sslmode=verify-full`；
- [ ] 手动账号使用稳定账号标识而不是 token；
- [ ] AT/RT 至少填写一项；
- [ ] 登记平台名与目标主机内置归类一致；
- [ ] 账号出站绑定只选择 `plain` 条目；
- [ ] 停用/删除出站前已检查其绑定账号。

完成这份清单后，再用实际客户端发起请求，并结合“访问审计”和“更新记录”验证结果，不要只根据页面里“已保存”就判断业务链路已经正确。

---

*本文只新增和说明配置，不改变 MITMRouter 的转发实现。*
