package upstream

import (
	"errors"
	"net/url"
	"strings"
)

// rewriteUser 复制 base 并把 userinfo 的用户名替换为 mutate 的返回值；
// 密码默认沿用原值，overridePass 非 nil 时覆盖（generic 的静态密码场景）。
// scheme/host/port 一律不动。
func rewriteUser(base *url.URL, mutate func(username string) (string, error), overridePass *string) (*url.URL, error) {
	oldUser := ""
	var oldPass string
	hasPass := false
	if base.User != nil {
		oldUser = base.User.Username()
		oldPass, hasPass = base.User.Password()
	}
	newUser, err := mutate(oldUser)
	if err != nil {
		return nil, err
	}
	u := *base // 浅拷贝即可：我们仅替换 User 指针
	pass := oldPass
	if overridePass != nil {
		pass, hasPass = *overridePass, true
	}
	if hasPass {
		u.User = url.UserPassword(newUser, pass)
	} else {
		u.User = url.User(newUser)
	}
	return &u, nil
}

// ---------- 共享：扁平键值对扫描 ----------
//
// 语法：<前缀(apikey/login)>[-<key>-<value>]...，整串以 '-' 连接。
// knownKeys 之外的 token 视为普通 token 原样保留；首个已知键之前的
// 连续普通 token 即身份前缀。目标键存在则原位替换其值（重复键丢弃），
// 不存在则插入到第一对已知键之前（即紧跟前缀），整串无任何已知键时追加尾部。

func flatRewrite(username string, knownKeys map[string]bool, key, newVal string) (string, error) {
	if username == "" {
		return "", errors.New("username is empty")
	}
	toks := strings.Split(username, "-")

	// 预扫描：目标键是否已存在于任意位置。
	// 若不存在，“插入到第一对之前”才成立；否则必须原位替换，
	// 否则会因插入点早于既有键而产生重复参数。
	exists := false
	for i := 0; i < len(toks); i++ {
		if i+1 < len(toks) && knownKeys[toks[i]] {
			if toks[i] == key {
				exists = true
			}
			i++ // 值 token 不参与键判定
		}
	}

	out := make([]string, 0, len(toks)+2)
	inserted := false
	found := false

	for i := 0; i < len(toks); i++ {
		isPairStart := i+1 < len(toks) && knownKeys[toks[i]]
		switch {
		case isPairStart && toks[i] == key:
			if !found {
				out = append(out, toks[i], newVal)
				found = true
			}
			i++ // 吞掉原值；重复的目标键整段丢弃
		case isPairStart:
			if !exists && !inserted {
				out = append(out, key, newVal) // 插在第一对之前 = 紧跟前缀
				inserted = true
			}
			out = append(out, toks[i], toks[i+1])
			i++
		default:
			out = append(out, toks[i])
		}
	}
	if !found && !inserted {
		out = append(out, key, newVal)
	}
	return strings.Join(out, "-"), nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
