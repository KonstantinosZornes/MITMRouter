package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mitmrouter/internal/acctmap"
	"mitmrouter/internal/store"
)

type directRuntime struct {
	manager  *Manager
	source   store.SyncSourceRow
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	dbReader *Sub2APIDirectReader
	cpaDir   *CPADirectReader
}

// Reconcile starts, stops, and retains direct readers according to persisted source configuration.
func (m *Manager) Reconcile(ctx context.Context) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	return m.reconcileLocked(ctx, 0, false)
}

// ReconcileSource forces one source to be recreated after its direct configuration changes.
func (m *Manager) ReconcileSource(ctx context.Context, sourceID int64) error {
	if sourceID <= 0 {
		return errors.New("source ID must be positive")
	}
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	err := m.reconcileLocked(ctx, sourceID, true)
	if err != nil {
		m.touchReaderErr(ctx, sourceID, "reconcile direct source: "+err.Error())
	}
	return err
}

func (m *Manager) reconcileLocked(ctx context.Context, onlyID int64, force bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	sources, err := m.st.ListSyncSources(ctx)
	if err != nil {
		return fmt.Errorf("list sync sources: %w", err)
	}
	// 没有全局开关（docs/012 v2）：启用条件只有一条——源启用且填了增量路径。
	desired := make(map[int64]store.SyncSourceRow)
	for _, source := range sources {
		if source.Enabled && hasIncrementalPath(source) {
			desired[source.ID] = source
		}
	}

	var stopIDs []int64
	m.lifecycleMu.Lock()
	for id, runtime := range m.runtimes {
		want, ok := desired[id]
		if onlyID > 0 && id != onlyID {
			continue
		}
		if !ok || force || !runtimeMatches(runtime, want) {
			stopIDs = append(stopIDs, id)
		}
	}
	m.lifecycleMu.Unlock()
	for _, id := range stopIDs {
		if err := m.stopDirectRuntime(id); err != nil {
			return err
		}
	}

	if onlyID > 0 {
		if source, ok := desired[onlyID]; ok {
			return m.ensureDirectRuntime(source)
		}
		return nil
	}
	for _, source := range desired {
		if err := m.ensureDirectRuntime(source); err != nil {
			m.touchReaderErr(ctx, source.ID, "start direct reader: "+err.Error())
		}
	}
	return nil
}

// hasIncrementalPath 判断 source 是否配置了增量直读路径：sub2api 看数据库
// DSN secret，cpa 看认证文件目录。填了即启用，清空即停，没有别的开关。
func hasIncrementalPath(source store.SyncSourceRow) bool {
	switch source.Kind {
	case acctmap.SourceKindSub2API:
		return strings.TrimSpace(source.DirectDBSecret) != ""
	case acctmap.SourceKindCLIProxyAPI:
		return strings.TrimSpace(source.DirectAuthDir) != ""
	default:
		return false
	}
}

// runtimeMatches 判断现有 reader 是否仍与最新配置一致；不一致由 Reconcile 重建。
func runtimeMatches(runtime *directRuntime, source store.SyncSourceRow) bool {
	if runtime == nil {
		return false
	}
	return runtime.source.ID == source.ID && runtime.source.Kind == source.Kind &&
		runtime.source.DirectAuthDir == source.DirectAuthDir &&
		runtime.source.DirectDBSecret == source.DirectDBSecret
}

func (m *Manager) ensureDirectRuntime(source store.SyncSourceRow) error {
	m.lifecycleMu.Lock()
	if existing := m.runtimes[source.ID]; existing != nil && runtimeMatches(existing, source) {
		m.lifecycleMu.Unlock()
		return nil
	}
	m.lifecycleMu.Unlock()

	rootCtx, running := m.context()
	if !running {
		// Configuration may be saved before the manager starts; Run will
		// reconcile and start the reader during normal process startup.
		return nil
	}
	runtime, err := m.newDirectRuntime(rootCtx, source)
	if err != nil {
		return err
	}
	m.lifecycleMu.Lock()
	if existing := m.runtimes[source.ID]; existing != nil {
		m.lifecycleMu.Unlock()
		runtime.closeUnstarted()
		return nil
	}
	m.runtimes[source.ID] = runtime
	m.lifecycleMu.Unlock()
	go runtime.run(runtime.ctx)
	return nil
}

func (m *Manager) newDirectRuntime(parent context.Context, source store.SyncSourceRow) (*directRuntime, error) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	runtime := &directRuntime{
		manager: m,
		source:  source,
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	switch source.Kind {
	case acctmap.SourceKindSub2API:
		dsn, err := m.st.GetSourceDirectDB(context.Background(), source.ID)
		if err != nil {
			cancel()
			return nil, errors.New("read direct Sub2API database configuration")
		}
		runtime.dbReader, err = NewSub2APIDirectReader(ctx, dsn)
		if err != nil {
			cancel()
			return nil, err
		}
	case acctmap.SourceKindCLIProxyAPI:
		cpaReader, err := NewCPADirectReader(source.DirectAuthDir)
		if err != nil {
			cancel()
			return nil, err
		}
		runtime.cpaDir = cpaReader
		if err := runtime.cpaDir.Start(ctx); err != nil {
			_ = runtime.cpaDir.Close()
			cancel()
			return nil, err
		}
	default:
		cancel()
		return nil, fmt.Errorf("unknown direct source kind %q", source.Kind)
	}
	return runtime, nil
}

func (m *Manager) context() (context.Context, bool) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.runCtx != nil {
		return m.runCtx, true
	}
	return nil, false
}

func (m *Manager) sourceLock(sourceID int64) *sync.Mutex {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.locks == nil {
		m.locks = make(map[int64]*sync.Mutex)
	}
	if lock := m.locks[sourceID]; lock != nil {
		return lock
	}
	lock := &sync.Mutex{}
	m.locks[sourceID] = lock
	return lock
}

func (m *Manager) stopDirectRuntime(sourceID int64) error {
	m.lifecycleMu.Lock()
	runtime := m.runtimes[sourceID]
	delete(m.runtimes, sourceID)
	m.lifecycleMu.Unlock()
	if runtime == nil {
		return nil
	}
	return runtime.stop()
}

func (m *Manager) stopAllDirectRuntimes() {
	m.lifecycleMu.Lock()
	runtimes := make([]*directRuntime, 0, len(m.runtimes))
	for _, runtime := range m.runtimes {
		runtimes = append(runtimes, runtime)
	}
	m.runtimes = make(map[int64]*directRuntime)
	m.lifecycleMu.Unlock()
	for _, runtime := range runtimes {
		_ = runtime.stop()
	}
}

// DeleteSource stops direct work and deletes the source while holding the same
// source lock used by API and direct synchronization.
func (m *Manager) DeleteSource(ctx context.Context, sourceID int64) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	if err := m.stopDirectRuntime(sourceID); err != nil {
		return err
	}
	lock := m.sourceLock(sourceID)
	lock.Lock()
	err := m.st.DeleteSyncSource(ctx, sourceID)
	lock.Unlock()
	if err == nil {
		m.lifecycleMu.Lock()
		delete(m.locks, sourceID)
		m.lifecycleMu.Unlock()
	}
	return err
}

// UpdateSourceConfig serializes source configuration changes with API and direct
// work. It waits for any in-flight sync on the source before committing, so an
// old snapshot or delta can never be committed after new configuration.
func (m *Manager) UpdateSourceConfig(ctx context.Context, row store.SyncSourceRow, apiKey, directDBDSN string, directDBSet, directDBClear bool) error {
	if row.ID <= 0 {
		return errors.New("source ID must be positive")
	}
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	if err := m.stopDirectRuntime(row.ID); err != nil {
		return err
	}
	lock := m.sourceLock(row.ID)
	lock.Lock()
	err := m.st.UpdateSyncSourceConfig(ctx, row, apiKey, directDBDSN, directDBSet, directDBClear)
	lock.Unlock()
	if err != nil {
		if restoreErr := m.reconcileLocked(ctx, row.ID, true); restoreErr != nil {
			m.directLogger().Warn("syncer: failed to restore source reader after source update failure", "source_id", row.ID, "err", restoreErr)
		}
		return err
	}
	return m.reconcileLocked(ctx, row.ID, true)
}

// TestDirectSource performs a read-only direct source check without changing acct_map.
func (m *Manager) TestDirectSource(ctx context.Context, sourceID int64) (string, error) {
	lock := m.sourceLock(sourceID)
	lock.Lock()
	defer lock.Unlock()
	source, ok, err := m.st.GetSyncSource(ctx, sourceID)
	if err != nil {
		return "", err
	}
	if !ok || !hasIncrementalPath(source) {
		return "", errors.New("source has no incremental path configured")
	}
	switch source.Kind {
	case acctmap.SourceKindSub2API:
		dsn, err := m.st.GetSourceDirectDB(ctx, sourceID)
		if err != nil {
			return "", errors.New("read direct Sub2API database configuration")
		}
		reader, err := NewSub2APIDirectReader(ctx, dsn)
		if err != nil {
			return "", err
		}
		defer reader.Close()
		return "database connection ok", nil
	case acctmap.SourceKindCLIProxyAPI:
		reader, err := NewCPADirectReader(source.DirectAuthDir)
		if err != nil {
			return "", err
		}
		if err := reader.Start(ctx); err != nil {
			_ = reader.Close()
			return "", err
		}
		if err := reader.Close(); err != nil {
			return "", err
		}
		return "auth directory watcher ready", nil
	default:
		return "", fmt.Errorf("unknown direct source kind %q", source.Kind)
	}
}

func (runtime *directRuntime) stop() error {
	if runtime == nil {
		return nil
	}
	runtime.cancel()
	<-runtime.done
	return nil
}

// closeUnstarted releases a runtime whose run goroutine was never started.
// It must not wait on done: only run closes that channel, so waiting here
// would block forever.
func (runtime *directRuntime) closeUnstarted() {
	if runtime == nil {
		return
	}
	runtime.cancel()
	if runtime.dbReader != nil {
		_ = runtime.dbReader.Close()
	}
	if runtime.cpaDir != nil {
		_ = runtime.cpaDir.Close()
	}
}

// run 驱动单个 source 的增量 reader：Sub2API 先跑一轮再按固定间隔轮询，
// CPA 消费文件事件。增量频率是固定默认值，不读配置（docs/012 v2）。
func (runtime *directRuntime) run(parent context.Context) {
	defer close(runtime.done)
	defer func() {
		if runtime.dbReader != nil {
			_ = runtime.dbReader.Close()
		}
		if runtime.cpaDir != nil {
			_ = runtime.cpaDir.Close()
		}
	}()

	var ticker *time.Ticker
	if runtime.dbReader != nil {
		runtime.manager.runDirectIncremental(parent, runtime)
		if parent.Err() != nil {
			return
		}
		ticker = time.NewTicker(directIncrementalInterval)
		defer ticker.Stop()
	}
	var tickC <-chan time.Time
	if ticker != nil {
		tickC = ticker.C
	}

	var changes <-chan cpaDirectChange
	if runtime.cpaDir != nil {
		changes = runtime.cpaDir.Changes()
	}
	for {
		select {
		case <-parent.Done():
			return
		case <-tickC: // nil channel（CPA 事件驱动）时该分支永不触发
			runtime.manager.runDirectIncremental(parent, runtime)
		case change, ok := <-changes:
			if !ok {
				changes = nil
				continue
			}
			runtime.manager.runDirectCPAFile(parent, runtime, change.Path)
		}
	}
}

func (m *Manager) runDirectIncremental(ctx context.Context, runtime *directRuntime) {
	if runtime == nil || runtime.dbReader == nil {
		return
	}
	lock := m.sourceLock(runtime.source.ID)
	lock.Lock()
	defer lock.Unlock()
	if err := contextErr(ctx); err != nil {
		return
	}
	accounts, err := runtime.dbReader.Incremental(ctx)
	if err != nil {
		m.touchIncrEvent(ctx, runtime.source.ID, store.UpdateKindDirectIncremental, sourceName(runtime.source), err.Error(), "")
		return
	}
	applied := 0
	for _, account := range accounts {
		if !account.Valid || account.Entry == nil {
			continue
		}
		if !m.sourceStillDirect(ctx, runtime.source) {
			return
		}
		if err := m.applyDirectEntryLocked(ctx, runtime.source, *account.Entry); err != nil {
			m.touchIncrEvent(ctx, runtime.source.ID, store.UpdateKindDirectIncremental, sourceName(runtime.source), "apply account delta: "+err.Error(), "")
			return
		}
		applied++
	}
	// 增量成功不覆盖 last_status/last_sync_at：那两个字段只反映全量同步，
	// 增量历史看更新记录页（docs/012 v2）。空轮不记事件——Sub2API 每 3 秒
	// 一轮，全记会把更新记录页刷爆；只有实际应用了账号才记。
	if applied > 0 {
		m.emitUpdate(store.UpdateKindDirectIncremental, sourceName(runtime.source), store.UpdateStatusOK,
			fmt.Sprintf("applied %d accounts", applied), "")
	}
}

// sourceStillDirect 确认该 source 的增量配置未被并发修改；变了就放弃本轮写入。
func (m *Manager) sourceStillDirect(ctx context.Context, expected store.SyncSourceRow) bool {
	if m == nil || m.st == nil || expected.ID <= 0 {
		return false
	}
	current, ok, err := m.st.GetSyncSource(ctx, expected.ID)
	if err != nil || !ok || !current.Enabled || !hasIncrementalPath(current) {
		return false
	}
	return current.Kind == expected.Kind &&
		current.DirectAuthDir == expected.DirectAuthDir &&
		current.DirectDBSecret == expected.DirectDBSecret
}

func (m *Manager) runDirectCPAFile(ctx context.Context, runtime *directRuntime, path string) {
	if runtime == nil || runtime.cpaDir == nil || path == "" {
		return
	}
	lock := m.sourceLock(runtime.source.ID)
	lock.Lock()
	defer lock.Unlock()
	if err := contextErr(ctx); err != nil {
		return
	}
	entry, _, err := runtime.cpaDir.ReadEntry(ctx, path)
	if err != nil {
		m.touchIncrEvent(ctx, runtime.source.ID, store.UpdateKindDirectFile, sourceName(runtime.source), "read failed: "+err.Error(), path)
		return
	}
	if entry == nil {
		// 未知 provider/type、无账号或无凭据：跳过不建映射，记脱敏告警
		//（docs/012 v2 §2.3），不阻塞同目录其他文件。
		m.directLogger().Warn("syncer: skipped unrecognized CPA auth file", "path", path)
		return
	}
	if !m.sourceStillDirect(ctx, runtime.source) {
		return
	}
	if err := m.applyDirectEntryLocked(ctx, runtime.source, *entry); err != nil {
		m.touchIncrEvent(ctx, runtime.source.ID, store.UpdateKindDirectFile, sourceName(runtime.source), "apply failed: "+err.Error(), path)
		return
	}
	m.emitUpdate(store.UpdateKindDirectFile, sourceName(runtime.source), store.UpdateStatusOK,
		fmt.Sprintf("%s → %s/%s", filepath.Base(path), entry.Platform, entry.Account), path)
}

func (m *Manager) applyDirectEntryLocked(ctx context.Context, source store.SyncSourceRow, entry Entry) error {
	up := entryToUpsert(entry)
	if err := m.st.ApplyAccountDelta(ctx, sourceName(source), acctmap.SourceTypeForKind(source.Kind), up); err != nil {
		return err
	}
	return m.finishMapChange(ctx)
}

func (m *Manager) finishMapChange(_ context.Context) error {
	return WithMapChangeLock(func() error {
		if m.reg != nil {
			if err := ReloadFromStore(m.st, m.reg); err != nil {
				return err
			}
		}
		if m.OnMapChange != nil {
			m.OnMapChange()
		}
		return nil
	})
}

func sourceName(source store.SyncSourceRow) string {
	return fmt.Sprintf("%s%d", acctmap.SourceInstancePrefix, source.ID)
}

func entryToUpsert(entry Entry) store.AcctUpsert {
	at, rt := "", ""
	atHint, rtHint := "", ""
	if entry.AtToken != "" {
		at = acctmap.Fingerprint(entry.Platform, acctmap.NormalizeCred(entry.AtToken))
		atHint = tail(entry.AtToken)
	}
	if entry.RtToken != "" {
		rt = acctmap.Fingerprint(entry.Platform, acctmap.NormalizeCred(entry.RtToken))
		rtHint = tail(entry.RtToken)
	}
	return store.AcctUpsert{
		Platform: entry.Platform,
		Account:  acctmap.NormalizeAccount(entry.Account),
		AtFP:     at,
		RtFP:     rt,
		AtHint:   atHint,
		RtHint:   rtHint,
	}
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (m *Manager) directLogger() *slog.Logger {
	if m != nil && m.log != nil {
		return m.log
	}
	return slog.Default()
}
