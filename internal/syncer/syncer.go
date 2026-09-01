// Package syncer 实现 acct_map 的拉取同步：从 CLIProxyAPI(cpa) 与 sub2api
// 定时拉取订阅号凭据，指纹化后快照写入 acct_map 并刷新内存注册表。
// 设计：docs/004-stable-account-hash-design.md §3。
//
// 上游对接形状（2026-08 实测）：
//   - cpa   GET /v0/management/auth-files           → {"files":[{name,email,provider,disabled,...}]}
//     GET /v0/management/auth-files/download?name=x.json → 认证文件原文 JSON
//     认证：Authorization: Bearer <management-key>
//   - sub2api GET /api/v1/admin/accounts/data       → {"data":{"accounts":[{name,platform,type,credentials{...}}]}}
//     认证：x-api-key: <admin-api-key>；凭据明文仅此导出接口返回（列表接口脱敏）
package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"mitmrouter/internal/acctmap"
	"mitmrouter/internal/store"
)

const (
	// MinIntervalSec 拉取间隔下限：防止把上游打挂。
	MinIntervalSec  = 60
	tickInterval    = 30 * time.Second
	fetchTimeout    = 60 * time.Second
	downloadWorkers = 6
	// 同步响应只包含账号元数据和凭据，限制上限避免异常源耗尽内存。
	maxSyncJSONBody = 16 << 20
)

// 平台白名单：白名单外的 provider/platform 跳过不入表（热路径只识别这些平台）。
var cpaPlatformMap = map[string]string{
	"codex": acctmap.PlatformOpenAI, "openai": acctmap.PlatformOpenAI,
	"claude": acctmap.PlatformAnthropic, "anthropic": acctmap.PlatformAnthropic,
	"gemini": acctmap.PlatformGemini, "antigravity": acctmap.PlatformGemini,
	"xai": acctmap.PlatformGrok, "grok": acctmap.PlatformGrok,
	"kimi": acctmap.PlatformKimi,
	"qwen": acctmap.PlatformQwen, "iflow": acctmap.PlatformIFlow,
}

var sub2apiPlatformMap = map[string]string{
	"openai": acctmap.PlatformOpenAI, "codex": acctmap.PlatformOpenAI,
	"anthropic": acctmap.PlatformAnthropic, "claude": acctmap.PlatformAnthropic,
	"gemini": acctmap.PlatformGemini,
	"grok":   acctmap.PlatformGrok, "xai": acctmap.PlatformGrok,
	"kimi": acctmap.PlatformKimi, "moonshot": acctmap.PlatformKimi,
	"deepseek": acctmap.PlatformDeepSeek,
	"zhipu":    acctmap.PlatformGLM, "glm": acctmap.PlatformGLM,
	"ollama": acctmap.PlatformOllama,
}

var (
	mapChangeMu      sync.Mutex
	registryReloadMu sync.Mutex
)

// WithMapChangeLock serializes Registry replacement and its dependent
// OnMapChange work across readers and management handlers.
func WithMapChangeLock(fn func() error) error {
	mapChangeMu.Lock()
	defer mapChangeMu.Unlock()
	return fn()
}

// Entry 是一次拉取解析出的中间产物：一个账号的 AT/RT。
type Entry struct {
	Platform string
	Account  string // 已归一化（小写）
	AtToken  string // access_token（可空）
	RtToken  string // refresh_token（可空）
}

func tail(tok string) string {
	if len(tok) <= 4 {
		return tok
	}
	return "…" + tok[len(tok)-4:]
}

// ---------- Manager ----------

// Manager 调度各启用源的周期拉取。零值不可用，须经 New。
type Manager struct {
	st   *store.Store
	reg  *acctmap.Registry
	log  *slog.Logger
	hc   *http.Client
	wake chan int64 // 手动触发：立即同步指定 source ID
	next map[int64]time.Time

	lifecycleMu sync.Mutex
	reconcileMu sync.Mutex
	runtimes    map[int64]*directRuntime
	locks       map[int64]*sync.Mutex
	runCtx      context.Context

	// OnMapChange 在 acct_map（及随级联清理的 acct_egress 绑定）落库并重载
	// 注册表之后回调；main 用它重建绑定快照。可 nil。
	// 回调运行在 mapChangeMu（WithMapChangeLock）之内：不得再次获取该锁，
	// 也不得调用最终会进入 WithMapChangeLock 的 API（如 ReloadFromStore），
	// 否则 sync.Mutex 不可重入，当前 goroutine 会永久自锁。
	OnMapChange func()

	// Updates 是映射更新记录的落库通道（docs/013-update-log-design.md）。
	// 可 nil（单测不接线）；写入永不阻塞，缓冲满时事件被丢弃。
	Updates chan<- store.UpdateEvent
}

// New 创建管理器。
func New(st *store.Store, reg *acctmap.Registry, logger *slog.Logger) *Manager {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	return &Manager{
		st:       st,
		reg:      reg,
		log:      logger,
		hc:       &http.Client{Timeout: fetchTimeout, Transport: transport},
		wake:     make(chan int64, 8),
		next:     map[int64]time.Time{},
		runtimes: map[int64]*directRuntime{},
		locks:    map[int64]*sync.Mutex{},
	}
}

// Wake 请求尽快同步指定源（页面「立即同步」按钮）。
func (m *Manager) Wake(sourceID int64) {
	select {
	case m.wake <- sourceID:
	default:
	}
}

// Run 阻塞运行调度循环；ctx 取消后停止所有 direct reader。启动时立即尝试一轮。
func (m *Manager) Run(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.lifecycleMu.Lock()
	m.runCtx = ctx
	m.lifecycleMu.Unlock()
	defer func() {
		m.reconcileMu.Lock()
		m.lifecycleMu.Lock()
		m.runCtx = nil
		m.lifecycleMu.Unlock()
		m.stopAllDirectRuntimes()
		m.reconcileMu.Unlock()
	}()
	if err := m.Reconcile(ctx); err != nil {
		m.directLogger().Warn("syncer: direct source reconcile failed", "err", err)
	}
	m.tickAll(ctx) // 启动即拉
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.Reconcile(ctx); err != nil {
				m.directLogger().Warn("syncer: direct source reconcile failed", "err", err)
			}
			m.tickAll(ctx)
		case id := <-m.wake:
			m.syncOne(ctx, id)
		}
	}
}

func (m *Manager) tickAll(ctx context.Context) {
	srcs, err := m.st.ListSyncSources(ctx)
	if err != nil {
		m.log.Error("syncer: list sources failed", "err", err)
		return
	}
	now := time.Now()
	for _, s := range srcs {
		if !s.Enabled {
			continue
		}
		interval := time.Duration(s.IntervalS) * time.Second
		if interval < MinIntervalSec*time.Second {
			interval = MinIntervalSec * time.Second
		}
		if nr, ok := m.next[s.ID]; ok && now.Before(nr) {
			continue
		}
		m.next[s.ID] = now.Add(interval)
		m.syncOne(ctx, s.ID)
	}
}

func (m *Manager) syncOne(ctx context.Context, id int64) {
	lock := m.sourceLock(id)
	lock.Lock()
	defer lock.Unlock()
	if err := contextErr(ctx); err != nil {
		return
	}
	src, ok, err := m.st.GetSyncSource(ctx, id)
	if err != nil || !ok {
		m.directLogger().Warn("syncer: source gone", "id", id, "err", err)
		return
	}
	if strings.TrimSpace(src.BaseURL) == "" {
		// 存量直读源可能没有 API 配置：全量没法跑；last_status 提示补全，
		// 不刷更新记录事件，已提示过也不再重复写（docs/012 v2 §10 迁移）。
		const hint = "error: please provide the API URL and key to enable scheduled full synchronization"
		if src.LastStatus != hint {
			_ = m.st.TouchSyncSourceStatus(ctx, id, hint)
		}
		return
	}
	key, err := m.st.GetSourceAPIKey(ctx, id)
	if err != nil {
		m.touchErrEvent(ctx, id, store.UpdateKindAPISync, sourceName(src), "read api key: "+err.Error(), "")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, fetchTimeout*2)
	defer cancel()
	var entries []Entry
	switch src.Kind {
	case acctmap.SourceKindCLIProxyAPI:
		entries, err = m.fetchCPA(cctx, src.BaseURL, key)
	case acctmap.SourceKindSub2API:
		entries, err = m.fetchSub2API(cctx, src.BaseURL, key)
	default:
		err = fmt.Errorf("unknown source kind %q", src.Kind)
	}
	if err != nil {
		m.touchErrEvent(ctx, id, store.UpdateKindAPISync, sourceName(src), err.Error(), "")
		return
	}
	source := sourceName(src)
	rows := make([]store.AcctUpsert, 0, len(entries))
	nTok := 0
	for _, e := range entries {
		rows = append(rows, entryToUpsert(e))
		if e.AtToken != "" {
			nTok++
		}
		if e.RtToken != "" {
			nTok++
		}
	}
	threshold := m.st.SyncEmptyClearThreshold(ctx)
	skipped, streak, err := m.st.ReplaceSourceSnapshotGuarded(ctx, source,
		acctmap.SourceTypeForKind(src.Kind), rows, threshold)
	if err != nil {
		m.touchErrEvent(ctx, id, store.UpdateKindAPISync, sourceName(src), "write snapshot: "+err.Error(), "")
		return
	}
	if skipped {
		// 空快照保护（docs/011 §2.3）：拉取连接正常但结果为空，且尚未连续达到
		// 阈值——保留该源名下映射行，避免同步闪断清掉账号及其出站绑定。
		_ = m.st.TouchSyncSource(ctx, id,
			fmt.Sprintf("ok: empty snapshot deferred %d/%d", streak, threshold))
		m.log.Warn("syncer: empty snapshot deferred by protection",
			"source", src.Name, "streak", streak, "threshold", threshold)
		m.emitUpdate(store.UpdateKindAPISync, sourceName(src), store.UpdateStatusOK,
			fmt.Sprintf("empty snapshot deferred %d/%d", streak, threshold), "")
		return
	}
	if err := m.finishMapChange(ctx); err != nil {
		m.touchErrEvent(ctx, id, store.UpdateKindAPISync, sourceName(src), "reload registry: "+err.Error(), "")
		return
	}
	status := fmt.Sprintf("ok: %d accounts, %d tokens", len(entries), nTok)
	if len(entries) == 0 {
		status = fmt.Sprintf("ok: empty snapshot cleared (streak %d/%d)", streak, threshold)
	}
	_ = m.st.TouchSyncSource(ctx, id, status)
	m.emitUpdate(store.UpdateKindAPISync, sourceName(src), store.UpdateStatusOK,
		fmt.Sprintf("%d accounts, %d tokens", len(entries), nTok), "")
	m.log.Info("syncer: source synced", "kind", src.Kind, "source", src.Name,
		"accounts", len(entries), "tokens", nTok)
}

func (m *Manager) touchErr(ctx context.Context, id int64, msg string) {
	_ = m.st.TouchSyncSource(ctx, id, "error: "+msg)
	m.log.Warn("syncer: source sync failed", "id", id, "err", msg)
}

// emitUpdate 记录一条映射更新事件（docs/013-update-log-design.md）。
func (m *Manager) emitUpdate(kind, source, status, summary, detail string) {
	store.SendUpdateEvent(m.Updates, store.NewUpdateEvent(kind, source, status, summary, detail))
}

// touchErrEvent 记录源失败状态（last_status + 日志），并按 docs/013 §3 同步
// 记一条同内容的 error 更新事件。事件摘要需要与 last_status 区分文案的
// 失败点（如 CPA 文件读取）仍分开调用 emitUpdate。
func (m *Manager) touchErrEvent(ctx context.Context, id int64, kind, source, msg, detail string) {
	m.touchErr(ctx, id, msg)
	m.emitUpdate(kind, source, store.UpdateStatusError, msg, detail)
}

// touchIncrEvent 增量失败专用：last_status 置 error 但不动 last_sync_at——
// 那两个字段只反映全量同步（docs/012 v2 §7）；同时记一条 error 更新事件。
func (m *Manager) touchIncrEvent(ctx context.Context, id int64, kind, source, msg, detail string) {
	_ = m.st.TouchSyncSourceStatus(ctx, id, "error: "+msg)
	m.log.Warn("syncer: incremental sync failed", "id", id, "err", msg)
	m.emitUpdate(kind, source, store.UpdateStatusError, msg, detail)
}

// touchReaderErr 增量 reader 生命周期失败（启动/重建失败）：同 touchIncrEvent
// 的字段约定，只写 last_status 不动 last_sync_at，但不产生更新记录事件。
func (m *Manager) touchReaderErr(ctx context.Context, id int64, msg string) {
	_ = m.st.TouchSyncSourceStatus(ctx, id, "error: "+msg)
	m.log.Warn("syncer: direct reader failed", "id", id, "err", msg)
}

// TestSource 用给定参数做一次只读连通性测试，返回摘要文本。
func (m *Manager) TestSource(ctx context.Context, kind, baseURL, key string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	var entries []Entry
	var err error
	switch kind {
	case acctmap.SourceKindCLIProxyAPI:
		entries, err = m.fetchCPA(cctx, baseURL, key)
	case acctmap.SourceKindSub2API:
		entries, err = m.fetchSub2API(cctx, baseURL, key)
	default:
		err = fmt.Errorf("unknown source kind %q", kind)
	}
	if err != nil {
		return "", err
	}
	n := 0
	for _, e := range entries {
		if e.AtToken != "" {
			n++
		}
		if e.RtToken != "" {
			n++
		}
	}
	return fmt.Sprintf("%d accounts, %d tokens", len(entries), n), nil
}

// ReloadFromStore 从库里全量重载注册表（任何 acct_map 写入后调用）。
func ReloadFromStore(st *store.Store, reg *acctmap.Registry) error {
	registryReloadMu.Lock()
	defer registryReloadMu.Unlock()
	return reloadFromStore(st, reg)
}

func reloadFromStore(st *store.Store, reg *acctmap.Registry) error {
	rows, err := st.LoadAcctMapAll(context.Background())
	if err != nil {
		return err
	}
	entries := make([]acctmap.Entry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, acctmap.Entry{
			Platform: r.Platform, Account: r.Account,
			AtFp: r.AtFP, RtFp: r.RtFP,
			AtHint: r.AtHint, RtHint: r.RtHint,
			Source: r.Source, SourceType: r.SourceType,
			UpdatedAt: r.UpdatedAt,
		})
	}
	reg.Reload(entries)
	return nil
}

// ---------- HTTP 公共 ----------

func (m *Manager) getJSON(ctx context.Context, url string, headers map[string]string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := m.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream %s: HTTP %d", strings.TrimSpace(url), resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSyncJSONBody+1))
	if err != nil {
		return fmt.Errorf("read upstream JSON: %w", err)
	}
	if len(raw) > maxSyncJSONBody {
		return fmt.Errorf("upstream JSON response exceeds %d bytes", maxSyncJSONBody)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	switch err := dec.Decode(&extra); {
	case err == io.EOF:
		return nil
	case err != nil:
		return fmt.Errorf("invalid trailing JSON: %w", err)
	default:
		return fmt.Errorf("upstream JSON contains multiple values")
	}
}
