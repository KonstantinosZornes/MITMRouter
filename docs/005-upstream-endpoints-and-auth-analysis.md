# CLIProxyAPI 与 sub2api 上游接口与认证机制全量分析

> 扫描日期：2026-08-26
> 对象：`~/codes/CLIProxyAPI`（Go 多后端代理网关）、`~/codes/sub2api`（订阅额度分发网关，Go 后端 + Vue3 面板）
> 结论先行：两个项目的凭据体系都以 **OAuth Refresh Token（RT）** 为主、**API Key（AK）** 为兼容回退。RT 只在后台发给认证服务器换新 AT；所有业务请求头里带的是 **AT（access token）**，形如 `Authorization: Bearer <AT>`。

---

## 0. 总体结论

| 维度 | CLIProxyAPI | sub2api |
|---|---|---|
| 定位 | 轻量多后端代理（本地/集群） | 完整账号池管理 + 计费分发的 SaaS 网关 |
| 凭据维护 | 被动：single-flight + 提前 5 分钟刷新 | 主动：TokenRefreshService 定时驱动，提前 1 小时预热 |
| 配额感知 | 从响应推断（SSE rate_limits 帧 / 错误体） | 主动查询（wham/usage、billing、oauth/usage、余额接口） |
| Grok 网页版接口 | 不使用 | 不使用（SSO cookie 仅作为换 RT 的原料） |
| 平台覆盖 | Codex、xAI、Claude、Gemini(AK)、Vertex、Antigravity、Kimi、openai-compatibility | Codex、Grok、Claude、Gemini(三模式)、Antigravity、GLM、DeepSeek、Moonshot/Kimi、Ollama Cloud |

---

## 1. CLIProxyAPI

### 1.1 OpenAI / Codex

**认证**：Codex CLI OAuth PKCE（client_id `app_EMoamEEZ73f0CkXaXp7hrann`，redirect `http://localhost:1455/auth/callback`，scope `openid profile email offline_access`）。AK（sk-）可选。

| 阶段 | URL | 方法 | 认证头 |
|---|---|---|---|
| 授权 | `https://auth.openai.com/oauth/authorize` | GET 浏览器 | — |
| 换码 | `POST https://auth.openai.com/oauth/token`（grant_type=authorization_code） | POST | 无 |
| 刷新 | 同上（grant_type=refresh_token，scope `openid profile email`） | POST | 无 |
| 设备流 | `POST auth.openai.com/api/accounts/deviceauth/usercode` → 页面 `auth.openai.com/codex/device` → `POST .../deviceauth/token` | POST | 无 |
| 推理(SSE) | `POST https://chatgpt.com/backend-api/codex/responses` | POST SSE | `Authorization: Bearer <AT>` |
| 推理(WS) | `wss://chatgpt.com/backend-api/codex/responses`（`OpenAI-Beta: responses_websockets=2026-02-06`） | WS | Bearer `<AT>` |
| 压缩 | `POST .../codex/responses/compact` | POST | Bearer `<AT>` |
| 联网搜索 | `POST .../codex/alpha/search` | POST | Bearer `<AT>` |
| 生图 | `POST .../codex/images/generations`、`/images/edits`，或经 `/responses` 注入 image_generation tool | POST | Bearer `<AT>` |
| models | `GET .../codex/models?client_version=0.144.1` | GET | Bearer `<AT>` |
| 实时语音 | `POST .../codex/realtime/calls?intent=quicksilver&architecture=avas`；媒体面 `wss://api.openai.com/v1/realtime?model=`、`/realtime/calls/{id}`；挂断 `POST api.openai.com/v1/realtime/calls/{id}/hangup` | POST/WS | Bearer `<AT>` |

推理附加头（`internal/runtime/executor/codex_executor_request.go:322-377`）：
- `Chatgpt-Account-Id: <从 id_token JWT claim "https://api.openai.com/auth" 解出>`
- `Originator: codex-tui`、UA `codex-tui/0.146.0 (Mac OS 26.5.0; arm64)`
- chatgpt.com 强制 Chrome uTLS 指纹规避 Cloudflare
- AK 模式只保留 `Authorization: Bearer sk-…`，去掉伪装头

代码：`internal/auth/codex/openai_auth.go`、`codex_executor_*.go`

### 1.2 xAI / Grok

**认证**：OAuth device flow（RFC 8628，client_id `b1a00492-073a-47ea-816f-4c329264a828`，scope 含 `grok-cli:access api:access`）；AK（xai-）可选。

| 阶段 | URL | 方法 | 认证头 |
|---|---|---|---|
| OIDC 发现 | `GET https://auth.x.ai/.well-known/openid-configuration` | GET | 无 |
| 设备码申请 | `POST {device_authorization_endpoint}` | POST | 无 |
| 换 token / 刷新 | `POST {token_endpoint}`（grant_type=device_code 轮询 / refresh_token，提前 5 分钟） | POST | 无 |
| 推理(OAuth) | `POST https://cli-chat-proxy.grok.com/v1/responses` | POST SSE | `Authorization: Bearer <AT>` |
| 压缩/WS | 强制走 `POST api.x.ai/v1/responses/compact`、`wss://api.x.ai/v1/responses`（chat-proxy 返回 404/405） | POST/WS | Bearer `<AT>` |
| 推理(AK) | `POST api.x.ai/v1/responses` | POST | Bearer `<xai-ak>` |
| 媒体 | `POST api.x.ai/v1/images/generations\|edits`、`/videos/generations\|edits\|extensions`、`GET /videos/{request_id}` | POST/GET | 同上 |

CLI 身份伪装头（`xai_executor_request.go:316-335`，仅 cli-chat-proxy 域）：`X-XAI-Token-Auth: xai-grok-cli`、`x-grok-client-version: 0.2.120`、`x-grok-client-identifier: grok-shell`、`x-authenticateresponse: authenticate-response`、UA `xai-grok-workspace/0.2.120`、`x-grok-conv-id: <会话>`

### 1.3 Claude / Anthropic

**认证**：OAuth PKCE（client_id `9d1c250a-e61b-44d9-88ed-5944d1962f5e`，回调 localhost:54545）；或 sk-ant API key。OAuth 凭据形如 `sk-ant-oat…`。

| 阶段 | URL | 方法 | 认证头 |
|---|---|---|---|
| 授权 | `https://claude.ai/oauth/authorize?code=true&client_id=…&scope=user:profile user:inference …` | GET 浏览器 | — |
| 换码 | `POST https://platform.claude.com/v1/oauth/token`（JSON body，UA 伪装 `axios/1.15.2`） | POST | **无 Authorization** |
| 刷新 | 同上（grant_type=refresh_token） | POST | 无 |
| 身份 | `GET api.anthropic.com/api/oauth/profile`、`/api/oauth/claude_cli/roles` | GET | Bearer `<AT>` |
| 推理 | `POST {base}/v1/messages?beta=true`（base 默认 api.anthropic.com） | POST | OAuth：**`Authorization: Bearer <AT>`**（删 x-api-key）；AK：**`x-api-key: <key>`** |
| 计数 | `POST {base}/v1/messages/count_tokens?beta=true` | POST | 同上 |

固定头（`claude_executor_request.go`）：
- `Anthropic-Version: 2023-06-01`
- `Anthropic-Beta`（线序）：`claude-code-20250219, oauth-2025-04-20`(仅OAuth), `context-1m-2025-08-07`, `interleaved-thinking-2025-05-14`, …, `extended-cache-ttl-2025-04-11`(仅OAuth)
- 可选 CLI 指纹：UA `claude-cli/x.y.z` + `X-Stainless-Package-Version/Runtime-Version/Os/Arch`
- CCH 计费签名不是 HTTP 头，是注入 body 的 `system[0].text = "x-anthropic-billing-header: …; cch=<xxhash64>"`（`claude_signing.go`）

### 1.4 Google 系：Gemini(AK) / Vertex / Antigravity

| 平台 | URL | 方法 | 认证头 |
|---|---|---|---|
| Gemini AK | `POST generativelanguage.googleapis.com/v1beta/models/{m}:generateContent\|:streamGenerateContent?alt=sse\|:countTokens`；`POST /v1beta/interactions`（`Api-Revision: 2026-05-20`） | POST | **`x-goog-api-key: <key>`** |
| Vertex AK | `POST aiplatform.googleapis.com/v1/publishers/google/models/{m}:{action}` | POST | `x-goog-api-key` |
| Vertex SA | `POST {loc}-aiplatform.googleapis.com/v1/projects/{pid}/locations/{loc}/publishers/google/models/{m}:{action}` | POST | **`Authorization: Bearer <SA_AT>`**（golang.org/x/oauth2 自动换/缓存） |
| Antigravity 授权 | `GET accounts.google.com/o/oauth2/v2/auth`（client_id `1071006060591-tmhssin2h21lcre235vtolojh4g403ep…`，无 PKCE，回调 localhost:51121）→ `POST oauth2.googleapis.com/token`（authorization_code / refresh_token，UA `Go-http-client/2.0`） | GET/POST | 无 |
| Antigravity 用户信息 | `GET www.googleapis.com/oauth2/v2/userinfo?alt=json` | GET | Bearer google_AT |
| Antigravity 开通 | `POST cloudcode-pa.googleapis.com/v1internal:loadCodeAssist`；`POST daily-cloudcode-pa.googleapis.com/v1internal:onboardUser` | POST | Bearer google_AT + `X-Goog-Api-Client: gl-node/22.21.1`（仅 onboard）+ UA `antigravity/hub/<ver>` |
| Antigravity 推理 | `POST {daily→prod}/v1internal:generateContent\|:streamGenerateContent?alt=sse\|:countTokens\|:fetchAvailableModels` | POST | Bearer google_AT + UA antigravity/\*（刻意不带 X-Goog-Api-Client） |

注：gemini-cli provider 在本 fork 由外部插件托管登录/刷新，Go 核心不含。

### 1.5 Kimi（OAuth 设备流）

| 阶段 | URL | 方法 | 认证头 |
|---|---|---|---|
| 设备码 | `POST https://auth.kimi.com/api/oauth/device_authorization`（client_id `17e5f671-d194-4dfb-9706-5516cb48c098`） | POST | 无 Bearer，带 `X-Msh-Platform/Version/Device-Name/Device-Model/Device-Id` 五件套 |
| 换 token / 刷新 | `POST https://auth.kimi.com/api/oauth/token`（device_code 轮询 / refresh_token） | POST | 同上 |
| 推理 | `POST https://api.kimi.com/coding/v1/chat/completions`、`/coding/v1/messages?beta=true`、`/coding/v1/messages/count_tokens?beta=true` | POST | `Authorization: Bearer <AT 或 ak>` + X-Msh-\* |

### 1.6 其他外部访问（非 AI 上游）

| URL | 用途 |
|---|---|
| `https://models.router-for.me/models.json`、`/codex_client_models.json` | 模型目录热更新 |
| `https://raw.githubusercontent.com/router-for-me/CLIProxyAPI-Plugins-Store/main/registry.json` | 插件商店 |
| `https://cpamc.router-for.me/` | 管理界面资源回退源 |
| 通用 openai-compatibility | 任意 base_url + `Authorization: Bearer <key>` |

---

## 2. sub2api

### 2.1 OpenAI / Codex

与 CLIProxyAPI 共享同一套 chatgpt.com 面（responses/wss/compact/alpha-search/models/realtime），差异：

| 增补项 | URL | 说明 |
|---|---|---|
| 账号信息 | `GET chatgpt.com/backend-api/accounts/check/v4-2023-04-27`、`GET /backend-api/subscriptions?account_id=` | plan_type、到期时间 |
| 配额 | `GET /backend-api/wham/usage`（5h/7d 窗口）、`GET /wham/rate-limit-reset-credits`、`POST .../consume` | 主动配额监控 |
| 隐私 | `PATCH /backend-api/settings/account_user_setting?feature=training_allowed&value=false` | 关训练共享 |
| 文件 | `POST /backend-api/files`、`GET /files/{id}/download`、`GET /conversation/{cid}/attachment/{aid}/download` | 生图上传/产物下载 |
| AK 回退面 | `POST api.openai.com/v1/responses(-ws)`、`/v1/chat/completions`、`/v1/input_tokens`、`/v1/embeddings`、`/v1/moderations`、`GET /v1/models`、`POST /v1/alpha/search`、`/v1/images/*` | Bearer sk- |
| PAT 校验 | `GET auth.openai.com/api/accounts/v1/user-auth-credential/whoami` | at- 令牌 |
| Agent Identity | `POST auth.openai.com/api/accounts/v1/agent/{runtime_id}/task/register` | Ed25519 签名 |

出站身份强制统一（`openai_codex_identity.go`）：originator + version≥0.144.0（低了上游直接 404）+ UA `codex_cli/<ver> (Ubuntu…) xterm-256color`；凭据面（auth.openai.com）只带 UA+originator 不带 version。

### 2.2 Grok / xAI

数据面同前（cli-chat-proxy responses/chat-completions/models/billing + api.x.ai 媒体/tts/stt/realtime）。登录面更丰富：

| 阶段 | URL | 方法 | 认证头 |
|---|---|---|---|
| OIDC/PKCE | `GET auth.x.ai/.well-known/openid-configuration`、`GET auth.x.ai/oauth2/authorize`（回调 localhost:56121） | GET | 无 |
| 换码/刷新 | `POST auth.x.ai/oauth2/token`（authorization_code / refresh_token，提前 1h） | POST | 无 |
| SSO→Build 转换 | `GET accounts.x.ai/`(验活) → `POST auth.x.ai/oauth2/device/code` → 打开 verification_uri → `POST /device/verify`、`/device/approve` → 轮询 `POST /oauth2/token`(device_code) | GET/POST | Cookie jar 带 `sso=` cookie |
| 账密登录 | `POST accounts.x.ai/api/rpc`（rpc=createSession，Turnstile 打码经 `api.yescaptcha.com/createTask\|getTaskResult`）→ cookieSetterUrl 领 sso cookie | POST | — |
| 配额 | `GET cli-chat-proxy.grok.com/v1/billing?format=credits`（周/credit）、`GET /v1/billing`（月度） | GET | Bearer Build-AT + CLI 伪装头 |

billing 探测 UA 伪装为 `grok-pager/… grok-shell/…`。

### 2.3 Claude / Anthropic（双模式）

**认证**：OAuth RT（含 sessionKey 半自动流）或 console API key。

| 阶段 | URL | 方法 | 认证头 |
|---|---|---|---|
| sessionKey 流 | `GET claude.ai/api/organizations`、`POST claude.ai/v1/oauth/{orgUUID}/authorize` | GET/POST | **Cookie: `sessionKey=sk-ant-sid01-…`** |
| 授权 | `https://claude.com/cai/oauth/authorize`（redirect `platform.claude.com/oauth/code/callback`） | 浏览器 | — |
| 换码 | `POST platform.claude.com/v1/oauth/token`（JSON，UA axios/1.13.6） | POST | 无 |
| 刷新 | 同上（grant_type=refresh_token） | POST | 无 |
| 推理 | `POST api.anthropic.com/v1/messages?beta=true`、`/count_tokens?beta=true`、`GET /v1/models` | POST/GET | OAuth：小写 **`authorization: Bearer <AT>`**；AK：**`x-api-key: <key>`**（extra `anthropic_apikey_auth_scheme=authorization_bearer` 时切 Bearer） |
| 配额 | `GET api.anthropic.com/api/oauth/usage` | GET | Bearer AT + `anthropic-beta: oauth-2025-04-20` |

固定头（`internal/pkg/claude/constants.go`）：
- `anthropic-version: 2023-06-01`（Vertex 服务号路径用 `vertex-2023-10-16`）
- OAuth beta 组合：`claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14`；count_tokens 追加 `token-counting-2024-11-01`；完整 mimicry 再含 `prompt-caching-scope-2026-01-05, effort-2025-11-24, context-management-2025-06-27, extended-cache-ttl-2025-04-11`
- 伪装：UA `claude-cli/2.1.220` + X-Stainless-\* 全套 + `x-app: cli` + `Anthropic-Dangerous-Direct-Browser-Access: true`

### 2.4 Gemini（三模式）/ Antigravity

| 模式 | URL | 认证头 |
|---|---|---|
| API Key | `generativelanguage.googleapis.com/v1beta/models/{m}:generateContent\|:streamGenerateContent?alt=sse\|:countTokens` | **`x-goog-api-key: <AIza…>`** |
| OAuth 无项目（AI Studio） | 同上路径 | **`Authorization: Bearer <google_AT>`** |
| Code Assist OAuth（有 project_id） | `cloudcode-pa.googleapis.com/v1internal:generateContent(:streamGenerateContent)`，body 包 `{model, project, request}` | Bearer google_AT + UA `GeminiCLI/0.1.5 (Windows; AMD64)` |
| 登录/刷新 | `accounts.google.com/o/oauth2/v2/auth` → `POST oauth2.googleapis.com/token`（code_assist 用内置 client `681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j…` / secret `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`；ai_studio 必须自备 client） | 无 |
| 项目探测 | `loadCodeAssist`/`onboardUser`、`GET www.googleapis.com/drive/v3/about?fields=storageQuota`（Google One 档位）、`GET cloudresourcemanager.googleapis.com/v1/projects` | Bearer google_AT |
| Antigravity | 同 CLIProxyAPI（client_id `1071006060591-tmhssin…`，redirect localhost:8085；动作含 setUserSettings/fetchUserInfo/fetchAvailableModels） | Bearer google_AT |
| Vertex SA | JWT grant 走 `oauth2.googleapis.com/token` | — |

### 2.5 国产三家（纯 AK，无 OAuth）

| 平台 | 数据面 | 配额/余额 | 认证头 |
|---|---|---|---|
| GLM 智谱 | `open.bigmodel.cn/api/paas/v4/(chat/completions\|responses)`、`/api/coding/paas/v4/*`、`/api/anthropic/v1/messages` | `GET /api/monitor/usage/quota/limit`（Coding Plan 5h/周额度） | 常规 `Authorization: Bearer <key>`；quota 接口**裸 key**：`Authorization: <apiKey>`；Anthropic 协议默认 `x-api-key`（可切 Bearer） |
| DeepSeek | `api.deepseek.com/v1/chat/completions`、`/responses`、`/anthropic/v1/messages` | `GET /user/balance`（双币种余额） | Bearer（Anthropic 协议 x-api-key） |
| Moonshot/Kimi | `api.moonshot.cn/v1/*`（payg）、`api.kimi.com/coding/v1/*`（Coding Plan）、两家 `/anthropic/v1/messages` | `GET api.moonshot.cn/v1/users/me/balance`；`GET api.kimi.com/coding/v1/usages`（5h 窗口+weekly） | Bearer（Anthropic 协议 x-api-key） |

### 2.6 Ollama Cloud 与面板周边

| 类别 | URL | 认证 |
|---|---|---|
| Ollama 数据面 | `ollama.com/v1/messages?beta=true` 等 | Bearer / x-api-key |
| Ollama 用量 | 抓取 `ollama.com/settings` HTML 解析 five_hour/seven_day 窗口 | 加密的 web session |
| LinuxDo OAuth | `connect.linux.do/oauth2/authorize`、`/oauth2/token`、`/api/user` | 面板第三方登录 |
| 支付 | alipay `openapi.alipay.com/gateway.do`；airwallex `api[-demo].airwallex.com/api/v1`（Bearer）；易支付 `{base}/submit.php\|mapi.php\|api.php`（MD5 签名） | — |
| 验证码 | `captcha.tencentcloudapi.com`（天御核验）、阿里云 VerifyIntelligentCaptcha、yescaptcha（Grok Turnstile 打码） | — |

---

## 3. 认证头速查矩阵

| 平台 | OAuth(AT) 形式 | API Key 形式 | 特殊凭据 |
|---|---|---|---|
| OpenAI Codex | `Authorization: Bearer <AT>` + `Chatgpt-Account-Id` + `Originator` | `Authorization: Bearer sk-…` | PAT(at-)、setup token |
| Anthropic | 小写 `authorization: Bearer <AT>` + `anthropic-beta: oauth-2025-04-20` | `x-api-key: <key>` | sub2api 支持 Cookie `sessionKey=sk-ant-sid01-…` 入口 |
| Gemini | `Authorization: Bearer <google_AT>` | `x-goog-api-key: <AIza…>` | Vertex SA（JWT 换 AT，库自动管理） |
| Antigravity | Bearer google_AT + UA `antigravity/hub/*` | — | — |
| Grok/xAI | Bearer Build-AT + `X-XAI-Token-Auth: xai-grok-cli` + 版本/identifier 头 | `Authorization: Bearer xai-…` | SSO cookie 仅用于 device flow 换 RT |
| Kimi(CLIProxyAPI) | Bearer AT + `X-Msh-*` 五件套 | Bearer | — |
| 国产三家(sub2api) | — | `Authorization: Bearer <key>`；Anthropic 协议 `x-api-key`；智谱 quota 接口裸 key | — |

## 4. RT 与 AT 分工总结

| | RT（refresh token） | AT（access token） |
|---|---|---|
| 发送目标 | 仅认证服务器：`auth.openai.com/oauth/token`、`auth.x.ai/oauth2/token`、`platform.claude.com/v1/oauth/token`、`oauth2.googleapis.com/token`、`auth.kimi.com/api/oauth/token` | 所有业务上游（chatgpt.com、cli-chat-proxy.grok.com、api.x.ai、api.anthropic.com、cloudcode-pa 等） |
| 出现位置 | 请求 body（grant_type=refresh_token），永不出现在请求头 | `Authorization: Bearer` 头 |
| 有效期 | 长（月级） | 约 1 小时级 |
| 刷新时机 | — | CLIProxyAPI 提前 5 分钟；sub2api 提前 1 小时 |

## 5. 关键代码索引

**CLIProxyAPI**
- OpenAI：`internal/auth/codex/openai_auth.go`、`internal/runtime/executor/codex_executor_{request,execute,stream}.go`
- xAI：`internal/auth/xai/{types,xai}.go`、`internal/runtime/executor/xai_executor_{request,execute}.go`
- Claude：`internal/auth/claude/anthropic_auth.go`、`claude_executor_request.go`、`claude_signing.go`
- Google/Antigravity：`internal/auth/antigravity/{constants,auth}.go`、`antigravity_executor_*.go`、`gemini_vertex_executor.go`
- Kimi：`internal/auth/kimi/kimi.go`、`kimi_executor.go`

**sub2api**（`backend/internal/`）
- OpenAI：`pkg/openai/oauth.go`、`service/openai_gateway_service.go`、`openai_codex_identity.go`、`repository/openai_oauth_service.go`
- Grok：`pkg/xai/{oauth,sso_device,billing,cli_identity}.go`、`repository/grok_oauth_client.go`
- Claude：`pkg/oauth/oauth.go`、`pkg/claude/constants.go`、`gateway_upstream_request.go`、`repository/claude_oauth_service.go`、`claude_usage_service.go`
- Gemini：`pkg/geminicli/constants.go`、`repository/gemini_oauth_client.go`、`repository/geminicli_codeassist_client.go`
- 国产：`domain_constants.go`、`cn_provider_quota_service.go`、`cn_provider_balance_service.go`
