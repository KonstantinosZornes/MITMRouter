// Package upstream 定义上游出口条目、粘滞身份注入器接口与运行时表快照。
package upstream

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Upstream 是一条上游出口条目（内存形态）。
type Upstream struct {
	ID       int64
	Name     string
	Platform string
	BaseURL  *url.URL // 解析后的原始上游地址（含凭据）
	Enabled  bool     // false 时不进入路由候选

	// generic 平台专用，其他平台为空
	UsernameTemplate string
	StaticPassword   string
}

// SessionInjector 把会话身份写入上游出口凭据。
// 实现必须是纯函数式（无共享状态），并发安全由“无状态”保证。
// InjectParams 是注入器所需的运行参数。
type InjectParams struct {
	Account string // 粘滞身份串（16位小写hex 或 兜底身份）
	TTLMin  int    // 设置项 session_ttl_min 原值；0=不干预平台 TTL 参数
	Country string // 预留字段，v1 恒为空
}

type SessionInjector interface {
	// base: 原始 base_url；p: 注入参数。
	Inject(base *url.URL, p InjectParams) (*url.URL, error)
}

// registry 平台名 → 注入器工厂。工厂接收条目实例（generic 需读取其模板字段）。
var registry = map[string]func(*Upstream) SessionInjector{}

// Register 注册平台注入器（各实现的 init 中调用）。
func Register(platform string, f func(*Upstream) SessionInjector) { registry[platform] = f }

// InjectorFor 取绑定到具体条目的平台注入器。
func InjectorFor(platform string, up *Upstream) (SessionInjector, bool) {
	f, ok := registry[platform]
	if !ok || up == nil {
		return nil, false
	}
	return f(up), true
}

// KnownPlatforms 返回全部已注册平台名（排序），供 UI 下拉使用。
func KnownPlatforms() []string {
	out := make([]string, 0, len(registry)+1)
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------- Table：启用条目的不可变快照 ----------

// Table 是上游条目的只读快照（热更新整体替换）。
type Table struct {
	items []*Upstream
	byID  map[int64]*Upstream
	def   string // default_upstream 名称
}

// NewTable 构建快照。
func NewTable(items []*Upstream, def string) *Table {
	t := &Table{items: items, byID: make(map[int64]*Upstream, len(items)), def: def}
	for _, u := range items {
		t.byID[u.ID] = u
	}
	return t
}

// EmptyTable 返回空快照。
func EmptyTable() *Table { return &Table{byID: map[int64]*Upstream{}} }

// Select 按名称取启用的条目；不存在或停用返回 nil。
func (t *Table) Select(name string) *Upstream {
	for _, u := range t.items {
		if u.Name == name {
			if !u.Enabled {
				return nil
			}
			return u
		}
	}
	return nil
}

// Items 返回全部条目。
func (t *Table) Items() []*Upstream { return t.items }

// DefaultName 返回默认条目名。
func (t *Table) DefaultName() string { return t.def }

// ---------- 通用注入结构（generic 平台） ----------

type genericInject struct {
	UsernameTemplate string `json:"username_template"`
	Password         string `json:"password,omitempty"`
}

// FromRow 把数据库行转成内存条目并做基础校验。
func FromRow(id int64, name, platform, baseURL string, injectJSON sql.NullString, enabled bool) (*Upstream, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("base_url parse failed: %w", err)
	}
	up := &Upstream{ID: id, Name: name, Platform: platform, BaseURL: u, Enabled: enabled}
	if platform == PlatformGeneric && injectJSON.Valid && strings.TrimSpace(injectJSON.String) != "" {
		var gi genericInject
		if err := json.Unmarshal([]byte(injectJSON.String), &gi); err != nil {
			return nil, fmt.Errorf("inject JSON parse failed: %w", err)
		}
		up.UsernameTemplate = gi.UsernameTemplate
		up.StaticPassword = gi.Password
	}
	return up, nil
}

// ---------- 保存校验（管理台写入路径复用） ----------

var allowedSchemes = map[string]bool{"http": true, "https": true, "socks5": true, "socks5h": true}

var knownPlaceholders = map[string]bool{"user": true, "sid": true, "ttl_min": true, "country": true}

// validateGenericTemplate 校验每个花括号包围的占位符。严格扫描还会拒绝不匹配的花括号，
// 防止手动编辑的记录将未经展开的字面会话占位符发送到上游。
func validateGenericTemplate(template string, sessionTTLMin int) error {
	for rest := template; rest != ""; {
		open := strings.IndexByte(rest, '{')
		close := strings.IndexByte(rest, '}')
		if close >= 0 && (open < 0 || close < open) {
			return errors.New("template contains unmatched closing brace")
		}
		if open < 0 {
			return nil
		}
		rest = rest[open+1:]
		close = strings.IndexByte(rest, '}')
		if close < 0 {
			return errors.New("template contains unclosed placeholder")
		}
		name := rest[:close]
		if !knownPlaceholders[name] {
			return fmt.Errorf("template contains undefined placeholder {%s}", name)
		}
		if name == "ttl_min" && sessionTTLMin <= 0 {
			return errors.New("template contains {ttl_min} but setting session_ttl_min is disabled (currently 0)")
		}
		rest = rest[close+1:]
	}
	return nil
}

// ValidateForSave 校验待保存的上游条目。
// sessionTTLMin 为当前设置值：>0 时模板才允许出现 {ttl_min}。
func ValidateForSave(platform, rawURL, injectJSON string, sessionTTLMin int) error {
	if _, ok := registry[platform]; !ok && platform != PlatformGeneric {
		return fmt.Errorf("unknown platform %q (options: %v / generic)", platform, KnownPlatforms())
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("base_url unparseable: %w", err)
	}
	if u.Host == "" {
		return errors.New("base_url missing host part")
	}
	if !allowedSchemes[u.Scheme] {
		return fmt.Errorf("unsupported scheme %q (allowed http/https/socks5/socks5h)", u.Scheme)
	}
	if (u.Scheme == "socks5" || u.Scheme == "socks5h") && u.Port() == "" {
		return errors.New("socks5/socks5h base_url must include an explicit port")
	}
	username := ""
	if u.User != nil {
		username = u.User.Username()
	}
	switch platform {
	case PlatformPlain:
		if err := validatePlain(injectJSON); err != nil {
			return err
		}
	case PlatformDecodo:
		if !strings.HasPrefix(username, "user-") {
			return errors.New("decodo username must start with user- prefix (official syntax)")
		}
	case PlatformResin:
		if username == "" {
			return errors.New("resin username must not be empty (format Platform[.Account], password is RESIN_TOKEN)")
		}
	case Platform1024Proxy:
		if username == "" {
			return errors.New("1024proxy username must not be empty (format APIKEY[-region-CC][-sid-ID][-t-MIN])")
		}
	case PlatformGeneric:
		var gi genericInject
		if strings.TrimSpace(injectJSON) == "" {
			return errors.New("generic platform requires inject.username_template")
		}
		if err := json.Unmarshal([]byte(injectJSON), &gi); err != nil {
			return fmt.Errorf("invalid inject JSON: %w", err)
		}
		if gi.UsernameTemplate == "" {
			return errors.New("inject.username_template must not be empty")
		}
		if err := validateGenericTemplate(gi.UsernameTemplate, sessionTTLMin); err != nil {
			return err
		}
	}
	return nil
}
