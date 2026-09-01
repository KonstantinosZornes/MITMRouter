package upstream

import (
	"errors"
	"net/url"
	"strings"
)

func init() {
	Register(PlatformDataImpulse, func(*Upstream) SessionInjector { return dataimpulse{} })
}

// dataimpulse 用户名语法（官方）：
//
//	<login>__<key>.<value>[;<key>.<value>...]
//
// 首参紧跟 `__` 进入参数区；参数间 `;`；键值间 `.`。会话键为 sessid，
// 仅用 sessid 时平均保持约 30 分钟且不可自定义 TTL（sessttl 属另一套机制，
// 仅适用于 Sticky 端口段，故此处忽略 TTLMin）。
type dataimpulse struct{}

func (dataimpulse) Inject(base *url.URL, p InjectParams) (*url.URL, error) {
	if p.Account == "" {
		return nil, errors.New("account is empty")
	}
	return rewriteUser(base, func(u string) (string, error) {
		login, params := u, ""
		if i := strings.Index(u, "__"); i >= 0 {
			login, params = u[:i], u[i+2:]
		}
		kept := make([]string, 0, 4)
		for _, seg := range strings.Split(params, ";") {
			seg = strings.TrimSpace(seg)
			if seg == "" || strings.HasPrefix(seg, "sessid.") {
				continue // 丢弃既有会话参数，实现“追加/替换”
			}
			kept = append(kept, seg)
		}
		kept = append(kept, "sessid."+p.Account)
		return login + "__" + strings.Join(kept, ";"), nil
	}, nil)
}
