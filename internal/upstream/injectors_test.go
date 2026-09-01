package upstream

import (
	"net/url"
	"testing"
)

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// runInject 构造条目并执行注入，返回注入后的用户名（含密码则 user:pass 形态）。
func runInject(t *testing.T, platform, baseURL string, p InjectParams, tpl ...string) string {
	t.Helper()
	up := &Upstream{ID: 1, Name: "t", Platform: platform, BaseURL: mustParse(t, baseURL), Enabled: true}
	if len(tpl) > 0 && platform == PlatformGeneric {
		up.UsernameTemplate = tpl[0]
		if len(tpl) > 1 {
			up.StaticPassword = tpl[1]
		}
	}
	inj, ok := InjectorFor(platform, up)
	if !ok {
		t.Fatalf("platform %s not registered", platform)
	}
	out, err := inj.Inject(up.BaseURL, p)
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if out.User == nil {
		t.Fatalf("userinfo empty after inject")
	}
	s := out.User.Username()
	if pw, has := out.User.Password(); has {
		s += ":" + pw
	}
	return s
}

const acct = "a1b2c3d4e5f60718"

func TestDataImpulse(t *testing.T) {
	cases := []struct {
		name, user, want string
	}{
		{"keep country param and append sessid", "samplelogin__cr.us", "samplelogin__cr.us;sessid." + acct},
		{"no param section appends __sessid directly", "dcuser", "dcuser__sessid." + acct},
		{"replace old sessid keeping other params", "u__cr.jp;sessid.OLD;x.1", "u__cr.jp;x.1;sessid." + acct},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runInject(t, PlatformDataImpulse, "http://"+c.user+":pw@gw.dataimpulse.com:823",
				InjectParams{Account: acct})
			if got != c.want+":pw" {
				t.Errorf("got %q want %q", got, c.want+":pw")
			}
		})
	}
}

func TestC1024(t *testing.T) {
	cases := []struct {
		name, user, want string
		ttl              int
	}{
		{"real sample replaces sid keeps region and t", "yourapikey-region-US-sid-yoursessid-t-5", "yourapikey-region-US-sid-" + acct + "-t-5", 0},
		{"apikey containing dashes not mis-split", "ab-cd-region-US-t-5", "ab-cd-sid-" + acct + "-region-US-t-5", 0},
		{"explicit TTL overrides t segment", "yourapikey-region-US-sid-x-t-5", "yourapikey-region-US-sid-" + acct + "-t-15", 15},
		{"append tail when no known key present", "plainkey", "plainkey-sid-" + acct, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := &Upstream{ID: 1, Name: "t", Platform: Platform1024Proxy, BaseURL: mustParse(t, "socks5://"+c.user+":pw@us.1024proxy.io:3000"), Enabled: true}
			inj, _ := InjectorFor(Platform1024Proxy, up)
			out, err := inj.Inject(up.BaseURL, InjectParams{Account: acct, TTLMin: c.ttl})
			if err != nil {
				t.Fatal(err)
			}
			want := c.want + ":pw"
			if got := out.User.Username() + ":" + mustPass(out); got != want {
				t.Errorf("got %q want %q", got, want)
			}
		})
	}
}

func TestDecodo(t *testing.T) {
	cases := []struct {
		name, user, want string
	}{
		{"official sample replaces session keeps rest", "user-alice-country-us-city-chicago-session-old-sessionduration-90",
			"user-alice-country-us-city-chicago-session-" + acct + "-sessionduration-90"},
		{"insert after login name when session missing", "user-bob-country-us-sessionduration-30",
			"user-bob-session-" + acct + "-country-us-sessionduration-30"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runInject(t, PlatformDecodo, "http://"+c.user+":pw@gate.decodo.com:7000",
				InjectParams{Account: acct})
			if got != c.want+":pw" {
				t.Errorf("got %q want %q", got, c.want+":pw")
			}
		})
	}
	t.Run("error without user prefix", func(t *testing.T) {
		up := &Upstream{Platform: PlatformDecodo, BaseURL: mustParse(t, "http://alice-session-x:p@gate.decodo.com:7000")}
		inj, _ := InjectorFor(PlatformDecodo, up)
		if _, err := inj.Inject(up.BaseURL, InjectParams{Account: acct}); err == nil {
			t.Fatal("expected error, got success")
		}
	})
}

func TestResin(t *testing.T) {
	cases := []struct {
		name, user, want string
	}{
		{"Default platform gets Account appended", "Default", "Default." + acct},
		{"existing Account is overridden", "MyPlatform.tom", "MyPlatform." + acct},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runInject(t, PlatformResin, "socks5://"+c.user+":tok@resin:2260",
				InjectParams{Account: acct})
			if got != c.want+":tok" {
				t.Errorf("got %q want %q", got, c.want+":tok")
			}
		})
	}
}

func TestGeneric(t *testing.T) {
	t.Run("template render with static password override", func(t *testing.T) {
		got := runInject(t, PlatformGeneric, "http://k1:oldpass@gate.x.io:8000",
			InjectParams{Account: acct}, "{user}-sessid-{sid}", "spass")
		want := "k1-sessid-" + acct + ":spass"
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})
	t.Run("no static password keeps original", func(t *testing.T) {
		got := runInject(t, PlatformGeneric, "http://k1:oldpass@gate.x.io:8000",
			InjectParams{Account: acct}, "{user}-s-{sid}")
		want := "k1-s-" + acct + ":oldpass"
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})
	t.Run("ttl placeholder render", func(t *testing.T) {
		got := runInject(t, PlatformGeneric, "http://k1@gate.x.io:8000",
			InjectParams{Account: acct, TTLMin: 30}, "{user}-{ttl_min}")
		if got != "k1-30" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("reject invalid template from manually edited database", func(t *testing.T) {
		up := &Upstream{Platform: PlatformGeneric, BaseURL: mustParse(t, "http://k1:pw@gate.x.io:8000"), UsernameTemplate: "{user}-{SID}"}
		inj, _ := InjectorFor(PlatformGeneric, up)
		if _, err := inj.Inject(up.BaseURL, InjectParams{Account: acct}); err == nil {
			t.Fatal("invalid template must not reach upstream unchanged")
		}
	})
}

func TestValidateForSave(t *testing.T) {
	ok := []struct{ name, platform, url, inject string }{
		{"dataimpulse sample", PlatformDataImpulse, "http://abc__cr.us:pw@gw.dataimpulse.com:823", ""},
		{"1024 sample", Platform1024Proxy, "socks5://yourapikey-region-US-sid-z-t-5:pw@us.1024proxy.io:3000", ""},
		{"decodo sample", PlatformDecodo, "http://user-alice-session-x-sessionduration-30:pw@gate.decodo.com:7000", ""},
		{"resin sample", PlatformResin, "socks5://Default:pw@resin:2260", ""},
		{"generic valid template", PlatformGeneric, "http://gate.x.io:8000", `{"username_template":"{user}-s-{sid}"}`},
	}
	for _, c := range ok {
		t.Run("pass/"+c.name, func(t *testing.T) {
			if err := ValidateForSave(c.platform, c.url, c.inject, 0); err != nil {
				t.Errorf("expected pass, got: %v", err)
			}
		})
	}
	bad := []struct {
		name, platform, url, inject, contain string
		ttl                                  int
	}{
		{"invalid scheme", PlatformDataImpulse, "ftp://gw.dataimpulse.com", "", "unsupported scheme", 0},
		{"socks5 missing port", PlatformResin, "socks5://Default:pw@resin", "", "port", 0},
		{"socks5h missing port", PlatformResin, "socks5h://Default:pw@resin", "", "port", 0},
		{"decodo missing prefix", PlatformDecodo, "http://alice:p@gate.decodo.com:7000", "", "user-", 0},
		{"resin empty username", PlatformResin, "socks5://:pw@resin:2260", "", "must not be empty", 0},
		{"1024proxy empty username", Platform1024Proxy, "socks5://:pw@us.1024proxy.io:3000", "", "must not be empty", 0},
		{"unknown platform", "nope", "http://g.x.io:80", "", "unknown platform", 0},
		{"generic undefined placeholder", PlatformGeneric, "http://g.x.io:8000", `{"username_template":"{user}-{foo}"}`, "undefined placeholder", 10},
		{"generic case mismatched placeholder", PlatformGeneric, "http://g.x.io:8000", `{"username_template":"{user}-{SID}"}`, "undefined placeholder", 10},
		{"generic unclosed placeholder", PlatformGeneric, "http://g.x.io:8000", `{"username_template":"{user}-{sid"}`, "unclosed placeholder", 10},
		{"generic ttl requires setting enabled", PlatformGeneric, "http://g.x.io:8000", `{"username_template":"{sid}-{ttl_min}"}`, "session_ttl_min", 0},
	}
	for _, c := range bad {
		t.Run("reject/"+c.name, func(t *testing.T) {
			err := ValidateForSave(c.platform, c.url, c.inject, c.ttl)
			if err == nil {
				t.Fatalf("expected error containing %q, got success", c.contain)
			}
		})
	}
}

func TestKnownPlatforms(t *testing.T) {
	got := KnownPlatforms()
	want := []string{Platform1024Proxy, PlatformDataImpulse, PlatformDecodo, PlatformGeneric, PlatformPlain, PlatformResin}
	if len(got) != len(want) {
		t.Fatalf("registered platforms %v != %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func mustPass(u *url.URL) string {
	if pw, ok := u.User.Password(); ok {
		return pw
	}
	return ""
}
