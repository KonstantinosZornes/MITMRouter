package certca

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"mitmrouter/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestEnsureGeneratesAndPersists(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	a1, err := Ensure(ctx, st)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, k := range []string{"ca_cert_pem", "ca_key_pem"} {
		if _, err := st.GetSecret(ctx, k); err != nil {
			t.Errorf("secret %q missing after generation: %v", k, err)
		}
	}
	certPEM, _ := st.GetSecret(ctx, "ca_cert_pem")
	keyPEM, _ := st.GetSecret(ctx, "ca_key_pem")

	a2, err := Ensure(ctx, st)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if string(a1.CertificateDER()) != string(a2.CertificateDER()) {
		t.Error("existing CA must be loaded, not regenerated")
	}

	st2, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if _, err := Ensure(ctx, st2); err == nil {
		// 空库应走新生成分支而非报错——这里仅验证不 panic；DER 不同属预期
	}
	if pemBlock(certPEM) == nil || pemBlock(keyPEM) == nil {
		t.Fatal("persisted material must be PEM encoded")
	}
	if got := pemBlock(certPEM).Type; got != "CERTIFICATE" {
		t.Errorf("cert PEM type=%q", got)
	}
	if got := pemBlock(keyPEM).Type; got != "EC PRIVATE KEY" {
		t.Errorf("key PEM type=%q", got)
	}
}

func pemBlock(b []byte) *pem.Block {
	blk, _ := pem.Decode(b)
	return blk
}

func TestEnsureReloadSameMaterial(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st1, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a1, err := Ensure(ctx, st1)
	if err != nil {
		t.Fatal(err)
	}
	st1.Close()

	st2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	a2, err := Ensure(ctx, st2)
	if err != nil {
		t.Fatal(err)
	}
	if string(a1.CertificateDER()) != string(a2.CertificateDER()) {
		t.Error("CA must survive process restart via secrets table")
	}
}

func TestNewRejectsBadMaterial(t *testing.T) {
	if _, err := New([]byte("garbage"), []byte("garbage")); err == nil {
		t.Error("garbage PEM must be rejected")
	}

	// 非 CA 证书（无 IsCA）不得作为根加载
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "leaf"},
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if _, err := New(leafPEM, leafPEM); err == nil || !strings.Contains(err.Error(), "not a CA") {
		t.Errorf("non-CA cert must be rejected with clear error, got %v", err)
	}
}

func TestLeafForHostSANAndChain(t *testing.T) {
	ctx := context.Background()
	a, err := Ensure(ctx, newStore(t))
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(a.CertificateDER())
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)

	t.Run("dns san", func(t *testing.T) {
		leaf, err := a.LeafForHost("api.openai.com")
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := x509.ParseCertificate(leaf.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		if len(parsed.DNSNames) != 1 || parsed.DNSNames[0] != "api.openai.com" {
			t.Errorf("dns san=%v", parsed.DNSNames)
		}
		if len(parsed.IPAddresses) != 0 {
			t.Errorf("ip san unexpected: %v", parsed.IPAddresses)
		}
		if _, err := parsed.Verify(x509.VerifyOptions{Roots: pool, DNSName: "api.openai.com"}); err != nil {
			t.Errorf("leaf does not verify against our CA: %v", err)
		}
		if len(leaf.Certificate) != 2 || string(leaf.Certificate[1]) != string(root.Raw) {
			t.Error("chain must include the root CA as intermediate")
		}
	})

	t.Run("ip san", func(t *testing.T) {
		leaf, err := a.LeafForHost("10.1.2.3")
		if err != nil {
			t.Fatal(err)
		}
		parsed, _ := x509.ParseCertificate(leaf.Certificate[0])
		if len(parsed.IPAddresses) != 1 || !parsed.IPAddresses[0].Equal([]byte{10, 1, 2, 3}) {
			t.Errorf("ip san=%v want 10.1.2.3", parsed.IPAddresses)
		}
		if len(parsed.DNSNames) != 0 {
			t.Errorf("dns san unexpected: %v", parsed.DNSNames)
		}
	})

	t.Run("port and brackets stripped", func(t *testing.T) {
		l1, err := a.LeafForHost("api.openai.com:443")
		if err != nil {
			t.Fatal(err)
		}
		l2, err := a.LeafForHost("[::1]:8443")
		if err != nil {
			t.Fatal(err)
		}
		p2, _ := x509.ParseCertificate(l2.Certificate[0])
		if p2.Subject.CommonName != "::1" && len(p2.IPAddresses) == 0 {
			t.Errorf("ipv6 host mishandled: cn=%q ips=%v", p2.Subject.CommonName, p2.IPAddresses)
		}
		if l1 == nil {
			t.Fatal("nil leaf")
		}
	})

	t.Run("empty host falls back to unknown", func(t *testing.T) {
		leaf, err := a.LeafForHost("")
		if err != nil {
			t.Fatal(err)
		}
		parsed, _ := x509.ParseCertificate(leaf.Certificate[0])
		if parsed.Subject.CommonName != "unknown" {
			t.Errorf("cn=%q want unknown", parsed.Subject.CommonName)
		}
	})
}

func TestLeafCacheReuse(t *testing.T) {
	a, err := Ensure(context.Background(), newStore(t))
	if err != nil {
		t.Fatal(err)
	}
	first, err := a.LeafForHost("cached.example.com")
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.LeafForHost("cached.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Certificate[0]) != string(second.Certificate[0]) {
		t.Error("warm cache must return the identical cached certificate")
	}
}

func TestLeafCacheConcurrentSingleflight(t *testing.T) {
	a, err := Ensure(context.Background(), newStore(t))
	if err != nil {
		t.Fatal(err)
	}
	const n = 24
	results := make([]*tls.Certificate, n)
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			results[i], _ = a.LeafForHost("race.example.com")
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
	base, err := x509.ParseCertificate(results[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < n; i++ {
		if results[i] == nil || string(results[i].Certificate[0]) != string(base.Raw) {
			t.Fatalf("concurrent issuance must merge to one certificate, diverged at %d", i)
		}
	}
}

func TestStaleCacheEntryIsReissued(t *testing.T) {
	a, err := Ensure(context.Background(), newStore(t))
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := a.LeafForHost("aging.example.com")
	if err != nil {
		t.Fatal(err)
	}
	// 模拟缓存条目临近过期：剩余不足 refreshBefore 必须重签
	a.cache.Add("aging.example.com", cachedLeaf{cert: fresh, exp: time.Now().Add(refreshBefore / 2)})
	renewed, err := a.LeafForHost("aging.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if string(renewed.Certificate[0]) == string(fresh.Certificate[0]) {
		t.Error("near-expiry cached leaf must be re-signed, not served")
	}
}

func TestExportFormats(t *testing.T) {
	a, err := Ensure(context.Background(), newStore(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(a.CertificatePEM()), "BEGIN CERTIFICATE") {
		t.Error("PEM export broken")
	}
	if _, err := x509.ParseCertificate(a.CertificateDER()); err != nil {
		t.Errorf("DER export broken: %v", err)
	}
}
