package identity

import (
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"mitmrouter/internal/acctmap"
	"mitmrouter/internal/acl"
	"mitmrouter/internal/marker"
)

// Resolution 是一次请求的身份解析结果。Credential 仅在内存中用于 SID 派生，
// 调用方不得把它记录到日志、审计或指标。
type Resolution struct {
	Credential string
	Platform   string
	Account    string // acct_map 命中的真实账号；空=未映射
	Mapped     bool
	RuleID     string // 非敏感规则标识，供审计/指标使用
}

func (r Resolution) HasCredential() bool { return r.Credential != "" }

// Options 是 Resolver 的运行依赖。规则与映射注册表都只读、并发安全。
type Options struct {
	MarkerRules    marker.Rules
	AcctMapEnabled bool
	AcctMap        *acctmap.Registry
}

// Resolver 先解析通用载体，未命中才按 URL 查 body parser。
type Resolver struct{}

func New() *Resolver { return &Resolver{} }

// ResolveWithBody 解析身份并返回必须转发的准确 body 流。body 解析绝不替换 req.Body；
// 需要解析时，返回的流会回放解析器已读取的字节。
func (r *Resolver) ResolveWithBody(req *http.Request, targetHost string, opts Options) (Resolution, io.ReadCloser) {
	body := req.Body
	host := acl.NormalizeHost(targetHost)
	platform := acctmap.PlatformForHost(host)

	// ① 通用已知平台 header/query 优先：命中时绝不读取 body。
	if platform != "" {
		if cred := acctmap.ExtractCred(req); cred != "" {
			return resolveCredential(platform, cred, "platform_carrier", opts), body
		}
	}

	// ② 兼容旧 MarkerRules；已知平台上的自定义载体也可查账号映射。
	if cred := marker.Extract(opts.MarkerRules, req); cred != "" {
		return resolveCredential(platform, cred, "marker", opts), body
	}

	// ③ 没有通用凭据才读取命中 URL 的 body。
	if rule := matchBodyRule(host, req); rule != nil {
		raw, replay, ok := snapshotBody(body, req.ContentLength, defaultBodyLimit)
		// 无论解析是否成功，转发都必须消费回放流，因为原始流可能已经被读取。
		if !ok {
			return Resolution{}, replay
		}
		if cred, ok := rule.Parse(raw); ok && cred != "" {
			return resolveCredential(rule.Platform, cred, rule.ID, opts), replay
		}
		return Resolution{}, replay
	}
	return Resolution{}, body
}

func resolveCredential(platform, credential, ruleID string, opts Options) Resolution {
	credential = acctmap.NormalizeCred(credential)
	out := Resolution{Credential: credential, Platform: platform, RuleID: ruleID}
	if platform == "" || !opts.AcctMapEnabled || opts.AcctMap == nil {
		return out
	}
	if e, ok := opts.AcctMap.Lookup(acctmap.Fingerprint(platform, credential)); ok {
		out.Account = e.Account
		out.Mapped = true
	}
	return out
}

// BodyParser 只处理 body 副本，不能消费 http.Request.Body。
type BodyParser func(body []byte) (credential string, ok bool)

type bodyRule struct {
	ID          string
	Host        string // 归一化后的精确 host，防止相似域名误触发 body 读取
	PathKey     string // path 片段；不含 query，最长命中者优先
	Platform    string
	Method      string
	ContentType string
	Parse       BodyParser
}

var bodyRules = []bodyRule{
	{
		ID:          "grok.oauth.refresh",
		Host:        "auth.x.ai",
		PathKey:     "/oauth2/token",
		Platform:    acctmap.PlatformGrok,
		Method:      http.MethodPost,
		ContentType: "application/x-www-form-urlencoded",
		Parse:       parseGrokRefreshToken,
	},
}

func matchBodyRule(host string, req *http.Request) *bodyRule {
	return matchBodyRuleFrom(bodyRules, host, req)
}

// matchBodyRuleFrom 从规则集中找精确 host 下 path 片段最长的规则。提取为独立函数，
// 让匹配边界和最长优先语义可在不修改全局规则表的情况下测试。
func matchBodyRuleFrom(rules []bodyRule, host string, req *http.Request) *bodyRule {
	mediaType := ""
	if contentType := req.Header.Get("Content-Type"); contentType != "" {
		parsed, _, err := mime.ParseMediaType(contentType)
		if err != nil {
			return nil
		}
		mediaType = parsed
	}

	var best *bodyRule
	for i := range rules {
		rule := &rules[i]
		if host != rule.Host || (rule.Method != "" && req.Method != rule.Method) {
			continue
		}
		if rule.ContentType != "" && !strings.EqualFold(mediaType, rule.ContentType) {
			continue
		}
		if !strings.Contains(req.URL.Path, rule.PathKey) {
			continue
		}
		if best == nil || len(rule.PathKey) > len(best.PathKey) {
			best = rule
		}
	}
	return best
}

func parseGrokRefreshToken(body []byte) (string, bool) {
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return "", false
	}
	token := acctmap.NormalizeCred(form.Get("refresh_token"))
	return token, token != ""
}
