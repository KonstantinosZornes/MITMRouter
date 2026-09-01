# 住宅出口平台粘滞会话(Sticky Session)接入凭据调研

> 用途：Go 程序按「每账号固定 session id 拼进出口用户名」方式对接三家住宅出口平台。
> 结论速览：**三家的会话参数全部拼在“用户名”里，密码字段保持原样**；TTL 单位三家均为**分钟**。
> 调研方式：全部关键语法逐条核对各家官方文档原文（docs/help 站点 Markdown 版），非二手转述。

## Decodo

- **网关**: `gate.decodo.com`（全球随机池入口）
  - **HTTP / HTTPS / SOCKS5 共用 `7000` 端口**，协议由 URL scheme 决定（SOCKS5 写 `socks5h://gate.decodo.com:7000`），无独立 SOCKS5 端口
  - `7000` — 轮换端口 + backconnect 参数入口（程序化对接推荐）；`10000` — 备用轮换端口（白名单 IP 的 session 子域名写法也用它）
  - `10001–49999` — Sticky 端口段（端口号即会话桶，同端口同 IP；改端口或改 session id 即提前换 IP）
  - 国家端点如 `us.decodo.com` / `de.decodo.com` … 各带专属 Sticky 端口段与轮换端口；**SOCKS5 仅 `gate.decodo.com` 支持**，国家定向需改用 `-country-xx` 参数；HTTP/3 端点为 `socks5-gate.decodo.com:10000`
  - 旧域名 `gate.smartproxy.com` 目前解析到与 `gate.decodo.com` 相同的网关 IP（兼容残留，勿依赖）
- **用户名模板**（注意：参数名是 `session` 和 `sessionduration`；旧 Smartproxy 社区流传的 `-sessid-…`/`-sesstime-…` 在现行官方文档中**不存在**）：

  ```
  user-{user}-session-{sessid}-sessionduration-{ttl}
  ```

  - 官方完整示例（逐字）：`curl -U "user-username-country-us-city-chicago-session-example1-sessionduration-90:password" -x "gate.decodo.com:7000"` —— 顺序 `user-{user}[-country-xx][-city-yy]-session-{sessid}-sessionduration-{ttl}`，参数均以连字符 `-` 连接、全小写
  - `{user}` = 控制台创建的出口用户名；`user-` 前缀强制；密码独立字段
  - ⚠️ 官方 Warning：**SOCKS5 粘滞必须同时携带 `session` 参数**——只给 `sessionduration` 仍会每次轮换
  - IP 锁定变体（IP 失联不换新 IP、返回 502，仅 user:pass 方式）：`user-{user}-session_iplock-{sessid}-sessionduration-{ttl}`
  - IP 白名单认证改为子域名形式：`{sessid}-sessionduration-{ttl}.gate.decodo.com:10000`
- **密码**: 独立密码字段（Basic Auth），任何会话参数都不进密码。
- **会话时长**（文档声明值）：
  - 默认 **10 分钟**
  - 控制台预设 1 / 10 / 30 / 60 分钟，或 `sessionduration` 自定义 **1–1440 整数（分钟）**，最长 24 小时
  - FAQ 补充：计时从首个请求开始；IP 无响应或 **60 秒无活动**即结束会话；会话越长因住宅设备离线提前轮换的概率越高
- **来源**:
  - [User:pass Requests [Residential]](https://help.decodo.com/docs/residential-proxy-user-pass-requests)
  - [Custom Sticky Sessions [Residential]](https://help.decodo.com/docs/residential-proxy-custom-sticky-sessions)
  - [Endpoints and Ports [Residential]](https://help.decodo.com/docs/residential-proxy-endpoints-and-ports)
  - [Session Types [Residential]](https://help.decodo.com/docs/residential-proxy-session-types)
  - [Protocols [Residential]](https://help.decodo.com/docs/residential-proxy-protocols)
  - [Sticky and Rotating Sessions FAQ](https://decodo.com/faq/getting-started/what-are-sticky-and-rotating-sessions)
  - [Quick Start [Residential]](https://help.decodo.com/docs/residential-proxy-quick-start)

## DataImpulse

- **网关**: `gw.dataimpulse.com`（备用直连 IP `74.81.81.81`，官方建议用域名）
  - `823` — Rotating（每请求换 IP），HTTP/HTTPS 入口
  - `824` — Rotating，SOCKS5 入口
  - `10000–20000` — Sticky 端口段（端口即会话：一个端口绑定一个 IP 一段时间；HTTP/SOCKS5 共用该段，靠 scheme 区分）
- **用户名模板**（官方规范：登录名后跟双下划线 `__`，键值用 `.`，多参数用 `;`，多值用 `,`；**正确参数名是 `sessid`，社区流传的 `__sid.xxx` 在官方文档中不存在**）：

  ```
  {user}__cr.{country};sessid.{sessid};sessttl.{ttl}
  ```

  - 最简粘滞形式：`{user}__sessid.{sessid}`
  - 官方原文示例：`curl -x "http://login__cr.au;sessid.123:password@gw.dataimpulse.com:823" https://api.ipify.org/`
  - `sessid.<任意字符串或数字>` —— 同一 sessid 在时长内固定映射同一出口 IP
  - `sessttl.<分钟>` —— Sticky 的轮换间隔（`sessttl.60` = 每 60 分钟换）；主要配合 10000–20000 端口使用
  - 注意：`sessid`（凭据级固定映射）与 `sessttl`/Sticky 端口是两套机制，官方称前者是后者的 “alternative”，二者组合未获官方明确背书
- **密码**: 独立密码字段，会话参数只进用户名；凭据由后台 Proxy Access 自动生成，可重置。
- **会话时长**（文档声明值）：
  - 仅用 `sessid`：平均保持 **30 分钟**，文档未提供针对 sessid 的自定义 TTL 语法
  - Sticky 端口：可在 **1–120 分钟**内设置（Dashboard 或 `sessttl.<分钟>`）；不指定或设 `"0"` 默认 **30 分钟**
  - IP 下线时自动替换为其他可用 IP
- **来源**:
  - [Session Id](https://docs.dataimpulse.com/proxies/parameters/session-id)
  - [Session Interval](https://docs.dataimpulse.com/proxies/parameters/session-interval)
  - [Parameters](https://docs.dataimpulse.com/proxies/parameters)
  - [Types of connections](https://docs.dataimpulse.com/proxies/types-of-connections)
  - [Connection Hosts](https://docs.dataimpulse.com/proxies/connection-hosts)
  - [User:pass authentication](https://docs.dataimpulse.com/authentication-methods/user-pass-authentication)

## 1024proxy

- **网关**: `*.1024proxy.io`（官方仅提供美国/香港两个转发区域）
  - 文档中唯一完整 host:port 字面值为 **`us.1024proxy.io:3000`**（动态住宅/端口套餐示例）；香港线路主机 `hk.1024proxy.io` 真实存在但文档未给出端口号字面值，**实际 host:port 以控制台生成的接入命令为准**；无限流量-带宽套餐主机形如 `bwxxx.1024proxy.io`
  - HTTP(S) 与 SOCKS5 使用同一 host:port（官方两种协议示例同地址），协议由客户端用法决定
  - 注意：`proxy.1024proxy.com` / `forward.1024proxy.com` 均不存在（NXDOMAIN），端口 31110~31113 未见于任何官方文档
- **用户名模板**（官方语法为 `-sid-…-t-…`；提问者猜测的 `-sessid-…-sesstime-…` 与官方不符）：

  ```
  {user}-region-{country}-sid-{sessid}-t-{ttl}
  ```

  - 轮换模式（不带 `-sid-` 即每请求换 IP）：`{user}-region-{country}`
  - 粘滞模式（官方原文示例）：`curl -x HOST:PORT -U "USERNAME-region-DE-sid-ENQzeWjG-t-5:PASSWORD" ipinfo.io`
  - 参数全小写、连字符连接、无 zone 前缀；国家代码 ISO 3166-1 两位；可扩展 `-st-{州}` / `-city-{城市}` / `-asn-{AS号}`（st/city 与 asn 二选一）
  - 官方所有粘滞示例都带 `-region-XX`，未见省略 region 的示例——稳妥做法是保留 `-region-US` 之类
  - `{sessid}` 无官方字符集/长度约束；两处官方示例值均为 8 位大小写字母数字混合（`ENQzeWjG`、`XX7t3mXR`）
- **密码**: 独立密码字段原样使用；动态住宅支持子账号按 GB 分流（带宽套餐仅单主账号）。
- **会话时长**（文档声明值，按套餐有差异）：
  - 动态住宅流量：粘性 **1–120 分钟**（API 提取模式的粘性档位为 10/15 分钟）
  - 无限流量-带宽：粘性 **1–120 分钟**
  - 无限流量-端口：仅粘性，**3–30 分钟**
  - 超过 120 分钟需改用长效静态 ISP 产品线；文档未说明只带 `-sid-` 不带 `-t-` 时的默认时长
- **替代接入**: IP 白名单 + API 提取（`https://white.1024proxy.com/white/api?region=US&num=1&time=10&format=1&type=txt` 返回 `ip:port` 列表）。
- **来源**:
  - [Session Management（动态住宅）](https://help.1024proxy.com/dynamic-residential-traffic/session-management)
  - [Session Management（无限流量-端口，3–30min 仅粘性）](https://help.1024proxy.com/unlimited-residential-traffic-port/session-management)
  - [Username & Password Authentication](https://help.1024proxy.com/dynamic-residential-traffic/username-and-password-authentication)
  - [Location Configuration](https://help.1024proxy.com/dynamic-residential-traffic/location-configuration)
  - [Network Protocol](https://help.1024proxy.com/dynamic-residential-traffic/network-protocol)
  - [API 提取模式](https://help.1024proxy.com/dynamic-residential-traffic/api)

## 通用结论

1. **三家会话参数都拼在用户名里**，密码字段一律原样放置 —— 适配器只需对用户名字符串做模板化，无需动密码。
2. **TTL 单位统一为分钟**，但取值范围不同：Decodo `1–1440`（默认 10）、DataImpulse `1–120`（默认 30，仅 Sticky 端口/sessttl 可调；`sessid` 固定约 30 分钟不可调）、1024proxy 主流套餐 `1–120`（无限流量-端口套餐仅 3–30 且只有粘性）。适配器应做 clamp。
3. **模板化适配器需要的占位符集合**：`{user}`、`{password}`、`{sessid}`、`{ttl_min}`，外加可选 `{country}`。各家的拼接规则差异只在分隔符与参数名：
   - Decodo：`user-{user}-session-{sessid}-sessionduration-{ttl_min}`（前缀 `user-` 强制，`-` 连接）
   - DataImpulse：`{user}__sessid.{sessid}`（`__` 进入参数区，`.` 连接键值，`;` 连接多参数）
   - 1024proxy：`{user}-region-{cc}-sid-{sessid}-t-{ttl_min}`（`-` 连接）
4. **sessid 字符集**：三家均只要求“任意字符串/数字”，但为避免与分隔符冲突（`-`、`__`、`.`、`;`），Go 侧生成时建议统一 `[a-zA-Z0-9]`、≤32 字符。
5. **行为差异需在适配层处理**：Decodo 有 `session_iplock` 可选“IP 死了不换、报 502”，且会话在 **60 秒无活动后过期**（空闲长连接场景需保活）；DataImpulse 与 1024proxy 在 IP 失效时自动换新 IP 且不保证通知。Decodo/DataImpulse 另有基于端口的 Sticky 形态，若想纯凭据驱动则分别用 `7000`+参数 / `823`+`sessid`。
