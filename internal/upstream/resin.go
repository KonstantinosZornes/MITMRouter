package upstream

import (
	"errors"
	"net/url"
	"strings"
)

func init() {
	Register(PlatformResin, func(*Upstream) SessionInjector { return resin{} })
}

// resin 上游出口凭据语法（官方）：
//
//	用户名 = Platform[.Account]，密码 = RESIN_TOKEN
//
// Account 留空即非粘滞；本注入器把第一个 '.' 之前视为 Platform、
// 之后整体替换为我们的 Marker 指纹，从而复用 Resin 的租约粘滞机制。
// （Resin 不对 Account 做任何哈希/加工，原样作为租约表主键。）
type resin struct{}

func (resin) Inject(base *url.URL, p InjectParams) (*url.URL, error) {
	if p.Account == "" {
		return nil, errors.New("account is empty")
	}
	return rewriteUser(base, func(u string) (string, error) {
		if u == "" {
			return "", errors.New("resin username must not be empty (expected Platform or Platform.Account)")
		}
		platform := u
		if i := strings.Index(u, "."); i >= 0 {
			platform = u[:i] // 第一个 '.' 前是 Platform，旧 Account 一律丢弃
		}
		return platform + "." + p.Account, nil
	}, nil)
}
