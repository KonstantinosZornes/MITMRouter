// Package certca 管理自签 CA（材料持久化于 secrets 表）与按 SNI 的叶子证书签发。
package certca

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/golang-lru/v2"
	"golang.org/x/sync/singleflight"

	"mitmrouter/internal/store"
)

// 叶子证书缓存条目：携带过期时间，避免"第 7 天起全量握手失败"的时间炸弹。
type cachedLeaf struct {
	cert *tls.Certificate
	exp  time.Time
}

const (
	leafTTL       = 7 * 24 * time.Hour // 与叶子证书 NotAfter 一致
	refreshBefore = 24 * time.Hour     // 剩余不足一天即视为失效重签
)

// Authority 持有根证书与签发私钥；并发安全。
type Authority struct {
	cert  *x509.Certificate
	key   *ecdsa.PrivateKey
	cache *lru.Cache[string, cachedLeaf]
	sf    singleflight.Group
}

// Ensure 从 secrets 加载 CA；不存在则生成并写回。
// 写入顺序刻意为先 key 后 cert：中途崩溃只会留下"有钥无证"，
// 下次启动因 cert 缺失走重新生成分支整体覆盖，可自愈；
// 反之（先 cert 后 key）会留下永久无法启动的孤儿 CA。
func Ensure(ctx context.Context, st *store.Store) (*Authority, error) {
	certPEM, err := st.GetSecret(ctx, "ca_cert_pem")
	if errors.Is(err, store.ErrNotFound) {
		certPEM, keyPEM, gerr := generateCA()
		if gerr != nil {
			return nil, gerr
		}
		if err := st.SetSecret(ctx, "ca_key_pem", keyPEM); err != nil {
			return nil, err
		}
		if err := st.SetSecret(ctx, "ca_cert_pem", certPEM); err != nil {
			return nil, err
		}
		return New(certPEM, keyPEM)
	}
	if err != nil {
		return nil, err
	}
	keyPEM, err := st.GetSecret(ctx, "ca_key_pem")
	if err != nil {
		return nil, err
	}
	return New(certPEM, keyPEM)
}

// CertificateDER 导出根证书 DER 编码（Windows 友好的 .crt 格式）。
func (a *Authority) CertificateDER() []byte { return a.cert.Raw }

// CertificatePEM 导出根证书 PEM（管理台下载端点使用）。
func (a *Authority) CertificatePEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.cert.Raw})
}

// generateCA 生成自签 ECDSA P-256 根证书（有效期 10 年）。
func generateCA() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 63))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "MITMRouter Root CA", Organization: []string{"MITMRouter"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// New 从 PEM 构造权威实例。
func New(certPEM, keyPEM []byte) (*Authority, error) {
	cb, _ := pem.Decode(certPEM)
	if cb == nil || cb.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("ca_cert_pem decode failed")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	if !cert.IsCA {
		return nil, errors.New("ca_cert_pem in secrets is not a CA certificate")
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, fmt.Errorf("ca_key_pem decode failed")
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA private key: %w", err)
	}
	cache, err := lru.New[string, cachedLeaf](4096)
	if err != nil {
		return nil, err
	}
	return &Authority{cert: cert, key: key, cache: cache}, nil
}

// LeafForHost 返回（缓存命中或现场签发的）目标域名叶子证书。
// 同域并发握手经 singleflight 合并，避免重复签发。
func (a *Authority) LeafForHost(host string) (*tls.Certificate, error) {
	hostOnly := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostOnly = h
	}
	hostOnly = strings.TrimSuffix(strings.TrimPrefix(hostOnly, "["), "]")
	if hostOnly == "" {
		hostOnly = "unknown"
	}
	// 命中且剩余有效期充足才复用；临近过期(≤24h)或已过期一律重签，
	// 避免 LRU 长期不淘汰导致"第 N 天起全量握手下发过期证书"。
	if c, ok := a.cache.Get(hostOnly); ok && time.Until(c.exp) > refreshBefore {
		return c.cert, nil
	}
	v, err, _ := a.sf.Do(hostOnly, func() (any, error) {
		// double-check：并发等方的其他请求可能已完成签发
		if c, ok := a.cache.Get(hostOnly); ok && time.Until(c.exp) > refreshBefore {
			return c, nil
		}
		cert, err := a.signLeaf(hostOnly)
		if err != nil {
			return nil, err
		}
		cl := cachedLeaf{cert: cert, exp: time.Now().Add(leafTTL)}
		a.cache.Add(hostOnly, cl) // Add 移入 flight 内，防止并发重复 Add 覆盖新鲜条目
		return cl, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(cachedLeaf).cert, nil
}

func (a *Authority) signLeaf(host string) (*tls.Certificate, error) {
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 63))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(leafTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &leafKey.PublicKey, crypto.Signer(a.key))
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, a.cert.Raw},
		PrivateKey:  leafKey,
	}, nil
}
