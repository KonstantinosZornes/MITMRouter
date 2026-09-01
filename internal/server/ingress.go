// Package server 实现本地 MITM 流量路由核心：
// 入站分流（路由面 vs 管理台）、Basic 认证、CONNECT 劫持与 TLS 终结、
// 盲隧道、经上游出口（或直连）的请求转发、审计通道投递。
package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"

	"mitmrouter/internal/acctegress"
	"mitmrouter/internal/acctmap"
	"mitmrouter/internal/acl"
	"mitmrouter/internal/certca"
	identityresolver "mitmrouter/internal/identity"
	"mitmrouter/internal/marker"
	"mitmrouter/internal/metrics"
	"mitmrouter/internal/reqid"
	"mitmrouter/internal/settings"
	"mitmrouter/internal/sticky"
	"mitmrouter/internal/store"
	"mitmrouter/internal/trace"
	"mitmrouter/internal/upstream"
)

// ctxKey 携带每请求解析出的上游出口 URL（nil=直连）。
type ctxKey struct{}

// blockPrivateDialKey 只存在于必须拒绝私网目标的直连出站请求中。
// 因此已配置的上游出口端点仍可以位于内网。
type blockPrivateDialKey struct{}

type publicTargetResolutionKey struct{}

type publicTargetResolution struct {
	host string
	ips  []netip.Addr
	err  error
}

// traceKey 在请求上下文中携带逐请求的流式追踪句柄。
type traceKey struct{}

// Server 是路由面主服务。
type Server struct {
	settings    *settings.Holder               // 设置快照
	ca          *certca.Authority              // CA 材料来自 secrets 表
	upstreams   atomic.Pointer[upstream.Table] // 启用条目快照（可热替换）
	markerSalts *marker.SaltStore              // 与 Marker 关联的动态盐值（上游TLS不可用时轮换）
	audit       chan<- store.LogEntry          // 审计异步批量写入
	dropped     atomic.Uint64                  // 审计队列满丢弃计数
	logger      *slog.Logger
	transport   *http.Transport // 共享出站传输层；按请求上下文选择上游
	trace       *trace.Writer   // nil=关闭；仅由启动参数装配

	saltSt   *store.Store         // 非 nil 时启用盐值持久化（AttachMarkerSaltStore 装配）
	saltCh   chan markerSaltEvent // 轮换事件异步落库队列
	saltDone chan struct{}        // 写入协程退出信号

	acctMap  *acctmap.Registry          // 账号映射注册表（可 nil：未装配时全部走 v3 公式）
	resolver *identityresolver.Resolver // 通用与 URL body 身份解析

	acctEgress atomic.Pointer[acctegress.Table] // 账户↔出站绑定快照（docs/011）

	admin http.Handler // 管理台 REST（/api/* 与 /metrics）
	ui    http.Handler // Web UI（/ui*）

	tunWGs sync.WaitGroup // 活动隧道计数（优雅退出等待）
}

// markerSaltEvent 是一次待落库的盐值轮换。
type markerSaltEvent struct {
	fp   string // marker.Fingerprint(Marker)，与 LRU 键同格式
	salt int64
}

// WaitTunnels 等待所有被劫持的隧道结束（上限 max），超时返回 false。
func (s *Server) WaitTunnels(max time.Duration) bool {
	done := make(chan struct{})
	go func() { s.tunWGs.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(max):
		return false
	}
}

// SwapUpstreams 原子替换上游表快照（管理台写入路径调用）。
func (s *Server) SwapUpstreams(t *upstream.Table) { s.upstreams.Store(t) }

// SwapAcctEgress 原子替换账户↔出站绑定快照（main 引导与管理台写入路径调用）。
func (s *Server) SwapAcctEgress(t *acctegress.Table) { s.acctEgress.Store(t) }

// AttachAcctMap 装配账号映射注册表（main 阶段调用；API/拉取器共享同一实例）。
func (s *Server) AttachAcctMap(reg *acctmap.Registry) { s.acctMap = reg }

// AttachTrace 启用明文请求/响应追踪。传入 nil 表示禁用追踪。
// 追踪写入器由调用方持有，其生命周期必须长于所有请求。
func (s *Server) AttachTrace(w *trace.Writer) { s.trace = w }

// SetAdmin 挂载管理面处理器。
func (s *Server) SetAdmin(admin, ui http.Handler) {
	s.admin = admin
	if ui == nil {
		ui = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, "<!doctype html><meta charset=utf-8><title>MITMRouter</title><p>Web UI is not built: cd web && npm install && npm run build")
		})
	}
	s.ui = ui
}

// New 装配服务。
func New(h *settings.Holder, ca *certca.Authority, ups *upstream.Table,
	audit chan<- store.LogEntry, logger *slog.Logger) *Server {

	s := &Server{
		settings: h, ca: ca, audit: audit, logger: logger,
		markerSalts: marker.NewSaltStore(marker.DefaultCapacity),
		resolver:    identityresolver.New(),
	}
	s.upstreams.Store(ups)
	s.acctEgress.Store(acctegress.EmptyTable())

	directDialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy: func(r *http.Request) (*url.URL, error) {
			if p, ok := r.Context().Value(ctxKey{}).(*url.URL); ok && p != nil {
				return p, nil
			}
			return nil, nil // 直连
		},
		// CONNECT 的非 200 不会进入普通响应路径；在这里记录状态码，
		// 排障时可区分认证/配额拒绝与建隧后的目标站失败。绝不记录 URL
		// （其中可能含上游凭据）。
		OnProxyConnectResponse: func(ctx context.Context, _ *url.URL, req *http.Request, resp *http.Response) error {
			m, _ := ctx.Value(fwdMetaKey{}).(*fwdMeta)
			logger.Log(ctx, slog.LevelDebug, "upstream CONNECT response",
				"target", req.Host,
				"upstream", upstreamName(m),
				"status", resp.StatusCode,
				"status_text", resp.Status,
			)
			return nil
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if ctx.Value(blockPrivateDialKey{}) != nil {
				return dialPublicTarget(ctx, directDialer, network, address)
			}
			return directDialer.DialContext(ctx, network, address)
		},
		ForceAttemptHTTP2: true,
		// The proxy must relay the target's bytes and headers exactly. Go's
		// default transport otherwise adds Accept-Encoding: gzip and transparently
		// decompresses the response, changing the external protocol semantics.
		DisableCompression:  true,
		MaxIdleConns:        0,   // 不限总空闲连接
		MaxIdleConnsPerHost: 256, // 默认仅2，高并发到同主机时避免连接抖动
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		// ResponseHeaderTimeout 不设：LLM 长推理响应无上限
	}
	s.transport = tr
	return s
}

// Handler 返回路由面入口：只承载 CONNECT 与绝对式明文请求；
// origin-form（浏览器直敲端口）一律返回静态提示页——管理面已硬拆至
// 独立监听（AdminHandler），接入端口不再暴露任何 /ui、/api、/metrics 能力。
func (s *Server) Handler() http.Handler { return s }

// AdminHandler 返回管理面入口：只承载 origin-form 的 /ui、/api/*、/metrics；
// 隧道类流量（CONNECT / 绝对式）到此一律 404 拒绝——两端口互不越界，
// 管理台监听即使绑到非回环地址也不会被当成开放入口滥用。
func (s *Server) AdminHandler() http.Handler { return adminPlane{s} }

type adminPlane struct{ s *Server }

func (h adminPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = withInternalRequestID(r)
	if r.Method == http.MethodConnect || r.URL.IsAbs() {
		writeJSONError(w, http.StatusNotFound, "admin_no_ingress",
			"this is the admin listener; point clients at the ingress port")
		return
	}
	h.s.serveAdminPlane(w, r)
}

// ServeHTTP 路由面分流：
//   - CONNECT     → 接入隧道（认证→劫持→TLS终结/盲隧道）
//   - 绝对式请求   → 按 HTTP 转发语义路由到出口
//   - origin-form → 静态提示页（不含任何动态信息，接入端口可能暴露公网）
//
// withInternalRequestID 在公开请求边界创建一个仅供服务器使用的关联 ID。
// 该 ID 仅存储在上下文中，用于日志、追踪和审计记录。
func withInternalRequestID(r *http.Request) *http.Request {
	ctx, _ := reqid.Ensure(r.Context())
	return r.WithContext(ctx)
}

// withFreshInternalRequestID 即使 TLS 隧道的上下文已携带 CONNECT 请求 ID，
// 仍会分配一个新的、仅供服务器使用的 ID。每个解密后的 HTTP/1.1 请求或 HTTP/2
// 流都需要独立的审计和追踪关联 ID。
func withFreshInternalRequestID(r *http.Request) *http.Request {
	return r.WithContext(reqid.With(r.Context(), reqid.New()))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = withInternalRequestID(r)
	switch {
	case r.Method == http.MethodConnect:
		s.handleConnect(w, r)
	case r.URL.IsAbs():
		if !s.ingressAuthOK(w, r) {
			return
		}
		s.forward(w, r, r.URL.Scheme, r.URL.Host)
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, ingressPortNotice)
	}
}

// ingressPortNotice 是浏览器访问接入端口时的静态提示页。
// 刻意不回显管理台地址：接入端口可能暴露在不可信网络，减少拓扑泄露。
const ingressPortNotice = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<title>MITMRouter</title></head>
<body style="font-family:system-ui;max-width:40rem;margin:4rem auto">
<h2>MITMRouter Ingress Port</h2>
<p>This is the HTTP traffic ingress port. Configure the client's network egress to use this address.</p>
<p>The Web admin console runs on a separate listening address (see <code>admin_listen</code> in the process startup log).</p>
<p>Connectivity example: <code>curl -x http://127.0.0.1:55666 https://api.ipify.org</code></p>
</body></html>`

// ---------- 管理面占位（M4 实现 REST 与 SPA） ----------

const uiPlaceholder = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<title>MITMRouter</title></head>
<body style="font-family:system-ui;max-width:40rem;margin:4rem auto">
<h2>MITMRouter Running</h2>
<p>The Web admin console will be available in the M4 milestone (placeholder page for now).</p>
<p>Connectivity example: <code>curl -x http://127.0.0.1:55666 https://api.ipify.org</code></p>
</body></html>`

func (s *Server) serveAdminPlane(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/ui" || strings.HasPrefix(p, "/ui/"):
		// SPA：/ui/ 前缀剥除后交给文件服务器，未命中回退 index.html（hash 路由下基本不触发）
		if s.ui != nil {
			http.StripPrefix("/ui", s.ui).ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, uiPlaceholder)
	case p == "/api" || strings.HasPrefix(p, "/api/") || p == "/metrics":
		if s.admin != nil {
			s.admin.ServeHTTP(w, r)
			return
		}
		writeJSONError(w, http.StatusNotFound, "not_implemented", "admin API not mounted")
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, uiPlaceholder)
	}
}

func writeJSONError(w http.ResponseWriter, code int, errCode, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": errCode, "message": msg}})
}

// ---------- 入站 Basic 认证 ----------

func (s *Server) ingressAuthOK(w http.ResponseWriter, r *http.Request) bool {
	expected := s.settings.Current().ListenAuth
	if expected == "" {
		return true
	}
	const prefix = "Basic "
	auth := r.Header.Get("Proxy-Authorization")
	if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		s.logger.Log(r.Context(), slog.LevelDebug, "ingress authentication rejected", "failure_stage", "ingress_auth", "reason", "missing_or_nonbasic")
		s.write407(w)
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(auth[len(prefix):])
	if err != nil || subtle.ConstantTimeCompare(raw, []byte(expected)) != 1 {
		s.logger.Log(r.Context(), slog.LevelDebug, "ingress authentication rejected", "failure_stage", "ingress_auth", "reason", "invalid_credentials")
		s.write407(w)
		return false
	}
	return true
}

func (s *Server) write407(w http.ResponseWriter) {
	metrics.Default.Inc("ingress_auth_failures_total", "Ingress auth failures", nil)
	w.Header().Set("Proxy-Authenticate", `Basic realm="sticky-mitm"`)
	http.Error(w, "Proxy Authentication Required", http.StatusProxyAuthRequired)
}

// recordBlockedTarget 写入一条仅含元数据的本地拒绝审计记录。
// 请求在送往目标前已经被拒绝，因此不记录 Marker、请求体或目标响应；
// 状态 0 表示这是本地安全决策，不是目标返回的 HTTP 状态。
func (s *Server) recordBlockedTarget(ctx context.Context, method, target, path, reason string) {
	entry := store.LogEntry{
		Ts:            time.Now().UnixMilli(),
		ReqID:         reqid.From(ctx),
		Method:        method,
		Host:          target,
		Path:          path,
		Status:        0,
		AccountFP:     "-",
		Upstream:      "direct",
		InternalError: reason,
	}
	select {
	case s.audit <- entry:
	default:
		if d := s.dropped.Add(1); d%100 == 1 {
			s.logger.Log(ctx, slog.LevelWarn, "audit queue full, dropping entries", "total_dropped", d)
		}
	}
}

func (s *Server) recordPrivateTargetConnectBlocked(ctx context.Context, target string) {
	s.recordBlockedTarget(ctx, http.MethodConnect, target, "", "private_target_blocked")
}

// ---------- CONNECT 隧道 ----------

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	if !s.ingressAuthOK(w, r) {
		return
	}
	target := r.Host
	snap := s.settings.Current()
	normalizedTarget := acl.NormalizeHost(target)
	if !snap.ACLAllowed(normalizedTarget) {
		s.logger.Log(r.Context(), slog.LevelWarn, "ACL CONNECT target blocked", "target", target)
		s.recordBlockedTarget(r.Context(), http.MethodConnect, target, "", "acl_blocked")
		writeJSONError(w, http.StatusForbidden, "acl_forbidden", "target is not allowed by access policy")
		return
	}
	if snap.BlockPrivateTargets {
		ips, err := publicTargetIPs(r.Context(), target)
		if err != nil {
			if errors.Is(err, errPrivateTargetBlocked) {
				s.logger.Log(r.Context(), slog.LevelWarn, "private CONNECT target blocked", "target", target)
				s.recordPrivateTargetConnectBlocked(r.Context(), target)
				writeJSONError(w, http.StatusForbidden, "private_target_forbidden", "private network targets are disabled")
			} else {
				s.logger.Log(r.Context(), slog.LevelWarn, "CONNECT target resolution failed", "target", target, "err", err)
				writeJSONError(w, http.StatusBadGateway, "bad_gateway", "upstream unavailable")
			}
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), publicTargetResolutionKey{}, publicTargetResolution{
			host: normalizedTargetHost(target), ips: ips,
		}))
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		s.logger.Log(r.Context(), slog.LevelWarn, "CONNECT hijack unsupported", "target", target)
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		s.logger.Log(r.Context(), slog.LevelDebug, "CONNECT hijack failed", "target", target, "err", err)
		return
	}
	// 先应答再握手（兼容各类客户端），随后连接归我们接管。
	if _, err := rw.Writer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		s.logger.Log(r.Context(), slog.LevelDebug, "CONNECT response write failed", "target", target, "failure_stage", "connect_response_write", "err", err)
		_ = conn.Close()
		return
	}
	if err := rw.Writer.Flush(); err != nil {
		s.logger.Log(r.Context(), slog.LevelDebug, "CONNECT response flush failed", "target", target, "failure_stage", "connect_response_flush", "err", err)
		_ = conn.Close()
		return
	}
	br := rw.Reader
	conn.SetReadDeadline(time.Now().Add(30 * time.Second)) // Peek/握手前空闲保护
	head, err := br.Peek(1)
	if err != nil || len(head) == 0 {
		s.logger.Log(r.Context(), slog.LevelDebug, "CONNECT client preface unavailable", "target", target, "err", err)
		conn.Close()
		return
	}
	bc := &bufConn{Conn: conn, r: br}
	remoteIP := remoteHost(r.RemoteAddr)
	metrics.Default.AddGauge("active_connections", "Active tunnel connections", nil, 1)
	defer metrics.Default.AddGauge("active_connections", "Active tunnel connections", nil, -1)
	s.tunWGs.Add(1)
	defer s.tunWGs.Done()
	// 入站空闲保护：首字节之后若 30s 内无法完成后续读取（慢速攻击），断开。
	// 握手完成后即清零，不影响长连接/SSE。
	// ACL 已在建立隧道前完成放行判断；对已放行的 TLS 目标按首字节选择 MITM，
	// 非 TLS 流量继续使用盲隧道，原样搬运双方字节。
	if head[0] == 0x16 && snap.ACLIntercept(normalizedTarget) {
		s.logger.Log(r.Context(), slog.LevelDebug, "CONNECT routed", "target", target, "mode", "tls_mitm")
		s.serveTLS(r.Context(), bc, target)
	} else { // 非 TLS 目标 → 盲隧道（无 Marker，走兜底身份）
		s.logger.Log(r.Context(), slog.LevelDebug, "CONNECT routed", "target", target, "mode", "blind_tunnel")
		s.blindTunnel(r.Context(), bc, target, remoteIP)
	}
}

// serveTLS 用按 SNI 签发的叶子证书终结 TLS，之后把解密流量交给 Handler。
func (s *Server) serveTLS(ctx context.Context, bc net.Conn, target string) {
	fallbackHost := remoteHost(target)
	cfg := &tls.Config{
		GetConfigForClient: func(hi *tls.ClientHelloInfo) (*tls.Config, error) {
			name := hi.ServerName
			if name == "" {
				name = fallbackHost // 客户端不带 SNI（如按 IP 直访）时回退 CONNECT 目标
			}
			leaf, err := s.ca.LeafForHost(name)
			if err != nil {
				s.logger.Log(ctx, slog.LevelError, "leaf certificate issue failed", "sni", hi.ServerName, "err", err)
				return nil, nil // 默认配置将导致握手失败——客户端必须先信任 CA
			}
			return &tls.Config{
				Certificates: []tls.Certificate{*leaf},
				NextProtos:   []string{"h2", "http/1.1"},
			}, nil
		},
	}
	tc := tls.Server(bc, cfg)
	bc.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tc.HandshakeContext(ctx); err != nil {
		s.logger.Log(ctx, slog.LevelDebug, "MITM TLS handshake failed", "target", target, "err", err)
		bc.Close()
		return
	}
	bc.SetDeadline(time.Time{}) // 解除握手期超时

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 仅复制 CONNECT 一侧的目标校验结果（含 DNS 答案）。保留内层 HTTP
		// 请求自己的 context，使每个 HTTP/2 流保有取消信号且不继承 CONNECT 请求 ID。
		r = withTunnelTargetResolution(r, ctx)
		r = withFreshInternalRequestID(r)
		// RFC 8441 扩展 CONNECT（基于 h2 的 WebSocket）尚不支持，拒绝后让客户端回落到 h1。
		if r.Method == http.MethodConnect && r.Header.Get(":protocol") != "" {
			s.logger.Log(r.Context(), slog.LevelDebug, "MITM extended CONNECT rejected",
				"failure_stage", "mitm_ws_h2_unsupported", "method", r.Method, "host", r.Host)
			writeJSONError(w, http.StatusNotImplemented, "ws_over_h2_unsupported", "use HTTP/1.1 to establish WebSocket")
			return
		}
		s.forward(w, r, "https", r.Host)
	})
	// ALPN 协商为 h2 时需显式用 http2 服务该连接
	if tc.ConnectionState().NegotiatedProtocol == "h2" {
		h2s := &http2.Server{
			IdleTimeout:     120 * time.Second,
			ReadIdleTimeout: 30 * time.Second, // 心跳探测半开连接
			PingTimeout:     15 * time.Second,
		}
		h2s.ServeConn(tc, &http2.ServeConnOpts{Handler: handler})
		return
	}
	hs := &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	if err := hs.Serve(newSingleConnListener(tc)); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
		s.logger.Log(ctx, slog.LevelWarn, "MITM HTTP/1 serving failed", "target", target, "failure_stage", "mitm_http_serve", "err", err)
	}
}

// ---------- 单连接 Listener（TLS 终结后把裸连接交给 http.Server） ----------

type singleConnListener struct {
	ch         chan net.Conn
	done       chan struct{}
	closeOnce  sync.Once
	connClosed chan struct{}
	connOnce   sync.Once
	addr       net.Addr
}

func newSingleConnListener(c net.Conn) *singleConnListener {
	l := &singleConnListener{
		ch:         make(chan net.Conn, 1),
		done:       make(chan struct{}),
		connClosed: make(chan struct{}),
		addr:       c.RemoteAddr(),
	}
	l.ch <- c
	return l
}

// wrapCloseTrack 让 Listener 感知唯一连接的关闭，从而结束 Serve。
func (l *singleConnListener) wrapCloseTrack(c net.Conn) net.Conn {
	return &closeTrackConn{Conn: c, closed: l.connClosed, once: &l.connOnce}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return l.wrapCloseTrack(c), nil
	case <-l.connClosed:
		return nil, net.ErrClosed
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *singleConnListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return nil
}

func (l *singleConnListener) Addr() net.Addr { return l.addr }

type closeTrackConn struct {
	net.Conn
	closed chan struct{}
	once   *sync.Once
}

func (c *closeTrackConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// blindTunnel 经上游（或直连）转发任意 TCP 字节流。
func (s *Server) blindTunnel(ctx context.Context, conn net.Conn, target, remoteIP string) {
	started := time.Now()
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(strings.TrimSuffix(strings.TrimPrefix(target, "["), "]"), "80")
	}
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	snap := s.settings.Current()
	decisionCtx := ctx
	if snap.BlockPrivateTargets {
		ips, err := publicTargetIPs(decisionCtx, target)
		decisionCtx = context.WithValue(decisionCtx, publicTargetResolutionKey{}, publicTargetResolution{
			host: normalizedTargetHost(target), ips: ips, err: err,
		})
	}
	saltKey := noMarkerSaltKey(snap.NoMarkerPolicy, remoteIP)
	pu, _, upstreamName, resolution, routeErr := s.resolveOutboundDetailed(decisionCtx, snap, "", identity{}, remoteIP, target)
	s.logger.Log(ctx, slog.LevelDebug, "blind tunnel route selected",
		"target", target,
		"route", routeName(pu != nil),
		"resolution_reason", resolution,
		"upstream", upstreamName,
		"marker_fp", markerFingerprint(saltKey),
	)
	if routeErr != nil {
		s.logger.Log(ctx, slog.LevelWarn, "blind tunnel route rejected",
			"target", target,
			"upstream", upstreamName,
			"resolution_reason", resolution,
			"failure_class", forwardFailureClass(routeErr),
		)
		_ = conn.Close()
		return
	}

	var up net.Conn
	var err error
	if pu == nil {
		if snap.BlockPrivateTargets {
			up, err = dialPublicTarget(decisionCtx, &net.Dialer{Timeout: 15 * time.Second}, "tcp", target)
		} else {
			up, err = net.DialTimeout("tcp", target, 15*time.Second)
		}
	} else {
		up, err = dialViaUpstream(pu, target)
	}
	if err != nil {
		stage, upstreamStatus, socksRep := tunnelFailureDetails(err, pu != nil)
		attrs := []any{
			"target", target,
			"route", routeName(pu != nil),
			"upstream", upstreamName,
			"failure_stage", stage,
			"failure_class", forwardFailureClass(err),
			"duration_ms", time.Since(started).Milliseconds(),
			"err", err,
		}
		if upstreamStatus != 0 {
			attrs = append(attrs, "upstream_status", upstreamStatus)
		}
		if socksRep != 0 {
			attrs = append(attrs, "socks_rep", socksRep)
		}
		s.logger.Log(ctx, slog.LevelWarn, "blind tunnel dial failed", attrs...)
		s.recordUnusableIdentityWithContext(ctx, saltKey, pu != nil, err)
		conn.Close()
		return
	}
	if saltKey != "" && pu != nil {
		// 上游隧道已经成功建立，说明该身份的出口链路可用。
		s.markerSalts.ClearFailures(saltKey)
	}
	s.logger.Log(ctx, slog.LevelDebug, "blind tunnel established",
		"target", target,
		"route", routeName(pu != nil),
		"upstream", upstreamName,
		"duration_ms", time.Since(started).Milliseconds(),
	)
	conn.SetDeadline(time.Time{})
	up.SetDeadline(time.Time{})
	go func() {
		n, copyErr := io.Copy(up, conn)
		// 半关闭透传：本侧 EOF 只关写方向，保留对端剩余数据可读
		if cw, ok := up.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		s.logger.Log(ctx, slog.LevelDebug, "blind tunnel client stream finished",
			"target", target,
			"route", routeName(pu != nil),
			"upstream", upstreamName,
			"duration_ms", time.Since(started).Milliseconds(),
			"bytes", n,
			"err", copyErr,
		)
	}()
	upstreamToClientBytes, upstreamToClientErr := io.Copy(conn, up)
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite() // 告知对端我方已发完
	}
	// 短暂排空对向残余数据（依赖 shutdown(SHUT_WR) 的协议需要这一步）
	up.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.Copy(io.Discard, up)
	_ = conn.Close()
	_ = up.Close()
	s.logger.Log(ctx, slog.LevelDebug, "blind tunnel upstream stream finished",
		"target", target,
		"route", routeName(pu != nil),
		"upstream", upstreamName,
		"duration_ms", time.Since(started).Milliseconds(),
		"bytes", upstreamToClientBytes,
		"err", upstreamToClientErr,
	)
}

// dialViaUpstream 支持经 http(s) CONNECT 或 socks5 上游建立到目标的连接。
func dialViaUpstream(pu *url.URL, target string) (net.Conn, error) {
	hostPort := pu.Host
	if _, _, err := net.SplitHostPort(hostPort); err != nil {
		port := "80"
		if strings.EqualFold(pu.Scheme, "https") {
			port = "443"
		}
		hostPort = net.JoinHostPort(pu.Hostname(), port)
	}
	switch strings.ToLower(pu.Scheme) {
	case "http", "https":
		pc, err := net.DialTimeout("tcp", hostPort, 15*time.Second)
		if err != nil {
			return nil, wrapTunnelDialError("upstream_tcp_dial", fmt.Errorf("dial upstream exit: %w", err))
		}
		// https 上游：先完成 TLS 包裹再发明文 CONNECT，凭据不走明文
		if strings.EqualFold(pu.Scheme, "https") {
			tc := tls.Client(pc, &tls.Config{ServerName: pu.Hostname()})
			pc.SetDeadline(time.Now().Add(15 * time.Second))
			if err := tc.HandshakeContext(context.Background()); err != nil {
				pc.Close()
				return nil, wrapTunnelDialError("upstream_tls_handshake", fmt.Errorf("https upstream handshake failed: %w", err))
			}
			pc.SetDeadline(time.Time{})
			pc = tc
		}
		pc.SetDeadline(time.Now().Add(15 * time.Second))
		req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
		if pu.User != nil {
			pass, _ := pu.User.Password()
			cred := pu.User.Username() + ":" + pass
			req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(cred)) + "\r\n"
		}
		req += "\r\n"
		if _, err := pc.Write([]byte(req)); err != nil {
			pc.Close()
			return nil, wrapTunnelDialError("upstream_connect_write", fmt.Errorf("write upstream CONNECT request: %w", err))
		}
		br := bufio.NewReader(pc)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			pc.Close()
			return nil, wrapTunnelDialError("upstream_connect_read", fmt.Errorf("read upstream CONNECT response: %w", err))
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			pc.Close()
			return nil, &tunnelDialError{
				stage:          "upstream_connect",
				upstreamStatus: resp.StatusCode,
				err:            fmt.Errorf("upstream CONNECT rejected status=%d %s", resp.StatusCode, resp.Status),
			}
		}
		pc.SetDeadline(time.Time{})
		return &bufConn{Conn: pc, r: br}, nil
	case "socks5", "socks5h":
		var user, pass string
		hasAuth := false
		if pu.User != nil {
			pass, _ = pu.User.Password()
			user, hasAuth = pu.User.Username(), true
		}
		return socksDial(hostPort, target, user, pass, hasAuth, 15*time.Second)
	default:
		return nil, fmt.Errorf("unsupported upstream scheme %q", pu.Scheme)
	}
}

// ---------- 请求转发（解密后 & 绝对式明文共用） ----------

func (s *Server) forward(w http.ResponseWriter, r *http.Request, scheme, host string) {
	r = withInternalRequestID(r)
	start := time.Now()
	snap := s.settings.Current()
	clientIP := remoteHost(r.RemoteAddr)

	// ACL 必须在身份解析、DNS 解析和出站选择之前执行。拒绝目标只收到本地
	// 403，不会产生任何外部请求；放行目标沿用原有的透明转发路径。
	normalizedTarget := acl.NormalizeHost(host)
	if !snap.ACLAllowed(normalizedTarget) {
		s.logger.Log(r.Context(), slog.LevelWarn, "ACL target blocked",
			"method", r.Method, "host", host, "path", r.URL.Path)
		s.recordBlockedTarget(r.Context(), r.Method, host, r.URL.Path, "acl_blocked")
		writeJSONError(w, http.StatusForbidden, "acl_forbidden", "target is not allowed by access policy")
		return
	}
	intercept := snap.ACLIntercept(normalizedTarget)

	mk := ""
	ident := identity{}
	mappedAccount := "" // 仅 acct_map 命中时记录真实账号；不记录 token
	forwardBody := r.Body
	if intercept {
		resolved, resolvedBody := s.resolver.ResolveWithBody(r, host, identityresolver.Options{
			MarkerRules:    snap.MarkerRules,
			AcctMapEnabled: snap.AcctMapEnabled,
			AcctMap:        s.acctMap,
		})
		forwardBody = resolvedBody
		mk = resolved.Credential
		if resolved.Mapped {
			ident = identity{
				key: resolved.Platform + "/" + resolved.Account, mapped: true,
				platform: resolved.Platform, account: resolved.Account,
			}
			mappedAccount = resolved.Account
		}
	}
	saltKey := requestSaltKey(snap.NoMarkerPolicy, mk, ident, clientIP)
	decisionCtx := r.Context()
	if snap.BlockPrivateTargets {
		ips, err := publicTargetIPs(decisionCtx, host)
		decisionCtx = context.WithValue(decisionCtx, publicTargetResolutionKey{}, publicTargetResolution{
			host: normalizedTargetHost(host), ips: ips, err: err,
		})
	}
	pu, account, upstreamName, resolution, routeErr := s.resolveOutboundDetailed(decisionCtx, snap, mk, ident, clientIP, host)
	viaUpstream := pu != nil

	ctx := context.WithValue(decisionCtx, ctxKey{}, pu)
	if pu == nil && snap.BlockPrivateTargets {
		ctx = context.WithValue(ctx, blockPrivateDialKey{}, true)
	}
	// 携带动态盐值键（Marker/账号映射身份，或无 Marker 兜底身份）及是否走上游出口，
	// 供 ErrorHandler 在转发失败时计数、达到阈值后轮换身份盐值。
	fwd := &fwdMeta{marker: saltKey, viaUpstream: viaUpstream, upstream: upstreamName, resolution: resolution, started: start}
	ctx = context.WithValue(ctx, fwdMetaKey{}, fwd)
	s.logger.Log(ctx, slog.LevelDebug, "outbound route selected",
		"method", r.Method,
		"host", host,
		"route", routeName(viaUpstream),
		"resolution_reason", resolution,
		"upstream", upstreamName,
		"marker_fp", markerFingerprint(saltKey),
	)
	requestTrace := s.trace.Start(r, scheme, host)
	if requestTrace != nil {
		ctx = context.WithValue(ctx, traceKey{}, requestTrace)
		defer requestTrace.Finish()
	}
	out := cloneForwardRequest(r, scheme, host, ctx)
	out.Body = requestTrace.WrapRequestBody(forwardBody)

	rec := newRespRec(w, fwd, s.logger, ctx, routeName(viaUpstream), upstreamName, start)
	if routeErr != nil {
		// 手工修改数据库可能留下格式错误的上游配置。此处返回受控的 502，
		// 绝不能把 nil 理解成可以直连。
		s.writeForwardFailure(rec, out, routeErr)
	} else {
		s.roundTripAndRelay(rec, out)
	}
	if fwd.marker != "" && fwd.viaUpstream && !fwd.transportFailed.Load() {
		// 任何收到的 HTTP 响应（包括业务 4xx/5xx）都说明出站链路可用，
		// 因此中断此前的连续可轮换错误计数。
		s.markerSalts.ClearFailures(fwd.marker)
	}

	responseStatus := rec.status
	if responseStatus == 0 {
		responseStatus = http.StatusOK
	}
	// 只有真正收到上游/目标的 HTTP 响应时，审计才记录非零状态码。
	// 传输失败会向客户端返回 502，但它属于 MITMRouter 自身失败，
	// 不能记作上游 5xx。
	auditStatus := responseStatus
	if fwd.transportFailed.Load() || fwd.localRejected.Load() {
		auditStatus = 0
	}
	s.logger.Log(ctx, slog.LevelDebug, "forward completed",
		"method", r.Method,
		"host", host,
		"route", routeName(viaUpstream),
		"upstream", upstreamName,
		"status", responseStatus,
		"audit_status", auditStatus,
		"duration_ms", time.Since(start).Milliseconds(),
		"bytes_out", rec.bytes,
	)
	metrics.Default.Inc("requests_total", "Total proxied requests", map[string]string{
		"upstream":   upstreamName,
		"has_marker": boolStr(mk != ""),
	})
	if responseStatus >= 500 && !fwd.transportFailed.Load() {
		metrics.Default.Inc("upstream_errors_total", "Upstream/target errors", map[string]string{"upstream": upstreamName})
	}
	entry := store.LogEntry{
		Ts:            start.UnixMilli(),
		ReqID:         reqid.From(ctx),
		Method:        r.Method,
		Host:          host,
		Path:          r.URL.Path,
		Status:        auditStatus,
		DurMS:         time.Since(start).Milliseconds(),
		TTFBMS:        rec.ttfbMS,
		BytesOut:      rec.bytes,
		HasMarker:     mk != "",
		Account:       mappedAccount,
		AccountFP:     account,
		Upstream:      upstreamName,
		InternalError: internalErrorFromMeta(fwd),
	}
	select {
	case s.audit <- entry:
	default:
		// 此处必须逐请求记录：后面的采样运维告警无法指出究竟哪一条请求
		// 没有进入审计历史。
		s.logger.Log(ctx, slog.LevelDebug, "request audit entry dropped", "audit_dropped", true)
		if d := s.dropped.Add(1); d%100 == 1 {
			s.logger.Log(ctx, slog.LevelWarn, "audit queue full, dropping entries", "total_dropped", d)
		}
	}
}

// cloneForwardRequest 为 http.Transport 创建仅供内部路由使用的副本。客户端请求保持不变：
// 保留其 URL 路径/查询参数、请求头和请求体。Scheme、host 与 RequestURI 仅供传输层使用，
// 以便服务端处理器连接源站。
func cloneForwardRequest(r *http.Request, scheme, host string, ctx context.Context) *http.Request {
	out := r.Clone(ctx)
	// Request.Clone 会复制一份空的 Trailer，但入站请求要在读取 body 后
	// 才填充 trailer 值。共享原 map，才能把客户端的最终值发给源站。
	out.Trailer = r.Trailer
	out.URL.Scheme = scheme
	out.URL.Host = host
	out.RequestURI = ""
	// Proxy-Authorization authenticates against MITMRouter itself. It is not a
	// target request header and must never be sent to the origin or next hop.
	out.Header.Del("Proxy-Authorization")
	// 如果 User-Agent 缺失，net/http 会自动加入 Go-http-client/1.1。
	// 显式设置空值可阻止传输层添加该请求头，保持客户端未提供 User-Agent 的语义。
	if _, present := out.Header["User-Agent"]; !present {
		out.Header["User-Agent"] = nil
	}
	return out
}

// roundTripAndRelay 通过配置的传输层转发，并原样流式传递返回的响应。
// 它不会新增、删除或改写对外的 URL、请求头或响应体值；仅设置私有出站副本上的
// 路由字段，让 http.Transport 知道应连接到哪里。
func (s *Server) roundTripAndRelay(w *respRec, out *http.Request) {
	clientTrace := &httptrace.ClientTrace{
		Got1xxResponse: func(status int, header textproto.MIMEHeader) error {
			relayInformationalResponse(w, status, http.Header(header))
			return nil
		},
	}
	out = out.WithContext(httptrace.WithClientTrace(out.Context(), clientTrace))
	resp, err := s.transport.RoundTrip(out)
	if err != nil {
		s.writeForwardFailure(w, out, err)
		return
	}
	if resp.StatusCode == http.StatusSwitchingProtocols {
		s.relayUpgrade(w, out, resp)
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	ctx := out.Context()
	body := resp.Body
	if m, _ := ctx.Value(fwdMetaKey{}).(*fwdMeta); m != nil {
		body = &responseReadLogBody{
			ReadCloser: body,
			meta:       m,
			logger:     s.logger,
			ctx:        ctx,
			status:     resp.StatusCode,
			route:      routeName(m.viaUpstream),
			upstream:   upstreamName(m),
			started:    m.started,
		}
	}
	if reqTrace, _ := ctx.Value(traceKey{}).(*trace.Request); reqTrace != nil {
		reqTrace.ResponseHeader(resp.StatusCode, resp.Header)
		body = &traceResponseBody{ReadCloser: body, trace: reqTrace}
	}

	announceTrailers(w.Header(), resp.Trailer)
	w.WriteHeader(resp.StatusCode)
	_, _ = copyStreamingResponse(w, body)
	copyTrailers(w.Header(), resp.Trailer)
}

// writeForwardFailure 仅在尚未收到上游响应时写入本地合成响应。
// 它绝不会修改已经开始发送的响应。
func (s *Server) writeForwardFailure(w http.ResponseWriter, r *http.Request, err error) {
	m, _ := r.Context().Value(fwdMetaKey{}).(*fwdMeta)
	if errors.Is(err, errPrivateTargetBlocked) {
		if m != nil {
			recordInternalError(m, "private_target_blocked")
			m.localRejected.Store(true)
		}
		s.logger.Log(r.Context(), slog.LevelWarn, "private target blocked",
			"method", r.Method, "host", r.Host, "path", r.URL.Path)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		body, _ := json.Marshal(map[string]any{
			"error": map[string]string{"code": "private_target_forbidden", "message": "private network targets are disabled"},
		})
		body = append(body, '\n')
		if reqTrace, _ := r.Context().Value(traceKey{}).(*trace.Request); reqTrace != nil {
			reqTrace.ResponseHeader(http.StatusForbidden, w.Header())
			reqTrace.ResponseBody(body)
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(body)
		return
	}

	failureClass := forwardFailureClass(err)
	if m != nil {
		recordInternalError(m, failureClass)
		m.transportFailed.Store(true)
	}
	s.logger.Log(r.Context(), slog.LevelWarn, "forward failed",
		"method", r.Method,
		"host", r.Host,
		"path", r.URL.Path,
		"route", routeName(m != nil && m.viaUpstream),
		"upstream", upstreamName(m),
		"failure_class", failureClass,
		"duration_ms", forwardDurationMS(m),
		"err", err,
	)
	// 对外固定文案，不泄露内部拓扑；细节只进日志。
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	body, _ := json.Marshal(map[string]any{
		"error": map[string]string{"code": "bad_gateway", "message": "upstream unavailable"},
	})
	body = append(body, '\n')
	if reqTrace, _ := r.Context().Value(traceKey{}).(*trace.Request); reqTrace != nil {
		reqTrace.ResponseHeader(http.StatusBadGateway, w.Header())
		reqTrace.ResponseBody(body)
	}
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write(body)
	s.rotateOnUnusable(r, err)
}

// relayInformationalResponse 在最终响应之前转发 1xx 响应。
// 随后清空临时请求头映射，使普通转发路径中仅保留待发送的最终响应头。
func relayInformationalResponse(w *respRec, status int, header http.Header) {
	copyHeaders(w.Header(), header)
	w.ResponseWriter.WriteHeader(status)
	clear(w.Header())
}

// relayUpgrade 将成功的 HTTP/1 协议升级保留为双向字节流。
// 它与普通 HTTP 响应体转发分开处理，因为 101 响应体是 ReadWriteCloser，
// 而不是有限的响应体。
func (s *Server) relayUpgrade(w *respRec, out *http.Request, resp *http.Response) {
	upstream, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		_ = resp.Body.Close()
		s.writeForwardFailure(w, out, fmt.Errorf("101 response has non-writable body %T", resp.Body))
		return
	}
	defer upstream.Close()

	client, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		s.writeForwardFailure(w, out, fmt.Errorf("hijack upgraded client connection: %w", err))
		return
	}
	defer client.Close()

	copyHeaders(w.Header(), resp.Header)
	if reqTrace, _ := out.Context().Value(traceKey{}).(*trace.Request); reqTrace != nil {
		reqTrace.ResponseHeader(resp.StatusCode, resp.Header)
	}
	w.status = resp.StatusCode
	w.headerWritten = true
	w.recordTTFB()
	resp.Header = w.Header()
	resp.Body = nil
	if err := resp.Write(buffered); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}

	cancelDone := make(chan struct{})
	go func() {
		select {
		case <-out.Context().Done():
			_ = upstream.Close()
		case <-cancelDone:
		}
	}()
	defer close(cancelDone)

	errs := make(chan error, 2)
	go copyUpgradeStream(errs, upstream, client)
	go copyUpgradeStream(errs, client, upstream)
	if err := <-errs; err == nil {
		<-errs
	}
}

func copyUpgradeStream(errs chan<- error, dst io.Writer, src io.Reader) {
	_, err := io.Copy(dst, src)
	if writer, ok := dst.(interface{ CloseWrite() error }); ok {
		if closeErr := writer.CloseWrite(); err == nil {
			err = closeErr
		}
	}
	errs <- err
}

// copyHeaders 复制每个响应头值，不做过滤或改写。
func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func announceTrailers(h http.Header, trailers http.Header) {
	for key := range trailers {
		h.Add("Trailer", key)
	}
}

// copyTrailers 使用响应头提交后所需的特殊 ResponseWriter 前缀。
// 值严格保持为源站提供的值。
func copyTrailers(h http.Header, trailers http.Header) {
	for key, values := range trailers {
		for _, value := range values {
			h.Add(http.TrailerPrefix+key, value)
		}
	}
}

// copyStreamingResponse 不经缓冲地转发响应字节，并刷新每个收到的数据块，
// 以保证 SSE 响应持续渐进输出。
func copyStreamingResponse(w http.ResponseWriter, body io.Reader) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			written, writeErr := w.Write(buf[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
	}
}

// isPrivateTarget 判断字面量目标是否为回环地址或其他非公网地址。
// 主机名特意由 dialPublicTarget 解析，以确保用于检查的 DNS 结果也是实际连接的地址。
func isPrivateTarget(hostport string) bool {
	h := remoteHost(hostport)
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	// CGNAT 100.64.0.0/10（运营商级 NAT，同样属内部空间）
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return cgnat.Contains(ip)
}

var lookupNetIP = net.DefaultResolver.LookupNetIP

func normalizedTargetHost(hostport string) string {
	host := remoteHost(hostport)
	return strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
}

// withTunnelTargetResolution 仅将 CONNECT 一侧的 DNS 校验结果复制到内部解密请求中。
// 它会特意保留 r.Context()，因为其取消信号属于该特定 HTTP/1.1 请求或 HTTP/2 流。
func withTunnelTargetResolution(r *http.Request, tunnelCtx context.Context) *http.Request {
	if r == nil || tunnelCtx == nil {
		return r
	}
	resolution, ok := tunnelCtx.Value(publicTargetResolutionKey{}).(publicTargetResolution)
	if !ok {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), publicTargetResolutionKey{}, resolution))
}

// publicTargetIPs 检查字面量目标，或只解析一次主机名。所有返回地址都必须是公网地址；
// 若接受同时包含公网和私网地址的结果，DNS 重绑定和分割视图 DNS 将可绕过此检查。
func publicTargetIPs(ctx context.Context, hostport string) ([]netip.Addr, error) {
	host := normalizedTargetHost(hostport)
	if cached, ok := ctx.Value(publicTargetResolutionKey{}).(publicTargetResolution); ok && cached.host == host {
		return cached.ips, cached.err
	}
	if host == "" || isPrivateTarget(host) {
		return nil, errPrivateTargetBlocked
	}
	if ip := net.ParseIP(host); ip != nil {
		return []netip.Addr{netip.MustParseAddr(ip.String())}, nil
	}
	ips, err := lookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no IP addresses found for target")
	}
	for _, ip := range ips {
		if isPrivateTarget(ip.String()) {
			return nil, errPrivateTargetBlocked
		}
	}
	return ips, nil
}

// validatePublicTarget 是路由预检，可防止私有目标隐藏在域名之后。
func validatePublicTarget(ctx context.Context, hostport string) error {
	_, err := publicTargetIPs(ctx, hostport)
	return err
}

// dialPublicTarget 只解析一次直连目标，拒绝所有私网结果，然后连接一个已检查的 IP，
// 而非原始主机名。这可避免 DNS 重绑定在校验和连接之间更换地址。
func dialPublicTarget(ctx context.Context, dialer *net.Dialer, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port == "" {
		return nil, fmt.Errorf("%w: invalid target address", errPrivateTargetBlocked)
	}
	ips, err := publicTargetIPs(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// identity 是一次请求解析出的粘滞身份来源。
// mapped=true 表示命中账号映射表：key 形如 "platform/account"，派生输入与
// 轮换键都以它为准；mapped=false 维持 v3 的纯 Marker 语义。
// platform/account 独立字段供账户↔出站绑定查找使用（docs/011）——账号串本身
// 可能含 "/"，不从 key 反向拆分。
type identity struct {
	key      string
	mapped   bool
	platform string // 仅 mapped=true 时非空
	account  string // 仅 mapped=true 时非空
}

// markerKey 返回盐值 LRU 的键（沿用 "marker" 命名）：映射命中为身份键，否则为 Marker 原文。
func (id identity) markerKey(mk string) string {
	if id.mapped {
		return id.key
	}
	return mk
}

const (
	// NUL 前缀与可作为 HTTP 凭据的 Marker 命名空间隔离，持久化时仅保存其哈希。
	noMarkerDefaultSaltKey     = "\x00no-marker/default"
	noMarkerClientIPSaltPrefix = "\x00no-marker/client-ip/"
)

// noMarkerSaltKey 返回可轮换的无 Marker 兜底身份键；直连策略不生成身份，故不轮换。
func noMarkerSaltKey(policy, clientIP string) string {
	switch policy {
	case settings.PolicyDefaultSession:
		return noMarkerDefaultSaltKey
	case settings.PolicyClientIPSession:
		return noMarkerClientIPSaltPrefix + clientIP
	default:
		return ""
	}
}

// requestSaltKey 返回本请求的动态盐值键。映射账号和 Marker 均有各自独立的键；
// 无 Marker 时，固定身份与来源 IP 策略各自用不含凭据的命名空间隔离键。
func requestSaltKey(policy, mk string, ident identity, clientIP string) string {
	if key := ident.markerKey(mk); key != "" {
		return key
	}
	return noMarkerSaltKey(policy, clientIP)
}

// resolveOutbound 依据快照/Marker/兜底策略决定出站方式。
// 返回 (出口URL(nil=直连), 粘滞身份串, 上游名)。
// 该兼容入口供非请求代码和单元测试使用。
func (s *Server) resolveOutbound(snap settings.Snapshot, mk string, ident identity, clientIP, targetHost string) (*url.URL, string, string) {
	pu, account, name, _, _ := s.resolveOutboundDetailed(context.Background(), snap, mk, ident, clientIP, targetHost)
	return pu, account, name
}

// resolveOutboundWithContext 保留给仅需路由描述的诊断和测试使用。
// 转发路径必须使用 resolveOutboundDetailed，避免将配置错误误解为有意的直连出站。
func (s *Server) resolveOutboundWithContext(ctx context.Context, snap settings.Snapshot, mk string, ident identity, clientIP, targetHost string) (*url.URL, string, string, string) {
	pu, account, name, reason, _ := s.resolveOutboundDetailed(ctx, snap, mk, ident, clientIP, targetHost)
	return pu, account, name, reason
}

// 这些哨兵错误用于产生受控客户端响应和安全的审计分类，且不泄露内部路由细节。
var (
	// errUpstreamConfig 表示本地保存的上游条目无效。它必须使请求失败，
	// 绝不能回退为直连而暴露本机 IP。
	errUpstreamConfig       = errors.New("upstream configuration invalid")
	errPrivateTargetBlocked = errors.New("private target blocked")
)

// resolveOutboundDetailed 返回不可变的路由决策。最终解析原因可安全记录日志，
// 并能区分有意的直连出站与必须中止转发的配置错误。
func (s *Server) resolveOutboundDetailed(ctx context.Context, snap settings.Snapshot, mk string, ident identity, clientIP, targetHost string) (*url.URL, string, string, string, error) {
	const directName = "direct"
	var account string
	forceDirect := false
	saltKey := requestSaltKey(snap.NoMarkerPolicy, mk, ident, clientIP)
	salt := sticky.CombineSalt(snap.Salt, s.markerSalts.Get(saltKey))
	switch {
	case mk != "":
		if ident.mapped {
			salt += "#a" // 与 v3 Marker 哈希命名空间隔离（设计文档 §2.2）
			account = sticky.Derive(salt, ident.key, snap.SIDLen)
		} else {
			account = sticky.Derive(salt, mk, snap.SIDLen)
		}
	case snap.NoMarkerPolicy == settings.PolicyDirect:
		account = "-"
		forceDirect = true
	case snap.NoMarkerPolicy == settings.PolicyClientIPSession:
		account = sticky.Derive(salt+"#ip", clientIP, snap.SIDLen)
	default: // default_session
		// 未轮换时保持历史固定身份；轮换后带入动态盐以切换出口。
		if s.markerSalts.Get(saltKey) == 0 {
			account = "default"
		} else {
			account = sticky.Derive(salt+"#default", "default", snap.SIDLen)
		}
	}
	if snap.BlockPrivateTargets {
		if err := validatePublicTarget(ctx, targetHost); err != nil {
			if errors.Is(err, errPrivateTargetBlocked) {
				return nil, account, directName, "private_target_blocked", err
			}
			return nil, account, directName, "target_resolution_failed", err
		}
	} else if isPrivateTarget(targetHost) {
		// 兼容模式刻意恢复旧版对字面私网目标的行为，但绝不是默认值。
		return nil, account, directName, "private_target_allowed", nil
	}
	if forceDirect {
		return nil, account, directName, "policy_direct", nil
	}
	// ③ 账户↔出站绑定（docs/011）：绑定命中即走出站，优先级高于默认粘滞路由。
	// 绑定存在但候选出站全部缺失/停用时受控失败——用户明确指定了出口，
	// 悄悄回落默认粘滞路由等于改变账户的出口 IP 语义。
	if up, mode, bound := s.selectBoundEgress(snap, ident, salt); bound {
		if up == nil {
			s.logger.Log(ctx, slog.LevelWarn, "bound egress unavailable",
				"identity", ident.key, "resolution_reason", "egress_none_enabled")
			return nil, account, directName, "egress_none_enabled",
				fmt.Errorf("%w: all bound plain exits for %q are missing or disabled", errUpstreamConfig, ident.key)
		}
		inj, ok := upstream.InjectorFor(up.Platform, up)
		if !ok {
			s.logger.Log(ctx, slog.LevelWarn, "platform injector not registered", "platform", up.Platform, "resolution_reason", "injector_missing")
			return nil, account, up.Name, "injector_missing", fmt.Errorf("%w: injector missing for platform %q", errUpstreamConfig, up.Platform)
		}
		purl, err := inj.Inject(up.BaseURL, upstream.InjectParams{
			Account: account,
			TTLMin:  snap.SessionTTLMin,
		})
		if err != nil {
			s.logger.Log(ctx, slog.LevelError, "credential injection failed", "upstream", up.Name, "resolution_reason", "injection_failed", "err", err)
			return nil, account, up.Name, "injection_failed", fmt.Errorf("%w: credential injection failed", errUpstreamConfig)
		}
		if strings.EqualFold(purl.Scheme, "socks5h") {
			purl.Scheme = "socks5"
		}
		reason := "egress_sticky"
		if mode == acctegress.ModeRandom {
			reason = "egress_random"
		}
		return purl, account, up.Name, reason, nil
	}
	up := s.upstreams.Load().Select(snap.DefaultUpstream)
	if up == nil {
		if snap.DefaultUpstream == "" {
			// 未配置默认上游：按“没有上游”语义直连。
			return nil, account, directName, "no_upstream", nil
		}
		// 已配置默认上游但运行时表里缺失或停用：必须受控失败，绝不能回退直连
		// 而暴露本机出口 IP。
		s.logger.Log(ctx, slog.LevelWarn, "configured default upstream missing or disabled",
			"default_upstream", snap.DefaultUpstream, "resolution_reason", "no_upstream")
		return nil, account, snap.DefaultUpstream, "no_upstream",
			fmt.Errorf("%w: default upstream %q not found or disabled", errUpstreamConfig, snap.DefaultUpstream)
	}
	inj, ok := upstream.InjectorFor(up.Platform, up)
	if !ok {
		s.logger.Log(ctx, slog.LevelWarn, "platform injector not registered", "platform", up.Platform, "resolution_reason", "injector_missing")
		return nil, account, up.Name, "injector_missing", fmt.Errorf("%w: injector missing for platform %q", errUpstreamConfig, up.Platform)
	}
	purl, err := inj.Inject(up.BaseURL, upstream.InjectParams{
		Account: account,
		TTLMin:  snap.SessionTTLMin,
	})
	if err != nil {
		s.logger.Log(ctx, slog.LevelError, "credential injection failed", "upstream", up.Name, "resolution_reason", "injection_failed", "err", err)
		return nil, account, up.Name, "injection_failed", fmt.Errorf("%w: credential injection failed", errUpstreamConfig)
	}
	// 归一 socks5h → socks5：Go 的 SOCKS5 客户端把域名直接交给远端做解析
	// （语义即为 socks5h）；旧工具链的 http.Transport 不识别 socks5h scheme。
	if strings.EqualFold(purl.Scheme, "socks5h") {
		purl.Scheme = "socks5"
	}
	return purl, account, up.Name, "upstream", nil
}

// selectBoundEgress 依据账户↔出站绑定挑选出站（docs/011 §3）。
// 返回 (选中的出站, 绑定模式, 是否绑定)：
//   - bound=false：账户未绑定（或绑定快照未装配），调用方走默认粘滞路由；
//   - bound=true 且 up=nil：绑定存在但候选出站全部缺失/停用，必须受控失败；
//   - 其余：up 为按绑定模式挑中的启用 plain 条目。
func (s *Server) selectBoundEgress(snap settings.Snapshot, ident identity, salt string) (*upstream.Upstream, string, bool) {
	if !ident.mapped {
		return nil, "", false
	}
	tbl := s.acctEgress.Load()
	if tbl == nil {
		return nil, "", false
	}
	binding, ok := tbl.Lookup(ident.platform, ident.account)
	if !ok || len(binding.EgressIDs) == 0 {
		return nil, "", false
	}
	bound := make(map[int64]struct{}, len(binding.EgressIDs))
	for _, id := range binding.EgressIDs {
		bound[id] = struct{}{}
	}
	var cands []*upstream.Upstream
	for _, u := range s.upstreams.Load().Items() {
		if u.Platform != upstream.PlatformPlain || !u.Enabled {
			continue
		}
		if _, hit := bound[u.ID]; hit {
			cands = append(cands, u)
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].ID < cands[j].ID })
	if len(cands) == 0 {
		return nil, binding.Mode, true
	}
	var pick *upstream.Upstream
	if binding.Mode == acctegress.ModeRandom {
		pick = cands[rand.IntN(len(cands))]
	} else {
		var best uint64
		for _, u := range cands {
			if sc := hrwScore(salt, ident.key, u.ID); pick == nil || sc > best {
				best, pick = sc, u
			}
		}
	}
	return pick, binding.Mode, true
}

// hrwScore 计算一条出站的 Rendezvous（HRW）分数。盐与粘滞身份推导同源：
// 连续可轮换错误触发动态盐 +1 后，该账户在粘滞模式下自动换到另一条出站
// （逃生阀复用，docs/011 §3.2）。
func hrwScore(salt, identKey string, egressID int64) uint64 {
	sum := sha256.Sum256([]byte(salt + "\x00" + identKey + "\x00" + strconv.FormatInt(egressID, 10)))
	return binary.BigEndian.Uint64(sum[:8])
}

// ---------- Marker 动态盐值轮换（上游不可用时） ----------

// AttachMarkerSaltStore 启用 per-marker 盐值持久化：启动时从库中恢复最近活跃的
// 盐值（上限为 LRU 容量），此后每次轮换经队列异步落库。失败不致命：
// 恢复失败按干净状态启动；落库失败仅记日志，内存态不受影响。
// 需随后以 RunMarkerSaltWriter 启动写入协程，并在退出前等待其排空。
func (s *Server) AttachMarkerSaltStore(st *store.Store) error {
	rows, err := st.LoadMarkerSalts(context.WithoutCancel(context.Background()), s.markerSalts.Cap())
	if err != nil {
		return err
	}
	for _, r := range rows {
		s.markerSalts.SeedFingerprint(r.FP, r.Salt)
	}
	s.saltSt = st
	s.saltCh = make(chan markerSaltEvent, 256)
	s.saltDone = make(chan struct{})
	s.logger.Info("marker salts restored", "count", len(rows), "capacity", s.markerSalts.Cap())
	return nil
}

// RunMarkerSaltWriter 消费轮换事件逐条落库（轮换低频，无需批量）；
// ctx 结束后先排空剩余事件再返回，配合优雅退出保证最后几次轮换不丢。
func (s *Server) RunMarkerSaltWriter(ctx context.Context) {
	defer close(s.saltDone)
	wctx := context.WithoutCancel(ctx) // 排空阶段落库须脱离已取消的 ctx
	persist := func(e markerSaltEvent) {
		pctx, cancel := context.WithTimeout(wctx, 5*time.Second)
		defer cancel()
		if err := s.saltSt.UpsertMarkerSalt(pctx, e.fp, e.salt); err != nil {
			s.logger.Error("marker salt persist failed", "marker_fp", e.fp, "salt", e.salt, "err", err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			deadline := time.After(3 * time.Second)
			for {
				select {
				case e := <-s.saltCh:
					persist(e)
				case <-deadline:
					s.logger.Warn("marker salt persistence drain deadline reached")
					return
				default:
					return
				}
			}
		case e := <-s.saltCh:
			persist(e)
		}
	}
}

// SaltWriterDone 返回写入协程退出信号；未启用持久化时返回 nil。
func (s *Server) SaltWriterDone() <-chan struct{} { return s.saltDone }

type fwdMetaKey struct{}

// fwdMeta 随请求上下文传递，供 ErrorHandler 在转发失败时判定是否轮换 Marker 盐值。
type fwdMeta struct {
	marker          string       // 动态盐值键：Marker/映射账号，或无 Marker 兜底身份（空=无）
	viaUpstream     bool         // 本次转发是否经由上游出口
	upstream        string       // 上游配置名；仅用于结构化日志，绝不含 URL 或凭据
	resolution      string       // 仅枚举值，说明上游或直连的选择原因
	started         time.Time    // 转发起始时间，供失败日志计算耗时
	internalError   atomic.Value // string：安全内部错误分类，绝不保存原始错误
	transportFailed atomic.Bool  // 是否在收到 HTTP 响应前发生传输失败
	localRejected   atomic.Bool  // 是否在收到上游响应前被本地路由拒绝
}

func routeName(viaUpstream bool) string {
	if viaUpstream {
		return "upstream"
	}
	return "direct"
}

func upstreamName(m *fwdMeta) string {
	if m == nil || m.upstream == "" {
		return "direct"
	}
	return m.upstream
}

func forwardDurationMS(m *fwdMeta) int64 {
	if m == nil || m.started.IsZero() {
		return 0
	}
	return time.Since(m.started).Milliseconds()
}

func recordInternalError(m *fwdMeta, class string) {
	if m == nil || class == "" {
		return
	}
	// 最先观察到的失败最接近根因。后续清理阶段的失败（例如上游读取失败后的
	// 客户端写入失败）不得覆盖它。
	m.internalError.CompareAndSwap(nil, class)
}

func internalErrorFromMeta(m *fwdMeta) string {
	if m == nil {
		return ""
	}
	failure, _ := m.internalError.Load().(string)
	return failure
}

// rotateOnUnusable 记录白名单中的上游不可用错误（TLS/证书、上游建隧被拒、对端 EOF）。
// 请求经上游且具有可轮换身份时，仅在连续错误达到设置阈值后将其动态盐值 +1；
// 下一次推导即得到新粘滞身份（等效更换出口 IP）。映射账号按账号级轮换，
// 无 Marker 的固定身份和来源 IP 策略也有各自独立的轮换键。
func (s *Server) rotateOnUnusable(r *http.Request, err error) {
	m, ok := r.Context().Value(fwdMetaKey{}).(*fwdMeta)
	if !ok {
		return
	}
	s.recordUnusableIdentityWithContext(r.Context(), m.marker, m.viaUpstream, err)
}

// recordUnusableIdentity 对一个经上游的可轮换身份记录错误，并在达到阈值时轮换盐值。
// 它同时服务于 HTTP 转发和无法解密的盲隧道。
func (s *Server) recordUnusableIdentity(saltKey string, viaUpstream bool, err error) {
	s.recordUnusableIdentityWithContext(context.Background(), saltKey, viaUpstream, err)
}

func (s *Server) recordUnusableIdentityWithContext(ctx context.Context, saltKey string, viaUpstream bool, err error) {
	if saltKey == "" || !viaUpstream {
		return
	}
	if !upstreamUnusable(err) {
		s.markerSalts.ClearFailures(saltKey)
		return
	}
	threshold := s.settings.Current().SaltRotateFailureThreshold
	rotated, salt, failures := s.markerSalts.RecordFailure(saltKey, threshold)
	failureClass := forwardFailureClass(err)
	if !rotated {
		s.logger.Log(ctx, slog.LevelWarn, "upstream unusable, awaiting salt rotation threshold",
			"marker_fp", sticky.Fingerprint(saltKey), "failures", failures, "threshold", threshold,
			"failure_class", failureClass, "err", err)
		return
	}
	metrics.Default.Inc("marker_salt_rotations_total", "Marker salt rotations due to unusable upstream", nil)
	s.logger.Log(ctx, slog.LevelWarn, "upstream unusable, rotated marker salt",
		"marker_fp", sticky.Fingerprint(saltKey), "salt", salt, "failures", failures, "threshold", threshold,
		"failure_class", failureClass, "err", err)
	if s.saltCh != nil { // 队列满时内存态已生效，持久化延后到下一次轮换。
		event := markerSaltEvent{fp: marker.Fingerprint(saltKey), salt: salt}
		select {
		case s.saltCh <- event:
		default:
			metrics.Default.Inc("marker_salt_persist_dropped_total", "Marker salt persistence events dropped", nil)
			s.logger.Log(ctx, slog.LevelWarn, "marker salt persistence queue full",
				"marker_fp", event.fp, "salt", event.salt, "queue_capacity", cap(s.saltCh))
		}
	}
}

// upstreamUnusable 判定转发错误是否属于“当前生成的上游凭据不可用”。
// 典型场景：住宅出口节点失效或被目标站封锁——表现为 TLS 握手失败、
// 证书校验失败、TLS alert、非法记录头、握手期对端断开；
// 以及上游侧拒绝建隧（Go 标准库错误里的 proxyconnect 标记）。目标站自身 5xx 属正常响应，不在此列。
func upstreamUnusable(err error) bool {
	if err == nil {
		return false
	}
	// 上游在完整 HTTP 响应前直接关闭连接时无状态码可供判断；经上游出口时，
	// 这通常意味着当前出口不可用，纳入轮换白名单。
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var hostErr x509.HostnameError
	var authErr x509.UnknownAuthorityError
	var constraintErr x509.ConstraintViolationError
	var certVerifErr *tls.CertificateVerificationError
	var recordErr tls.RecordHeaderError
	var alertErr tls.AlertError
	if errors.As(err, &hostErr) ||
		errors.As(err, &authErr) ||
		errors.As(err, &constraintErr) ||
		errors.As(err, &certVerifErr) ||
		errors.As(err, &recordErr) ||
		errors.As(err, &alertErr) {
		return true
	}
	msg := err.Error()
	for _, kw := range []string{"tls:", "x509", "certificate", "handshake", "proxyconnect", "unexpected EOF"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// forwardFailureClass 对传输错误进行归一化以便聚合，同时在日志中单独保留原始错误。
// 它不得检查请求凭据。
// tunnelDialError 保留协议级拒绝细节，使盲隧道日志无需解析错误字符串即可筛选。
// 它绝不保存 URL、凭据、请求头或目标地址。
type tunnelDialError struct {
	stage          string
	upstreamStatus int
	socksRep       byte
	err            error
}

func (e *tunnelDialError) Error() string { return e.err.Error() }
func (e *tunnelDialError) Unwrap() error { return e.err }

func wrapTunnelDialError(stage string, err error) error {
	return &tunnelDialError{stage: stage, err: err}
}

func tunnelFailureDetails(err error, viaUpstream bool) (stage string, upstreamStatus, socksRep int) {
	if viaUpstream {
		stage = "upstream_dial"
	} else {
		stage = "target_dial"
	}
	var dialErr *tunnelDialError
	if errors.As(err, &dialErr) {
		return dialErr.stage, dialErr.upstreamStatus, int(dialErr.socksRep)
	}
	return stage, 0, 0
}

func forwardFailureClass(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, errPrivateTargetBlocked) {
		return "private_target_blocked"
	}
	if errors.Is(err, errUpstreamConfig) {
		return "upstream_config"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "eof"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	msg := strings.ToLower(err.Error())
	switch {
	// Go 的传输层会把上游 CONNECT 拒绝格式化为例如
	// "proxyconnect tcp: ...: 407 Proxy Authentication Required"。先检查
	// 明确拒绝，避免被通用前缀分类吞掉。
	case strings.Contains(msg, "proxy authentication") || strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden") || strings.Contains(msg, "too many requests"):
		return "upstream_connect_rejected"
	case strings.Contains(msg, "proxyconnect"):
		return "upstream_connect"
	case strings.Contains(msg, "tls") || strings.Contains(msg, "x509") || strings.Contains(msg, "certificate"):
		return "tls"
	case strings.Contains(msg, "dial") || strings.Contains(msg, "connect:"):
		return "dial"
	default:
		return "transport"
	}
}

func markerFingerprint(key string) string {
	if key == "" {
		return ""
	}
	return sticky.Fingerprint(key)
}

// ---------- 小工具 ----------

// bufConn 让后续读取复用已缓冲的数据（Hijack 后 Peek 过的字节不能丢）。
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// CloseWrite 保留底层连接的半关闭能力。Hijack 后的 bufio 缓冲只负责读取，
// 写方向仍由原始连接承载；没有该转发时，盲隧道无法向对端发送 FIN。
func (c *bufConn) CloseWrite() error {
	cw, ok := c.Conn.(interface{ CloseWrite() error })
	if !ok {
		return errors.New("underlying connection does not support half-close")
	}
	return cw.CloseWrite()
}

// responseReadLogBody 在响应头已转发后记录上游响应体读取失败。此时下游状态
// 已提交，无法再走本地失败响应，因此这里是唯一能关联该请求的诊断点。
// 正常 EOF 不记录。
type responseReadLogBody struct {
	io.ReadCloser
	meta     *fwdMeta
	logger   *slog.Logger
	ctx      context.Context
	status   int
	route    string
	upstream string
	started  time.Time
	once     sync.Once
}

func (b *responseReadLogBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		b.once.Do(func() {
			internalError := "upstream_response_read"
			switch {
			case errors.Is(err, io.ErrUnexpectedEOF):
				// 外层守卫已排除 io.EOF，这里只剩意外截断。
				internalError = "upstream_response_eof"
			case errors.Is(err, context.Canceled):
				// 下游断开导致请求上下文取消，与转发前路径 forwardFailureClass
				// 的 "canceled" 归类一致，不归咎上游。
				internalError = "canceled"
			}
			recordInternalError(b.meta, internalError)
			b.logger.Log(b.ctx, slog.LevelWarn, "upstream response body read failed",
				"failure_stage", "upstream_response_body",
				"failure_class", internalError,
				"status", b.status,
				"route", b.route,
				"upstream", b.upstream,
				"duration_ms", time.Since(b.started).Milliseconds(),
				"err", err,
			)
		})
	}
	return n, err
}

// traceResponseBody 在每个上游响应块流向客户端时记录它。
// 它既不预读也不缓冲，保持 SSE 行为不变。
type traceResponseBody struct {
	io.ReadCloser
	trace *trace.Request
}

func (b *traceResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.trace.ResponseBody(p[:n])
	}
	return n, err
}

// respRec 记录状态码、首字节时延与响应字节数，并透传 Flush（SSE 关键路径）。
type respRec struct {
	http.ResponseWriter
	meta     *fwdMeta
	logger   *slog.Logger
	ctx      context.Context
	route    string
	upstream string
	started  time.Time
	status   int
	bytes    int64
	// ttfbMS 在首次提交响应头时设置一次。无法提交客户端响应时为 nil，
	// 这与有效测量值 0ms 不同。
	ttfbMS        *int64
	headerWritten bool
	writeErrOnce  sync.Once
}

func newRespRec(w http.ResponseWriter, meta *fwdMeta, logger *slog.Logger, ctx context.Context, route, upstream string, started time.Time) *respRec {
	return &respRec{ResponseWriter: w, meta: meta, logger: logger, ctx: ctx, route: route, upstream: upstream, started: started}
}

// recordTTFB 记录路由器首次提交下游响应所需的时间。
// WriteHeader 仅在服务端边界提交 HTTP 响应头，并不代表客户端已在其 socket 上收到字节。
func (r *respRec) recordTTFB() {
	if r.ttfbMS != nil {
		return
	}
	v := time.Since(r.started).Milliseconds()
	r.ttfbMS = &v
}

func (r *respRec) WriteHeader(code int) {
	if !r.headerWritten {
		r.status = code
		r.headerWritten = true
		r.recordTTFB()
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *respRec) Write(b []byte) (int, error) {
	if !r.headerWritten {
		r.status = http.StatusOK
		r.headerWritten = true
		r.recordTTFB()
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	if err != nil && r.logger != nil {
		r.writeErrOnce.Do(func() {
			recordInternalError(r.meta, "downstream_write")
			r.logger.Log(r.ctx, slog.LevelDebug, "downstream response write failed",
				"failure_stage", "downstream_response_write",
				"failure_class", forwardFailureClass(err),
				"status", r.status,
				"route", r.route,
				"upstream", r.upstream,
				"duration_ms", time.Since(r.started).Milliseconds(),
				"bytes_out", r.bytes,
				"err", err,
			)
		})
	}
	return n, err
}

// Flush 透传给底层 Flusher，显式流式转发器依赖它即时刷新 SSE。与
// net/http 的 ResponseWriter 一样，首次 Flush 隐式提交 200 响应头，因此也是
// 是首字节时延的有效终点。
func (r *respRec) Flush() {
	if !r.headerWritten {
		r.status = http.StatusOK
		r.headerWritten = true
		r.recordTTFB()
	}
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 支持 http.ResponseController 穿透取底层能力。
func (r *respRec) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func remoteHost(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// ---------- SOCKS5 客户端（RFC 1928/1929，支持全程超时注入） ----------

// socksDial 经 SOCKS5 上游建立到 target 的连接。x/net 的 proxy.SOCKS5 无法
// 外挂超时，半开网关会永久阻塞泄漏 goroutine，故自实现握手。
func socksDial(upstreamAddr, target, user, pass string, hasAuth bool, timeout time.Duration) (net.Conn, error) {
	pc, err := net.DialTimeout("tcp", upstreamAddr, timeout)
	if err != nil {
		return nil, wrapTunnelDialError("socks_tcp_dial", fmt.Errorf("socks dial failed: %w", err))
	}
	defer func() {
		if pc != nil {
			pc.Close()
		}
	}()
	pc.SetDeadline(time.Now().Add(timeout))

	writeAll := func(b []byte) error {
		_, e := pc.Write(b)
		return e
	}

	// ① 方法协商
	methods := []byte{0x02} // 用户名/密码
	if !hasAuth {
		methods = []byte{0x00}
	}
	if err := writeAll(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return nil, wrapTunnelDialError("socks_method_write", fmt.Errorf("socks method negotiation write: %w", err))
	}
	rep := make([]byte, 2)
	if _, err := io.ReadFull(pc, rep); err != nil {
		return nil, wrapTunnelDialError("socks_method_read", fmt.Errorf("socks method negotiation read: %w", err))
	}
	if rep[0] != 0x05 {
		return nil, &tunnelDialError{stage: "socks_method", err: errors.New("socks invalid protocol version")}
	}
	switch rep[1] {
	case 0x00: // 无需认证
	case 0x02:
		if !hasAuth {
			return nil, &tunnelDialError{stage: "socks_auth", err: errors.New("upstream requires auth but no credentials provided")}
		}
		ul, pl := len(user), len(pass)
		authReq := []byte{0x01, byte(ul)}
		authReq = append(authReq, user...)
		authReq = append(authReq, byte(pl))
		authReq = append(authReq, pass...)
		if err := writeAll(authReq); err != nil {
			return nil, wrapTunnelDialError("socks_auth_write", fmt.Errorf("socks authentication write: %w", err))
		}
		ares := make([]byte, 2)
		if _, err := io.ReadFull(pc, ares); err != nil {
			return nil, wrapTunnelDialError("socks_auth_read", fmt.Errorf("socks authentication read: %w", err))
		}
		if ares[1] != 0x00 {
			return nil, &tunnelDialError{stage: "socks_auth", err: errors.New("socks auth rejected")}
		}
	default:
		return nil, &tunnelDialError{stage: "socks_method", err: errors.New("socks no acceptable auth method")}
	}

	// ② CONNECT 请求
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		host, portStr = target, "80"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, wrapTunnelDialError("socks_connect_request", fmt.Errorf("invalid port %q", portStr))
	}
	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req = append(req, 0x01)
			req = append(req, v4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return nil, wrapTunnelDialError("socks_connect_request", errors.New("target hostname too long"))
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	req = append(req, byte(port>>8), byte(port&0xff))
	if err := writeAll(req); err != nil {
		return nil, wrapTunnelDialError("socks_connect_write", fmt.Errorf("socks CONNECT write: %w", err))
	}

	// ③ 应答：VER REP RSV ATYP BND.ADDR BND.PORT
	head := make([]byte, 4)
	if _, err := io.ReadFull(pc, head); err != nil {
		return nil, wrapTunnelDialError("socks_connect_read", fmt.Errorf("socks CONNECT response read: %w", err))
	}
	if head[1] != 0x00 {
		return nil, &tunnelDialError{
			stage:    "socks_connect",
			socksRep: head[1],
			err:      fmt.Errorf("socks CONNECT failed rep=%d", head[1]),
		}
	}
	switch head[3] {
	case 0x01:
		if _, err := io.ReadFull(pc, make([]byte, 6)); err != nil {
			return nil, wrapTunnelDialError("socks_connect_reply", fmt.Errorf("socks CONNECT bound IPv4 read: %w", err))
		}
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(pc, l); err != nil {
			return nil, wrapTunnelDialError("socks_connect_reply", fmt.Errorf("socks CONNECT bound domain length read: %w", err))
		}
		if _, err := io.ReadFull(pc, make([]byte, int(l[0])+2)); err != nil {
			return nil, wrapTunnelDialError("socks_connect_reply", fmt.Errorf("socks CONNECT bound domain read: %w", err))
		}
	case 0x04:
		if _, err := io.ReadFull(pc, make([]byte, 18)); err != nil {
			return nil, wrapTunnelDialError("socks_connect_reply", fmt.Errorf("socks CONNECT bound IPv6 read: %w", err))
		}
	default:
		return nil, wrapTunnelDialError("socks_connect_reply", fmt.Errorf("socks invalid ATYP %d", head[3]))
	}

	pc.SetDeadline(time.Time{}) // 清除超时进入转发阶段
	out := pc
	pc = nil // 成功后转交所有权，禁止 defer 关闭
	return out, nil
}
