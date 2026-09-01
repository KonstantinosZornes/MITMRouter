# 功能测试方案 v2：破坏性在本地，非破坏性在远端

## 一、远端（现有生产部署）——只做非破坏性验证

全部通过 `ssh mitm-vps` 在远端 127.0.0.1 执行，**零写操作**（不登录改配置、不调写 API、不动数据）。

1. **免密登录**：`ssh -o BatchMode=yes mitm-vps 'echo ok'` 验证；不通则 `ssh-copy-id` 补公钥。
2. **服务体检**（[DEPLOY.md](../DEPLOY.md) §5）：`systemctl is-active`、双端口监听、journalctl `started` 行 ingress_tls/admin_tls=true、二进制时间戳、数据目录 0700/0600、近 24h 无 `forward failed` 刷屏/panic。
3. **管理台只读链路**（127.0.0.1:55667，curl -k）：`/ui/` 200 → 未登录 401 → 用你给的口令登录 → `auth/me`、`settings`（读 listen_auth 供下一步，报告脱敏）、`upstreams`、`sources`、`acctmap`、`acctegress`、`updates`、`logs`、`ca.pem`、`/metrics`（若开启）→ 校验响应无 Token 原文 → logout 后 401。
4. **接入口非破坏场景**（127.0.0.1:55666）：无凭据 407；错误凭据 407；正确凭据 `api.ipify.org` 返回出口 IP；明文 http 连接被拒（TLS-only）；`169.254.169.254` → 403；**透明转发一致性**：经代理 vs 直连取同一页面响应体逐字节一致；同凭据连续多次请求审计日志 account_fp/upstream 稳定（粘滞读验证）。

## 二、本地（本机 WSL2）——破坏性场景，随便折腾

**环境**：直接跑今天构建的 `bin/mitmrouter-linux-amd64`，临时目录 `/tmp/mitmtest/`，接入口 127.0.0.1:45666、管理台 45667；3 个 mock 出口 `go run ./tools/mockexit`（18081/18082/18083，各自 logfile）；首启口令从进程 stdout 抓。测完 kill 进程 + 删临时目录。若二进制与 HEAD 存疑先 `./scripts/build.sh` 重建。

**场景清单**（编号对应 docs/011 §8 验收表）：
- L1 首启引导：一次性管理员口令只打印一次；数据目录自动收紧 0700/库 0600
- L2 管理台写路径：改设置热生效、upstream 增/改/删、设默认、upstream test 动作
- L3 入站认证：407 → 配 listen_auth 热生效放行 → 改密后旧凭据失效
- L4 TLS：明文拒；自签证书配 TLS 路径 → 重启生效（验证"仅 TLS 路径变更需重启"）
- L5 `block_private_targets`：内网/云元数据 403
- L6 ACL：空白名单默认全部放行（HTTPS 默认 MITM）→ 加白后仅名单内放行、名单外 403
- L7 粘滞（011-1）：同身份多请求 upstream 恒定，**重启进程后仍恒定**
- L8 随机模式（011-3）：多次请求覆盖全部候选出站
- L9 粘滞池增删出站（011-2）：仅命中被删出站的账户换，其余不动
- L10 绑定命中不走默认（011-4）：手动登记账号（AT/RT）建映射 + 绑定出站
- L11 绑定出站全停用 → 502 受控失败，不回落（011-6）
- L12 删出站级联清理绑定（011-8）；删账户清理绑定（011-7）
- L13 盐轮换（011-10）：reset-salt 逃生阀；上游指死端口模拟连续失败 → 自动换出站
- L14 `acctmap_enabled=false` 回落纯 Marker（011-12）
- L15 metrics 开关 404/200；L16 API 校验（011-11）：未知账户 404 / 非 plain id 400 / 坏 mode 400
- L17 隐私红线：/api/acctmap 只有 fingerprint/hint，无 Token 原文
- L18 CPA 文件目录增量（012 §2.3 格式）：写 auth 文件 → watch 增量入 map → 产生更新事件（013）
- L19 审计日志含 account_fp/upstream，可定位路由

## 三、汇总报告
远端非破坏结果 + 本地破坏结果，逐项通过/失败 + 证据（状态码、journalctl/审计行）；未覆盖项明确列出（Sub2API 增量需真实 PostgreSQL、真实住宅出口供应商语法、Let's Encrypt 真证书）。

## 四、复核（自查）
- 约束对照：不修改外部请求/响应——测试断言恰恰验证"逐字节一致"；报告说人话；不用 PR。
- 风险控制：远端零写；本地全部在 /tmp 临时目录 + 非冲突端口；口令/Cookie/凭据报告打码、临时 Cookie 文件即用即删。
- 依据：DEPLOY.md §5/§9、011 §8、012 §3.5 全覆盖（可离线验证部分）。

批准后按 远端 → 本地 → 报告 顺序执行。
