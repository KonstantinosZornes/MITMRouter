package upstream

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
)

func init() {
	Register(PlatformDecodo, func(*Upstream) SessionInjector { return decodo{} })
}

// decodo（原 Smartproxy）用户名语法（官方）：
//
//	user-<login>[-country-XX][-city-..]-session-{sessid}[-sessionduration-{ttl}]
//
// 强制 user- 前缀 + 扁平键值对以 '-' 连接。粘滞必须带 session 参数；
// sessionduration 支持 1–1440 分钟；60 秒空闲即过期。
var decodoKeys = map[string]bool{
	"country": true, "city": true, "st": true, "state": true, "asn": true,
	"session": true, "sessionduration": true, "session_iplock": true,
}

type decodo struct{}

func (decodo) Inject(base *url.URL, p InjectParams) (*url.URL, error) {
	if p.Account == "" {
		return nil, errors.New("account is empty")
	}
	return rewriteUser(base, func(u string) (string, error) {
		if !strings.HasPrefix(u, "user-") {
			return "", errors.New("decodo username must start with user- prefix")
		}
		out, err := flatRewrite(u, decodoKeys, "session", p.Account)
		if err != nil {
			return "", err
		}
		if p.TTLMin > 0 {
			out, err = flatRewrite(out, decodoKeys, "sessionduration", strconv.Itoa(clampInt(p.TTLMin, 1, 1440)))
			if err != nil {
				return "", err
			}
		}
		return out, nil
	}, nil)
}
