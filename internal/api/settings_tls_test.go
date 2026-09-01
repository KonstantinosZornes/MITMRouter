package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// genCertPair 生成一套自签证书/私钥 PEM（org 区分指纹），返回路径。
func genCertPair(t *testing.T, dir, org string) (string, string) {
	t.Helper()
	return genCertPairValidity(t, dir, org, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
}

// genCertPairValidity 同上，但有效期窗口可指定（用于过期/未生效用例）。
func genCertPairValidity(t *testing.T, dir, org string, notBefore, notAfter time.Time) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{Organization: []string{org}},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
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
	cp := filepath.Join(dir, org+".pem")
	kp := filepath.Join(dir, org+".key")
	if err := os.WriteFile(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0600); err != nil {
		t.Fatal(err)
	}
	return cp, kp
}

func TestSettingsTLSPairAcceptedAndEchoed(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)

	dir := t.TempDir()
	cp, kp := genCertPair(t, dir, "test")

	dto := validDTO()
	dto.AdminTLSCert, dto.AdminTLSKey = cp, kp
	rec := f.do(t, "PUT", "/api/settings", cookie, dto)
	if rec.Code != 200 {
		t.Fatalf("valid TLS pair must be accepted: %d %s", rec.Code, rec.Body.String())
	}

	get := f.do(t, "GET", "/api/settings", cookie, nil)
	if !strings.Contains(get.Body.String(), `"admin_tls_cert":"`+cp+`"`) {
		t.Errorf("admin_tls_cert must echo verbatim: %s", get.Body.String())
	}

	snap := f.holder.Current()
	if snap.AdminTLSCert != cp || snap.AdminTLSKey != kp {
		t.Errorf("snapshot tls fields not applied: %q %q", snap.AdminTLSCert, snap.AdminTLSKey)
	}

	// 同值重存：不触发重启提示
	dto2 := validDTO()
	dto2.AdminTLSCert, dto2.AdminTLSKey = cp, kp
	rec2 := f.do(t, "PUT", "/api/settings", cookie, dto2)
	out := decode[struct {
		RestartRequired bool `json:"restart_required"`
	}](t, rec2)
	if out.RestartRequired {
		t.Error("unchanged TLS paths must not require restart")
	}
}

func TestSettingsBrokenTLSPairRejected(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)

	dir := t.TempDir()
	cp, _ := genCertPair(t, dir, "good")
	badKey := filepath.Join(dir, "garbage.key")
	if err := os.WriteFile(badKey, []byte("not a key"), 0600); err != nil {
		t.Fatal(err)
	}

	dto := validDTO()
	dto.ListenTLSCert = cp
	dto.ListenTLSKey = badKey
	rec := f.do(t, "PUT", "/api/settings", cookie, dto)
	if rec.Code != 400 {
		t.Fatalf("unparseable pair must be rejected: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_tls_pair") {
		t.Errorf("error code must be invalid_tls_pair: %s", rec.Body.String())
	}
}

// 有效期异常属于时间性状态：不拒绝保存（避免到期后设置页锁死），
// 但必须在响应里带 warnings 提醒。
func TestSettingsExpiredCertWarnsButSaves(t *testing.T) {
	f := newFixture(t)
	cookie := f.login(t)
	dir := t.TempDir()

	cases := []struct {
		name      string
		notBefore time.Time
		notAfter  time.Time
		wantSub   string
	}{
		{"expired", time.Now().Add(-48 * time.Hour), time.Now().Add(-24 * time.Hour), "EXPIRED"},
		{"not yet valid", time.Now().Add(24 * time.Hour), time.Now().Add(48 * time.Hour), "not yet valid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cp, kp := genCertPairValidity(t, dir, c.name, c.notBefore, c.notAfter)
			dto := validDTO()
			dto.AdminTLSCert, dto.AdminTLSKey = cp, kp
			rec := f.do(t, "PUT", "/api/settings", cookie, dto)
			if rec.Code != 200 {
				t.Fatalf("validity-warning pair must still save: %d %s", rec.Code, rec.Body.String())
			}
			out := decode[struct {
				Warnings []string `json:"warnings"`
			}](t, rec)
			if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "admin tls") || !strings.Contains(out.Warnings[0], c.wantSub) {
				t.Errorf("warnings = %v, want one admin tls warning containing %q", out.Warnings, c.wantSub)
			}
			// 快照已应用（保存生效）
			if snap := f.holder.Current(); snap.AdminTLSCert != cp {
				t.Errorf("snapshot admin_tls_cert = %q, want %q", snap.AdminTLSCert, cp)
			}
		})
	}
}
