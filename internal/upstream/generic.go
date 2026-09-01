package upstream

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

func init() {
	Register(PlatformGeneric, func(up *Upstream) SessionInjector {
		return generic{tpl: up.UsernameTemplate, staticPass: up.StaticPassword}
	})
}

// generic 是任意“会话参数拼在用户名里”平台的通用模板引擎：
// 替换 {user}/{sid}/{ttl_min}/{country}。保存时和运行时均验证模板，
// 防止手改数据库使未展开的占位符被发送给上游。
type generic struct {
	tpl        string
	staticPass string
}

func (g generic) Inject(base *url.URL, p InjectParams) (*url.URL, error) {
	if g.tpl == "" {
		return nil, fmt.Errorf("generic entry missing username_template")
	}
	if err := validateGenericTemplate(g.tpl, p.TTLMin); err != nil {
		return nil, err
	}
	oldUser := ""
	if base.User != nil {
		oldUser = base.User.Username()
	}
	r := strings.NewReplacer(
		"{user}", oldUser,
		"{sid}", p.Account,
		"{ttl_min}", strconv.Itoa(p.TTLMin),
		"{country}", p.Country,
	)
	newUser := r.Replace(g.tpl)
	var passOverride *string
	if g.staticPass != "" {
		passOverride = &g.staticPass
	}
	return rewriteUser(base, func(string) (string, error) { return newUser, nil }, passOverride)
}
