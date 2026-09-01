// Package settings 维护全局设置的内存快照（atomic.Value）与热更新。
// 转发路径只读快照、零锁；保存路径：校验 → 落库 → 换快照。
package settings

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync/atomic"

	"mitmrouter/internal/acl"
	"mitmrouter/internal/httpnames"
	"mitmrouter/internal/marker"
	"mitmrouter/internal/store"
)

// MarkerRules 与文档命名保持一致的别名。
type MarkerRules = marker.Rules

// 无 Marker 时的兜底策略。
const (
	PolicyDefaultSession  = "default_session"   // 固定身份 "default"
	PolicyClientIPSession = "client_ip_session" // 按来源 IP 推导身份
	PolicyDirect          = "direct"            // 不经上游直连目标
)

// Snapshot 是转发路径使用的不可变设置集合。
// 注意：仅承载转发路径所需字段；log_retention_days 等运维项由 store 层按需查库。
// 监听地址（接入口/管理台 IP:port）不在此处——由启动参数指定，每次启动生效。
type Snapshot struct {
	ListenAuth                 string `json:"listen_auth"` // "user:pass"，空=关闭入站认证
	DefaultUpstream            string `json:"default_upstream"`
	NoMarkerPolicy             string `json:"no_marker_policy"`
	MarkerRules                marker.Rules
	Salt                       string `json:"hash_salt"`
	SIDLen                     int    `json:"sid_len"`
	SessionTTLMin              int    `json:"session_ttl_min"`               // 0=不干预平台 TTL 参数
	SaltRotateFailureThreshold int    `json:"salt_rotate_failure_threshold"` // 连续可轮换错误达到此数时换盐

	// 监听 TLS（可选）：证书/私钥 PEM 文件路径，成对填写才启用；
	// 启用后该端口强制 HTTPS-only（明文连接在握手期即被拒）。
	// 路径变更需重启生效；同路径下的文件内容变化（如 certbot 续期）由运行期热重载。
	ListenTLSCert string `json:"listen_tls_cert"`
	ListenTLSKey  string `json:"listen_tls_key"`
	AdminTLSCert  string `json:"admin_tls_cert"`
	AdminTLSKey   string `json:"admin_tls_key"`

	// BlockPrivateTargets 拒绝访问回环和私网目标的转发请求。默认值为 true，
	// 因为允许这些请求会让公网可达的本服务成为访问本机和内网的通道。
	BlockPrivateTargets bool `json:"block_private_targets"`

	// AcctMapEnabled: 账号映射表开关（docs/004-stable-account-hash-design.md）。
	// 关闭后全部流量回落 v3 纯 Marker 哈希公式（逃生门）。默认开启。
	AcctMapEnabled bool `json:"acctmap_enabled"`

	// 目标黑白名单（ACL）：条目支持 IP / CIDR / 精确域名 / *.通配符域名。
	// 黑名单命中即拒绝；白名单非空时仅放行命中目标；黑名单永远优先。
	ACLWhitelist []string `json:"acl_whitelist"` // 空 = 不做放行限制
	ACLBlacklist []string `json:"acl_blacklist"`

	acl *acl.Rules // 编译后的匹配器（非序列化字段），Holder.Set 时重建
}

// ACLAllowed 判定目标主机是否允许转发。
// 未编译的快照（acl==nil）默认全放行，保证手工构造的快照不会意外阻断流量。
func (s Snapshot) ACLAllowed(target string) bool {
	if s.acl == nil {
		return true
	}
	return s.acl.Allowed(target)
}

// ACLIntercept 判定已允许目标主机是否应被拦截解析（MITM）。
// false 只表示不进入解析路径；调用方必须先调用 ACLAllowed，不能把 false
// 当成透明放行的依据。未编译快照默认返回 true。
func (s Snapshot) ACLIntercept(target string) bool {
	if s.acl == nil {
		return true
	}
	return s.acl.Intercept(target)
}

// compile 重建 ACL 匹配器。运行期容错：非法条目跳过并告警，
// 避免手改数据库后路由整体瘫痪；但白名单原始配置过且没有有效条目时
// 仍然 fail-closed。管理台保存路径另有严格校验。
func (s *Snapshot) compile() {
	r, skipped := acl.NewRules(s.ACLWhitelist, s.ACLBlacklist)
	if skipped > 0 {
		slog.Warn("settings: ACL contains invalid entries, skipped", "skipped", skipped)
	}
	s.acl = r
}

// DefaultSnapshot 返回默认设置（Salt 由引导阶段随机生成后覆盖）。
func DefaultSnapshot() Snapshot {
	return Snapshot{
		ListenAuth:     "",
		NoMarkerPolicy: PolicyDefaultSession,
		MarkerRules: MarkerRules{
			// PathParts 留空 = 所有 URL 同一规则（推荐默认，保证同凭据跨路径身份一致）
			Headers: []string{httpnames.HeaderAuthorization, httpnames.HeaderXAPIKey, httpnames.HeaderAPIKey, httpnames.HeaderXGoogAPIKey},
		},
		SIDLen:                     16,
		SaltRotateFailureThreshold: 2,
		BlockPrivateTargets:        true,
		AcctMapEnabled:             true,
	}
}

// Holder 以 atomic.Value 承载快照。
type Holder struct{ v atomic.Value }

// NewHolder 创建并填充初始快照。
func NewHolder(snap Snapshot) *Holder {
	h := &Holder{}
	h.Set(snap)
	return h
}

// Current 读取当前快照（零锁）。
func (h *Holder) Current() Snapshot {
	if s, ok := h.v.Load().(Snapshot); ok {
		return s
	}
	return Snapshot{}
}

// Set 原子替换快照，并重建 ACL 编译缓存（保证转发路径读到的快照总是可判定）。
func (h *Holder) Set(snap Snapshot) {
	snap.compile()
	h.v.Store(snap)
}

// LoadFromStore 从库里读全部设置键合成快照；缺失键用 fallback 兜底。
func LoadFromStore(ctx context.Context, st *store.Store, fb Snapshot) (Snapshot, error) {
	m, err := st.AllSettings(ctx)
	if err != nil {
		return fb, err
	}
	get := func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
	unmarshal := func(k string, dst any) {
		if v, ok := get(k); ok {
			if err := json.Unmarshal([]byte(v), dst); err != nil {
				slog.Warn("settings: parse failed, using default", "key", k, "err", err)
			}
		}
	}
	snap := fb
	unmarshal("listen_auth", &snap.ListenAuth)
	unmarshal("default_upstream", &snap.DefaultUpstream)
	unmarshal("no_marker_policy", &snap.NoMarkerPolicy)
	unmarshal("marker_path_parts", &snap.MarkerRules.PathParts)
	unmarshal("marker_headers", &snap.MarkerRules.Headers)
	unmarshal("hash_salt", &snap.Salt)
	unmarshal("sid_len", &snap.SIDLen)
	unmarshal("session_ttl_min", &snap.SessionTTLMin)
	unmarshal("salt_rotate_failure_threshold", &snap.SaltRotateFailureThreshold)
	unmarshal("block_private_targets", &snap.BlockPrivateTargets)
	unmarshal("acctmap_enabled", &snap.AcctMapEnabled)
	unmarshal("listen_tls_cert", &snap.ListenTLSCert)
	unmarshal("listen_tls_key", &snap.ListenTLSKey)
	unmarshal("admin_tls_cert", &snap.AdminTLSCert)
	unmarshal("admin_tls_key", &snap.AdminTLSKey)
	unmarshal("acl_whitelist", &snap.ACLWhitelist)
	unmarshal("acl_blacklist", &snap.ACLBlacklist)
	// 规整：TLS 路径去空白。旧库遗留的 listen_addr/admin_addr 键不再读取
	// （监听地址已改为启动参数指定），留在表中无副作用。
	snap.ListenTLSCert = strings.TrimSpace(snap.ListenTLSCert)
	snap.ListenTLSKey = strings.TrimSpace(snap.ListenTLSKey)
	snap.AdminTLSCert = strings.TrimSpace(snap.AdminTLSCert)
	snap.AdminTLSKey = strings.TrimSpace(snap.AdminTLSKey)
	if snap.SIDLen < 4 {
		snap.SIDLen = 4
	} else if snap.SIDLen > 64 {
		snap.SIDLen = 64
	}
	if snap.SaltRotateFailureThreshold < 1 {
		snap.SaltRotateFailureThreshold = 2
	} else if snap.SaltRotateFailureThreshold > 100 {
		snap.SaltRotateFailureThreshold = 100
	}
	switch snap.NoMarkerPolicy {
	case PolicyDefaultSession, PolicyClientIPSession, PolicyDirect:
	default:
		snap.NoMarkerPolicy = PolicyDefaultSession
	}
	return snap, nil
}

func jsonSetting(v any) string {
	out, _ := json.Marshal(v)
	return string(out)
}

func snapshotSettings(snap Snapshot) map[string]string {
	return map[string]string{
		"listen_auth":                   jsonSetting(snap.ListenAuth),
		"default_upstream":              jsonSetting(snap.DefaultUpstream),
		"no_marker_policy":              jsonSetting(snap.NoMarkerPolicy),
		"marker_path_parts":             jsonSetting(snap.MarkerRules.PathParts),
		"marker_headers":                jsonSetting(snap.MarkerRules.Headers),
		"hash_salt":                     jsonSetting(snap.Salt),
		"sid_len":                       jsonSetting(snap.SIDLen),
		"session_ttl_min":               jsonSetting(snap.SessionTTLMin),
		"salt_rotate_failure_threshold": jsonSetting(snap.SaltRotateFailureThreshold),
		"block_private_targets":         jsonSetting(snap.BlockPrivateTargets),
		"acctmap_enabled":               jsonSetting(snap.AcctMapEnabled),
		"listen_tls_cert":               jsonSetting(snap.ListenTLSCert),
		"listen_tls_key":                jsonSetting(snap.ListenTLSKey),
		"admin_tls_cert":                jsonSetting(snap.AdminTLSCert),
		"admin_tls_key":                 jsonSetting(snap.AdminTLSKey),
		"acl_whitelist":                 jsonSetting(snap.ACLWhitelist),
		"acl_blacklist":                 jsonSetting(snap.ACLBlacklist),
	}
}

// SaveSnapshot 把核心快照全量写回设置表（管理台 PUT 路径使用）。
func SaveSnapshot(ctx context.Context, st *store.Store, snap Snapshot) error {
	return st.SetSettingsTx(ctx, snapshotSettings(snap))
}

// SaveSnapshotWithOps 把核心快照和运维设置放进同一个事务。
func SaveSnapshotWithOps(ctx context.Context, st *store.Store, snap Snapshot,
	logRetentionDays int, metricsEnabled bool, syncEmptyClearThreshold int) error {
	kv := snapshotSettings(snap)
	kv["log_retention_days"] = jsonSetting(logRetentionDays)
	kv["metrics_enabled"] = jsonSetting(metricsEnabled)
	kv["sync_empty_clear_threshold"] = jsonSetting(syncEmptyClearThreshold)
	return st.SetSettingsTx(ctx, kv)
}
