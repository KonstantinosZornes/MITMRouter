// Package api 实现管理台 REST：会话认证、设置热更新、上游 CRUD、审计查询。
package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"mitmrouter/internal/acctegress"
	"mitmrouter/internal/acctmap"
	"mitmrouter/internal/acl"
	"mitmrouter/internal/certca"
	"mitmrouter/internal/metrics"
	"mitmrouter/internal/reqid"
	"mitmrouter/internal/settings"
	"mitmrouter/internal/store"
	"mitmrouter/internal/upstream"
)

const (
	cookieName = "sticky_session"
	sessionTTL = 7 * 24 * time.Hour
)

// Deps 注入的依赖。
type Deps struct {
	Store         *store.Store
	Settings      *settings.Holder
	CA            *certca.Authority
	SwapUpstreams func(*upstream.Table)
	Logger        *slog.Logger
	Metrics       *metrics.Registry // nil 时兼容回退到 metrics.Default

	AcctMap *acctmap.Registry // 账号映射内存注册表（可 nil）
	Syncer  SourceSyncer      // 拉取器（可 nil：仅禁用 test/sync 端点）

	// Updates 是映射更新记录的落库通道（docs/013-update-log-design.md）；
	// 可 nil（单测不接线），写入永不阻塞。
	Updates chan<- store.UpdateEvent

	// SwapAcctEgress 原子替换服务端账户↔出站绑定快照；nil 时绑定端点仍可写库，
	// 但路由面不会感知变化（仅测试或降级装配场景）。
	SwapAcctEgress func(*acctegress.Table)

	// IngressPort 是接入面监听端口（如 "55666"）。设置页回显的接入地址 =
	// 本次请求 Host 的主机名 + 该端口。双监听拆分后若沿用请求自带端口，
	// 会把管理台端口回显给用户复制，导致客户端打到接入端口收 404。
	IngressPort string
	// IngressTLS 表示当前运行中的接入监听是否启用 TLS。管理台与接入监听独立，
	// 因而不能根据管理台请求的协议猜测接入协议。
	IngressTLS bool
}

type API struct {
	d               Deps
	backoffMu       sync.Mutex          // 保护 backoff；登录路径低频，无需 sync.Map
	backoff         map[string]*attempt // ip -> 失败计数与退避期
	backoffPrunedAt time.Time           // 上次整表过期清扫时间；由 backoffMu 保护
}

func New(d Deps) *API { return &API{d: d, backoff: map[string]*attempt{}} }

func (a *API) logger() *slog.Logger {
	if a.d.Logger != nil {
		return a.d.Logger
	}
	return slog.Default()
}

// ---------- 路由 ----------

func (a *API) Router() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /api/auth/login", a.login)
	m.HandleFunc("POST /api/auth/logout", a.logout)
	m.HandleFunc("GET /api/auth/me", a.auth(a.me))
	m.HandleFunc("POST /api/auth/password", a.auth(a.changePassword))
	m.HandleFunc("GET /api/settings", a.auth(a.getSettings))
	m.HandleFunc("PUT /api/settings", a.auth(a.putSettings))
	m.HandleFunc("POST /api/settings/reset-salt", a.auth(a.resetSalt))
	m.HandleFunc("GET /api/upstreams", a.auth(a.listUpstreams))
	m.HandleFunc("POST /api/upstreams", a.auth(a.createUpstream))
	m.HandleFunc("PUT /api/upstreams/{id}", a.auth(a.updateUpstream))
	m.HandleFunc("DELETE /api/upstreams/{id}", a.auth(a.deleteUpstream))
	m.HandleFunc("POST /api/upstreams/{id}/default", a.auth(a.setDefault))
	m.HandleFunc("POST /api/upstreams/{id}/test", a.auth(a.testUpstream))
	m.HandleFunc("GET /api/logs", a.auth(a.listLogs))
	m.HandleFunc("DELETE /api/logs", a.auth(a.clearLogs))
	m.HandleFunc("GET /api/updates", a.auth(a.listUpdates))
	m.HandleFunc("DELETE /api/updates", a.auth(a.clearUpdates))
	m.HandleFunc("GET /api/ca.pem", a.auth(a.caPEM))
	m.HandleFunc("GET /api/ca.crt", a.auth(a.caCRT))
	m.HandleFunc("GET /metrics", a.auth(a.metrics))
	// 账号映射：拉取源与映射表
	m.HandleFunc("GET /api/sources", a.auth(a.listSources))
	m.HandleFunc("POST /api/sources", a.auth(a.createSource))
	m.HandleFunc("PUT /api/sources/{id}", a.auth(a.updateSource))
	m.HandleFunc("DELETE /api/sources/{id}", a.auth(a.deleteSource))
	m.HandleFunc("POST /api/sources/{id}/test", a.auth(a.testSource))
	m.HandleFunc("POST /api/sources/{id}/sync", a.auth(a.syncSourceNow))
	m.HandleFunc("GET /api/acctmap", a.auth(a.listAcctMap))
	m.HandleFunc("GET /api/acctmap/stats", a.auth(a.acctMapStats))
	m.HandleFunc("PUT /api/acctmap/{platform}/{account}", a.auth(a.putAcctMapAccount))
	m.HandleFunc("DELETE /api/acctmap/{platform}/{account}", a.auth(a.deleteAcctMapAccount))
	m.HandleFunc("DELETE /api/acctmap/{platform}/{account}/tokens/{fp}", a.auth(a.deleteAcctMapToken))
	// 账户 ↔ 出站绑定（docs/011）。字面量 egress 段优先于 {platform} 通配，
	// 两条 PUT 路径互不冲突。
	m.HandleFunc("GET /api/acctegress", a.auth(a.listAcctEgress))
	m.HandleFunc("PUT /api/acctegress/{platform}/{account}", a.auth(a.putAcctEgress))
	m.HandleFunc("DELETE /api/acctegress/{platform}/{account}", a.auth(a.deleteAcctEgress))
	m.HandleFunc("DELETE /api/acctegress", a.auth(a.clearAcctEgress))
	m.HandleFunc("PUT /api/acctegress/egress/{id}", a.auth(a.putAcctEgressBatch))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, _ := reqid.Ensure(r.Context())
		m.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ---------- JSON 与错误信封 ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func readJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	const maxJSONBody = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody+1))
	if err != nil {
		return err
	}
	if len(raw) > maxJSONBody {
		return errors.New("request body exceeds 1 MiB limit")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	err = dec.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return errors.New("request body must contain exactly one JSON value")
}

// ---------- 会话（HMAC 签名 Cookie） ----------

type sessionClaims struct {
	Exp int64 `json:"exp"`
}

func (a *API) hmacKey() ([]byte, error) {
	return a.d.Store.GetSecret(ctxBG(), "session_hmac_key")
}

func (a *API) signSession(exp int64) (string, error) {
	raw, _ := json.Marshal(sessionClaims{Exp: exp})
	payload := base64.RawURLEncoding.EncodeToString(raw)
	key, err := a.hmacKey()
	if err != nil {
		return "", fmt.Errorf("read session signing key: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (a *API) verifySession(token string) bool {
	i := strings.LastIndexByte(token, '.')
	if i <= 0 {
		return false
	}
	payload, sigB64 := token[:i], token[i+1:]
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return false
	}
	key, err := a.hmacKey()
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	if subtle.ConstantTimeCompare(sig, mac.Sum(nil)) != 1 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return false
	}
	var c sessionClaims
	if json.Unmarshal(raw, &c) != nil {
		return false
	}
	return c.Exp > time.Now().Unix()
}

// issueSession 为管理员签发新的会话；当请求通过 TLS 到达 MITMRouter 时，
// 会话将限定为 HTTPS。默认本地控制台仍支持回环 HTTP；如果部署在其他位置终止 TLS，
// 必须先明确边界层的信任策略，才能安全地在此处使用转发头。
func (a *API) issueSession(w http.ResponseWriter, r *http.Request) error {
	exp := time.Now().Add(sessionTTL)
	tok, err := a.signSession(exp.Unix())
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/",
		HttpOnly: true, Secure: r != nil && r.TLS != nil, SameSite: http.SameSiteLaxMode,
		Expires: exp,
	})
	return nil
}

func (a *API) sessionOK(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	return err == nil && a.verifySession(c.Value)
}

// auth 中间件：除登录外全部要求有效会话。
func (a *API) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.sessionOK(r) {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "please log in first")
			return
		}
		// 滑动续期：剩余不足一半时重发（cl.Exp 为 Unix 秒，勿与 Duration 纳秒混算）
		if c, err := r.Cookie(cookieName); err == nil {
			if i := strings.LastIndexByte(c.Value, '.'); i > 0 {
				if raw, e := base64.RawURLEncoding.DecodeString(c.Value[:i]); e == nil {
					var cl sessionClaims
					if json.Unmarshal(raw, &cl) == nil &&
						cl.Exp-time.Now().Unix() < int64(sessionTTL/(2*time.Second)) {
						if err := a.issueSession(w, r); err != nil {
							a.logger().Log(r.Context(), slog.LevelWarn, "session renewal skipped", "err", err)
						}
					}
				}
			}
		}
		next(w, r)
	}
}

// ---------- 登录/口令 ----------

// logout 删除浏览器保存的管理会话 Cookie。会话签名是无状态的，因此不能仅靠
// 服务端删除记录；必须向浏览器返回同路径、同安全属性的过期 Cookie。
func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/",
		HttpOnly: true, Secure: r != nil && r.TLS != nil, SameSite: http.SameSiteLaxMode,
		MaxAge: -1, Expires: time.Unix(1, 0),
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type attempt struct {
	n     int
	until time.Time // 锁定期截止；n<2 时为零值
	last  time.Time // 最近一次失败时间，清扫依据
}

const (
	maxBackoff           = 15 * time.Minute
	maxBackoffEntries    = 10_000
	backoffPruneInterval = time.Minute
)

// backoffRemaining 返回一个来源 IP 的剩余锁定时间。超过最长可能锁定期仍未活跃的记录，
// 会在访问时延迟过期，从而避免陈旧的单次失败记录持续占用内存。
func (a *API) backoffRemaining(ip string) (time.Duration, bool) {
	now := time.Now()
	a.backoffMu.Lock()
	defer a.backoffMu.Unlock()
	at := a.backoff[ip]
	if at == nil {
		return 0, false
	}
	if now.Sub(at.last) > maxBackoff {
		delete(a.backoff, ip)
		return 0, false
	}
	if d := time.Until(at.until); d > 0 {
		return d, true
	}
	return 0, false
}

// pruneExpiredFailuresLocked 删除已超过所有可能退避期的记录。调用方必须持有 backoffMu。
func (a *API) pruneExpiredFailuresLocked(now time.Time) {
	for ip, at := range a.backoff {
		if now.Sub(at.last) > maxBackoff {
			delete(a.backoff, ip)
		}
	}
}

// evictOldestFailureLocked 在分布式密码猜测攻击下限制内存使用。调用方必须持有
// backoffMu，且只能在达到容量上限时调用。
func (a *API) evictOldestFailureLocked() {
	var oldestIP string
	var oldest time.Time
	for ip, at := range a.backoff {
		if oldestIP == "" || at.last.Before(oldest) {
			oldestIP, oldest = ip, at.last
		}
	}
	if oldestIP != "" {
		delete(a.backoff, oldestIP)
	}
}

// registerFailure 记录一次失败并应用指数退避。该表容量固定，会过期清除不活跃记录，
// 但绝不存储密码或其他请求凭据。
func (a *API) registerFailure(ip string) int {
	now := time.Now()
	a.backoffMu.Lock()
	defer a.backoffMu.Unlock()
	at := a.backoff[ip]
	if at == nil {
		if a.backoffPrunedAt.IsZero() || now.Sub(a.backoffPrunedAt) >= backoffPruneInterval {
			a.pruneExpiredFailuresLocked(now)
			a.backoffPrunedAt = now
		}
		if len(a.backoff) >= maxBackoffEntries {
			a.evictOldestFailureLocked()
		}
		at = &attempt{}
		a.backoff[ip] = at
	}
	at.n++
	at.last = now
	if at.n >= 2 { // 首次失败不锁定，自第二次起指数退避
		d := time.Duration(1<<min(at.n-1, 10)) * time.Second
		if d > maxBackoff {
			d = maxBackoff
		}
		at.until = now.Add(d)
	}
	return at.n
}

func (a *API) clearFailures(ip string) {
	a.backoffMu.Lock()
	delete(a.backoff, ip)
	a.backoffMu.Unlock()
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	ip := remoteHostStr(r.RemoteAddr)
	if d, locked := a.backoffRemaining(ip); locked {
		writeErr(w, http.StatusTooManyRequests, "backoff",
			fmt.Sprintf("too many attempts, retry in %ds", int(d.Seconds())+1))
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil || body.Password == "" {
		writeErr(w, 400, "bad_request", "missing password")
		return
	}
	hash, err := a.d.Store.GetSecret(ctxBG(), "admin_password_bcrypt")
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 500, "no_admin_password", "admin password not initialized")
		return
	} else if err != nil {
		a.failInternal(w, r, err)
		return
	}
	if !store.CheckPassword(string(hash), body.Password) {
		n := a.registerFailure(ip)
		metrics.Default.Inc("auth_failures_total", "Admin console auth failures", nil)
		a.logger().Log(r.Context(), slog.LevelWarn, "admin login failed", "ip", ip, "attempts", n)
		writeErr(w, 401, "bad_credentials", "wrong password")
		return
	}
	a.clearFailures(ip)
	if err := a.issueSession(w, r); err != nil {
		a.failInternal(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"user": "admin"})
}

func (a *API) changePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := readJSON(r, &body); err != nil || body.OldPassword == "" || len(body.NewPassword) < 6 {
		writeErr(w, 400, "bad_request", "old_password and new_password (min 6 chars) required")
		return
	}
	hash, err := a.d.Store.GetSecret(ctxBG(), "admin_password_bcrypt")
	if err != nil {
		a.failInternal(w, r, err)
		return
	}
	if !store.CheckPassword(string(hash), body.OldPassword) {
		writeErr(w, 401, "bad_credentials", "wrong old password")
		return
	}
	newHash, err := store.HashPassword(body.NewPassword)
	if err != nil {
		a.failInternal(w, r, err)
		return
	}
	ctx := ctxBG()
	if err := a.d.Store.SetSecret(ctx, "admin_password_bcrypt", []byte(newHash)); err != nil {
		a.failInternal(w, r, err)
		return
	}
	// 轮换签名密钥 → 全量吊销旧会话
	if err := a.d.Store.SetSecret(ctx, "session_hmac_key", []byte(randHex(32))); err != nil {
		a.failInternal(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 设置 ----------

type settingsDTO struct {
	// 监听地址（接入口/管理台 IP:port）由启动参数指定，不经设置接口。

	IngressURL     string `json:"ingress_url"`      // http(s)://host:port，主机名取本次请求、协议取当前接入 TLS 状态
	IngressURLAuth string `json:"ingress_url_auth"` // 含入站认证凭据的完整地址；未启用认证时为空

	ListenTLSCert string `json:"listen_tls_cert"` // 证书/私钥 PEM 路径，成对填写才启用
	ListenTLSKey  string `json:"listen_tls_key"`
	AdminTLSCert  string `json:"admin_tls_cert"`
	AdminTLSKey   string `json:"admin_tls_key"`

	ListenAuth                 string   `json:"listen_auth"`
	DefaultUpstream            string   `json:"default_upstream"`
	NoMarkerPolicy             string   `json:"no_marker_policy"`
	MarkerPathParts            []string `json:"marker_path_parts"`
	MarkerHeaders              []string `json:"marker_headers"`
	HashSalt                   string   `json:"hash_salt"`
	SIDLen                     int      `json:"sid_len"`
	SessionTTLMin              int      `json:"session_ttl_min"`
	SaltRotateFailureThreshold int      `json:"salt_rotate_failure_threshold"`
	LogRetentionDays           int      `json:"log_retention_days"`
	MetricsEnabled             bool     `json:"metrics_enabled"`
	// 同步空快照保护阈值（docs/011 §2.3）：连续 N 次「连接正常但 0 账号」才按空快照清空
	SyncEmptyClearThreshold int `json:"sync_empty_clear_threshold"`

	// PUT 中的 nil 表示旧客户端未发送这个新设置；保留当前安全值，
	// 不能静默关闭目标保护。
	BlockPrivateTargets *bool `json:"block_private_targets"`

	ACLWhitelist []string `json:"acl_whitelist"` // 目标白名单：IP/CIDR/域名/*.通配符，空=不限制
	ACLBlacklist []string `json:"acl_blacklist"` // 目标黑名单，命中即拒绝且优先于白名单
}

func (a *API) loadOps(ctx context.Context) (retention int, metEnabled bool, syncEmpty int) {
	retention, metEnabled, syncEmpty = 30, false, 3
	if m, err := a.d.Store.AllSettings(ctx); err == nil {
		if v, ok := m["log_retention_days"]; ok {
			json.Unmarshal([]byte(v), &retention)
		}
		if v, ok := m["metrics_enabled"]; ok {
			json.Unmarshal([]byte(v), &metEnabled)
		}
		if v, ok := m["sync_empty_clear_threshold"]; ok {
			json.Unmarshal([]byte(v), &syncEmpty)
		}
	}
	return
}

func (a *API) getSettings(w http.ResponseWriter, r *http.Request) {
	snap := a.d.Settings.Current()
	retention, met, syncEmpty := a.loadOps(ctxBG())
	writeJSON(w, 200, settingsDTO{
		IngressURL:     ingressBaseURL(r, a.d.IngressPort, a.d.IngressTLS),
		IngressURLAuth: ingressURLWithAuth(ingressBaseURL(r, a.d.IngressPort, a.d.IngressTLS), snap.ListenAuth),
		ListenTLSCert:  snap.ListenTLSCert,
		ListenTLSKey:   snap.ListenTLSKey,
		AdminTLSCert:   snap.AdminTLSCert,
		AdminTLSKey:    snap.AdminTLSKey,
		// 管理台已认证，按配置原样回显，便于直接复制到客户端；地址字段同样包含真实凭据。
		ListenAuth:                 snap.ListenAuth,
		DefaultUpstream:            snap.DefaultUpstream,
		NoMarkerPolicy:             snap.NoMarkerPolicy,
		MarkerPathParts:            snap.MarkerRules.PathParts,
		MarkerHeaders:              snap.MarkerRules.Headers,
		HashSalt:                   snap.Salt,
		SIDLen:                     snap.SIDLen,
		SessionTTLMin:              snap.SessionTTLMin,
		SaltRotateFailureThreshold: snap.SaltRotateFailureThreshold,
		LogRetentionDays:           retention,
		MetricsEnabled:             met,
		SyncEmptyClearThreshold:    syncEmpty,
		BlockPrivateTargets:        boolPtr(snap.BlockPrivateTargets),
		ACLWhitelist:               snap.ACLWhitelist,
		ACLBlacklist:               snap.ACLBlacklist,
	})
}

func (a *API) putSettings(w http.ResponseWriter, r *http.Request) {
	old := a.d.Settings.Current()
	var dto settingsDTO
	if err := readJSON(r, &dto); err != nil {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}
	dto.ListenAuth = mergeAuth(old.ListenAuth, dto.ListenAuth)
	// 兼容未发送新字段的旧版客户端：沿用当前阈值。
	if dto.SaltRotateFailureThreshold == 0 {
		dto.SaltRotateFailureThreshold = old.SaltRotateFailureThreshold
	}
	_, _, oldSyncEmpty := a.loadOps(ctxBG())
	if dto.SyncEmptyClearThreshold == 0 {
		dto.SyncEmptyClearThreshold = oldSyncEmpty // 未发送字段时沿用现值
	}
	if dto.BlockPrivateTargets == nil {
		dto.BlockPrivateTargets = boolPtr(old.BlockPrivateTargets)
	}
	if dto.ListenAuth != "" {
		i := strings.IndexByte(dto.ListenAuth, ':')
		if i <= 0 || i == len(dto.ListenAuth)-1 {
			writeErr(w, 400, "invalid_listen_auth", "listen_auth must be user:pass")
			return
		}
	}
	// 监听 TLS 路径：去空白、成对校验、可解析校验（启用前就把配置错误挡在保存时）。
	// 有效期异常（已过期/未生效）不拒绝保存——证书会自然到期，硬拒会造成
	// "设置页存不了任何改动"的反向锁死；改为随保存结果返回警告。
	dto.ListenTLSCert = strings.TrimSpace(dto.ListenTLSCert)
	dto.ListenTLSKey = strings.TrimSpace(dto.ListenTLSKey)
	dto.AdminTLSCert = strings.TrimSpace(dto.AdminTLSCert)
	dto.AdminTLSKey = strings.TrimSpace(dto.AdminTLSKey)
	var tlsWarnings []string
	for _, p := range []struct{ what, cert, key string }{
		{"ingress", dto.ListenTLSCert, dto.ListenTLSKey},
		{"admin", dto.AdminTLSCert, dto.AdminTLSKey},
	} {
		warn, err := validateTLSPair(p.what, p.cert, p.key)
		if err != nil {
			writeErr(w, 400, "invalid_tls_pair", err.Error())
			return
		}
		if warn != "" {
			tlsWarnings = append(tlsWarnings, warn)
		}
	}
	switch dto.NoMarkerPolicy {
	case settings.PolicyDefaultSession, settings.PolicyClientIPSession, settings.PolicyDirect:
	default:
		writeErr(w, 400, "invalid_policy", "invalid no_marker_policy")
		return
	}
	if len(dto.MarkerHeaders) == 0 {
		writeErr(w, 400, "invalid_rules", "header list must not be empty")
		return
	}
	// 路径片段允许为空 = 对所有路径生效（推荐）；填写时片段须以 / 开头
	for _, p := range dto.MarkerPathParts {
		if !strings.HasPrefix(p, "/") {
			writeErr(w, 400, "invalid_rules", "path part must start with /: "+p)
			return
		}
	}
	if dto.HashSalt == "" {
		writeErr(w, 400, "invalid_salt", "salt must not be empty")
		return
	}
	if dto.SIDLen < 4 || dto.SIDLen > 64 {
		writeErr(w, 400, "invalid_sidlen", "sid_len must be 4-64")
		return
	}
	if dto.SessionTTLMin < 0 || dto.SessionTTLMin > 10080 {
		writeErr(w, 400, "invalid_ttl", "invalid session_ttl_min")
		return
	}
	if dto.SaltRotateFailureThreshold < 1 || dto.SaltRotateFailureThreshold > 100 {
		writeErr(w, 400, "invalid_salt_rotate_failure_threshold", "salt_rotate_failure_threshold must be 1-100")
		return
	}
	// 黑白名单条目严格校验：非法项整体拒绝保存（运行期另有容错跳过）
	if err := acl.ValidateLists(dto.ACLWhitelist, dto.ACLBlacklist); err != nil {
		writeErr(w, 400, "invalid_acl", err.Error())
		return
	}
	if dto.LogRetentionDays < 1 || dto.LogRetentionDays > 3650 {
		writeErr(w, 400, "invalid_retention", "log_retention_days must be 1-3650")
		return
	}
	if dto.SyncEmptyClearThreshold < 1 || dto.SyncEmptyClearThreshold > 100 {
		writeErr(w, 400, "invalid_sync_empty_clear_threshold", "sync_empty_clear_threshold must be 1-100")
		return
	}
	if dto.DefaultUpstream != "" {
		rows, err := a.d.Store.ListUpstreams(ctxBG())
		if err != nil {
			a.failInternal(w, r, err)
			return
		}
		found := false
		for _, rw := range rows {
			if rw.Name == dto.DefaultUpstream && rw.Enabled {
				found = true
			}
		}
		if !found {
			writeErr(w, 409, "unknown_upstream", "default upstream missing or disabled")
			return
		}
	}

	newSnap := settings.Snapshot{
		ListenTLSCert: dto.ListenTLSCert, ListenTLSKey: dto.ListenTLSKey,
		AdminTLSCert:    dto.AdminTLSCert,
		AdminTLSKey:     dto.AdminTLSKey,
		ListenAuth:      dto.ListenAuth,
		DefaultUpstream: dto.DefaultUpstream, NoMarkerPolicy: dto.NoMarkerPolicy,
		MarkerRules: settings.MarkerRules{PathParts: dto.MarkerPathParts, Headers: dto.MarkerHeaders},
		Salt:        dto.HashSalt, SIDLen: dto.SIDLen, SessionTTLMin: dto.SessionTTLMin,
		SaltRotateFailureThreshold: dto.SaltRotateFailureThreshold,
		BlockPrivateTargets:        *dto.BlockPrivateTargets,
		ACLWhitelist:               dto.ACLWhitelist,
		ACLBlacklist:               dto.ACLBlacklist,
		// 设置接口不暴露 acctmap_enabled；保存时必须沿用现值，
		// 否则零值 false 会在每次保存时静默关闭账号映射。
		AcctMapEnabled: old.AcctMapEnabled,
	}
	if newSnap.SessionTTLMin != old.SessionTTLMin {
		rows, err := a.d.Store.ListUpstreams(ctxBG())
		if err != nil {
			a.failInternal(w, r, err)
			return
		}
		for _, row := range rows {
			if row.Platform != upstream.PlatformGeneric || !row.Inject.Valid || strings.TrimSpace(row.Inject.String) == "" {
				continue
			}
			if err := upstream.ValidateForSave(row.Platform, row.BaseURL, row.Inject.String, newSnap.SessionTTLMin); err != nil {
				writeErr(w, http.StatusBadRequest, "invalid_ttl",
					fmt.Sprintf("existing generic upstream %q is incompatible with session_ttl_min: %v", row.Name, err))
				return
			}
		}
	}
	ctx := ctxBG()
	if err := settings.SaveSnapshotWithOps(ctx, a.d.Store, newSnap,
		dto.LogRetentionDays, dto.MetricsEnabled, dto.SyncEmptyClearThreshold); err != nil {
		a.failInternal(w, r, err)
		return
	}

	reloaded, err := settings.LoadFromStore(ctx, a.d.Store, newSnap)
	if err != nil {
		a.failInternal(w, r, err)
		return
	}
	a.d.Settings.Set(reloaded)
	a.rebuildTable(r.Context())

	resp := map[string]any{
		"ok": true,
		"restart_required": old.ListenTLSCert != reloaded.ListenTLSCert || old.ListenTLSKey != reloaded.ListenTLSKey ||
			old.AdminTLSCert != reloaded.AdminTLSCert || old.AdminTLSKey != reloaded.AdminTLSKey,
	}
	if len(tlsWarnings) > 0 {
		resp["warnings"] = tlsWarnings
	}
	writeJSON(w, 200, resp)
}

// ingressBaseURL 根据当前接入监听还原对外接入地址：主机名取自管理台请求
// Host，端口固定为接入监听端口。接入协议由 ingressTLS 决定，不能由独立管理台
// 监听的请求协议推断。
func ingressBaseURL(r *http.Request, ingressPort string, ingressTLS bool) string {
	scheme := "http"
	if ingressTLS {
		scheme = "https"
	}
	host := ""
	if r != nil {
		host = r.Host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h // 剥掉请求自带的端口（那是管理台的）
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if ingressPort == "" {
		ingressPort = "55666" // 与 cmd 默认监听一致，兜底
	}
	return scheme + "://" + net.JoinHostPort(host, ingressPort)
}

// ingressURLWithAuth 在接入基础地址中插入入站认证 user:pass；
// 未启用认证时返回空串（前端据此隐藏带凭据行）。使用 url.UserPassword
// 编码保留字符；此函数只生成管理台展示值，不参与流量转发。
func ingressURLWithAuth(base, listenAuth string) string {
	if listenAuth == "" {
		return ""
	}
	i := strings.IndexByte(listenAuth, ':')
	if i <= 0 {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	u.User = url.UserPassword(listenAuth[:i], listenAuth[i+1:])
	return u.String()
}

// validateTLSPair 校验监听 TLS 路径对：要么都空（不启用），要么都填且当前进程
// 就能成功加载解析——把配置错误挡在保存时，而不是重启失败。
// 返回值：(警告, 硬错误)。可解析但有效期异常（已过期/未生效）属于时间性状态，
// 只告警不拒绝——硬拒会导致证书自然到期后设置页无法保存任何改动的锁死。
func validateTLSPair(what, certPath, keyPath string) (warn string, err error) {
	if certPath == "" && keyPath == "" {
		return "", nil
	}
	if certPath == "" || keyPath == "" {
		return "", fmt.Errorf("%s tls: certificate and key paths must be set together", what)
	}
	certObj, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return "", fmt.Errorf("%s tls: cannot load certificate pair: %v", what, err)
	}
	leaf, err := x509.ParseCertificate(certObj.Certificate[0])
	if err != nil {
		return "", fmt.Errorf("%s tls: cannot parse certificate: %v", what, err)
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) {
		return fmt.Sprintf("%s tls: certificate is not yet valid (not_before=%s)", what, leaf.NotBefore.Format(time.RFC3339)), nil
	}
	if now.After(leaf.NotAfter) {
		return fmt.Sprintf("%s tls: certificate EXPIRED at %s; renew (e.g. certbot renew) then restart to load it", what, leaf.NotAfter.Format(time.RFC3339)), nil
	}
	return "", nil
}

func (a *API) resetSalt(w http.ResponseWriter, r *http.Request) {
	snap := a.d.Settings.Current()
	snap.Salt = randHex(16)
	ctx := ctxBG()
	if err := settings.SaveSnapshot(ctx, a.d.Store, snap); err != nil {
		a.failInternal(w, r, err)
		return
	}
	a.d.Settings.Set(snap)
	a.logger().Log(r.Context(), slog.LevelWarn, "sticky salt reset: all accounts will be re-assigned exits")
	writeJSON(w, 200, map[string]any{"ok": true, "hash_salt": snap.Salt})
}

// ---------- 上游 ----------

type upstreamDTO struct {
	ID       int64           `json:"id"`
	Name     string          `json:"name"`
	Platform string          `json:"platform"`
	BaseURL  string          `json:"base_url"` // 密码段打码为 ____
	Inject   json.RawMessage `json:"inject,omitempty"`
	Enabled  bool            `json:"enabled"`
	Default  bool            `json:"default"`
}

// maskInjectPassword 对 generic 的 inject JSON 中密码字段打码为 ____。
func maskInjectPassword(raw string) json.RawMessage {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	m := map[string]any{}
	if json.Unmarshal([]byte(raw), &m) != nil {
		return nil
	}
	if pw, ok := m["password"]; ok && fmt.Sprint(pw) != "" && fmt.Sprint(pw) != maskToken {
		m["password"] = maskToken
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return out
}

// mergeInjectPassword 用旧 JSON 的真实密码回填新提交里的 ____/keep 占位。
func mergeInjectPassword(newRaw, oldRaw string) string {
	var nm, om map[string]any
	json.Unmarshal([]byte(newRaw), &nm)
	json.Unmarshal([]byte(oldRaw), &om)
	if nm == nil {
		return newRaw
	}
	if pw, ok := nm["password"]; ok {
		if s, _ := pw.(string); s == maskToken || s == keepToken {
			if op, ok2 := om["password"]; ok2 {
				nm["password"] = op
			} else {
				delete(nm, "password")
			}
		}
	}
	out, _ := json.Marshal(nm)
	return string(out)
}

func maskURLPassword(raw string) string {
	u, err := urlParse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, has := u.User.Password(); !has {
		return raw
	}
	user := u.User.Username()
	cp := *u
	cp.User = urlUserPassword(user, maskToken)
	return cp.String()
}

// mergeURLPassword：新提交 URL 的密码段为 ____/__unchanged__ 时沿用旧密码。
func mergeURLPassword(newRaw, oldRaw string) (string, error) {
	nu, err := urlParse(newRaw)
	if err != nil {
		return "", err
	}
	ou, oerr := urlParse(oldRaw)
	if nu.User != nil {
		if pw, has := nu.User.Password(); has && (pw == maskToken || pw == keepToken) {
			if oerr == nil && ou.User != nil {
				if oldPw, has2 := ou.User.Password(); has2 {
					cp := *nu
					cp.User = urlUserPassword(nu.User.Username(), oldPw)
					return cp.String(), nil
				}
			}
			cp := *nu
			cp.User = urlUser(nu.User.Username())
			return cp.String(), nil
		}
	}
	return newRaw, nil
}

func (a *API) rebuildTable(ctx context.Context) {
	rows, err := a.d.Store.ListUpstreams(ctxBG())
	if err != nil {
		a.logger().Log(ctx, slog.LevelError, "failed to rebuild upstream table", "err", err)
		return
	}
	items := make([]*upstream.Upstream, 0, len(rows))
	for _, rw := range rows {
		if u, err := upstream.FromRow(rw.ID, rw.Name, rw.Platform, rw.BaseURL, rw.Inject, rw.Enabled); err == nil {
			items = append(items, u)
		}
	}
	a.d.SwapUpstreams(upstream.NewTable(items, a.d.Settings.Current().DefaultUpstream))
}

func (a *API) listUpstreams(w http.ResponseWriter, r *http.Request) {
	rows, err := a.d.Store.ListUpstreams(ctxBG())
	if err != nil {
		a.failInternal(w, r, err)
		return
	}
	def := a.d.Settings.Current().DefaultUpstream
	out := make([]upstreamDTO, 0, len(rows))
	for _, rw := range rows {
		dto := upstreamDTO{
			ID: rw.ID, Name: rw.Name, Platform: rw.Platform,
			BaseURL: maskURLPassword(rw.BaseURL), Enabled: rw.Enabled,
			Default: rw.Name == def,
		}
		if rw.Platform == upstream.PlatformGeneric && rw.Inject.Valid && strings.TrimSpace(rw.Inject.String) != "" {
			dto.Inject = maskInjectPassword(rw.Inject.String)
		}
		out = append(out, dto)
	}
	writeJSON(w, 200, map[string]any{"items": out, "total": len(out)})
}

type upsertBody struct {
	Name     string          `json:"name"`
	Platform string          `json:"platform"`
	BaseURL  string          `json:"base_url"`
	Inject   json.RawMessage `json:"inject"`
	Enabled  *bool           `json:"enabled"`
}

// normalizeInject 把请求中的 inject 字段统一为"对象形态的 JSON 文本"。
// Web UI 的模板文本框以字符串承载 JSON（"inject":"{\"u\":...}"，textarea 内容
// 原样入 payload），程序化调用则内嵌对象（"inject":{...}）；两种形态都接受。
// 空值返回空串；字符串内容不是 JSON 对象时原样保留（是否合法模板统一由
// ValidateForSave 判定并报错，这里不做第二套字段校验）。
func normalizeInject(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return string(raw) // 非字符串：即对象形态
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return string(raw) // 字符串内容不是 JSON 对象：原样保留
	}
	return strings.TrimSpace(s)
}

func (a *API) createUpstream(w http.ResponseWriter, r *http.Request) {
	var b upsertBody
	if err := readJSON(r, &b); err != nil {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}
	if b.Name == "" || b.BaseURL == "" {
		writeErr(w, 400, "bad_request", "name/base_url required")
		return
	}
	if b.Enabled == nil {
		b.Enabled = boolPtr(true)
	}
	snap := a.d.Settings.Current()
	inj := normalizeInject(b.Inject)
	if err := upstream.ValidateForSave(b.Platform, b.BaseURL, inj, snap.SessionTTLMin); err != nil {
		writeErr(w, 400, "validation_failed", err.Error())
		return
	}
	rows, _ := a.d.Store.ListUpstreams(ctxBG())
	for _, rw := range rows {
		if rw.Name == b.Name {
			writeErr(w, 409, "duplicate_name", "name already exists")
			return
		}
	}
	id, err := a.d.Store.CreateUpstream(ctxBG(), b.Name, b.Platform, b.BaseURL, inj, *b.Enabled)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeErr(w, 409, "duplicate_name", "name already exists")
			return
		}
		a.failInternal(w, r, err)
		return
	}
	a.rebuildTable(r.Context())
	writeJSON(w, 200, map[string]any{"id": id})
}

func (a *API) updateUpstream(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad_id", "invalid id")
		return
	}
	var b upsertBody
	if err := readJSON(r, &b); err != nil {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}

	rows, _ := a.d.Store.ListUpstreams(ctxBG())
	var old *store.UpstreamRow
	for i := range rows {
		if rows[i].ID == id {
			old = &rows[i]
		}
	}
	if old == nil {
		writeErr(w, 404, "not_found", "entry not found")
		return
	}

	snap := a.d.Settings.Current()
	isDefaultRow := old.Name == snap.DefaultUpstream

	name := old.Name
	if b.Name != "" {
		name = b.Name
	}
	// 默认条目不可停用（与删除守卫对齐），否则转发静默回落兜底策略
	if isDefaultRow && b.Enabled != nil && !*b.Enabled {
		writeErr(w, 409, "is_default", "cannot disable the default upstream; switch default first")
		return
	}
	platform := old.Platform
	if b.Platform != "" {
		platform = b.Platform
	}
	baseURL := old.BaseURL
	if b.BaseURL != "" {
		merged, merr := mergeURLPassword(b.BaseURL, old.BaseURL)
		if merr != nil {
			writeErr(w, 400, "bad_url", merr.Error())
			return
		}
		baseURL = merged
	}
	inj := old.Inject.String
	if platform == upstream.PlatformGeneric {
		if txt := normalizeInject(b.Inject); txt != "" {
			inj = mergeInjectPassword(txt, old.Inject.String)
		}
	}
	enabled := old.Enabled
	if b.Enabled != nil {
		enabled = *b.Enabled
	}

	if err := upstream.ValidateForSave(platform, baseURL, inj, snap.SessionTTLMin); err != nil {
		writeErr(w, 400, "validation_failed", err.Error())
		return
	}
	updated := store.UpstreamRow{
		ID: id, Name: name, Platform: platform, BaseURL: baseURL,
		Inject: nullStr(inj), Enabled: enabled,
		CreatedAt: old.CreatedAt, UpdatedAt: time.Now().UnixMilli(),
	}
	var updateErr error
	if isDefaultRow && name != old.Name {
		updateErr = a.d.Store.UpdateUpstreamAndDefault(ctxBG(), updated, name)
	} else {
		updateErr = a.d.Store.UpdateUpstream(ctxBG(), updated)
	}
	if updateErr != nil {
		if errors.Is(updateErr, store.ErrUpstreamHasBindings) {
			writeErr(w, http.StatusConflict, "bound_upstream", "cannot change platform while account bindings exist")
			return
		}
		if strings.Contains(updateErr.Error(), "UNIQUE") {
			writeErr(w, 409, "duplicate_name", "name already exists")
			return
		}
		a.failInternal(w, r, updateErr)
		return
	}
	// 默认条目改名时级联更新 default_upstream 指向，避免悬空回落
	if isDefaultRow && name != old.Name {
		if s2 := a.d.Settings.Current(); s2.DefaultUpstream == old.Name {
			s2.DefaultUpstream = name
			a.d.Settings.Set(s2)
		}
	}
	a.rebuildTable(r.Context())
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *API) deleteUpstream(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad_id", "invalid id")
		return
	}
	snap := a.d.Settings.Current()
	rows, _ := a.d.Store.ListUpstreams(ctxBG())
	for _, rw := range rows {
		if rw.ID == id {
			if rw.Name == snap.DefaultUpstream {
				writeErr(w, 409, "is_default", "entry is the current default upstream; switch default first")
				return
			}
			if err := a.d.Store.DeleteUpstream(ctxBG(), id); err != nil {
				a.failInternal(w, r, err)
				return
			}
			a.rebuildTable(r.Context())
			a.reloadAcctEgress() // 删出站级联清了绑定行（store 层同事务）
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
	}
	writeErr(w, 404, "not_found", "entry not found")
}

func (a *API) setDefault(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad_id", "invalid id")
		return
	}
	rows, _ := a.d.Store.ListUpstreams(ctxBG())
	for _, rw := range rows {
		if rw.ID == id {
			if !rw.Enabled {
				writeErr(w, 409, "disabled", "disabled entry cannot be set as default")
				return
			}
			b, _ := json.Marshal(rw.Name)
			if err := a.d.Store.SetSetting(ctxBG(), "default_upstream", string(b)); err != nil {
				a.failInternal(w, r, err)
				return
			}
			s := a.d.Settings.Current()
			s.DefaultUpstream = rw.Name
			a.d.Settings.Set(s)
			a.rebuildTable(r.Context())
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
	}
	writeErr(w, 404, "not_found", "entry not found")
}

func (a *API) testUpstream(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad_id", "invalid id")
		return
	}
	rows, _ := a.d.Store.ListUpstreams(ctxBG())
	var row *store.UpstreamRow
	for i := range rows {
		if rows[i].ID == id {
			row = &rows[i]
		}
	}
	if row == nil {
		writeErr(w, 404, "not_found", "entry not found")
		return
	}
	up, err := upstream.FromRow(row.ID, row.Name, row.Platform, row.BaseURL, row.Inject, row.Enabled)
	if err != nil {
		writeErr(w, 400, "bad_row", err.Error())
		return
	}
	inj, ok := upstream.InjectorFor(row.Platform, up)
	if !ok {
		writeErr(w, 400, "no_injector", "platform injector not registered")
		return
	}
	purl, err := inj.Inject(up.BaseURL, upstream.InjectParams{Account: "healthcheck",
		TTLMin: a.d.Settings.Current().SessionTTLMin})
	if err != nil {
		writeErr(w, 400, "inject_failed", err.Error())
		return
	}

	start := time.Now()
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(purl), DisableCompression: true},
	}
	fail := func(err error) {
		writeJSON(w, 200, map[string]any{"egress_ip": "", "dur_ms": time.Since(start).Milliseconds(), "err": err.Error()})
	}
	// 主路径：ipinfo.io 免 token 返回出口 IP 与地理位置；失败回退 ipify 仅取出口 IP。
	resp, err := client.Get("https://ipinfo.io/json")
	if err != nil {
		if resp2, err2 := client.Get("https://api.ipify.org"); err2 == nil {
			body, _ := io.ReadAll(io.LimitReader(resp2.Body, 128))
			resp2.Body.Close()
			writeJSON(w, 200, map[string]any{
				"egress_ip": strings.TrimSpace(string(body)),
				"dur_ms":    time.Since(start).Milliseconds(),
				"status":    resp2.StatusCode,
			})
			return
		}
		fail(err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var gi struct {
		IP       string `json:"ip"`
		City     string `json:"city"`
		Region   string `json:"region"`
		Country  string `json:"country"`
		Loc      string `json:"loc"`
		Org      string `json:"org"`
		Timezone string `json:"timezone"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &gi) != nil || gi.IP == "" {
		fail(fmt.Errorf("geo probe http %d", resp.StatusCode))
		return
	}
	writeJSON(w, 200, map[string]any{
		"egress_ip": gi.IP,
		"country":   gi.Country,
		"region":    gi.Region,
		"city":      gi.City,
		"loc":       gi.Loc,
		"org":       gi.Org,
		"timezone":  gi.Timezone,
		"dur_ms":    time.Since(start).Milliseconds(),
		"status":    resp.StatusCode,
	})
}

// ---------- 审计 ----------

func (a *API) listLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.LogFilter{
		Q: q.Get("q"), Account: q.Get("account"), Upstream: q.Get("upstream"),
		StatusClass: q.Get("class"),
	}
	if v, err := strconv.ParseInt(q.Get("from"), 10, 64); err == nil {
		f.FromMs = v
	}
	if v, err := strconv.ParseInt(q.Get("to"), 10, 64); err == nil {
		f.ToMs = v
	}
	if v, err := strconv.Atoi(q.Get("page")); err == nil {
		f.Page = v
	}
	if v, err := strconv.Atoi(q.Get("page_size")); err == nil {
		f.PageSize = v
	}
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize <= 0 || f.PageSize > 200 {
		f.PageSize = 50
	}
	if f.StatusClass == "" {
		if v, err := strconv.Atoi(q.Get("status")); err == nil && v > 0 {
			f.StatusClass = classOf(v)
		}
	}
	items, total, err := a.d.Store.ListLogs(ctxBG(), f)
	if err != nil {
		a.failInternal(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total, "page": f.Page, "page_size": f.PageSize})
}

func classOf(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500:
		return "5xx"
	}
	return ""
}

func (a *API) clearLogs(w http.ResponseWriter, r *http.Request) {
	if err := a.d.Store.ClearLogs(ctxBG()); err != nil {
		a.failInternal(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 更新记录（docs/013-update-log-design.md） ----------

// emitUpdate 记录一条映射更新事件；通道未接线时为 no-op。
func (a *API) emitUpdate(kind, source, status, summary, detail string) {
	store.SendUpdateEvent(a.d.Updates, store.NewUpdateEvent(kind, source, status, summary, detail))
}

func (a *API) listUpdates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.UpdateFilter{Kind: q.Get("kind"), Source: q.Get("source"), Status: q.Get("status")}
	if v, err := strconv.ParseInt(q.Get("from"), 10, 64); err == nil {
		f.FromMs = v
	}
	if v, err := strconv.ParseInt(q.Get("to"), 10, 64); err == nil {
		f.ToMs = v
	}
	if v, err := strconv.Atoi(q.Get("page")); err == nil {
		f.Page = v
	}
	if v, err := strconv.Atoi(q.Get("page_size")); err == nil {
		f.PageSize = v
	}
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize <= 0 || f.PageSize > 200 {
		f.PageSize = 50
	}
	items, total, err := a.d.Store.ListUpdateEvents(ctxBG(), f)
	if err != nil {
		a.failInternal(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total, "page": f.Page, "page_size": f.PageSize})
}

func (a *API) clearUpdates(w http.ResponseWriter, r *http.Request) {
	if err := a.d.Store.ClearUpdateEvents(ctxBG()); err != nil {
		a.failInternal(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 其他 ----------

func (a *API) caPEM(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="mitmsticky-root-ca.pem"`)
	w.Write(a.d.CA.CertificatePEM())
}

func (a *API) metrics(w http.ResponseWriter, r *http.Request) {
	_, enabled, _ := a.loadOps(ctxBG())
	if !enabled {
		writeErr(w, http.StatusNotFound, "disabled", "metrics disabled (enable in base settings)")
		return
	}
	a.metricRegistry().HTTPHandler()(w, r)
}

func (a *API) metricRegistry() *metrics.Registry {
	if a.d.Metrics != nil {
		return a.d.Metrics
	}
	return metrics.Default
}

// caCRT 输出 DER 编码证书（Windows 双击即可导入）。
func (a *API) caCRT(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="mitmsticky-root-ca.crt"`)
	w.Write(a.d.CA.CertificateDER())
}
