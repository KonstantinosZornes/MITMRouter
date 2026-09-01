package upstream

import (
	"errors"
	"net/url"
	"strconv"
)

func init() {
	Register(Platform1024Proxy, func(*Upstream) SessionInjector { return c1024{} })
}

// c1024 用户名语法（官方）：
//
//	<apikey>-region-{cc}-sid-{sessid}-t-{分钟}
//
// 扁平键值对以 '-' 连接；已知键 region/st/city/asn/sid/t。
// 粘性时长 1–120 分钟（端口套餐 3–30）；base_url 务必自带 -t-N。
var c1024Keys = map[string]bool{
	"region": true, "st": true, "city": true,
	"asn": true, "sid": true, "t": true,
}

type c1024 struct{}

func (c1024) Inject(base *url.URL, p InjectParams) (*url.URL, error) {
	if p.Account == "" {
		return nil, errors.New("account is empty")
	}
	return rewriteUser(base, func(u string) (string, error) {
		out, err := flatRewrite(u, c1024Keys, "sid", p.Account)
		if err != nil {
			return "", err
		}
		if p.TTLMin > 0 { // 显式设置优先于 base_url 原值
			out, err = flatRewrite(out, c1024Keys, "t", strconv.Itoa(clampInt(p.TTLMin, 1, 120)))
			if err != nil {
				return "", err
			}
		}
		return out, nil
	}, nil)
}
