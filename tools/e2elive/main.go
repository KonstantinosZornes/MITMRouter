// Command e2elive 用真实双源对 acct_map 做端到端验证：
// 同步入库 → 同账号双源并集（两行）→ 单源重拉不误删 → 推送(自定义类型) → 删除。
// 凭据经环境变量注入，绝不写入代码；所有输出仅含脱敏尾缀。
//
//	go run ./tools/e2elive
//	  E2E_SUB2API_URL / E2E_SUB2API_KEY / E2E_CPA_URL / E2E_CPA_KEY 必填。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"mitmrouter/internal/acctmap"
	"mitmrouter/internal/store"
	"mitmrouter/internal/syncer"
)

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		fmt.Println("SKIP: missing env", k)
		os.Exit(2)
	}
	return v
}

var failures int

func check(name string, ok bool, detail string) {
	mark := "PASS"
	if !ok {
		mark = "FAIL"
		failures++
	}
	fmt.Printf("[%s] %s %s\n", mark, name, detail)
}

func main() {
	subURL, subKey := mustEnv("E2E_SUB2API_URL"), mustEnv("E2E_SUB2API_KEY")
	cpaURL, cpaKey := mustEnv("E2E_CPA_URL"), mustEnv("E2E_CPA_KEY")

	dir, err := os.MkdirTemp("", "e2elive-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	st, err := store.Open(dir)
	if err != nil {
		panic(err)
	}
	defer st.Close()
	reg := acctmap.New()
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	idSub, err := st.CreateSyncSource(ctx, acctmap.SourceKindSub2API, "e2e-sub2api", subURL, subKey, 600, true)
	if err != nil {
		panic(err)
	}
	idCpa, err := st.CreateSyncSource(ctx, acctmap.SourceKindCLIProxyAPI, "e2e-cpa", cpaURL, cpaKey, 600, true)
	if err != nil {
		panic(err)
	}
	srcSub, srcCpa := fmt.Sprintf("%s%d", acctmap.SourceInstancePrefix, idSub), fmt.Sprintf("%s%d", acctmap.SourceInstancePrefix, idCpa)

	mgr := syncer.New(st, reg, log)
	go mgr.Run(ctx)

	// ---------- 阶段 1：首轮同步（等两个来源实例都出数据）----------
	rows := waitBothSources(st, srcSub, srcCpa, 240*time.Second)
	check("initial-sync", len(rows) > 0, fmt.Sprintf("rows=%d", len(rows)))
	dumpBySource(rows)

	// ---------- 阶段 2：同账号双源并集 ----------
	union := intersectAccounts(rows, srcSub, srcCpa)
	check("dual-source-union", len(union) > 0,
		fmt.Sprintf("accounts-in-both=%d sample=%v", len(union), unionHeads(union, 3)))

	// ---------- 热路径指纹查找 ----------
	hit := false
	for _, r := range rows {
		if r.AtFP != "" {
			e, ok := reg.Lookup(r.AtFP)
			hit = ok && e.Account == r.Account && e.Source == r.Source
			break
		}
	}
	check("hotpath-lookup", hit, "")

	// ---------- 阶段 3：单源重拉不误删 ----------
	baseCpa := countBySource(rows, srcCpa)
	baseMaxSub := maxUpdated(rows, srcSub)
	mgr.Wake(idSub)
	rows = waitRewrite(st, srcSub, baseMaxSub, 120*time.Second)
	check("repull-done", len(rows) > 0, "")
	gotCpa := countBySource(rows, srcCpa)
	check("repull-no-clobber", gotCpa == baseCpa && baseCpa > 0,
		fmt.Sprintf("cpa-before=%d cpa-after=%d", baseCpa, gotCpa))
	dumpBySource(rows)

	// ---------- 阶段 4：推送（自定义 source_type）----------
	pf, acct := "openai", "push-e2e@example.com"
	up := store.AcctUpsert{Platform: pf, Account: acct, RtFP: "rt-push-1", AtFP: "at-push-1", AtHint: "…p1"}
	if err := st.ReplaceAccountSnapshot(ctx, pf, acct, "MyRelay", up); err != nil {
		panic(err)
	}
	rows, _ = st.LoadAcctMapAll(ctx)
	check("push-custom-type", countBy(rows, pf, acct, "MyRelay") == 1, fmt.Sprintf("total=%d", len(rows)))

	up.AtFP = "at-push-2"
	if err := st.ReplaceAccountSnapshot(ctx, pf, acct, "MyRelay",
		store.AcctUpsert{Platform: pf, Account: acct, RtFP: "rt-push-1", AtFP: "at-push-2"}); err != nil {
		panic(err)
	}
	rows, _ = st.LoadAcctMapAll(ctx)
	r := findRow(rows, pf, acct, "MyRelay")
	check("push-at-update-inplace", r != nil && r.AtFP == "at-push-2" && countBy(rows, pf, acct, "MyRelay") == 1, "")

	// ---------- 阶段 5：删除 ----------
	n, err := st.DeleteAcctMapAccount(ctx, pf, acct, "") // 全来源删除
	check("delete-account-all-sources", err == nil && n >= 1, fmt.Sprintf("removed=%d", n))

	// 取双源交集账号做范围删除样本（保证删除后仍有他源行可校验保留）
	sample := sampleFromUnion(rows, union, srcSub)
	if sample == nil {
		sample = firstRowOf(rows, srcSub)
	}
	var nDel int64
	if sample != nil {
		nDel, _ = st.DeleteAcctMapAccount(ctx, sample.Platform, sample.Account, srcSub)
	}
	rows, _ = st.LoadAcctMapAll(ctx)
	stillThere := false
	if sample != nil {
		for _, r := range rows {
			if r.Platform == sample.Platform && r.Account == sample.Account && r.Source != srcSub {
				stillThere = true
			}
		}
	}
	check("delete-scoped-to-source", nDel >= 1 && stillThere,
		fmt.Sprintf("removed=%d other-source-row-kept=%v sample=%s", nDel, stillThere, sample.Account))

	for _, id := range []int64{idSub, idCpa} {
		src := fmt.Sprintf("src:%d", id)
		before := countBySource(rows, src)
		n, err := st.DeleteSourceRows(ctx, src)
		if err != nil {
			panic(err)
		}
		if err := st.DeleteSyncSource(ctx, id); err != nil {
			panic(err)
		}
		rows, _ = st.LoadAcctMapAll(ctx)
		check("cascade-delete-source "+src, int(n) == before && countBySource(rows, src) == 0,
			fmt.Sprintf("removed=%d", n))
	}

	if failures > 0 {
		fmt.Printf("\nRESULT: %d check(s) FAILED\n", failures)
		os.Exit(1)
	}
	fmt.Println("\nRESULT: all checks passed")
}

func waitRows(st *store.Store, d time.Duration) []store.AcctRow {
	ddl := time.Now().Add(d)
	for time.Now().Before(ddl) {
		rows, err := st.LoadAcctMapAll(context.Background())
		if err == nil && len(rows) > 0 {
			return rows
		}
		time.Sleep(2 * time.Second)
	}
	return nil
}

// waitBothSources 等待两个来源实例都出现数据（首轮同步完成）。
func waitBothSources(st *store.Store, a, b string, d time.Duration) []store.AcctRow {
	ddl := time.Now().Add(d)
	for time.Now().Before(ddl) {
		rows, err := st.LoadAcctMapAll(context.Background())
		if err == nil && countBySource(rows, a) > 0 && countBySource(rows, b) > 0 {
			return rows
		}
		time.Sleep(2 * time.Second)
	}
	return nil
}

// waitRewrite 等待指定来源实例的行出现比 base 新的 updated_at（重拉完成）。
func waitRewrite(st *store.Store, source string, base int64, d time.Duration) []store.AcctRow {
	ddl := time.Now().Add(d)
	for time.Now().Before(ddl) {
		rows, err := st.LoadAcctMapAll(context.Background())
		if err == nil && maxUpdated(rows, source) > base {
			return rows
		}
		time.Sleep(2 * time.Second)
	}
	rows, _ := st.LoadAcctMapAll(context.Background())
	return rows
}

func dumpBySource(rows []store.AcctRow) {
	perType, perSrc := map[string]int{}, map[string]int{}
	for _, r := range rows {
		perType[r.SourceType]++
		perSrc[r.Source]++
	}
	fmt.Printf("      by_source_type=%v by_source=%v\n", perType, perSrc)
}

func countBySource(rows []store.AcctRow, source string) int {
	n := 0
	for _, r := range rows {
		if r.Source == source {
			n++
		}
	}
	return n
}

func countBy(rows []store.AcctRow, pf, acct, stype string) int {
	n := 0
	for _, r := range rows {
		if r.Platform == pf && r.Account == acct && r.SourceType == stype {
			n++
		}
	}
	return n
}

func findRow(rows []store.AcctRow, pf, acct, stype string) *store.AcctRow {
	for i := range rows {
		if rows[i].Platform == pf && rows[i].Account == acct && rows[i].SourceType == stype {
			return &rows[i]
		}
	}
	return nil
}

func maxUpdated(rows []store.AcctRow, source string) int64 {
	var m int64
	for _, r := range rows {
		if r.Source == source && r.UpdatedAt > m {
			m = r.UpdatedAt
		}
	}
	return m
}

// intersectAccounts 返回两个来源实例都收录的 (platform, account) 对。
func intersectAccounts(rows []store.AcctRow, a, b string) [][2]string {
	inA := map[string]bool{}
	for _, r := range rows {
		if r.Source == a {
			inA[r.Platform+"\x00"+r.Account] = true
		}
	}
	seen := map[string]bool{}
	var out [][2]string
	for _, r := range rows {
		key := r.Platform + "\x00" + r.Account
		if r.Source == b && inA[key] && !seen[key] {
			seen[key] = true
			out = append(out, [2]string{r.Platform, r.Account})
		}
	}
	return out
}

func sampleFromUnion(rows []store.AcctRow, union [][2]string, source string) *store.AcctRow {
	for _, p := range union {
		for i := range rows {
			if rows[i].Platform == p[0] && rows[i].Account == p[1] && rows[i].Source == source {
				return &rows[i]
			}
		}
	}
	return nil
}

func firstRowOf(rows []store.AcctRow, source string) *store.AcctRow {
	for i := range rows {
		if rows[i].Source == source {
			return &rows[i]
		}
	}
	return nil
}

func unionHeads(ps [][2]string, n int) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p[0]+"/"+p[1])
	}
	if len(out) > n {
		return out[:n]
	}
	return out
}
