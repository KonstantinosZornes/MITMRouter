// Package acctegress 维护「账户 ↔ 出站」绑定的进程内只读快照。
// 设计：docs/011-plain-binding-design.md。
//
// 线程模型与 upstream.Table / acctmap.Registry 一致：热路径零锁只读，
// 写入走整体换表（管理台写绑定、acct_map 级联 GC 之后由调用方重建）。
package acctegress

import (
	"context"
	"sort"

	"mitmrouter/internal/store"
)

// 绑定模式（与 store 层常量同值，本包聚焦内存形态）。
const (
	ModeSticky = "sticky"
	ModeRandom = "random"
)

// Key 定位一个账户的绑定。与 acct_map 的账号键一致（platform 归一小写、
// account 统一小写），不含来源维度——同账号多来源凭据共享同一份绑定。
type Key struct {
	Platform string
	Account  string
}

// Binding 是一个账户的完整绑定：出站 ID 集合与路由模式。
type Binding struct {
	Key
	EgressIDs []int64 `json:"egress_ids"`
	Mode      string  `json:"mode"` // sticky | random
}

// Table 是绑定全集的不可变快照。
type Table struct {
	byAccount map[Key]Binding
}

// NewTable 从绑定行构建快照；同账户的 mode 取最后出现的行（数据库内同账户
// 全行 mode 本应一致，此处仅做防御性收敛）。
func NewTable(rows []store.AcctEgressRow) *Table {
	t := &Table{byAccount: make(map[Key]Binding)}
	for _, r := range rows {
		k := Key{Platform: r.Platform, Account: r.Account}
		b, ok := t.byAccount[k]
		if !ok {
			b = Binding{Key: k, Mode: r.Mode}
		}
		b.EgressIDs = append(b.EgressIDs, r.EgressID)
		if r.Mode != "" {
			b.Mode = r.Mode
		}
		t.byAccount[k] = b
	}
	for k, b := range t.byAccount {
		sort.Slice(b.EgressIDs, func(i, j int) bool { return b.EgressIDs[i] < b.EgressIDs[j] })
		t.byAccount[k] = b
	}
	return t
}

// EmptyTable 返回空快照。
func EmptyTable() *Table { return &Table{byAccount: map[Key]Binding{}} }

// Lookup 按账户取绑定；未绑定返回 false。
func (t *Table) Lookup(platform, account string) (Binding, bool) {
	b, ok := t.byAccount[Key{Platform: platform, Account: account}]
	return b, ok
}

// Items 返回全部绑定（管理面展示用）。
func (t *Table) Items() []Binding {
	out := make([]Binding, 0, len(t.byAccount))
	for _, b := range t.byAccount {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Platform != out[j].Platform {
			return out[i].Platform < out[j].Platform
		}
		return out[i].Account < out[j].Account
	})
	return out
}

// LoadFromStore 从库里读全部绑定并构建快照（启动引导与每次写入后调用）。
func LoadFromStore(ctx context.Context, st *store.Store) (*Table, error) {
	rows, err := st.ListAcctEgress(ctx)
	if err != nil {
		return nil, err
	}
	return NewTable(rows), nil
}
