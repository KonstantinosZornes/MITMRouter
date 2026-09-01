// plain 平台：普通代理（非粘滞）。设计：docs/011-plain-binding-design.md。
// plain 不做任何会话注入——凭据按 base_url 原样使用，恒等注入器保证选中它时
// 发给出口代理的 CONNECT 凭据与用户录入完全一致。
package upstream

import (
	"errors"
	"net/url"
	"strings"
)

func init() {
	Register(PlatformPlain, func(*Upstream) SessionInjector { return plainInject{} })
}

// plainInject 是恒等注入器：原样返回 base_url（浅拷贝避免调用方持有同一指针）。
type plainInject struct{}

func (plainInject) Inject(base *url.URL, _ InjectParams) (*url.URL, error) {
	u := *base
	return &u, nil
}

// ValidateForSave 的 plain 分支：不携带 inject（无模板语义），其余
// scheme/host 校验与各平台共用逻辑一致。
func validatePlain(injectJSON string) error {
	if strings.TrimSpace(injectJSON) != "" {
		return errors.New("plain platform does not accept inject template")
	}
	return nil
}
