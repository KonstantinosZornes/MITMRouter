// mitmrouter —— MITMRouter 主程序入口。
// 唯一命令行形态：mitmrouter -data ./data [-addr :55666]
// -addr 仅在首次初始化时作为初始监听地址写入设置表，之后一切以管理台设置为准。
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"time"

	"mitmrouter/internal/acctegress"
	"mitmrouter/internal/acctmap"
	"mitmrouter/internal/api"
	"mitmrouter/internal/certca"
	"mitmrouter/internal/reqid"
	"mitmrouter/internal/server"
	"mitmrouter/internal/settings"
	"mitmrouter/internal/store"
	"mitmrouter/internal/syncer"
	"mitmrouter/internal/tlsreload"
	"mitmrouter/internal/trace"
	"mitmrouter/internal/upstream"
	"mitmrouter/internal/webui"
)

func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}

// 监听地址默认值：接入口与管理台均仅回环，避免首次启动即暴露到局域网。
const (
	defaultIngressListen = "127.0.0.1:55666"
	defaultAdminListen   = "127.0.0.1:55667"
)

func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unsupported log level %q (want debug, info, warn, or error)", raw)
	}
}

func main() {
	dataDir := flag.String("data", "./data", "data directory (contains router.db)")
	addrFlag := flag.String("addr", defaultIngressListen, "ingress listen address; takes effect on every start")
	adminFlag := flag.String("admin-addr", defaultAdminListen, "admin listen address; takes effect on every start")
	traceFile := flag.String("trace-file", "", "append plaintext request/response trace to this file (debug only; records secrets; empty disables)")
	logLevelFlag := flag.String("log-level", "info", "log verbosity: debug, info, warn, or error")
	flag.Parse()

	logLevel, err := parseLogLevel(*logLevelFlag)
	if err != nil {
		fatal("invalid log level", err)
	}
	logger := slog.New(reqid.NewHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})))
	slog.SetDefault(logger)

	// ---------- 引导 ----------
	st, boot, err := store.Bootstrap(*dataDir)
	if err != nil {
		fatal("failed to initialize storage", err)
	}
	defer st.Close()

	if boot.AdminPassword != "" {
		fmt.Println("==================================================================")
		fmt.Println("!! First-time init generated a random admin password (shown only once, change it after login) !!")
		fmt.Println()
		fmt.Println("    Admin password: " + boot.AdminPassword)
		fmt.Println()
		fmt.Println("==================================================================")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	sigRaw := make(chan os.Signal, 1)
	signal.Notify(sigRaw, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigRaw)

	// ---------- 设置快照 ----------
	snap, err := settings.LoadFromStore(ctx, st, settings.DefaultSnapshot())
	if err != nil {
		fatal("failed to load settings", err)
	}
	holder := settings.NewHolder(snap)

	// ---------- CA / 上游表 / 审计 ----------
	ca, err := certca.Ensure(ctx, st)
	if err != nil {
		fatal("failed to initialize CA", err)
	}
	rows, err := st.ListUpstreams(ctx)
	if err != nil {
		fatal("failed to list upstreams", err)
	}
	items := make([]*upstream.Upstream, 0, len(rows))
	for _, rw := range rows {
		u, err := upstream.FromRow(rw.ID, rw.Name, rw.Platform, rw.BaseURL, rw.Inject, rw.Enabled)
		if err != nil {
			logger.Error("skipping invalid upstream entry", "name", rw.Name, "err", err)
			continue
		}
		items = append(items, u)
	}
	table := upstream.NewTable(items, snap.DefaultUpstream)

	retDone := make(chan struct{})
	auditCh := make(chan store.LogEntry, 4096)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		st.RunLogWriter(ctx, auditCh)
	}()
	go func() {
		defer close(retDone)
		st.RunRetention(ctx)
	}()
	// 映射更新记录（docs/013-update-log-design.md）：syncer 与管理台写入，
	// writer 批量落库并在退出时排空。
	updateCh := make(chan store.UpdateEvent, 4096)
	updateDone := make(chan struct{})
	go func() {
		defer close(updateDone)
		st.RunUpdateEventWriter(ctx, updateCh)
	}()

	// ---------- 启动 HTTP 服务（接入面与管理台独立监听） ----------
	srv := server.New(holder, ca, table, auditCh, logger)
	if *traceFile != "" {
		tw, err := trace.Open(*traceFile)
		if err != nil {
			fatal("failed to open plaintext trace file", err)
		}
		defer tw.Close()
		srv.AttachTrace(tw)
		logger.Warn("plaintext request/response trace enabled; file contains secrets", "trace_file", *traceFile)
	}
	if err := srv.AttachMarkerSaltStore(st); err != nil {
		logger.Warn("marker salt restore failed; starting with clean slate", "err", err)
	}
	var saltDone <-chan struct{}
	if saltDone = srv.SaltWriterDone(); saltDone != nil {
		go srv.RunMarkerSaltWriter(ctx)
	}

	// ---------- 账号映射：注册表 + 拉取器 + 账户↔出站绑定快照 ----------
	reg := acctmap.New()
	if err := syncer.ReloadFromStore(st, reg); err != nil {
		logger.Warn("acctmap restore failed; starting empty", "err", err)
	} else {
		logger.Info("acctmap restored", "entries", reg.Len())
	}
	egressTable, err := acctegress.LoadFromStore(ctx, st)
	if err != nil {
		fatal("failed to load account-egress bindings", err)
	}
	srv.AttachAcctMap(reg)
	srv.SwapAcctEgress(egressTable)
	reloadEgressBindings := func() {
		if t, err := acctegress.LoadFromStore(ctx, st); err == nil {
			srv.SwapAcctEgress(t)
		} else {
			logger.Warn("acct_egress rebuild failed; keeping previous snapshot", "err", err)
		}
	}
	mgr := syncer.New(st, reg, logger)
	mgr.OnMapChange = reloadEgressBindings // 同步快照级联 GC 会改动绑定表
	mgr.Updates = updateCh
	mgrDone := make(chan struct{})
	go func() {
		defer close(mgrDone)
		mgr.Run(ctx)
	}()

	_, ingressPort, _ := net.SplitHostPort(*addrFlag)
	apiH := api.New(api.Deps{
		Store: st, Settings: holder, CA: ca,
		SwapUpstreams: srv.SwapUpstreams, Logger: logger,
		AcctMap: reg, Syncer: mgr,
		SwapAcctEgress: srv.SwapAcctEgress,
		Updates:        updateCh,
		IngressPort:    ingressPort,
		IngressTLS:     snap.ListenTLSCert != "" && snap.ListenTLSKey != "",
	})
	srv.SetAdmin(apiH.Router(), webui.Handler())

	// ---------- 双监听：接入面与管理台硬拆，各自独立可选 TLS ----------
	// 监听地址由启动参数指定（每次启动生效，不入库）；证书路径成对配置即强制
	// HTTPS-only，文件内容变化（certbot 续期）由热重载跟进，路径变更需重启。
	listenAddr, adminAddr := *addrFlag, *adminFlag
	for _, a := range []struct{ what, addr string }{{"ingress", listenAddr}, {"admin", adminAddr}} {
		if _, port, err := net.SplitHostPort(a.addr); err != nil {
			fatal("invalid "+a.what+" listen address (want host:port)", err)
		} else if p, perr := strconv.Atoi(port); perr != nil || p < 1 || p > 65535 {
			fatal("invalid "+a.what+" listen port: "+port, nil)
		}
	}
	if listenAddr == adminAddr {
		fatal("ingress and admin listen addresses must differ: "+listenAddr, nil)
	}

	newListener := func(addr string, pair tlsreload.Pair, what string) net.Listener {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			fatal("failed to listen on "+addr, err)
		}
		if !pair.Enabled() {
			return ln
		}
		ld, err := tlsreload.New(pair, logger)
		if err != nil {
			fatal("failed to load "+what+" TLS certificate", err)
		}
		go ld.Run(ctx)
		logger.Info("tls enabled; plaintext connections will be rejected", "listener", what, "cert", pair.CertPath)
		return tls.NewListener(ln, ld.Config())
	}
	ingressPair := tlsreload.Pair{CertPath: snap.ListenTLSCert, KeyPath: snap.ListenTLSKey}
	adminPair := tlsreload.Pair{CertPath: snap.AdminTLSCert, KeyPath: snap.AdminTLSKey}
	ingressLn := newListener(listenAddr, ingressPair, "ingress")
	adminLn := newListener(adminAddr, adminPair, "admin")

	hsIngress := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	hsAdmin := &http.Server{Handler: srv.AdminHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}

	if host, _, e := net.SplitHostPort(listenAddr); e == nil && host != "127.0.0.1" && snap.ListenAuth == "" {
		slog.Warn("listening on non-loopback address without inbound auth; LAN abuse risk", "listen", listenAddr)
	}
	if host, _, e := net.SplitHostPort(adminAddr); e == nil && host != "127.0.0.1" && !adminPair.Enabled() {
		slog.Warn("admin listener exposed off loopback without TLS; consider configuring admin_tls_cert/admin_tls_key", "admin_listen", adminAddr)
	}
	logger.Info("started",
		"listen", ingressLn.Addr().String(),
		"admin_listen", adminLn.Addr().String(),
		"ingress_tls", snap.ListenTLSCert != "",
		"admin_tls", snap.AdminTLSCert != "",
		"data_dir", boot.DataDir,
		"fresh_install", boot.FreshInstall,
		"upstreams", len(items),
		"default_upstream", snap.DefaultUpstream,
		"no_marker_policy", snap.NoMarkerPolicy,
		"inbound_auth", snap.ListenAuth != "",
	)

	go func() { // 第二个 OS 信号 = 强杀；第一个已由 NotifyContext 发起优雅退出。
		first := true
		for range sigRaw {
			if first {
				first = false
				continue
			}
			slog.Warn("second signal received, forcing exit")
			os.Exit(1)
		}
	}()

	errCh := make(chan error, 2)
	go func() { errCh <- hsIngress.Serve(ingressLn) }()
	go func() { errCh <- hsAdmin.Serve(adminLn) }()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal("server exited unexpectedly", err)
		}
	case <-ctx.Done():
	}
	// ---------- 优雅退出：停 accept → 等隧道 → 排空审计 → 关库 ----------
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := hsIngress.Shutdown(shutdownCtx); err != nil {
		logger.Warn("ingress listener shutdown incomplete", "err", err)
	}
	if err := hsAdmin.Shutdown(shutdownCtx); err != nil {
		logger.Warn("admin server shutdown incomplete", "err", err)
	}
	if !srv.WaitTunnels(8 * time.Second) {
		logger.Warn("tunnels still active beyond deadline, giving up waiting")
	} else {
		logger.Debug("all active tunnels drained")
	}
	cancel()
	<-mgrDone
	<-updateDone
	<-writerDone
	if saltDone != nil {
		<-saltDone
	}
	<-retDone
	logger.Info("graceful shutdown complete")
}
