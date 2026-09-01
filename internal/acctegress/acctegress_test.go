package acctegress

// 绑定快照测试：按账户聚合、模式防御性收敛、ID 排序与未命中语义。

import (
	"testing"

	"mitmrouter/internal/store"
)

func TestNewTableGroupsAndSorts(t *testing.T) {
	rows := []store.AcctEgressRow{
		{Platform: "openai", Account: "a@x.io", EgressID: 7, Mode: ModeSticky},
		{Platform: "openai", Account: "a@x.io", EgressID: 3, Mode: ""},
		{Platform: "anthropic", Account: "b@x.io", EgressID: 5, Mode: ModeRandom},
	}
	tbl := NewTable(rows)

	b, ok := tbl.Lookup("openai", "a@x.io")
	if !ok {
		t.Fatal("missing binding for a@x.io")
	}
	if b.Mode != ModeSticky || len(b.EgressIDs) != 2 || b.EgressIDs[0] != 3 || b.EgressIDs[1] != 7 {
		t.Fatalf("binding=%+v (mode from non-empty row, ids sorted)", b)
	}
	b2, ok := tbl.Lookup("anthropic", "b@x.io") // 精确匹配；大小写归一由写入方负责
	if !ok || b2.Mode != ModeRandom {
		t.Fatalf("anthropic binding=%+v ok=%v", b2, ok)
	}
	if _, hit := tbl.Lookup("gemini", "nobody"); hit {
		t.Fatal("unbound account must miss")
	}

	items := tbl.Items()
	if len(items) != 2 || items[0].Platform != "anthropic" {
		t.Fatalf("items must be platform-ordered: %+v", items)
	}
	if _, hit := EmptyTable().Lookup("openai", "a@x.io"); hit {
		t.Fatal("empty table must miss everything")
	}
}
