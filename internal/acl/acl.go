// Package acl 实现目标黑白名单（访问控制）。
//
// 条目支持四类形态（大小写不敏感，自动去空白与结尾根点）：
//
//	单个 IP     1.2.3.4 / ::1
//	IP 段 CIDR  10.0.0.0/8 / 2001:db8::/32
//	精确域名    api.openai.com / localhost
//	通配符域名  *.openai.com   （匹配任意层级子域，不含 openai.com 本身）
//
// 判定语义：
//
//	① 命中黑名单            → 拒绝
//	② 白名单非空且未命中     → 拒绝
//	③ 其余                   → 放行
//
// 黑名单永远优先；白名单为空表示不限制放行目标。放行后，HTTPS 目标
// 仍由接入层按首字节决定是 MITM 解析还是盲隧道；ACL 本身不改写业务流量。
// 匹配只针对目标的字面主机名/IP，不做 DNS 解析——纯 IP 字面量目标
// 不会命中域名类条目，域名目标也不会命中 IP 类条目。
package acl

import (
	"fmt"
	"net"
	"strings"
)

// MaxListLen 单个名单的条目上限，防止误粘贴超长列表拖慢热更新。
const MaxListLen = 500

// List 一组 ACL 条目的编译结果。只读、并发安全。
type List struct {
	ips      []net.IP            // 单个 IP 条目
	nets     []*net.IPNet        // CIDR 条目
	exact    map[string]struct{} // 精确域名
	suffixes []string            // 通配符域名 → 含前导点后缀 ".openai.com"
	size     int                 // 有效条目数（空名单 Match 恒 false）
}

// Compile 编译名单；非法条目跳过并计数（运行期容错：手改数据库不应让路由瘫痪）。
func Compile(entries []string) (*List, int) {
	l := &List{exact: map[string]struct{}{}}
	skipped := 0
	for _, e := range entries {
		k, v, err := classifyEntry(e)
		if err != nil {
			skipped++
			continue
		}
		l.size++
		switch k {
		case kindIP:
			l.ips = append(l.ips, net.ParseIP(v))
		case kindNet:
			if _, n, cerr := net.ParseCIDR(v); cerr == nil {
				l.nets = append(l.nets, n)
			}
		case kindWild:
			l.suffixes = append(l.suffixes, v[1:]) // "*." → "."
		default:
			l.exact[v] = struct{}{}
		}
	}
	return l, skipped
}

// Match 判定目标（可含端口）是否命中本名单。nil 或空名单恒 false。
func (l *List) Match(target string) bool {
	if l == nil || l.size == 0 {
		return false
	}
	h := NormalizeHost(target)
	if h == "" {
		return false
	}
	if ip := net.ParseIP(h); ip != nil {
		for _, e := range l.ips {
			if e.Equal(ip) {
				return true
			}
		}
		for _, n := range l.nets {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}
	if _, ok := l.exact[h]; ok {
		return true
	}
	for _, s := range l.suffixes {
		if strings.HasSuffix(h, s) {
			return true
		}
	}
	return false
}

// Len 返回有效条目数。
func (l *List) Len() int {
	if l == nil {
		return 0
	}
	return l.size
}

// Rules 黑白名单组合规则。零值即「全放行」。
type Rules struct {
	white, black    *List
	whiteConfigured bool
}

// NewRules 编译黑白名单（跳过非法条目，返回跳过数量供日志提示）。
// 白名单是否原始配置过会单独保留，避免全是非法条目时退化为全放行。
func NewRules(whitelist, blacklist []string) (*Rules, int) {
	w, sw := Compile(whitelist)
	b, sb := Compile(blacklist)
	return &Rules{
		white:           w,
		black:           b,
		whiteConfigured: len(whitelist) > 0,
	}, sw + sb
}

// Allowed 判定目标是否允许进入转发路径。
// 黑名单优先；白名单非空时，只有命中白名单的目标允许通过。
func (r *Rules) Allowed(target string) bool {
	if r == nil {
		return true
	}
	if r.black.Match(target) {
		return false
	}
	if !r.whiteConfigured {
		return true
	}
	// A configured whitelist with no valid entries must fail closed. Compile
	// skips malformed entries for runtime tolerance, but that must not turn a
	// malformed non-empty allowlist into an unrestricted one.
	return r.white.Match(target)
}

// Intercept 判定已允许目标是否应被拦截解析（MITM）。
// 当前 ACL 对所有允许的 HTTPS 目标都进入解析路径；被拒绝目标也返回
// false 以保持旧调用方的安全默认值，但调用方必须先检查 Allowed。
func (r *Rules) Intercept(target string) bool {
	return r.Allowed(target)
}

// ---------- 条目校验与归一化 ----------

type entryKind int

const (
	kindExact entryKind = iota
	kindWild
	kindIP
	kindNet
)

// NormalizeEntry 校验并归一化单条条目，返回规范形式；非法条目返回带说明的错误
// （管理台保存路径使用）。
func NormalizeEntry(raw string) (string, error) {
	_, v, err := classifyEntry(raw)
	return v, err
}

// classifyEntry 归一化并判定条目形态。
func classifyEntry(raw string) (entryKind, string, error) {
	e := strings.ToLower(strings.TrimSpace(raw))
	e = strings.TrimSuffix(e, ".")
	switch {
	case e == "":
		return 0, "", fmt.Errorf("empty entry")
	}
	if ip := net.ParseIP(e); ip != nil { // 含 IPv6 冒号形态，须先于字符黑名单判定
		return kindIP, e, nil
	}
	if strings.Contains(e, "/") { // CIDR：'/' 同样先于字符黑名单判定
		if _, _, cerr := net.ParseCIDR(e); cerr != nil {
			return 0, "", fmt.Errorf("invalid CIDR %q", raw)
		}
		return kindNet, e, nil
	}
	if strings.ContainsAny(e, "/@:? \t") { // 域名形态不允许端口/协议/路径等残留
		return 0, "", fmt.Errorf("contains illegal characters %q", raw)
	}
	// 域名形态：可选前缀 "*."，其余按域名标签校验
	host := strings.TrimPrefix(e, "*.")
	if host == "" {
		return 0, "", fmt.Errorf("invalid wildcard %q", raw)
	}
	total := 0
	for _, label := range strings.Split(host, ".") {
		if !validLabel(label) {
			return 0, "", fmt.Errorf("domain label %q invalid", label)
		}
		total += len(label) + 1
	}
	if total-1 > 253 {
		return 0, "", fmt.Errorf("domain too long")
	}
	if strings.HasPrefix(e, "*.") {
		return kindWild, e, nil
	}
	return kindExact, e, nil
}

// validLabel 校验单个域名标签：字母/数字/下划线/连字符，1–63 字符，
// 不以 '-' 开头或结尾（下划线放宽允许，兼容内部服务名）。
func validLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// ValidateLists 管理台保存前的严格校验：逐条校验两个名单并限制长度。
func ValidateLists(whitelist, blacklist []string) error {
	lists := []struct {
		name  string
		items []string
	}{{"whitelist", whitelist}, {"blacklist", blacklist}}
	for _, l := range lists {
		if len(l.items) > MaxListLen {
			return fmt.Errorf("%s exceeds limit of %d entries", l.name, MaxListLen)
		}
		for _, e := range l.items {
			if _, err := NormalizeEntry(e); err != nil {
				return fmt.Errorf("%s has invalid entry: %v", l.name, err)
			}
		}
	}
	return nil
}

// NormalizeHost 归一化目标地址：去端口、去 IPv6 括号、转小写、去结尾根点。
// 接受 "example.com"、"example.com:443"、"[::1]:443"、"::1" 等形态。
func NormalizeHost(hostport string) string {
	h := strings.TrimSpace(hostport)
	if h == "" {
		return ""
	}
	if hp, _, err := net.SplitHostPort(h); err == nil {
		h = hp
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	h = strings.ToLower(h)
	return strings.TrimSuffix(h, ".")
}
