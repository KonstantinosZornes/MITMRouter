package tlsreload

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
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// writePair 在 dir 下生成一套自签证书/私钥（org 区分指纹），返回路径。
func writePair(t *testing.T, dir, org string) (certPath, keyPath string) {
	t.Helper()
	cp, kp := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	writePairAt(t, cp, kp, org)
	return cp, kp
}

// writePairAt 把自签证书/私钥写到指定路径（模拟 certbot 续期原地覆盖）。
func writePairAt(t *testing.T, certPath, keyPath, org string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{Organization: []string{org}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0600); err != nil {
		t.Fatal(err)
	}
}

// orgOf 解析证书 DER 返回 Subject.Organization 首项（作为指纹比对依据）。
func orgOf(t *testing.T, c *tls.Certificate) string {
	t.Helper()
	if c == nil || len(c.Certificate) == 0 {
		t.Fatal("no certificate")
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.Subject.Organization[0]
}

func TestPairEnabled(t *testing.T) {
	if (Pair{CertPath: "a", KeyPath: ""}).Enabled() {
		t.Error("half-configured pair must not enable TLS")
	}
	if (Pair{}).Enabled() {
		t.Error("empty pair must not enable TLS")
	}
	if !(Pair{CertPath: "a", KeyPath: "b"}).Enabled() {
		t.Error("full pair must enable TLS")
	}
}

func TestNewLoadAndConfig(t *testing.T) {
	dir := t.TempDir()
	cp, kp := writePair(t, dir, "v1")
	l, err := New(Pair{CertPath: cp, KeyPath: kp}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cert := l.Certificate()
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatal("certificate must be loaded at construction")
	}
	if orgOf(t, cert) != "v1" {
		t.Errorf("wrong certificate loaded: %s", orgOf(t, cert))
	}

	// Config 返回的 GetCertificate 必须取到当前证书
	hello := &tls.ClientHelloInfo{}
	got, err := l.Config().GetCertificate(hello)
	if err != nil || got != cert {
		t.Fatalf("GetCertificate: %v", err)
	}
}

func TestReloadIfChangedSwapsCertificate(t *testing.T) {
	dir := t.TempDir()
	cp, kp := writePair(t, dir, "v1")
	l, err := New(Pair{CertPath: cp, KeyPath: kp}, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldOrg := orgOf(t, l.Certificate())

	// 无变化 → no-op
	if err := l.ReloadIfChanged(); err != nil {
		t.Fatalf("no-op reload must succeed: %v", err)
	}
	if orgOf(t, l.Certificate()) != oldOrg {
		t.Fatal("unchanged files must not swap certificate")
	}

	// 续期：覆盖写同一组路径（mtime 变化）
	time.Sleep(10 * time.Millisecond) // 保证 mtime 可分辨
	writePairAt(t, cp, kp, "v2")      // 原地覆盖，模拟 certbot renew
	if err := l.ReloadIfChanged(); err != nil {
		t.Fatalf("reload after renewal: %v", err)
	}
	if orgOf(t, l.Certificate()) == oldOrg {
		t.Fatal("changed files must swap certificate")
	}
}

func TestReloadKeepsOldOnBrokenFile(t *testing.T) {
	dir := t.TempDir()
	cp, kp := writePair(t, dir, "good")
	l, err := New(Pair{CertPath: cp, KeyPath: kp}, nil)
	if err != nil {
		t.Fatal(err)
	}
	goodOrg := orgOf(t, l.Certificate())

	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(cp, []byte("garbage"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := l.ReloadIfChanged(); err == nil {
		t.Fatal("broken cert file must report error")
	}
	if orgOf(t, l.Certificate()) != goodOrg {
		t.Fatal("failed reload must keep previous certificate")
	}
	// GetCertificate 仍可用旧证书服务新握手
	if _, err := l.Config().GetCertificate(&tls.ClientHelloInfo{}); err != nil {
		t.Fatalf("old certificate must remain servable: %v", err)
	}
}

func TestDisabledPairIsNoop(t *testing.T) {
	l, err := New(Pair{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if l.Certificate() != nil {
		t.Fatal("disabled pair must not load any certificate")
	}
	if err := l.ReloadIfChanged(); err != nil {
		t.Fatalf("reload on disabled pair must be no-op: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run must return after ctx cancel")
	}
}

func TestConcurrentReloadRace(t *testing.T) {
	dir := t.TempDir()
	cp, kp := writePair(t, dir, "race")
	l, err := New(Pair{CertPath: cp, KeyPath: kp}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	go func() { // 并发重载与并发握手读取
		for !stop.Load() {
			_ = l.ReloadIfChanged()
		}
	}()
	for i := 0; i < 200; i++ {
		if _, err := l.Config().GetCertificate(&tls.ClientHelloInfo{}); err != nil {
			t.Fatalf("concurrent read: %v", err)
		}
	}
	stop.Store(true)
}
