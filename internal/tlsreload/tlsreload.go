// Package tlsreload 提供"证书/私钥文件对"的加载与 mtime 热重载。
// 面向外部签发的证书（如 certbot/Let's Encrypt）：续期落盘后无需重启，
// 新握手自动使用新证书，存量连接不受影响；坏文件只告警不中断监听。
package tlsreload

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Interval 是 Run 的轮询周期。certbot 续期约每 60 天一次，
// 分钟级延迟完全可接受，无需文件系统 watch 依赖。
const Interval = 60 * time.Second

// Pair 是证书与私钥 PEM 文件路径对；两者都非空才视为启用 TLS。
type Pair struct {
	CertPath string
	KeyPath  string
}

// Enabled 报告该路径对是否启用 TLS。半配（只填一项）恒为未启用——
// 配置保存路径另有成对校验，此处兜底保证半配配置不会生效。
func (p Pair) Enabled() bool { return p.CertPath != "" && p.KeyPath != "" }

// Loader 持有一份可原子替换的 *tls.Certificate：
// New 时从磁盘加载一次；之后 ReloadIfChanged 在证书文件 mtime+大小变化时重载。
type Loader struct {
	pair   Pair
	logger *slog.Logger

	cur     atomic.Pointer[tls.Certificate]
	lastMod time.Time // 上次加载时证书文件的 mtime（变更探测基准）
	lastSz  int64     // 上次加载时证书文件的大小

	mu sync.Mutex // 串行化 stat/load，避免并发重载交错读
}

// New 创建 Loader 并在启用时立即从磁盘加载一次。
// 加载失败返回错误（启动期应 fail-fast）；未启用的 Pair 返回空 Loader。
func New(pair Pair, logger *slog.Logger) (*Loader, error) {
	if logger == nil {
		logger = slog.Default()
	}
	l := &Loader{pair: pair, logger: logger}
	if !pair.Enabled() {
		return l, nil
	}
	if err := l.load(); err != nil {
		return nil, err
	}
	return l, nil
}

// load 从磁盘读取证书对并记录探测基准。调用方须持有 mu（New 阶段除外）。
func (l *Loader) load() error {
	cert, err := tls.LoadX509KeyPair(l.pair.CertPath, l.pair.KeyPath)
	if err != nil {
		return err
	}
	l.cur.Store(&cert)
	if fi, err := os.Stat(l.pair.CertPath); err == nil {
		l.lastMod, l.lastSz = fi.ModTime(), fi.Size()
	}
	return nil
}

// Certificate 返回当前证书快照；未启用或尚未加载成功时为 nil。
func (l *Loader) Certificate() *tls.Certificate { return l.cur.Load() }

// Config 构造绑定本 Loader 的 TLS 服务端配置。
// 每次握手经 GetCertificate 现取指针：热重载后新握手立即用新证书。
func (l *Loader) Config() *tls.Config {
	return &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		c := l.cur.Load()
		if c == nil {
			return nil, fmt.Errorf("tlsreload: no certificate loaded")
		}
		return c, nil
	}}
}

// ReloadIfChanged 在证书文件 mtime 或大小变化时重新加载；
// 无变化或未启用时不执行任何操作。失败返回错误但保留旧证书继续服务。
//
// 以证书文件为唯一探测基准：certbot 续期时证书/私钥几乎同时落盘，
// 若分别探测两个文件会在中间态触发撕裂读；统一以证书变化为准即可。
func (l *Loader) ReloadIfChanged() error {
	if !l.pair.Enabled() {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fi, err := os.Stat(l.pair.CertPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", l.pair.CertPath, err)
	}
	if fi.ModTime().Equal(l.lastMod) && fi.Size() == l.lastSz {
		return nil
	}
	if err := l.load(); err != nil {
		return err
	}
	l.logger.Info("tls certificate reloaded", "cert", l.pair.CertPath)
	return nil
}

// Run 周期检查直到 ctx 取消；单次失败仅告警（保留旧证书继续服务）。
func (l *Loader) Run(ctx context.Context) {
	tk := time.NewTicker(Interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			if err := l.ReloadIfChanged(); err != nil {
				l.logger.Warn("tls certificate reload failed; keeping previous", "cert", l.pair.CertPath, "err", err)
			}
		}
	}
}
