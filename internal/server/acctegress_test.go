package server

// 账户↔出站绑定路由测试（docs/011）：
// 绑定命中优先于默认粘滞路由；粘滞 HRW 确定性；随机模式在候选池内分散；
// 候选全部停用时受控失败绝不回落；出站凭据零注入；未绑定身份走默认粘滞路由。

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"mitmrouter/internal/acctegress"
	"mitmrouter/internal/store"
	"mitmrouter/internal/upstream"
)

var testCtx = context.Background()

type egressBind struct {
	pf, acct, mode string
	ids            []int64
}

func bindTable(binds ...egressBind) *acctegress.Table {
	var rows []store.AcctEgressRow
	for _, b := range binds {
		for _, id := range b.ids {
			rows = append(rows, store.AcctEgressRow{Platform: b.pf, Account: b.acct, EgressID: id, Mode: b.mode})
		}
	}
	return acctegress.NewTable(rows)
}

func newEgressTestServer(t *testing.T, def string) *Server {
	t.Helper()
	s := newSaltTestServer()
	mk := func(id int64, name, platform string, enabled bool) *upstream.Upstream {
		u, err := upstream.FromRow(id, name, platform,
			"http://user-"+name+":pw@10.254.0.1:9000", sql.NullString{}, enabled)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	items := []*upstream.Upstream{
		mk(1, "eg-a", "plain", true),
		mk(2, "eg-b", "plain", true),
		mk(3, "eg-c", "plain", true),
		mk(4, "eg-d", "plain", false), // 停用
		mk(5, "di-default", "dataimpulse", true),
	}
	s.upstreams.Store(upstream.NewTable(items, def))
	return s
}

func TestBoundEgressStickyDeterministicAndPriority(t *testing.T) {
	const cred, pf, acct = "sk-eg-1", "openai", "s@x.io"
	ident := identity{key: pf + "/" + acct, mapped: true, platform: pf, account: acct}

	build := func() *Server {
		s := newEgressTestServer(t, "di-default")
		s.SwapAcctEgress(bindTable(egressBind{pf, acct, "sticky", []int64{1, 2, 3}}))
		return s
	}

	s1 := build()
	snap := s1.settings.Current()
	snap.DefaultUpstream = "di-default" // 路由读快照的默认上游名，非表的 def 字段
	pu1, account, name, reason, err := s1.resolveOutboundDetailed(testCtx, snap, cred, ident, "9.9.9.9", "api.openai.com")
	if err != nil {
		t.Fatalf("bound sticky route: %v", err)
	}
	if name != "eg-a" && name != "eg-b" && name != "eg-c" {
		t.Fatalf("picked %q outside bound set", name)
	}
	if reason != "egress_sticky" {
		t.Fatalf("reason=%q", reason)
	}
	if account == "" || account == "-" {
		t.Fatalf("account identity must persist: %q", account)
	}
	// 恒等注入：URL 保持 base_url 原样，零注入查询参数
	if pu1.User == nil || pu1.User.Username() != "user-"+name {
		t.Fatalf("identity inject violated: %q", pu1.String())
	}
	if pu1.RawQuery != "" {
		t.Fatalf("egress must not receive injected query params: %q", pu1.RawQuery)
	}

	// 确定性：同盐同池多次解析结果一致
	for range 5 {
		pu2, _, name2, _, err2 := s1.resolveOutboundDetailed(testCtx, snap, cred, ident, "9.9.9.9", "api.openai.com")
		if err2 != nil || pu2.String() != pu1.String() || name2 != name {
			t.Fatalf("nondeterministic pick: %q vs %q (%v)", name2, name, err2)
		}
	}
	// 重启不漂：同盐新实例选同一条出站
	s2 := build()
	snap2 := s2.settings.Current()
	snap2.DefaultUpstream = "di-default"
	_, _, name3, _, err3 := s2.resolveOutboundDetailed(testCtx, snap2, cred, ident, "9.9.9.9", "api.openai.com")
	if err3 != nil || name3 != name {
		t.Fatalf("pick must survive restart with same salt: %q vs %q (%v)", name3, name, err3)
	}
}

func TestUnboundIdentityUsesDefaultSticky(t *testing.T) {
	s := newEgressTestServer(t, "di-default")
	s.SwapAcctEgress(acctegress.NewTable(nil)) // 无任何绑定
	snap := s.settings.Current()
	snap.DefaultUpstream = "di-default"

	ident := identity{key: "openai/u@x.io", mapped: true, platform: "openai", account: "u@x.io"}
	const cred = "sk-plain"
	pu, _, name, reason, err := s.resolveOutboundDetailed(testCtx, snap, cred, ident, "9.9.9.9", "api.openai.com")
	if err != nil {
		t.Fatalf("unbound route: %v", err)
	}
	if name != "di-default" || reason != "upstream" {
		t.Fatalf("unbound must use default sticky upstream: name=%s reason=%s", name, reason)
	}
	// dataimpulse 注入器照常给默认上游注入 sessid 形态的用户名
	if pu.User == nil || pu.User.Username() == "user-di-default" {
		t.Fatalf("default sticky upstream must keep session injection, got %q", pu.User)
	}
}

func TestBoundEgressAllDisabledFailsControlled(t *testing.T) {
	s := newEgressTestServer(t, "di-default")
	// 只绑定到已停用的 eg-d：有绑定就绝不允许静默回落
	s.SwapAcctEgress(bindTable(egressBind{"anthropic", "f@x.io", "random", []int64{4}}))
	snap := s.settings.Current()
	snap.DefaultUpstream = "di-default"
	ident := identity{key: "anthropic/f@x.io", mapped: true, platform: "anthropic", account: "f@x.io"}

	pu, _, name, reason, err := s.resolveOutboundDetailed(testCtx, snap, "sk-f", ident, "9.9.9.9", "claude.ai")
	if err == nil {
		t.Fatal("bound-but-all-disabled must fail controlled, got success")
	}
	if !errors.Is(err, errUpstreamConfig) {
		t.Fatalf("err=%v want errUpstreamConfig", err)
	}
	if pu != nil {
		t.Fatal("must not return an exit URL on controlled failure")
	}
	if reason != "egress_none_enabled" {
		t.Fatalf("reason=%q", reason)
	}
	if name != "direct" {
		t.Fatalf("route must not silently become another upstream: %q", name)
	}
}

func TestBoundEgressRandomSpreadsOverCandidates(t *testing.T) {
	s := newEgressTestServer(t, "di-default")
	s.SwapAcctEgress(bindTable(egressBind{"gemini", "r@x.io", "random", []int64{1, 2, 3}}))
	snap := s.settings.Current()
	snap.DefaultUpstream = "di-default"
	ident := identity{key: "gemini/r@x.io", mapped: true, platform: "gemini", account: "r@x.io"}

	seen := map[string]int{}
	for range 300 {
		_, _, name, reason, err := s.resolveOutboundDetailed(testCtx, snap, "sk-r", ident, "9.9.9.9", "a.googleapis.com")
		if err != nil {
			t.Fatalf("random route: %v", err)
		}
		if reason != "egress_random" {
			t.Fatalf("reason=%q", reason)
		}
		seen[name]++
	}
	hits := 0
	for _, n := range []string{"eg-a", "eg-b", "eg-c"} {
		if seen[n] > 0 {
			hits++
		}
	}
	if hits < 2 {
		t.Fatalf("random mode must spread over candidates (only %d distinct): %v", hits, seen)
	}
}

func TestBoundEgressReshuffleOnlyAffectsRemovedCandidate(t *testing.T) {
	// HRW 性质：候选池减一条时，只有原本命中被删条目的账户换出站，其余原地不动。
	idA := identity{key: "grok/a@x.io", mapped: true, platform: "grok", account: "a@x.io"}
	idB := identity{key: "grok/b@x.io", mapped: true, platform: "grok", account: "b@x.io"}

	sFull := newEgressTestServer(t, "")
	sFull.SwapAcctEgress(bindTable(
		egressBind{"grok", "a@x.io", "sticky", []int64{1, 2, 3}},
		egressBind{"grok", "b@x.io", "sticky", []int64{1, 2, 3}},
	))
	sReduced := newEgressTestServer(t, "")
	sReduced.SwapAcctEgress(bindTable( // 删掉 ID=1
		egressBind{"grok", "a@x.io", "sticky", []int64{2, 3}},
		egressBind{"grok", "b@x.io", "sticky", []int64{2, 3}},
	))

	pick := func(s *Server, id identity) string {
		t.Helper()
		_, _, name, _, err := s.resolveOutboundDetailed(testCtx, s.settings.Current(), "any-key", id, "9.9.9.9", "api.x.ai")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return name
	}

	for _, id := range []identity{idA, idB} {
		before := pick(sFull, id)
		after := pick(sReduced, id)
		if before == "eg-a" {
			if after == before {
				t.Fatalf("%s previously on removed egress must re-shuffle", id.key)
			}
		} else if after != before {
			t.Fatalf("%s not on removed egress must stay: %s vs %s", id.key, before, after)
		}
	}
}

func TestAcctEgressSwapNilSafeLookup(t *testing.T) {
	// 未装配绑定快照（Load 返回 nil）时不得 panic，一律视为未绑定。
	s := newSaltTestServer() // 不初始化 acctEgress，模拟旧装配路径
	snap := s.settings.Current()
	ident := identity{key: "openai/n@x.io", mapped: true, platform: "openai", account: "n@x.io"}
	_, _, _, _, err := s.resolveOutboundDetailed(testCtx, snap, "sk-n", ident, "9.9.9.9", "api.openai.com")
	// 无绑定 + 快照未配置默认上游：保持「无上游=直连」的现状语义
	if err != nil {
		t.Fatalf("no binding with empty default_upstream must stay legacy direct path, got %v", err)
	}
}
