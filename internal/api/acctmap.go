package api

// 账号映射管理面：拉取源 CRUD / 连通性测试 / 立即同步、映射预览与推送 upsert。
// 设计：docs/004-stable-account-hash-design.md §4-5。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"mitmrouter/internal/acctmap"
	"mitmrouter/internal/store"
	"mitmrouter/internal/syncer"
)

// SourceSyncer 是 syncer.Manager 中管理面用到能力的窄接口（便于测试替换）。
type SourceSyncer interface {
	TestSource(ctx context.Context, kind, baseURL, key string) (string, error)
	Wake(sourceID int64)
}

type directSourceManager interface {
	Reconcile(ctx context.Context) error
	ReconcileSource(ctx context.Context, sourceID int64) error
	DeleteSource(ctx context.Context, sourceID int64) error
	UpdateSourceConfig(ctx context.Context, row store.SyncSourceRow, apiKey, directDBDSN string, directDBSet, directDBClear bool) error
	TestDirectSource(ctx context.Context, sourceID int64) (string, error)
}

func (a *API) reconcileSource(sourceID int64) {
	if manager, ok := a.d.Syncer.(directSourceManager); ok {
		if err := manager.ReconcileSource(context.Background(), sourceID); err != nil {
			a.logger().Warn("direct source reconcile failed", "source_id", sourceID, "err", err)
		}
	}
}

func (a *API) finishMapChange() error {
	return syncer.WithMapChangeLock(func() error {
		if a.d.AcctMap != nil {
			if err := syncer.ReloadFromStore(a.d.Store, a.d.AcctMap); err != nil {
				return err
			}
		}
		a.reloadAcctEgressLocked()
		return nil
	})
}

func intClampInterval(v int) int {
	if v <= 0 {
		return 600
	}
	if v < syncer.MinIntervalSec {
		return syncer.MinIntervalSec
	}
	return v
}

type sourceDTO struct {
	ID                 int64  `json:"id"`
	Kind               string `json:"kind"`
	Name               string `json:"name"`
	BaseURL            string `json:"base_url"`
	DirectAuthDir      string `json:"direct_auth_dir,omitempty"`
	DirectDBConfigured bool   `json:"direct_db_configured,omitempty"`
	IntervalS          int    `json:"interval_s"`
	Enabled            bool   `json:"enabled"`
	LastSyncAt         int64  `json:"last_sync_at"`
	LastStatus         string `json:"last_status"`
}

func (a *API) listSources(w http.ResponseWriter, r *http.Request) {
	rows, err := a.d.Store.ListSyncSources(ctxBG())
	if err != nil {
		a.failInternal(w, r, err)
		return
	}
	out := make([]sourceDTO, 0, len(rows))
	for _, s := range rows {
		out = append(out, sourceDTO{
			ID: s.ID, Kind: s.Kind, Name: s.Name,
			BaseURL: sourceURLForDisplay(s.BaseURL), DirectAuthDir: s.DirectAuthDir,
			DirectDBConfigured: s.DirectDBSecret != "",
			IntervalS:          s.IntervalS, Enabled: s.Enabled,
			LastSyncAt: s.LastSyncAt, LastStatus: s.LastStatus,
		})
	}
	writeJSON(w, 200, map[string]any{"items": out, "total": len(out)})
}

type sourceBody struct {
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	BaseURL       string `json:"base_url"`
	APIKey        string `json:"api_key"`
	DirectDBDSN   string `json:"direct_db_dsn"`
	DirectDBClear *bool  `json:"direct_db_clear"` // 置 true 清除 Sub2API 增量 DSN（停增量）
	// 指针语义：nil = 沿用旧值（未提交该字段）；非 nil = 按提交值设置，空串即清空。
	DirectAuthDir *string `json:"direct_auth_dir"`
	IntervalS     int     `json:"interval_s"`
	Enabled       *bool   `json:"enabled"`
}

var validSourceKinds = map[string]bool{
	acctmap.SourceKindCLIProxyAPI: true,
	acctmap.SourceKindSub2API:     true,
}

const (
	invalidSourceKindMessage  = "kind must be " + acctmap.SourceKindCLIProxyAPI + " or " + acctmap.SourceKindSub2API
	directDBOnlyMessage       = "direct_db_dsn only applies to " + acctmap.SourceKindSub2API + " sources"
	directAuthOnlyMessage     = "direct_auth_dir only applies to " + acctmap.SourceKindCLIProxyAPI + " sources"
	directDBUpdateOnlyMessage = "direct_db_dsn/direct_db_clear only apply to " + acctmap.SourceKindSub2API + " sources"
)

func validateSourceBaseURL(raw string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid source URL: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return "", errors.New("source URL scheme must be http or https")
	}
	if u.Hostname() == "" {
		return "", errors.New("source URL must include a host")
	}
	if u.User != nil {
		return "", errors.New("source URL must not contain userinfo")
	}
	if u.RawQuery != "" {
		return "", errors.New("source URL must not contain query parameters")
	}
	if u.Fragment != "" {
		return "", errors.New("source URL must not contain a fragment")
	}
	return base, nil
}

func sourceURLForDisplay(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/")
}

func validateDirectAuthDir(raw string) (string, error) {
	dir := filepath.Clean(strings.TrimSpace(raw))
	if dir == "." || dir == "" || !filepath.IsAbs(dir) {
		return "", errors.New("direct auth directory must be an absolute path")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("direct auth directory is not accessible: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("direct auth directory must not be a symlink")
	}
	if !info.IsDir() {
		return "", errors.New("direct auth path must be a directory")
	}
	return dir, nil
}

func (a *API) createSource(w http.ResponseWriter, r *http.Request) {
	var b sourceBody
	if err := readJSON(r, &b); err != nil {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}
	b.Kind = strings.ToLower(strings.TrimSpace(b.Kind))
	b.Name = strings.TrimSpace(b.Name)
	if !validSourceKinds[b.Kind] {
		writeErr(w, 400, "bad_request", invalidSourceKindMessage)
		return
	}
	if b.Name == "" {
		writeErr(w, 400, "bad_request", "name required")
		return
	}
	// 全量同步是每个 source 的主干：base_url/api_key 必填；
	// 增量路径选填，填了即启用（docs/012 v2）。
	base, err := validateSourceBaseURL(b.BaseURL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	apiKey := strings.TrimSpace(b.APIKey)
	if apiKey == "" {
		writeErr(w, 400, "bad_request", "api_key required")
		return
	}
	cfg := store.SyncSourceConfig{
		Kind: b.Kind, Name: b.Name, BaseURL: base,
		IntervalS: intClampInterval(b.IntervalS),
		Enabled:   b.Enabled == nil || *b.Enabled,
	}
	switch b.Kind {
	case acctmap.SourceKindCLIProxyAPI:
		if strings.TrimSpace(b.DirectDBDSN) != "" {
			writeErr(w, 400, "bad_request", directDBOnlyMessage)
			return
		}
		if b.DirectAuthDir != nil && strings.TrimSpace(*b.DirectAuthDir) != "" {
			dir, err := validateDirectAuthDir(*b.DirectAuthDir)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
				return
			}
			cfg.DirectAuthDir = dir
		}
	case acctmap.SourceKindSub2API:
		if b.DirectAuthDir != nil && strings.TrimSpace(*b.DirectAuthDir) != "" {
			writeErr(w, 400, "bad_request", directAuthOnlyMessage)
			return
		}
		cfg.DirectDBDSN = strings.TrimSpace(b.DirectDBDSN)
	}
	id, err := a.d.Store.CreateSyncSourceConfig(ctxBG(), cfg, apiKey)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeErr(w, 409, "conflict", "source name already exists")
			return
		}
		a.failInternal(w, r, err)
		return
	}
	a.reconcileSource(id)
	writeJSON(w, 200, map[string]any{"id": id})
}

func (a *API) updateSource(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad_request", "invalid id")
		return
	}
	old, ok, err := a.d.Store.GetSyncSource(ctxBG(), id)
	if err != nil {
		a.failInternal(w, r, err)
		return
	}
	if !ok {
		writeErr(w, 404, "not_found", "source not found")
		return
	}
	var b sourceBody
	if err := readJSON(r, &b); err != nil {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}
	nk := strings.ToLower(strings.TrimSpace(b.Kind))
	if nk == "" {
		nk = old.Kind
	}
	if !validSourceKinds[nk] {
		writeErr(w, 400, "bad_request", invalidSourceKindMessage)
		return
	}
	name := strings.TrimSpace(b.Name)
	if name == "" {
		name = old.Name
	}
	interval := old.IntervalS
	if b.IntervalS > 0 {
		interval = intClampInterval(b.IntervalS)
	}
	enabled := old.Enabled
	if b.Enabled != nil {
		enabled = *b.Enabled
	}

	row := old
	row.Kind, row.Name = nk, name
	row.IntervalS, row.Enabled = interval, enabled

	// 全量同步配置：base_url 必填（新值或沿用旧值），api_key 选填（空=沿用）。
	// 例外：存量直读源本来就没有 API 配置（base_url 为空），允许继续留空——
	// 否则这类源连“清空增量路径停 reader”都做不到（docs/012 v2 §10 迁移）；
	// 调度侧会在 last_status 里提示补全。
	base := old.BaseURL
	if strings.TrimSpace(b.BaseURL) != "" {
		base, err = validateSourceBaseURL(b.BaseURL)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	} else if strings.TrimSpace(base) != "" {
		if _, vErr := validateSourceBaseURL(base); vErr != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "stored source URL is invalid; provide a replacement base_url")
			return
		}
	}
	row.BaseURL = base
	apiKey := ""
	if k := strings.TrimSpace(b.APIKey); k != "" && k != keepToken && k != maskToken {
		apiKey = k
	} else if strings.TrimSpace(row.BaseURL) != "" {
		// 源带全量配置就必须有 api_key；存量直读源留空时不需要
		if _, keyErr := a.d.Store.GetSourceAPIKey(ctxBG(), id); keyErr != nil {
			writeErr(w, 400, "bad_request", "api_key required")
			return
		}
	}

	// 增量路径（选填）：随 kind 校验；CPA 目录与 Sub2API DSN 都可清空
	// （空串 / direct_db_clear），清空即停。
	directDBDSN := ""
	directDBSet := false
	directDBClear := false
	switch nk {
	case acctmap.SourceKindCLIProxyAPI:
		if strings.TrimSpace(b.DirectDBDSN) != "" || (b.DirectDBClear != nil && *b.DirectDBClear) {
			writeErr(w, 400, "bad_request", directDBUpdateOnlyMessage)
			return
		}
		dir := old.DirectAuthDir
		if b.DirectAuthDir != nil {
			dir = strings.TrimSpace(*b.DirectAuthDir)
		}
		if dir != "" {
			dir, err = validateDirectAuthDir(dir)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
				return
			}
		}
		row.DirectAuthDir = dir
	case acctmap.SourceKindSub2API:
		row.DirectAuthDir = ""
		if b.DirectAuthDir != nil && strings.TrimSpace(*b.DirectAuthDir) != "" {
			writeErr(w, 400, "bad_request", directAuthOnlyMessage)
			return
		}
		if b.DirectDBClear != nil && *b.DirectDBClear {
			if strings.TrimSpace(b.DirectDBDSN) != "" {
				writeErr(w, 400, "bad_request", "direct_db_clear and direct_db_dsn are mutually exclusive")
				return
			}
			directDBClear = true
		}
		if dsn := strings.TrimSpace(b.DirectDBDSN); dsn != "" && dsn != keepToken && dsn != maskToken {
			directDBDSN, directDBSet = dsn, true
		}
	}
	ctx := ctxBG()
	var updateErr error
	if manager, ok := a.d.Syncer.(directSourceManager); ok {
		// Manager.UpdateSourceConfig waits for the source lock, commits the new
		// config, then reconciles the incremental reader for this source.
		updateErr = manager.UpdateSourceConfig(ctx, row, apiKey, directDBDSN, directDBSet, directDBClear)
	} else {
		updateErr = a.d.Store.UpdateSyncSourceConfig(ctx, row, apiKey, directDBDSN, directDBSet, directDBClear)
		if updateErr == nil {
			a.reconcileSource(id)
		}
	}
	if updateErr != nil {
		if strings.Contains(updateErr.Error(), "UNIQUE") {
			writeErr(w, 409, "conflict", "source name already exists")
			return
		}
		a.failInternal(w, r, updateErr)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *API) deleteSource(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad_request", "invalid id")
		return
	}
	if _, ok, err := a.d.Store.GetSyncSource(ctxBG(), id); err != nil {
		a.failInternal(w, r, err)
		return
	} else if !ok {
		writeErr(w, 404, "not_found", "source not found")
		return
	}
	if manager, ok := a.d.Syncer.(directSourceManager); ok {
		if err := manager.DeleteSource(ctxBG(), id); err != nil {
			a.failInternal(w, r, err)
			return
		}
	} else if err := a.d.Store.DeleteSyncSource(ctxBG(), id); err != nil {
		a.failInternal(w, r, err)
		return
	}
	if err := a.finishMapChange(); err != nil {
		a.failInternal(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// testSource 测试连通性。默认测试 API 全量同步（body 可给临时 base_url/api_key
// 建源前预检，否则用已存配置）；`?target=incremental` 测试已保存的增量路径
// （Sub2API 数据库连接 / CPA 目录 watcher），不改动 acct_map。
func (a *API) testSource(w http.ResponseWriter, r *http.Request) {
	if a.d.Syncer == nil {
		writeErr(w, 503, "unavailable", "syncer not running")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	src, ok, err := a.d.Store.GetSyncSource(ctxBG(), id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		a.failInternal(w, r, err)
		return
	}
	if !ok || errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "source not found")
		return
	}
	var b sourceBody
	if r.Body != nil && r.Body != http.NoBody {
		if err := readJSON(r, &b); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}
	kind := strings.ToLower(strings.TrimSpace(firstNonEmptyStr(b.Kind, src.Kind)))
	if !validSourceKinds[kind] {
		writeErr(w, http.StatusBadRequest, "bad_request", invalidSourceKindMessage)
		return
	}
	if r.URL.Query().Get("target") == "incremental" {
		manager, ok := a.d.Syncer.(directSourceManager)
		if !ok {
			writeErr(w, http.StatusServiceUnavailable, "unavailable", "direct reader not running")
			return
		}
		summary, err := manager.TestDirectSource(r.Context(), id)
		if err != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "summary": summary})
		return
	}
	if strings.TrimSpace(b.BaseURL) == "" && strings.TrimSpace(src.BaseURL) == "" {
		// 存量直读源没有 API 配置：直说缺什么，别抛 URL 解析错误
		writeErr(w, http.StatusBadRequest, "bad_request", "source has no full-sync API config; save base_url and api_key first")
		return
	}
	base, err := validateSourceBaseURL(firstNonEmptyStr(b.BaseURL, src.BaseURL))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	key := strings.TrimSpace(b.APIKey)
	if key == "" || key == maskToken || key == keepToken {
		key, err = a.d.Store.GetSourceAPIKey(ctxBG(), id)
		if err != nil {
			a.failInternal(w, r, err)
			return
		}
	}
	if base == "" || key == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "kind/base_url/api_key incomplete")
		return
	}
	summary, err := a.d.Syncer.TestSource(r.Context(), kind, base, key)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "summary": summary})
}

func (a *API) syncSourceNow(w http.ResponseWriter, r *http.Request) {
	if a.d.Syncer == nil {
		writeErr(w, 503, "unavailable", "syncer not running")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad_request", "invalid id")
		return
	}
	if _, ok, _ := a.d.Store.GetSyncSource(ctxBG(), id); !ok {
		writeErr(w, 404, "not_found", "source not found")
		return
	}
	a.d.Syncer.Wake(id)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 映射表 ----------

// listAcctMap 预览：内存过滤 + 分页。表规模小，全量加载可接受。
func (a *API) listAcctMap(w http.ResponseWriter, r *http.Request) {
	if a.d.AcctMap == nil {
		writeJSON(w, 200, map[string]any{"items": []any{}, "total": 0})
		return
	}
	q := r.URL.Query()
	pf := strings.ToLower(strings.TrimSpace(q.Get("platform")))
	acct := strings.ToLower(strings.TrimSpace(q.Get("account")))
	source := strings.TrimSpace(q.Get("source"))
	stype := strings.TrimSpace(q.Get("source_type"))
	// binding 只认 bound/unbound，其余值（含空）视同不筛选，与 platform 等
	// 参数的宽松处理一致。
	binding := strings.ToLower(strings.TrimSpace(q.Get("binding")))

	items := a.d.AcctMap.Snapshot()
	sort.Slice(items, func(i, j int) bool {
		if items[i].Platform != items[j].Platform {
			return items[i].Platform < items[j].Platform
		}
		if items[i].SourceType != items[j].SourceType {
			return items[i].SourceType < items[j].SourceType
		}
		return items[i].Account < items[j].Account
	})
	// 绑定挂在 (platform, account) 上，与来源实例无关；仅筛选时才查一次绑定表。
	var bound map[string]bool
	if binding == "bound" || binding == "unbound" {
		rows, err := a.d.Store.ListAcctEgress(ctxBG())
		if err != nil {
			a.failInternal(w, r, err)
			return
		}
		bound = make(map[string]bool, len(rows))
		for _, row := range rows {
			bound[row.Platform+"\x00"+row.Account] = true
		}
	}
	filtered := make([]acctmap.Entry, 0, len(items))
	for _, e := range items {
		if pf != "" && e.Platform != pf {
			continue
		}
		if acct != "" && !strings.Contains(e.Account, acct) {
			continue
		}
		if source != "" && e.Source != source {
			continue
		}
		if stype != "" && e.SourceType != stype {
			continue
		}
		if bound != nil && bound[e.Platform+"\x00"+e.Account] != (binding == "bound") {
			continue
		}
		filtered = append(filtered, e)
	}
	page, pageSize := parsePage(q)
	start, end := pageBounds(len(filtered), page, pageSize)

	type rowDTO struct {
		Platform   string `json:"platform"`
		Account    string `json:"account"`
		AtFP       string `json:"at_fp"`
		RtFP       string `json:"rt_fp"`
		AtHint     string `json:"at_hint"`
		RtHint     string `json:"rt_hint"`
		Source     string `json:"source"`
		SourceType string `json:"source_type"`
		UpdatedAt  int64  `json:"updated_at"`
	}
	out := make([]rowDTO, 0, end-start)
	for _, e := range filtered[start:end] {
		out = append(out, rowDTO{
			Platform: e.Platform, Account: e.Account,
			AtFP: e.AtFp, RtFP: e.RtFp, AtHint: e.AtHint, RtHint: e.RtHint,
			Source: e.Source, SourceType: e.SourceType,
			UpdatedAt: e.UpdatedAt,
		})
	}
	writeJSON(w, 200, map[string]any{"items": out, "total": len(filtered)})
}

func (a *API) acctMapStats(w http.ResponseWriter, r *http.Request) {
	if a.d.AcctMap == nil {
		writeJSON(w, 200, map[string]any{"total": 0})
		return
	}
	items := a.d.AcctMap.Snapshot()
	byPf := map[string]int{}
	bySrc := map[string]int{}
	accts := map[string]bool{}
	for _, e := range items {
		byPf[e.Platform]++
		bySrc[e.SourceType]++
		accts[e.Platform+"\x00"+e.Account] = true
	}
	writeJSON(w, 200, map[string]any{
		"total":       len(items), // 行数：同账号不同来源实例/类型各一行
		"accounts":    len(accts), // 去重账号数
		"by_platform": byPf,
		"by_source":   bySrc, // 按来源类型全名分组
	})
}

// putAcctMapAccount 推送通道：upsert 单账号凭据集（source='api'，同账号同类型
// 一行；source_type 由调用方指定，可为任意非空自定义类型）。
func (a *API) putAcctMapAccount(w http.ResponseWriter, r *http.Request) {
	pf := strings.ToLower(strings.TrimSpace(r.PathValue("platform")))
	account := acctmap.NormalizeAccount(r.PathValue("account"))
	if pf == "" || account == "" {
		writeErr(w, 400, "bad_request", "platform/account required")
		return
	}
	var b struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		SourceType   string `json:"source_type"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}
	st := strings.TrimSpace(b.SourceType)
	if st == "" {
		writeErr(w, 400, "bad_request", "source_type required")
		return
	}
	if len(st) > 64 {
		writeErr(w, 400, "bad_request", "source_type too long (max 64)")
		return
	}
	at := acctmap.NormalizeCred(b.AccessToken)
	rt := acctmap.NormalizeCred(b.RefreshToken)
	if at == "" && rt == "" {
		writeErr(w, 400, "bad_request", "access_token/refresh_token at least one required")
		return
	}
	up := store.AcctUpsert{Platform: pf, Account: account}
	if at != "" {
		up.AtFP = acctmap.Fingerprint(pf, at)
		up.AtHint = tailOf(at)
	}
	if rt != "" {
		up.RtFP = acctmap.Fingerprint(pf, rt)
		up.RtHint = tailOf(rt)
	}
	if err := a.d.Store.ReplaceAccountSnapshot(ctxBG(), pf, account, st, up); err != nil {
		a.failInternal(w, r, err)
		a.emitUpdate(store.UpdateKindPush, acctmap.SourcePush, store.UpdateStatusError, "write failed: "+err.Error(), pf+"/"+account)
		return
	}
	if err := a.finishMapChange(); err != nil {
		a.failInternal(w, r, err)
		a.emitUpdate(store.UpdateKindPush, acctmap.SourcePush, store.UpdateStatusError, "reload failed: "+err.Error(), pf+"/"+account)
		return
	}
	a.emitUpdate(store.UpdateKindPush, acctmap.SourcePush, store.UpdateStatusOK, pf+"/"+account, "")
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *API) deleteAcctMapAccount(w http.ResponseWriter, r *http.Request) {
	pf := strings.ToLower(strings.TrimSpace(r.PathValue("platform")))
	account := acctmap.NormalizeAccount(r.PathValue("account"))
	if pf == "" || account == "" {
		writeErr(w, 400, "bad_request", "platform/account required")
		return
	}
	source := strings.TrimSpace(r.URL.Query().Get("source")) // 空 = 不限来源实例
	n, err := a.d.Store.DeleteAcctMapAccount(ctxBG(), pf, account, source)
	if err != nil {
		a.failInternal(w, r, err)
		a.emitUpdate(store.UpdateKindDelete, source, store.UpdateStatusError, "delete failed: "+err.Error(), pf+"/"+account)
		return
	}
	if err := a.finishMapChange(); err != nil {
		a.logger().Error("failed to rebuild account map after account deletion", "err", err)
	}
	if n > 0 { // 没删到行就不记：更新记录只收真实发生的映射变更
		a.emitUpdate(store.UpdateKindDelete, source, store.UpdateStatusOK,
			fmt.Sprintf("%s/%s, removed %d", pf, account, n), "")
	}
	writeJSON(w, 200, map[string]any{"ok": true, "removed": n})
}

func (a *API) deleteAcctMapToken(w http.ResponseWriter, r *http.Request) {
	pf := strings.ToLower(strings.TrimSpace(r.PathValue("platform")))
	account := acctmap.NormalizeAccount(r.PathValue("account"))
	fp := strings.TrimSpace(r.PathValue("fp"))
	field, src, ok, err := a.d.Store.ClearAcctMapFp(ctxBG(), pf, account, fp)
	if err != nil {
		a.failInternal(w, r, err)
		a.emitUpdate(store.UpdateKindDelete, src, store.UpdateStatusError, "delete failed: "+err.Error(), pf+"/"+account)
		return
	}
	if !ok {
		writeErr(w, 404, "not_found", "token not found under this account")
		a.emitUpdate(store.UpdateKindDelete, src, store.UpdateStatusError, "token not found under this account", pf+"/"+account)
		return
	}
	if err := a.finishMapChange(); err != nil {
		a.logger().Error("failed to rebuild account map after token deletion", "err", err)
	}
	a.emitUpdate(store.UpdateKindDelete, src, store.UpdateStatusOK,
		fmt.Sprintf("%s/%s (%s %s)", pf, account, field, tailOf(fp)), "")
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 小工具 ----------

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func tailOf(t string) string {
	if len(t) <= 4 {
		return t
	}
	return "…" + t[len(t)-4:]
}

// parsePage 解析分页参数并钳位。
func parsePage(q map[string][]string) (page, size int) {
	get := func(k string, def int) int {
		if len(q[k]) == 0 {
			return def
		}
		v, err := strconv.Atoi(q[k][0])
		if err != nil || v < 1 {
			return def
		}
		return v
	}
	page = get("page", 1)
	size = get("page_size", 50)
	// 单页上限 2000：绑定出站弹窗按页循环拉取全部账号（docs/011 §5），
	// 避免旧上限 500 把大账号库静默截断。
	if size > 2000 {
		size = 2000
	}
	return page, size
}

// pageBounds 计算内存切片的安全边界。先用数据长度判断页码是否超出范围，
// 只有乘法结果已被证明不超过 length 时才计算 offset，避免 int 溢出。
func pageBounds(length, page, size int) (start, end int) {
	if length == 0 || page < 1 || size < 1 {
		return 0, 0
	}
	pageOffset := page - 1
	if pageOffset > length/size {
		return length, length
	}
	start = pageOffset * size
	if start >= length {
		return length, length
	}
	end = start + size
	if end > length {
		end = length
	}
	return start, end
}
