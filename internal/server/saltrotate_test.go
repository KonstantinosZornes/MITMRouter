package server

// Marker 动态盐值：推导合成、TLS 不可用判定与轮换触发的单元测试。

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"mitmrouter/internal/marker"
	"mitmrouter/internal/settings"
	"mitmrouter/internal/sticky"
	"mitmrouter/internal/upstream"
)

func TestUpstreamUnusable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain dial error", fmt.Errorf("dial tcp 1.2.3.4:443: connect: connection refused"), false},
		{"context canceled", context.Canceled, false},
		{"record header error", fmt.Errorf("wrapped: %w", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}), true},
		{"unknown authority", fmt.Errorf("wrapped: %w", x509.UnknownAuthorityError{}), true},
		{"hostname mismatch", fmt.Errorf("wrapped: %w", x509.HostnameError{Host: "h"}), true},
		{"cert verification", fmt.Errorf("wrapped: %w", &tls.CertificateVerificationError{}), true},
		{"tls alert", errors.New("remote error: tls: handshake failure"), true},
		{"proxyconnect", errors.New("proxyconnect tcp: EOF"), true},
		{"bare eof before response", io.EOF, true},
		{"unexpected eof mid-handshake", io.ErrUnexpectedEOF, true},
	}
	for _, c := range cases {
		if got := upstreamUnusable(c.err); got != c.want {
			t.Errorf("%s: upstreamUnusable=%v want %v (err=%v)", c.name, got, c.want, c.err)
		}
	}
}

func newSaltTestServer() *Server {
	h := settings.NewHolder(settings.DefaultSnapshot())
	s := &Server{settings: h, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), markerSalts: marker.NewSaltStore(8)}
	s.upstreams.Store(upstream.EmptyTable())
	return s
}

func TestResolveOutboundUsesRotatedSalt(t *testing.T) {
	s := newSaltTestServer()
	const mk = "sk-test-mk-0001"
	snap := s.settings.Current()

	_, acc0, _ := s.resolveOutbound(snap, mk, identity{}, "9.9.9.9", "api.example.com")
	want0 := sticky.Derive(sticky.CombineSalt(snap.Salt, 0), mk, snap.SIDLen)
	if acc0 != want0 {
		t.Fatalf("unrotated account mismatch: got %s want %s", acc0, want0)
	}

	s.markerSalts.Rotate(mk)
	_, acc1, _ := s.resolveOutbound(snap, mk, identity{}, "9.9.9.9", "api.example.com")
	if acc1 == acc0 {
		t.Fatalf("account must change after salt rotation, still %s", acc1)
	}

	// 未轮换过的其他 Marker 保持与历史行为一致（仅系统盐）
	const other = "sk-other-mk-0002"
	_, accOther, _ := s.resolveOutbound(snap, other, identity{}, "9.9.9.9", "api.example.com")
	if accOther != sticky.Derive(snap.Salt, other, snap.SIDLen) {
		t.Fatalf("unrotated Marker must derive identically to legacy scheme")
	}
}

func TestResolveOutboundRotatesNoMarkerFallbackIdentities(t *testing.T) {
	s := newSaltTestServer()
	snap := s.settings.Current()

	t.Run("mapped account identity changes after threshold", func(t *testing.T) {
		mapped := identity{key: "xai/account-42", mapped: true}
		const credential = "token-for-account-42"
		_, before, _ := s.resolveOutbound(snap, credential, mapped, "9.9.9.9", "api.example.com")
		key := requestSaltKey(snap.NoMarkerPolicy, credential, mapped, "9.9.9.9")
		for range 2 {
			s.recordUnusableIdentity(key, true, io.EOF)
		}
		_, after, _ := s.resolveOutbound(snap, credential, mapped, "9.9.9.9", "api.example.com")
		if after == before {
			t.Fatal("mapped account must use a new identity after its error threshold")
		}
	})

	t.Run("fixed default identity changes only after fallback salt rotation", func(t *testing.T) {
		snap.NoMarkerPolicy = settings.PolicyDefaultSession
		_, before, _ := s.resolveOutbound(snap, "", identity{}, "9.9.9.9", "api.example.com")
		if before != "default" {
			t.Fatalf("unrotated default fallback = %q, want default", before)
		}
		s.markerSalts.Rotate(noMarkerSaltKey(snap.NoMarkerPolicy, "9.9.9.9"))
		_, after, _ := s.resolveOutbound(snap, "", identity{}, "9.9.9.9", "api.example.com")
		if after == before {
			t.Fatal("rotated default fallback must use a new identity")
		}
	})

	t.Run("source IP identity changes independently", func(t *testing.T) {
		snap.NoMarkerPolicy = settings.PolicyClientIPSession
		_, ip1Before, _ := s.resolveOutbound(snap, "", identity{}, "1.2.3.4", "api.example.com")
		_, ip2Before, _ := s.resolveOutbound(snap, "", identity{}, "5.6.7.8", "api.example.com")
		s.markerSalts.Rotate(noMarkerSaltKey(snap.NoMarkerPolicy, "1.2.3.4"))
		_, ip1After, _ := s.resolveOutbound(snap, "", identity{}, "1.2.3.4", "api.example.com")
		_, ip2After, _ := s.resolveOutbound(snap, "", identity{}, "5.6.7.8", "api.example.com")
		if ip1After == ip1Before {
			t.Fatal("rotated source-IP fallback must use a new identity")
		}
		if ip2After != ip2Before {
			t.Fatal("one source IP must not rotate another source IP's identity")
		}
	})
}

func TestRotateOnUnusableTriggerConditions(t *testing.T) {
	s := newSaltTestServer()
	tlsErr := fmt.Errorf("read: remote error: tls: handshake failure")
	netErr := errors.New("dial tcp: connection refused")

	req := func(key string, viaUpstream bool) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://t.example.com/v1/x", nil)
		return r.Clone(context.WithValue(r.Context(), fwdMetaKey{}, &fwdMeta{marker: key, viaUpstream: viaUpstream}))
	}

	t.Run("rotates after default two eligible errors", func(t *testing.T) {
		s.rotateOnUnusable(req("K1", true), tlsErr)
		if v := s.markerSalts.Get("K1"); v != 0 {
			t.Fatalf("first failure must not rotate, got salt %d", v)
		}
		s.rotateOnUnusable(req("K1", true), tlsErr)
		if v := s.markerSalts.Get("K1"); v != 1 {
			t.Fatalf("second consecutive failure must rotate to 1, got %d", v)
		}
	})

	t.Run("no rotation without identity", func(t *testing.T) {
		s.rotateOnUnusable(req("", true), tlsErr)
		if s.markerSalts.Len() != 1 {
			t.Fatalf("no entry expected for empty key, len=%d", s.markerSalts.Len())
		}
	})

	t.Run("no rotation when direct", func(t *testing.T) {
		s.rotateOnUnusable(req("K2", false), tlsErr)
		if v := s.markerSalts.Get("K2"); v != 0 {
			t.Fatalf("direct request must not rotate, got %d", v)
		}
	})

	t.Run("non-eligible error resets streak", func(t *testing.T) {
		s.rotateOnUnusable(req("K3", true), tlsErr)
		s.rotateOnUnusable(req("K3", true), netErr)
		s.rotateOnUnusable(req("K3", true), tlsErr)
		if v := s.markerSalts.Get("K3"); v != 0 {
			t.Fatalf("a non-eligible error must break the consecutive streak, got %d", v)
		}
	})

	t.Run("each threshold crossing increments salt", func(t *testing.T) {
		for range 4 {
			s.rotateOnUnusable(req("K4", true), tlsErr)
		}
		if v := s.markerSalts.Get("K4"); v != 2 {
			t.Fatalf("four failures at threshold two should yield salt 2, got %d", v)
		}
	})
}

func TestFwdMetaContextMissing(t *testing.T) {
	s := newSaltTestServer()
	r := httptest.NewRequest(http.MethodGet, "http://t.example.com/v1/x", nil)
	s.rotateOnUnusable(r, errors.New("remote error: tls: bad certificate")) // 无 meta：不得 panic
	if s.markerSalts.Len() != 0 {
		t.Fatal("rotation must not happen without meta")
	}
}
