// Package acctmap 维护"订阅号凭据指纹 → (平台, 账号ID)"的内存注册表，
// 以及凭据归一化、指纹计算与 AI 上游 Host→平台 的映射。
// 设计：docs/004-stable-account-hash-design.md。
//
// 线程模型：热路径只读（RWMutex 读锁），写入走全量重载——
// 表规模为订阅号凭据数（千级以内），写路径低频（拉取周期/手动操作），全量换表最简单且无竞态。
package acctmap

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"

	"mitmrouter/internal/httpnames"
)

// Entry 是映射表的一行：一个账号在某来源实例下的一套凭据。
type Entry struct {
	Platform   string `json:"platform"`
	Account    string `json:"account"`
	AtFp       string `json:"at_fp"`   // access_token 指纹（空=未知）
	RtFp       string `json:"rt_fp"`   // refresh_token 指纹
	AtHint     string `json:"at_hint"` // 展示尾缀："…xx"
	RtHint     string `json:"rt_hint"`
	Source     string `json:"source"`      // 来源实例：'src:<id>' / 'api'
	SourceType string `json:"source_type"` // 来源类型全名：CLIProxyAPI/Sub2API/自定义
	UpdatedAt  int64  `json:"updated_at"`  // 最近写入时间（毫秒），管理面展示用，不参与索引
}

// KnownSourceTypes 内置来源类型清单（前端建议项；不限制自定义值）。
var KnownSourceTypes = []string{SourceTypeCLIProxyAPI, SourceTypeSub2API}

// SourceTypeForKind 把 sync_sources.kind 映射为来源类型全名。
func SourceTypeForKind(kind string) string {
	switch kind {
	case SourceKindCLIProxyAPI:
		return SourceTypeCLIProxyAPI
	case SourceKindSub2API:
		return SourceTypeSub2API
	default:
		return kind
	}
}

// Match 报告该行是否包含给定指纹。
func (e Entry) Match(fp string) bool { return fp != "" && (fp == e.AtFp || fp == e.RtFp) }

// Fingerprint 计算 (platform, credential) 的查表键。credential 须先经 Normalize。
func Fingerprint(platform, cred string) string {
	sum := sha256.Sum256([]byte(platform + ":" + cred))
	return hex.EncodeToString(sum[:])
}

// NormalizeCred 归一化请求中提取到的凭据，保证同一 key 的不同书写形态得到同一指纹：
// trim 空白与成对引号；剥离 "Bearer "/"Token " 类 scheme 前缀（大小写不敏感，
// 空白含 tab）。
func NormalizeCred(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		v = strings.TrimSpace(v[1 : len(v)-1])
	}
	if i := strings.IndexAny(v, " \t"); i > 0 {
		if strings.EqualFold(v[:i], "bearer") || strings.EqualFold(v[:i], "token") {
			v = strings.TrimSpace(v[i+1:])
		}
	}
	return v
}

// NormalizeAccount 账号标识统一小写（邮箱/uuid 不区分大小写场景）。
func NormalizeAccount(a string) string { return strings.ToLower(strings.TrimSpace(a)) }

// rowKey 与 acct_map 唯一键一一对应。
type rowKey struct{ platform, source, sourceType, account, rt string }

func keyOf(e Entry) rowKey {
	return rowKey{e.Platform, e.Source, e.SourceType, e.Account, e.RtFp}
}

// Registry 是 acct_map 的进程内只读镜像。零值不可用，须经 New 构造。
// 内部按行键存储，另建两张指纹索引（AT/RT 各一）供热路径 O(1) 查询。
type Registry struct {
	mu    sync.RWMutex
	rows  map[rowKey]Entry
	atIdx map[string]rowKey
	rtIdx map[string]rowKey
}

// New 创建空注册表。
func New() *Registry {
	return &Registry{
		rows:  map[rowKey]Entry{},
		atIdx: map[string]rowKey{},
		rtIdx: map[string]rowKey{},
	}
}

// Lookup 按凭据指纹查询；未命中返回 false。
// 同一凭据被多个来源收录时返回任意一行（账号身份一致，热路径等价）。
func (r *Registry) Lookup(fp string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if key, ok := r.atIdx[fp]; ok {
		return r.rows[key], true
	}
	if key, ok := r.rtIdx[fp]; ok {
		return r.rows[key], true
	}
	return Entry{}, false
}

// Len 返回行数（同账号不同来源实例/类型各计一行）。
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.rows)
}

// Snapshot 返回全部行的拷贝（管理面预览用）。
func (r *Registry) Snapshot() []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Entry, 0, len(r.rows))
	for _, e := range r.rows {
		out = append(out, e)
	}
	return out
}

// Reload 全量替换内存镜像（调用方先从 store.LoadAcctMapAll 取最新全集）。
func (r *Registry) Reload(entries []Entry) {
	rows := make(map[rowKey]Entry, len(entries))
	atIdx := make(map[string]rowKey, len(entries))
	rtIdx := make(map[string]rowKey, len(entries))
	for _, e := range entries {
		key := keyOf(e)
		rows[key] = e
		if e.AtFp != "" {
			atIdx[e.AtFp] = key
		}
		if e.RtFp != "" {
			rtIdx[e.RtFp] = key
		}
	}
	r.mu.Lock()
	r.rows, r.atIdx, r.rtIdx = rows, atIdx, rtIdx
	r.mu.Unlock()
}

// ---------- Host → 平台 ----------

// hostPlatforms 按 AI 上游域名归类平台。前缀匹配按段边界（子域支持通配）。
var hostPlatforms = []struct {
	suffix   string
	platform string
}{
	{"chatgpt.com", PlatformOpenAI},
	{"openai.com", PlatformOpenAI},
	{"anthropic.com", PlatformAnthropic},
	{"claude.ai", PlatformAnthropic},
	{"claude.com", PlatformAnthropic},
	{"googleapis.com", PlatformGemini},
	{"ai.google.dev", PlatformGemini},
	{"x.ai", PlatformGrok},
	{"grok.com", PlatformGrok},
	{"api.kimi.com", PlatformKimi},
	{"moonshot.cn", PlatformKimi},
	{"deepseek.com", PlatformDeepSeek},
	{"bigmodel.cn", PlatformGLM},
	{"z.ai", PlatformGLM},
	{"dashscope.aliyuncs.com", PlatformQwen},
	{"apis.iflow.cn", PlatformIFlow},
	{"ollama.com", PlatformOllama},
}

// PlatformForHost 返回目标主机所属平台；未知返回空串（该流量不参与映射表查找）。
func PlatformForHost(host string) string {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	if i := strings.LastIndexByte(h, ':'); i > -1 && !strings.Contains(h[i:], "]") {
		h = h[:i] // 去端口
	}
	for _, hp := range hostPlatforms {
		if h == hp.suffix || strings.HasSuffix(h, "."+hp.suffix) {
			return hp.platform
		}
	}
	return ""
}

// ExtractCred 从请求中提取原始凭据串（已知 AI 平台专用通道）。
// 与通用 marker.Extract 不同：命中已知平台时不做路径白名单限制
// （AI 平台上任何携带凭据的请求都可用于粘滞归属），但仍只读取常规凭据载体：
// Authorization: Bearer / x-api-key / api-key / x-goog-api-key / ?key=。
func ExtractCred(r *http.Request) string {
	auth := func(v string) string {
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = strings.TrimSpace(v[1 : len(v)-1])
		}
		i := strings.IndexAny(v, " \t")
		if i <= 0 || !strings.EqualFold(v[:i], "bearer") {
			return ""
		}
		return NormalizeCred(v)
	}
	get := func(name string) string {
		return NormalizeCred(r.Header.Get(name))
	}
	for _, v := range []string{
		auth(r.Header.Get(httpnames.HeaderAuthorization)),
		get(httpnames.HeaderXAPIKey),
		get(httpnames.HeaderAPIKey),
		get(httpnames.HeaderXGoogAPIKey),
	} {
		if v != "" {
			return v
		}
	}
	if k := r.URL.Query().Get("key"); k != "" {
		return NormalizeCred(k)
	}
	return ""
}
