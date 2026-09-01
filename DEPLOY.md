[English](DEPLOY.en.md)

# MITMRouter 部署指导

> 适用版本：main 分支。数据库为幂等建表、无迁移脚本，升级只需换二进制重启。
> 本文记录一次完整的生产部署流程（参考实例：Ubuntu 24.04 VPS），
> 可作为新机器部署与存量升级的操作手册。
> 文中一律使用占位符：`<服务器IP>`、`<域名>`，落地时替换为实际值。

---

## 1. 架构与端口约定

| 组件 | 说明 |
|---|---|
| 接入口监听 | `0.0.0.0:55666`，TLS-only（配了证书后明文连接握手期即拒），入站 Basic 认证 |
| 管理台监听 | `0.0.0.0:55667`，TLS-only，会话 Cookie 认证 |
| 数据目录 | `/opt/mitmrouter/data`（SQLite，含 CA/口令/设置/审计）。启动时程序自动收紧权限：目录 `0700`、库及 WAL/SHM 文件 `0600` |
| 证书 | Let's Encrypt（certbot），拷贝副本放 `/opt/mitmrouter/certs/` |

两个监听地址都是**启动参数**，不落库；其余全部运行期配置存 SQLite 并支持热更新。

## 2. 本地构建

```bash
# 默认产出 bin/mitmrouter-linux-amd64：静态、去符号二进制，无 CGO 依赖
./scripts/build.sh
```

脚本固定执行顺序：以 lockfile 安装前端依赖 → 构建管理台到 `internal/webui/dist` → 通过
`go:embed` 编译 Go 二进制。交叉编译时通过参数指定目标，例如：

```bash
./scripts/build.sh --os linux --arch arm64
```

## 3. 服务器初始化

```bash
ssh root@<SERVER_IP>
mkdir -p /opt/mitmrouter/{bin,data,certs}
```

### 3.1 证书

前提：certbot 已为服务器域名签发证书。确认哪个域名解析到本机：

```bash
getent hosts <域名>                    # 应返回本机公网 IP
openssl x509 -in /etc/letsencrypt/live/<域名>/cert.pem -noout -enddate -ext subjectAltName
```

拷贝**解引用后的副本**到部署目录（live/ 下是指向 archive/ 的软链）：

```bash
cp -L /etc/letsencrypt/live/<域名>/fullchain.pem /opt/mitmrouter/certs/fullchain.pem
cp -L /etc/letsencrypt/live/<域名>/privkey.pem   /opt/mitmrouter/certs/privkey.pem
chmod 600 /opt/mitmrouter/certs/privkey.pem
```

安装续期钩子——certbot 续期后自动同步副本；应用按 mtime 变化热重载，无需重启：

```bash
cat > /etc/letsencrypt/renewal-hooks/deploy/mitmrouter-certs.sh <<'EOF'
#!/bin/sh
cp -L /etc/letsencrypt/live/<域名>/fullchain.pem /opt/mitmrouter/certs/fullchain.pem
cp -L /etc/letsencrypt/live/<域名>/privkey.pem   /opt/mitmrouter/certs/privkey.pem
chmod 600 /opt/mitmrouter/certs/privkey.pem
EOF
chmod +x /etc/letsencrypt/renewal-hooks/deploy/mitmrouter-certs.sh
```

### 3.2 systemd 单元

```ini
# /etc/systemd/system/mitmrouter.service
[Unit]
Description=MITMRouter ingress + admin console
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/opt/mitmrouter/bin/mitmrouter -data /opt/mitmrouter/data -addr 0.0.0.0:55666 -admin-addr 0.0.0.0:55667
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
```

### 3.3 防火墙：仅允许同机 Docker 容器访问接入口

若接入口仅供**同一台服务器**上的 Docker 服务（例如 Sub2API）使用，不应把接入端口
`55666/tcp` 对公网开放。容器访问 `<域名>:55666` 时会经 Docker 网桥进入宿主机，
因此即使域名解析为本机公网 IP，仍会被 UFW 视为来自 Docker 子网的入站流量。

使用 UFW 时，仅放行 Docker 私网网段到接入端口：

```bash
# 先确认实际 Docker 子网；示例中的容器地址为 172.18.x.x
# 输出示例：172.18.0.0/16
docker network inspect <容器网络> \
  --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}'

# Docker 默认地址池通常落在 172.16.0.0/12；也可收窄为上一步查到的实际子网。
ufw allow from 172.16.0.0/12 to any port 55666 proto tcp

# 不要执行 `ufw allow 55666/tcp`：这会把接入口暴露到公网。
ufw status numbered
```

> 若 Docker 使用自定义地址池，请将 `172.16.0.0/12` 替换为实际容器子网。此规则仅用于
> 入站接入端口 `55666/tcp`；管理台端口 `55667/tcp` 应按独立的运维访问策略配置。

验证同机容器可以访问、而公网仍不可访问：

```bash
# 同机容器内应成功（按部署的根证书信任状态决定是否需要 -k）
docker exec <sub2api容器> curl -k --connect-timeout 5 --max-time 15 \
  -o /dev/null -s -w '%{http_code}\n' https://<域名>:55666/

# 从另一台机器测试应连接超时或被拒绝
curl -k --connect-timeout 5 --max-time 15 https://<域名>:55666/
```

## 4. 部署流程（全新安装）

1. **上传二进制**：`scp bin/mitmrouter-linux-amd64 root@<SERVER>:/opt/mitmrouter/bin/mitmrouter && chmod +x …`
2. **首次启动引导**（此时还没有 TLS 配置，先以明文起一次生成数据库）：
   ```bash
   systemctl daemon-reload && systemctl enable --now mitmrouter
   # 抓取一次性管理员口令（只在日志出现）
   journalctl -u mitmrouter | grep "Admin password:"
   ```
3. **网页端完成全部配置**（管理台 `https://<域名>:55667/ui/`，用上一步的一次性口令登录）。
   运行期配置一律走管理台保存，**不要手工改库**：

   - **TLS 证书路径**：基础设置页 → 接入口/管理台 TLS 证书四项，分别填
     `/opt/mitmrouter/certs/fullchain.pem` 与 `/opt/mitmrouter/certs/privkey.pem`
     （成对填写才启用；保存后页面会提示"需要重启"）：
     ```bash
     systemctl restart mitmrouter
     ```
   - **入站认证**：基础设置页 → 入站认证，填 `用户名:密码` 保存，立即热生效
     （已保存的用户名和密码会在已登录的管理台中直接回显，接入地址也会带上真实认证信息；请勿外传）；
   - 其余（默认上游、无 Marker 策略、黑白名单、日志保留等）均在各页面保存即生效。

   > 唯一需要重启的就是 TLS 路径变更（监听器在启动时绑定证书）；同路径下证书文件
   > 内容更新（如 certbot 续期）由热重载接管，无需重启。

## 5. 验证清单

```bash
systemctl is-active mitmrouter                       # active
ss -tlnp | grep -E ":5566[67]"                        # 双端口监听
journalctl -u mitmrouter -n 1 | grep started          # ingress_tls/admin_tls 应为 true
                                                      # inbound_auth 反映认证开关
# 对外证书是否正确（不加 -k 才是真验证）
curl -s -o /dev/null -w "%{http_code}\n" https://<域名>:55667/ui/     # 200

# 接入口连通性：无凭据应 407，带凭据应返回出口 IP
curl -x https://<域名>:55666 https://api.ipify.org          # 期望失败(407)
curl -x "https://用户名:密码@<域名>:55666" https://api.ipify.org
```

## 6. 客户端信任 MITM 根 CA

默认 ACL 白名单为空 = **所有 HTTPS 目标都会被拦截解析**，客户端必须信任部署生成的根
CA，否则报自签证书错误。CA 下载：管理台 `GET /api/ca.pem`（PEM）/ `/api/ca.crt`（DER）。

### Linux 主机（非容器）

```bash
cp ca.pem /usr/local/share/ca-certificates/mitm-router-root-ca.crt
update-ca-certificates
```

### 容器：宿主机合并 + 只读映射（标准做法）

每个需要经接入口出网的容器**各自合并一份 bundle**（以它自己镜像的系统根证书为底），
在宿主机生成，再以只读 bind mount 映射回各自的容器（覆盖容器的
`/etc/ssl/certs/ca-certificates.crt`）。不在容器内部做任何追加——`docker exec`
的修改随容器重建丢失。

```bash
# ① 准备 MITM 根证书：从管理台下载 ca.pem，保存到固定位置（各容器合并的是同一个根）
mkdir -p /opt/mitmrouter/client-ca
#    上传 ca.pem 为 /opt/mitmrouter/client-ca/mitm-root-ca.pem

# ② 每个容器单独执行一轮，产出自己的 bundle 文件
docker exec <容器> cat /etc/ssl/certs/ca-certificates.crt \
  > /opt/mitmrouter/client-ca/<容器名>.crt    # 该容器镜像的系统根证书为底
cat /opt/mitmrouter/client-ca/mitm-root-ca.pem \
  >> /opt/mitmrouter/client-ca/<容器名>.crt   # 追加 MITM 根证书（PEM）
```

各容器的服务定义里映射**各自的**文件：

```yaml
    volumes:
      - /opt/mitmrouter/client-ca/<容器名>.crt:/etc/ssl/certs/ca-certificates.crt:ro
```

要点：

- **不跨容器复用 bundle**：不同基础镜像的系统根证书集合不同，谁的信息以谁的为准；
- **单个容器需要重新生成其 bundle 的时机**：它的基础镜像升级后系统根证书有变；或
  实例重置、根 CA 换新（§8，此时全部容器都要重做）。certbot 续期的是监听 TLS
  证书（§3.1），与本文件无关；
- 映射覆盖后重启该容器才会重新读取；运行中的进程不会热加载。

> 原理：Alpine/Debian 下 Go/OpenSSL 的默认验证路径就是这一个文件，
> 把 MITM 根证书（PEM 格式）合并进去即可进入信任链。

### 容器内验证接入口可用（不带 -k 才算验证了信任链）

```bash
docker exec <容器> curl --max-time 30 \
  -x "https://用户名:密码@<域名>:55666" https://api.ipify.org
```

## 7. 上游配置

管理台「上游出口列表」→ 新增。字段要点：

| 平台 | base_url 用户名语法 | 说明 |
|---|---|---|
| dataimpulse | `<login>__cr.us` | 会话参数自动追加 `;sessid.<身份>` |
| decodo | 必须 `user-` 开头 | 注入 `-session-` / `-sessionduration-` |
| 1024proxy | `<apikey>-region-US-sid-x-t-5` | sid/t 由应用改写，t 受 session_ttl_min 控制 |
| resin | `Platform[.Account]`，密码为 token | 用户名改写为 `Platform.<身份>` |
| generic | 任意 + inject 模板 | `{user}/{sid}/{ttl_min}/{country}` 占位符 |

> **platform 必须选对供应商**：它决定用户名改写器语法。选错时认证通常仍能通过
> （网关只认 apikey+密码），看起来"能用"，但会话参数注入无效，粘滞路由实际失效。
> 修改 platform 走管理台编辑保存即可热生效，无需重启。

### 7.1 账号映射与同步源（可选）

凭据是 API Key（`sk-…` 等）时无需本页——本服务直接按 Key 哈希粘滞。若上游账号走
Bearer AT/RT（订阅号），建议在「账号映射」页建映射，把粘滞身份挂在**账号**上：
token 刷新轮换后出口 IP 不变。

- **同步源拉取**：新增来源实例（选 CLIProxyAPI 或 Sub2API），填实例地址与凭据，
  内置同步器定时拉取其账号的 AT/RT；同类源可配多个实例。
- **手动登记**：直接填账号标识（邮箱/uuid）与其 AT/RT。
- 整体停用设 `acctmap_enabled=false`，全部流量回落纯 Marker 哈希路由。

不用订阅号场景可跳过本节。

## 8. 升级 / 回滚

**升级**（schema 为幂等建表，跨版本直接换二进制）：

```bash
scp bin/mitmrouter-linux-amd64 root@<SERVER>:/opt/mitmrouter/bin/mitmrouter.new
ssh root@<SERVER> 'cd /opt/mitmrouter/bin && mv mitmrouter mitmrouter.bak && mv mitmrouter.new mitmrouter && systemctl restart mitmrouter'
```

**回滚**：反向重复上述动作即可；数据目录不受影响。

**重置整个实例**（未上线阶段允许）：`systemctl stop mitmrouter && rm -rf /opt/mitmrouter/data/*`
再启动即全新引导（新 CA、新管理员口令，旧 CA 信任关系作废）。

## 9. 排障速查

| 症状 | 定位 |
|---|---|
| 明文连接被拒 | 该监听已启用 TLS-only，属预期 |
| curl 显示 000 且 exit 56 | 接入口回了 407 要求认证——检查凭据或 `listen_auth` 是否符合 `user:pass` |
| 经本服务偶发 502 / 超时 | 住宅出口死节点常态，重试即可；持续失败看 journalctl 里 `forward failed` 的 err 详情 |
| 出口 IP 不随身份变化 | 先查审计日志 `account_fp` 是否不同（路由端）；相同则是上游供应商侧未按会话参数分流 |
| 访问 `/metrics` 返回 404 | metrics 未开启：基础设置页打开后再访问，且需管理员登录态 |
| 改了设置不生效 | 直接改库需要重启；走管理台/API 保存则热生效 |

## 10. 部署信息登记（部署完成后自行填写）

部署完成后建议将实例参数登记在**私有运维渠道**（不要提交进仓库）：

- 服务器 IP / SSH 方式：
- 域名及证书 SAN 覆盖范围、有效期：
- 管理员口令：仅存在于首启日志（`journalctl -u mitmrouter | grep "Admin password:"`），
  登录后请立即改密
- 接入口 `listen_auth` 凭据：管理台设置页可查看/修改
- 客户端 CA 分发范围与方式（哪些设备/容器已导入根 CA）
