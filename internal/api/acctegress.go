// Package api：账户 ↔ 出站绑定端点（docs/011-plain-binding-design.md §4）。
// 双向操作均为「整体替换」语义，单事务落库后原子换内存快照：
//   - 账户方向：先删该账户全部绑定行，再按 mode 插入出站全集；
//   - 出站方向：整体替换该出站关联的账户集合；mode 仅对本次新关联的账户生效，
//     已有绑定的账户保留自己的路由语义。
package api

import (
	"net/http"
	"strconv"
	"strings"

	"mitmrouter/internal/acctegress"
	"mitmrouter/internal/acctmap"
	"mitmrouter/internal/store"
	"mitmrouter/internal/syncer"
	"mitmrouter/internal/upstream"
)

// reloadAcctEgress 从库里重建绑定快照并原子换表。acct_map 的任何级联 GC、
// 绑定的任何写入之后都必须调用一次。失败仅记日志：快照落后于库时路由仍安全
// （最多多走一条已删除的出站直到下次重建），不应把已生效的写操作回滚给用户。
func (a *API) reloadAcctEgress() {
	_ = syncer.WithMapChangeLock(func() error {
		a.reloadAcctEgressLocked()
		return nil
	})
}

func (a *API) reloadAcctEgressLocked() {
	if a.d.SwapAcctEgress == nil {
		return
	}
	t, err := acctegress.LoadFromStore(ctxBG(), a.d.Store)
	if err != nil {
		a.logger().Error("failed to rebuild acct_egress table", "err", err)
		return
	}
	a.d.SwapAcctEgress(t)
}

type bindingDTO struct {
	Platform    string   `json:"platform"`
	Account     string   `json:"account"`
	Mode        string   `json:"mode"`
	EgressIDs   []int64  `json:"egress_ids"`
	EgressNames []string `json:"egress_names"`
}

// listAcctEgress 返回全部绑定（含出站名与每个出站的关联计数），一次喂饱
// 管理台的双向视图。
func (a *API) listAcctEgress(w http.ResponseWriter, r *http.Request) {
	rows, err := a.d.Store.ListAcctEgress(ctxBG())
	if err != nil {
		a.failInternal(w, r, err)
		return
	}
	names := map[int64]string{}
	ups, _ := a.d.Store.ListUpstreams(ctxBG())
	for _, u := range ups {
		names[u.ID] = u.Name
	}
	counts := map[int64]int{}
	grouped := map[string]*bindingDTO{}
	items := make([]*bindingDTO, 0, len(rows))
	for _, row := range rows {
		counts[row.EgressID]++
		key := row.Platform + "\x00" + row.Account
		dto, ok := grouped[key]
		if !ok {
			dto = &bindingDTO{
				Platform:    row.Platform,
				Account:     row.Account,
				Mode:        row.Mode,
				EgressIDs:   []int64{},
				EgressNames: []string{},
			}
			grouped[key] = dto
			items = append(items, dto)
		}
		// ListAcctEgress 按 (platform,account,egress_id) 排序，且主键保证无重复行。
		dto.EgressIDs = append(dto.EgressIDs, row.EgressID)
		dto.EgressNames = append(dto.EgressNames, names[row.EgressID])
	}
	out := make([]bindingDTO, 0, len(items))
	for _, it := range items {
		out = append(out, *it)
	}
	writeJSON(w, 200, map[string]any{"items": out, "counts": counts})
}

// putAcctEgress 账户方向：整体替换该账户的出站绑定。body:
// {"mode":"sticky","egress_ids":[3,7]}；egress_ids 为空等价于清除绑定。
func (a *API) putAcctEgress(w http.ResponseWriter, r *http.Request) {
	pf := strings.ToLower(strings.TrimSpace(r.PathValue("platform")))
	account := acctmap.NormalizeAccount(r.PathValue("account"))
	if pf == "" || account == "" {
		writeErr(w, 400, "bad_request", "platform/account required")
		return
	}
	var b struct {
		Mode      string  `json:"mode"`
		EgressIDs []int64 `json:"egress_ids"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}
	mode := b.Mode
	if mode == "" {
		mode = store.EgressModeSticky // 与表默认一致；显式非法值仍然拒绝
	}
	if !store.ValidEgressMode(mode) {
		writeErr(w, 400, "bad_mode", "mode must be sticky or random")
		return
	}
	ids, ok := a.validateEgressIDs(w, r, b.EgressIDs)
	if !ok {
		return
	}
	exists, err := a.d.Store.AcctExists(ctxBG(), pf, account)
	if err != nil {
		a.failInternal(w, r, err)
		return
	}
	if !exists {
		writeErr(w, 404, "account_not_found", "account not in acct_map; register credentials first")
		return
	}
	if err := a.d.Store.ReplaceAccountBinding(ctxBG(), pf, account, mode, ids); err != nil {
		a.failInternal(w, r, err)
		return
	}
	a.reloadAcctEgress()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// deleteAcctEgress 清除某账户的全部绑定。
func (a *API) deleteAcctEgress(w http.ResponseWriter, r *http.Request) {
	pf := strings.ToLower(strings.TrimSpace(r.PathValue("platform")))
	account := acctmap.NormalizeAccount(r.PathValue("account"))
	if pf == "" || account == "" {
		writeErr(w, 400, "bad_request", "platform/account required")
		return
	}
	if err := a.d.Store.ReplaceAccountBinding(ctxBG(), pf, account, store.EgressModeSticky, nil); err != nil {
		a.failInternal(w, r, err)
		return
	}
	a.reloadAcctEgress()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// clearAcctEgress 一键清空全部出站绑定。破坏性操作，管理台调用前须二次确认。
func (a *API) clearAcctEgress(w http.ResponseWriter, r *http.Request) {
	n, err := a.d.Store.ClearAcctEgress(ctxBG())
	if err != nil {
		a.failInternal(w, r, err)
		return
	}
	a.reloadAcctEgress()
	writeJSON(w, 200, map[string]any{"ok": true, "removed": n})
}

// putAcctEgressBatch 出站方向：整体替换该出站关联的账户集合。body:
// {"accounts":[{"platform","account"}...],"mode":"random"}；mode 只对本次新
// 关联的账户生效（缺省 sticky）。
func (a *API) putAcctEgressBatch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad_id", "invalid id")
		return
	}
	var b struct {
		Accounts []struct {
			Platform string `json:"platform"`
			Account  string `json:"account"`
		} `json:"accounts"`
		Mode string `json:"mode"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}
	mode := b.Mode
	if mode == "" {
		mode = store.EgressModeSticky
	}
	if !store.ValidEgressMode(mode) {
		writeErr(w, 400, "bad_mode", "mode must be sticky or random")
		return
	}
	row, found := a.upstreamByID(id)
	if !found {
		writeErr(w, 404, "not_found", "entry not found")
		return
	}
	if row.Platform != upstream.PlatformPlain {
		writeErr(w, 400, "not_plain", "only platform=plain entries accept account bindings")
		return
	}
	assign := make([]store.EgressAssign, 0, len(b.Accounts))
	seen := map[string]bool{}
	var missing []string
	for _, acc := range b.Accounts {
		pf := strings.ToLower(strings.TrimSpace(acc.Platform))
		name := acctmap.NormalizeAccount(acc.Account)
		if pf == "" || name == "" {
			writeErr(w, 400, "bad_request", "platform/account required")
			return
		}
		key := pf + "\x00" + name
		if seen[key] {
			continue
		}
		seen[key] = true
		exists, err := a.d.Store.AcctExists(ctxBG(), pf, name)
		if err != nil {
			a.failInternal(w, r, err)
			return
		}
		if !exists {
			missing = append(missing, pf+"/"+name)
			continue
		}
		assign = append(assign, store.EgressAssign{Platform: pf, Account: name, Mode: mode})
	}
	if len(missing) > 0 {
		// 整单原子不落库：批量里出现未知账号时明确报错，而不是静默部分成功。
		writeErr(w, 404, "account_not_found", "unknown accounts: "+strings.Join(missing, ", "))
		return
	}
	if err := a.d.Store.ReplaceEgressAccounts(ctxBG(), id, assign); err != nil {
		a.failInternal(w, r, err)
		return
	}
	a.reloadAcctEgress()
	writeJSON(w, 200, map[string]any{"ok": true, "bound": len(assign)})
}

// validateEgressIDs 校验 ID 集合：全部必须是 egress 条目（启用与否不限——停用的
// 出站允许留在绑定里，运行期会因无可用候选而受控失败），去重后返回。
// 校验失败时已写好响应并返回 false。
func (a *API) validateEgressIDs(w http.ResponseWriter, r *http.Request, ids []int64) ([]int64, bool) {
	if len(ids) == 0 {
		return nil, true
	}
	rows, err := a.d.Store.ListUpstreams(ctxBG())
	if err != nil {
		a.failInternal(w, r, err)
		return nil, false
	}
	known := make(map[int64]bool)
	for _, rw := range rows {
		if rw.Platform == upstream.PlatformPlain {
			known[rw.ID] = true
		}
	}
	out := make([]int64, 0, len(ids))
	dedupe := make(map[int64]bool)
	for _, id := range ids {
		if !known[id] {
			writeErr(w, 400, "not_plain", "id is not a plain-proxy entry or does not exist")
			return nil, false
		}
		if dedupe[id] {
			continue
		}
		dedupe[id] = true
		out = append(out, id)
	}
	return out, true
}

// upstreamByID 按 ID 取上游行。
func (a *API) upstreamByID(id int64) (store.UpstreamRow, bool) {
	rows, _ := a.d.Store.ListUpstreams(ctxBG())
	for _, rw := range rows {
		if rw.ID == id {
			return rw, true
		}
	}
	return store.UpstreamRow{}, false
}
