package syncer

// sub2api 拉取客户端：管理员备份导出接口（唯一返回凭据原文的只读端点）。
// GET /api/v1/admin/accounts/data，认证头 x-api-key: <admin-api-key>。
// 注意：step-up 2FA 开启时该端点拒绝机器凭证——属部署侧配置取舍。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"mitmrouter/internal/httpnames"
)

type sub2apiDataResp struct {
	Code *int `json:"code"`
	Data *struct {
		Accounts *[]sub2apiAccount `json:"accounts"`
	} `json:"data"`
}

type sub2apiAccount struct {
	Name        string         `json:"name"`
	Platform    string         `json:"platform"`
	Type        string         `json:"type"`
	Credentials map[string]any `json:"credentials"`
}

func (m *Manager) fetchSub2API(ctx context.Context, baseURL, key string) ([]Entry, error) {
	base := strings.TrimRight(baseURL, "/")
	// credentials 里存在非字符串字段（数字/嵌套），宽松解析：按 map[string]any 收。
	raw := &sub2apiDataResp{}
	if err := m.getJSON(ctx, base+sub2APIAccountsDataPath,
		map[string]string{httpnames.HeaderXAPIKey: key}, raw); err != nil {
		return nil, fmt.Errorf("sub2api export: %w", err)
	}
	if raw.Data == nil || raw.Data.Accounts == nil {
		return nil, errors.New("sub2api export: response missing data.accounts")
	}
	if raw.Code != nil && *raw.Code != 0 && *raw.Code != 200 {
		return nil, fmt.Errorf("sub2api export: code=%d", *raw.Code)
	}

	out := make([]Entry, 0, len(*raw.Data.Accounts))
	for _, a := range *raw.Data.Accounts {
		t := strings.ToLower(strings.TrimSpace(a.Type))
		if t != accountTypeOAuth && t != accountTypeSetupToken {
			continue // apikey/upstream/bedrock 等不入映射（无 RT/AT）
		}
		pf, ok := sub2apiPlatformMap[strings.ToLower(strings.TrimSpace(a.Platform))]
		if !ok {
			continue
		}
		creds := a.Credentials
		account := strings.ToLower(strings.TrimSpace(
			strVal(creds["email"], strVal(a.Name, ""))))
		at := strVal(creds["access_token"], "")
		rt := strVal(creds["refresh_token"], "")
		if account == "" || (at == "" && rt == "") {
			continue
		}
		out = append(out, Entry{Platform: pf, Account: account, AtToken: at, RtToken: rt})
	}
	return out, nil
}

func strVal(v any, fb string) string {
	if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	return fb
}
