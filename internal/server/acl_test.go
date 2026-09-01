package server

// 黑白名单（ACL）集成测试：
//   - 空名单放行所有目标；
//   - 白名单非空时只放行命中目标；
//   - 黑名单命中始终拒绝，且优先于白名单；
//   - 放行请求继续走原有透明转发路径。

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mitmrouter/internal/store"
)

// setACL 更新快照中的黑白名单。
func setACL(t *testing.T, s *stack, white, black []string) {
	t.Helper()
	snap := s.holder.Current()
	snap.ACLWhitelist = white
	snap.ACLBlacklist = black
	s.holder.Set(snap)
}

func TestACLForwardPath(t *testing.T) {
	s := newStack(t)
	audit := make(chan store.LogEntry, 4)
	s.srv.audit = audit
	var hits atomic.Int64
	type observedRequest struct {
		method string
		path   string
		header string
		body   string
	}
	observed := make(chan observedRequest, 4)
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		observed <- observedRequest{
			method: r.Method,
			path:   r.URL.RequestURI(),
			header: r.Header.Get("X-ACL-Test"),
			body:   string(body),
		}
		w.Header().Set("X-ACL-Response", "keep-response")
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(be.Close)

	getBody := func(url string) (int, string) {
		t.Helper()
		client := ingressClient(t, s.feURL)
		resp, err := client.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return resp.StatusCode, string(b)
	}

	assertACLBlocked := func(t *testing.T, url string) {
		t.Helper()
		st, body := getBody(url)
		if st != http.StatusForbidden || !strings.Contains(body, `"acl_forbidden"`) {
			t.Fatalf("ACL-blocked target should return 403 acl_forbidden, got %d %q", st, body)
		}
		select {
		case entry := <-audit:
			if entry.Method != http.MethodGet || entry.Path != "/v1/x" || entry.Status != 0 || entry.InternalError != "acl_blocked" {
				t.Fatalf("unexpected ACL rejection audit entry: %+v", entry)
			}
		case <-time.After(time.Second):
			t.Fatal("ACL rejection was not added to audit")
		}
	}

	t.Run("no list allows all", func(t *testing.T) {
		setACL(t, s, nil, nil)
		st, body := getBody(be.URL + "/v1/x")
		if st != http.StatusOK || body != "ok" {
			t.Fatalf("empty list should allow local backend, got %d %q", st, body)
		}
		<-observed // 丢弃本子测试的观测值，后面只校验白名单请求。
		<-audit    // 同样丢弃本子测试的审计记录。
	})

	t.Run("whitelisted request is forwarded unchanged", func(t *testing.T) {
		setACL(t, s, []string{"127.0.0.1"}, nil)
		before := hits.Load()
		req, err := http.NewRequest(http.MethodPost, be.URL+"/v1/x?keep=query", strings.NewReader("request-body"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-ACL-Test", "keep-me")
		resp, err := ingressClient(t, s.feURL).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "ok" || resp.Header.Get("X-ACL-Response") != "keep-response" {
			t.Fatalf("whitelisted target should be forwarded, got %d %q", resp.StatusCode, body)
		}
		if hits.Load() != before+1 {
			t.Fatalf("whitelisted request should reach backend once, hits=%d before=%d", hits.Load(), before)
		}
		select {
		case got := <-observed:
			if got.method != http.MethodPost || got.path != "/v1/x?keep=query" || got.header != "keep-me" || got.body != "request-body" {
				t.Fatalf("allowed request was changed in transit: %+v", got)
			}
		case <-time.After(time.Second):
			t.Fatal("backend did not report the allowed request")
		}
		<-audit // 丢弃放行请求的审计记录，后面只校验拒绝事件。
	})

	t.Run("outside whitelist is rejected", func(t *testing.T) {
		setACL(t, s, []string{"*.openai.com"}, nil) // 不含 127.0.0.1
		before := hits.Load()
		assertACLBlocked(t, be.URL+"/v1/x")
		if hits.Load() != before {
			t.Fatalf("outside-whitelist request reached backend, hits=%d before=%d", hits.Load(), before)
		}
	})

	t.Run("blacklisted target is rejected", func(t *testing.T) {
		setACL(t, s, nil, []string{"127.0.0.1"})
		before := hits.Load()
		assertACLBlocked(t, be.URL+"/v1/x")
		if hits.Load() != before {
			t.Fatalf("blacklisted request reached backend, hits=%d before=%d", hits.Load(), before)
		}
	})

	t.Run("blacklist takes precedence", func(t *testing.T) {
		setACL(t, s, []string{"127.0.0.1"}, []string{"127.0.0.1"})
		before := hits.Load()
		assertACLBlocked(t, be.URL+"/v1/x")
		if hits.Load() != before {
			t.Fatalf("blacklist-overridden request reached backend, hits=%d before=%d", hits.Load(), before)
		}
	})
}

func TestACLConnectPath(t *testing.T) {
	s := newStack(t)
	connect := func(t *testing.T, target string) (int, string) {
		t.Helper()
		fe, err := urlParseFE(s.feURL)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.DialTimeout("tcp", fe, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := fmt.Fprintf(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		br := bufio.NewReader(conn)
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusOK {
			return resp.StatusCode, ""
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		return resp.StatusCode, string(body)
	}

	t.Run("allowed target establishes CONNECT", func(t *testing.T) {
		setACL(t, s, []string{"127.0.0.1"}, nil)
		st, body := connect(t, "127.0.0.1:1")
		if st != http.StatusOK || body != "" {
			t.Fatalf("allowed CONNECT should return 200, got %d %q", st, body)
		}
	})

	t.Run("outside whitelist is rejected before tunnel", func(t *testing.T) {
		setACL(t, s, []string{"api.openai.com"}, nil)
		st, body := connect(t, "127.0.0.1:1")
		if st != http.StatusForbidden || !strings.Contains(body, `"acl_forbidden"`) {
			t.Fatalf("CONNECT outside whitelist should return 403 acl_forbidden, got %d %q", st, body)
		}
	})

	t.Run("blacklist is rejected before tunnel", func(t *testing.T) {
		setACL(t, s, nil, []string{"127.0.0.1"})
		st, body := connect(t, "127.0.0.1:1")
		if st != http.StatusForbidden || !strings.Contains(body, `"acl_forbidden"`) {
			t.Fatalf("blacklisted CONNECT should return 403 acl_forbidden, got %d %q", st, body)
		}
	})

	t.Run("blacklist takes precedence over whitelist", func(t *testing.T) {
		setACL(t, s, []string{"127.0.0.1"}, []string{"127.0.0.1"})
		st, body := connect(t, "127.0.0.1:1")
		if st != http.StatusForbidden || !strings.Contains(body, `"acl_forbidden"`) {
			t.Fatalf("blacklisted CONNECT should return 403 even when allowlisted, got %d %q", st, body)
		}
	})
}

func TestACLAllowedConnectRouting(t *testing.T) {
	s := newStack(t)

	listenEcho := func(t *testing.T) string {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					_, _ = io.Copy(c, c)
					_ = c.Close()
				}(conn)
			}
		}()
		return ln.Addr().String()
	}

	openConnect := func(t *testing.T, target string) (*bufio.Reader, net.Conn) {
		t.Helper()
		fe, err := urlParseFE(s.feURL)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.DialTimeout("tcp", fe, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		br := bufio.NewReader(conn)
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			resp.Body.Close()
			conn.Close()
			t.Fatalf("allowed CONNECT status=%d; body=%q", resp.StatusCode, body)
		}
		conn.SetReadDeadline(time.Time{})
		return br, conn
	}

	t.Run("allowed non-TLS target uses blind tunnel", func(t *testing.T) {
		target := listenEcho(t)
		setACL(t, s, []string{"127.0.0.1"}, nil)
		br, conn := openConnect(t, target)
		defer conn.Close()

		probe := []byte("ping")
		if _, err := conn.Write(probe); err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		got := make([]byte, len(probe))
		if _, err := io.ReadFull(br, got); err != nil || string(got) != string(probe) {
			t.Fatalf("allowed non-TLS CONNECT should use blind tunnel, got %q err=%v", got, err)
		}
	})

	t.Run("allowed TLS-shaped target uses MITM", func(t *testing.T) {
		target := listenEcho(t)
		setACL(t, s, []string{"127.0.0.1"}, nil)
		br, conn := openConnect(t, target)
		defer conn.Close()

		probe := []byte{0x16, 0x03, 0x01, 0x00, 0x00}
		if _, err := conn.Write(probe); err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		got := make([]byte, len(probe))
		n, err := io.ReadFull(br, got)
		if err == nil && n == len(probe) && got[0] == 0x16 {
			t.Fatal("allowed TLS-shaped CONNECT was passed through as a blind tunnel")
		}
	})
}

func TestACLBlockedConnectIsNotDialed(t *testing.T) {
	s := newStack(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan struct{}, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			accepted <- struct{}{}
			_ = conn.Close()
		}
	}()

	snap := s.holder.Current()
	snap.ACLBlacklist = []string{"127.0.0.1"}
	s.holder.Set(snap)

	fe, err := urlParseFE(s.feURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", fe, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", ln.Addr(), ln.Addr()); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("blocked CONNECT status=%d want 403; body=%q", resp.StatusCode, body)
	}
	select {
	case <-accepted:
		t.Fatal("blocked CONNECT dialed the target")
	case <-time.After(200 * time.Millisecond):
	}
}

// urlParseFE 提取前端地址的 host:port。
func urlParseFE(feURL string) (string, error) {
	i := strings.Index(feURL, "://")
	if i < 0 {
		return "", fmt.Errorf("invalid frontend address %q", feURL)
	}
	return feURL[i+3:], nil
}
