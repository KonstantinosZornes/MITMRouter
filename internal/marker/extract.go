// Package marker 定义并维护调用方唯一标识（Marker）：
// 从 HTTP 请求中按规则提取（不限于 API Key 的任何唯一标识形态），
// 并为每个 Marker 维护可在上游不可用时轮换的动态整数盐值，
// 供 sticky 层推导稳定的粘滞身份。
package marker

import (
	"net/http"
	"strings"

	"mitmrouter/internal/httpnames"
)

// Rules 对应设置项 marker_path_parts / marker_headers。
type Rules struct {
	// PathParts 路径包含片段（须以 '/' 开头）。空 = 不做路径过滤，
	// 对所有 URL 同一规则——同一凭据无论访问哪个路径都得到同一身份（推荐默认）。
	PathParts []string `json:"path_parts"`
	Headers   []string `json:"headers"`
}

// Extract：存在配置头时返回 Marker，否则返回空串。
// PathParts 非空时要求 URL 路径包含任一片段（朴素子串包含，可位于任意位置）；
// 空 = 不做路径过滤，对所有 URL 同一规则。
// Authorization 头仅识别 "Bearer <key>" 形态；其余头取原值。
func Extract(rules Rules, r *http.Request) string {
	if len(rules.PathParts) > 0 {
		matched := false
		for _, p := range rules.PathParts {
			if strings.Contains(r.URL.Path, p) {
				matched = true
				break
			}
		}
		if !matched {
			return ""
		}
	}
	for _, h := range rules.Headers {
		v := strings.TrimSpace(r.Header.Get(h))
		if v == "" {
			continue
		}
		if strings.EqualFold(h, httpnames.HeaderAuthorization) {
			i := strings.IndexByte(v, ' ')
			if i <= 0 || !strings.EqualFold(v[:i], "Bearer") {
				continue // 仅识别 Bearer 形态
			}
			v = strings.TrimSpace(v[i+1:])
		}
		if v != "" {
			return v
		}
	}
	return ""
}
