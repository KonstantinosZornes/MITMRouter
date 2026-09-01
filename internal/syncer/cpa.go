package syncer

// CLIProxyAPI 拉取客户端：列表 + 逐文件下载。
// 列表项字段（2026-08 实测）：name/id、email/account、provider、disabled。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"mitmrouter/internal/acctmap"
	"mitmrouter/internal/httpnames"
)

type cpaFileMeta struct {
	Name     string `json:"name"`
	ID       string `json:"id"`
	Email    string `json:"email"`
	Account  string `json:"account"`
	Label    string `json:"label"`
	Provider string `json:"provider"`
	Disabled bool   `json:"disabled"`
}

type cpaListResp struct {
	Files *[]cpaFileMeta `json:"files"`
}

type cpaAuthFile struct {
	Type         string `json:"type"`
	Provider     string `json:"provider"`
	Email        string `json:"email"`
	EmailAddress string `json:"email_address"` // claude 系
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	// 部分形态把 token 包一层，如 {"tokens":{...}}；此处兼容常见扁平结构即可。
	Tokens json.RawMessage `json:"tokens"`
}

var errUnsupportedCPAAuth = errors.New("unsupported CPA auth format")

// parseCPAAuthFile parses the small, verified subset of CPA auth JSON used by
// direct mode. It returns nil for a supported file without usable credentials;
// callers must not treat that as a delete during a file event.
func parseCPAAuthFile(raw []byte) (*Entry, error) {
	var af cpaAuthFile
	if err := json.Unmarshal(raw, &af); err != nil {
		return nil, fmt.Errorf("parse CPA auth JSON: %w", err)
	}
	if len(af.Tokens) > 0 && (strings.TrimSpace(af.AccessToken) == "" || strings.TrimSpace(af.RefreshToken) == "") {
		var nested struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.Unmarshal(af.Tokens, &nested); err != nil {
			return nil, fmt.Errorf("parse CPA nested tokens: %w", err)
		}
		if strings.TrimSpace(af.AccessToken) == "" {
			af.AccessToken = nested.AccessToken
		}
		if strings.TrimSpace(af.RefreshToken) == "" {
			af.RefreshToken = nested.RefreshToken
		}
	}
	provider := strings.ToLower(strings.TrimSpace(firstNonEmpty(af.Type, af.Provider)))
	if provider == "" {
		return nil, fmt.Errorf("%w: no type/provider", errUnsupportedCPAAuth)
	}
	platform, ok := cpaPlatformMap[provider]
	if !ok {
		return nil, fmt.Errorf("%w: provider %q", errUnsupportedCPAAuth, provider)
	}
	account := acctmap.NormalizeAccount(firstNonEmpty(af.Email, af.EmailAddress))
	at := strings.TrimSpace(af.AccessToken)
	rt := strings.TrimSpace(af.RefreshToken)
	if account == "" || (at == "" && rt == "") {
		return nil, nil
	}
	return &Entry{Platform: platform, Account: account, AtToken: at, RtToken: rt}, nil
}

func (m *Manager) fetchCPA(ctx context.Context, baseURL, key string) ([]Entry, error) {
	base := strings.TrimRight(baseURL, "/")
	var list cpaListResp
	if err := m.getJSON(ctx, base+cpaAuthFilesPath,
		map[string]string{httpnames.HeaderAuthorization: bearerAuthPrefix + key}, &list); err != nil {
		return nil, fmt.Errorf("cpa list: %w", err)
	}
	if list.Files == nil {
		return nil, errors.New("cpa list: response missing files")
	}
	files := *list.Files

	type job struct {
		meta     cpaFileMeta
		platform string
	}
	jobs := make([]job, 0, len(files))
	for _, f := range files {
		pf, ok := cpaPlatformMap[strings.ToLower(strings.TrimSpace(f.Provider))]
		if !ok {
			continue // 白名单外平台跳过
		}
		// 注意：disabled 的文件同样同步——凭据指纹仍可归属账号；
		// 停用与否是上游路由决策，不影响本表的粘滞归属语义。
		jobs = append(jobs, job{f, pf})
	}

	out := make([]Entry, 0, len(jobs))
	var mu sync.Mutex
	var skipped int
	sem := make(chan struct{}, downloadWorkers)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			e, err := m.fetchCPAFile(ctx, base, key, j.meta, j.platform)
			if err != nil {
				m.log.Warn("syncer: cpa file fetch failed", "file", j.meta.Name, "err", err)
				mu.Lock()
				skipped++
				mu.Unlock()
				return
			}
			if e == nil {
				return
			}
			mu.Lock()
			out = append(out, *e)
			mu.Unlock()
		}(j)
	}
	wg.Wait()
	if skipped > 0 {
		m.log.Warn("syncer: cpa files skipped; preserving existing snapshot", "count", skipped)
		return nil, fmt.Errorf("cpa: %d auth file(s) failed to download or parse", skipped)
	}
	return out, nil
}

func (m *Manager) fetchCPAFile(ctx context.Context, base, key string, meta cpaFileMeta, platform string) (*Entry, error) {
	name := meta.Name
	if name == "" {
		name = meta.ID
	}
	if name == "" {
		return nil, nil
	}
	var raw json.RawMessage
	url := base + cpaAuthFileDownloadPath + "?name=" + url.QueryEscape(name)
	if err := m.getJSON(ctx, url, map[string]string{httpnames.HeaderAuthorization: bearerAuthPrefix + key}, &raw); err != nil {
		return nil, err
	}
	var af cpaAuthFile
	if err := json.Unmarshal(raw, &af); err != nil {
		// 认证文件结构因 provider 而异：解析不出已知字段时按无凭据处理
		return nil, fmt.Errorf("parse auth file: %w", err)
	}
	account := strings.ToLower(firstNonEmpty(af.Email, af.EmailAddress, meta.Email, meta.Account, meta.Label))
	at := strings.TrimSpace(af.AccessToken)
	rt := strings.TrimSpace(af.RefreshToken)
	if len(af.Tokens) > 0 && (at == "" || rt == "") {
		var nested struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.Unmarshal(af.Tokens, &nested); err != nil {
			return nil, fmt.Errorf("parse nested tokens: %w", err)
		}
		if at == "" {
			at = strings.TrimSpace(nested.AccessToken)
		}
		if rt == "" {
			rt = strings.TrimSpace(nested.RefreshToken)
		}
	}
	if account == "" || (at == "" && rt == "") {
		return nil, nil // 无法归属或无凭据的文件不入表
	}
	return &Entry{Platform: platform, Account: account, AtToken: at, RtToken: rt}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func nonEmpty(vals ...string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
