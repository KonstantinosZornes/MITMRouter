package acl

import (
	"strings"
	"testing"
)

func TestNormalizeHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"example.com", "example.com"},
		{"example.com:443", "example.com"},
		{"EXAMPLE.com.", "example.com"},
		{"[::1]:443", "::1"},
		{"[2001:db8::1]", "2001:db8::1"},
		{"::1", "::1"},
		{"  Api.OpenAI.Com:8443 ", "api.openai.com"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeHost(c.in); got != c.want {
			t.Errorf("NormalizeHost(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestListMatch(t *testing.T) {
	list := func(entries ...string) *List { l, _ := Compile(entries); return l }

	ip := list("127.0.0.1", "::1")
	cidr := list("10.0.0.0/8", "2001:db8::/32")
	exact := list("API.OpenAI.COM", "localhost")
	wild := list("*.openai.com")

	matchCases := []struct {
		name string
		l    *List
		in   string
		want bool
	}{
		{"ip exact hit", ip, "127.0.0.1:8080", true},
		{"ipv6 hit", ip, "[::1]:443", true},
		{"ip miss", ip, "127.0.0.2", false},
		{"cidr hit at boundary", cidr, "10.255.255.255:443", true},
		{"cidr miss", cidr, "11.0.0.1", false},
		{"ipv6 cidr hit", cidr, "[2001:db8::dead]:443", true},
		{"domain case and port normalized", exact, "api.openai.com:443", true},
		{"exact domain excludes subdomains", exact, "x.api.openai.com", false},
		{"single label domain", exact, "LOCALHOST", true},
		{"wildcard hits one-level subdomain", wild, "a.openai.com:443", true},
		{"wildcard hits multi-level subdomain", wild, "a.b.openai.com", true},
		{"wildcard does not hit bare domain", wild, "openai.com", false},
		{"wildcard dot boundary prevents spoofing", wild, "evil-openai.com", false},
		{"empty list never matches", list(), "anything.com", false},
	}
	for _, c := range matchCases {
		if got := c.l.Match(c.in); got != c.want {
			t.Errorf("%s: Match(%q)=%v want %v", c.name, c.in, got, c.want)
		}
	}

	var nilList *List
	if nilList.Match("x") {
		t.Error("nil list should never match")
	}
}

func TestRulesIntercept(t *testing.T) {
	var zero Rules
	if !zero.Intercept("anything.com") {
		t.Errorf("zero-value rules should always intercept, got %v", zero.Intercept("anything.com"))
	}
	var nilp *Rules
	if !nilp.Intercept("anything.com") {
		t.Error("nil rules should always intercept")
	}

	rulesFrom := func(w, b []string) *Rules { r, _ := NewRules(w, b); return r }

	// 黑名单优先：即使同时命中白名单也拒绝。
	r := rulesFrom([]string{"*.openai.com"}, []string{"bad.openai.com"})
	if r.Allowed("bad.openai.com") || r.Intercept("bad.openai.com") {
		t.Error("blacklist should take precedence and reject")
	}
	if !r.Allowed("api.openai.com") || !r.Intercept("api.openai.com") {
		t.Error("whitelisted target not in blacklist should be allowed and intercepted")
	}

	// 白名单非空即限定放行范围，名单外拒绝。
	r = rulesFrom([]string{"api.openai.com"}, nil)
	if r.Allowed("evil.com") || r.Intercept("evil.com") {
		t.Errorf("target outside whitelist should be rejected")
	}

	// 仅黑名单：其余放行，命中项拒绝。
	r = rulesFrom(nil, []string{"10.0.0.0/8"})
	if !r.Allowed("8.8.8.8") || !r.Intercept("8.8.8.8") {
		t.Error("with blacklist only, non-matching target should be allowed and intercepted")
	}
	if r.Allowed("10.1.2.3:443") || r.Intercept("10.1.2.3:443") {
		t.Error("blacklisted CIDR should be rejected")
	}
}

func TestNewRulesSkipsInvalid(t *testing.T) {
	r, skipped := NewRules(
		[]string{"api.openai.com", "1.2.3.4:80", "*.com..", "*"},
		[]string{"10.0.0.0/33", "good.example.com"},
	)
	if skipped != 4 {
		t.Errorf("expected 4 invalid entries skipped, got %d", skipped)
	}
	if !r.Intercept("api.openai.com") {
		t.Error("valid whitelist entry should take effect")
	}
	if r.Intercept("good.example.com") {
		t.Error("valid blacklist entry should take effect")
	}
}

func TestInvalidWhitelistFailsClosed(t *testing.T) {
	r, skipped := NewRules([]string{"not a valid target"}, nil)
	if skipped != 1 {
		t.Fatalf("expected one invalid entry skipped, got %d", skipped)
	}
	if r.Allowed("api.openai.com") || r.Intercept("api.openai.com") {
		t.Fatal("configured whitelist with no valid entries must reject targets")
	}

	r, skipped = NewRules([]string{"api.openai.com", "still invalid"}, nil)
	if skipped != 1 {
		t.Fatalf("expected one invalid entry skipped, got %d", skipped)
	}
	if !r.Allowed("api.openai.com") || r.Allowed("evil.example.com") {
		t.Fatal("valid whitelist entries must remain restrictive when another entry is invalid")
	}
}

func TestNormalizeEntry(t *testing.T) {
	valid := []string{
		"1.2.3.4", "::1", "10.0.0.0/8", "::ffff:0:0/96",
		"API.Example.com.", "*.Example.COM", "localhost", "host_docker.internal",
		"a.io", "xn--fiqs8s.icom.museum",
	}
	for _, e := range valid {
		if _, err := NormalizeEntry(e); err != nil {
			t.Errorf("valid entry %q rejected: %v", e, err)
		}
	}
	invalid := []string{
		"", "   ", "1.2.3.4:80", "example.com/path",
		"10.0.0.0/33", "10.0.0.0/", "*", "*.", "*.*.com",
		"-bad.com", "bad-.com", ".abc.com", "a..b.com",
		"*.openai.com/x", "exa mple.com", strings.Repeat("a", 64) + ".com",
	}
	for _, e := range invalid {
		if _, err := NormalizeEntry(e); err == nil {
			t.Errorf("invalid entry %q not rejected", e)
		}
	}
}

func TestValidateListsLimit(t *testing.T) {
	big := make([]string, MaxListLen+1)
	for i := range big {
		big[i] = "a" + string(rune('a'+i%26)) + ".com"
	}
	if err := ValidateLists(big, nil); err == nil {
		t.Error("oversized whitelist should error")
	}
	if err := ValidateLists(nil, []string{"ok.com"}); err != nil {
		t.Errorf("valid list should not error: %v", err)
	}
}
