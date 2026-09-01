package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	cpaDirectMaxFileSize = 2 << 20
	cpaDirectDebounce    = 200 * time.Millisecond
	cpaDirectMaxPending  = 20000
)

type cpaDirectChange struct {
	Path string
}

// CPADirectReader watches and reads one CPA authentication directory.
type CPADirectReader struct {
	root string

	mu      sync.Mutex
	watcher *fsnotify.Watcher
	started bool
	changes chan cpaDirectChange
	done    chan struct{}
	closed  chan struct{}
	once    sync.Once
}

// NewCPADirectReader validates a CPA auth directory without following a root symlink.
func NewCPADirectReader(dir string) (*CPADirectReader, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("CPA auth directory is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, errors.New("CPA auth directory path is invalid")
	}
	abs = filepath.Clean(abs)
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, errors.New("CPA auth directory is not accessible")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("CPA auth directory must not be a symlink")
	}
	if !info.IsDir() {
		return nil, errors.New("CPA auth path is not a directory")
	}
	return &CPADirectReader{
		root:    abs,
		changes: make(chan cpaDirectChange, 64),
		done:    make(chan struct{}),
		closed:  make(chan struct{}),
	}, nil
}

// Start installs recursive directory watches and begins event coalescing.
func (r *CPADirectReader) Start(ctx context.Context) error {
	if r == nil {
		return errors.New("CPA direct reader is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	watcher, err := newCPAWatcher(r.root)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		_ = watcher.Close()
		return errors.New("CPA direct reader already started")
	}
	r.watcher = watcher
	r.started = true
	r.mu.Unlock()
	go r.run(ctx)
	return nil
}

func newCPAWatcher(root string) (*fsnotify.Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create CPA watcher: %w", err)
	}
	if err := addCPAWatchTree(watcher, root); err != nil {
		_ = watcher.Close()
		return nil, fmt.Errorf("watch CPA auth directory: %w", err)
	}
	return watcher, nil
}

func addCPAWatchTree(watcher *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			if err := watcher.Add(path); err != nil {
				return err
			}
		}
		return nil
	})
}

// Changes returns coalesced direct file changes.
func (r *CPADirectReader) Changes() <-chan cpaDirectChange {
	if r == nil {
		return nil
	}
	return r.changes
}

func (r *CPADirectReader) run(ctx context.Context) {
	r.mu.Lock()
	watcher := r.watcher
	changes := r.changes
	r.mu.Unlock()
	defer close(r.done)
	defer close(changes)
	defer func() {
		if watcher != nil {
			_ = watcher.Close()
		}
	}()

	pending := make(map[string]time.Time)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	emit := func(change cpaDirectChange) bool {
		select {
		case changes <- change:
			return true
		default:
			return false
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.closed:
			return
		case event, ok := <-watcher.Events:
			if !ok {
				watcher = r.restartWatcher(ctx, watcher)
				if watcher == nil {
					return
				}
				continue
			}
			path := filepath.Clean(event.Name)
			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				if path == r.root {
					watcher = r.restartWatcher(ctx, watcher)
					if watcher == nil {
						return
					}
				} else if event.Op&fsnotify.Rename != 0 {
					// Atomic replacement can report Rename for the new
					// pathname. Process it when the replacement is visible.
					if info, statErr := os.Lstat(path); statErr == nil &&
						info.Mode().IsRegular() && isCPAJSON(path) {
						queuePending(pending, path)
					}
				}
				continue
			}
			if event.Op&(fsnotify.Create|fsnotify.Write) != 0 {
				if info, statErr := os.Lstat(path); statErr == nil && info.IsDir() {
					if event.Op&fsnotify.Create != 0 {
						if addErr := watcher.Add(path); addErr != nil {
							watcher = r.restartWatcher(ctx, watcher)
							if watcher == nil {
								return
							}
						}
						continue
					}
				}
				if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
					delete(pending, path)
					continue
				}
			}
			if event.Op&(fsnotify.Create|fsnotify.Write) != 0 && isCPAJSON(path) {
				queuePending(pending, path)
			}
		case _, ok := <-watcher.Errors:
			if !ok {
				watcher = r.restartWatcher(ctx, watcher)
				if watcher == nil {
					return
				}
				continue
			}
			watcher = r.restartWatcher(ctx, watcher)
			if watcher == nil {
				return
			}
		case now := <-ticker.C:
			for path, due := range pending {
				if now.Before(due) {
					continue
				}
				if !emit(cpaDirectChange{Path: path}) {
					break
				}
				delete(pending, path)
			}
		}
	}
}

func queuePending(pending map[string]time.Time, path string) {
	if _, exists := pending[path]; !exists && len(pending) >= cpaDirectMaxPending {
		var oldestPath string
		var oldestDue time.Time
		for candidate, due := range pending {
			if oldestPath == "" || due.Before(oldestDue) {
				oldestPath, oldestDue = candidate, due
			}
		}
		delete(pending, oldestPath)
	}
	pending[path] = time.Now().Add(cpaDirectDebounce)
}

func (r *CPADirectReader) restartWatcher(ctx context.Context, old *fsnotify.Watcher) *fsnotify.Watcher {
	if old != nil {
		_ = old.Close()
	}
	r.mu.Lock()
	r.watcher = nil
	r.mu.Unlock()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.closed:
			return nil
		case <-ticker.C:
			watcher, err := newCPAWatcher(r.root)
			if err != nil {
				continue
			}
			r.mu.Lock()
			r.watcher = watcher
			r.mu.Unlock()
			return watcher
		}
	}
}

func isCPAJSON(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".json")
}

// Close stops the watcher and waits for its event loop to exit.
func (r *CPADirectReader) Close() error {
	if r == nil {
		return nil
	}
	r.once.Do(func() {
		close(r.closed)
		r.mu.Lock()
		watcher, started := r.watcher, r.started
		r.mu.Unlock()
		if watcher != nil {
			_ = watcher.Close()
		}
		if started {
			<-r.done
		}
	})
	return nil
}

// ReadEntry reads and parses one auth file. It never treats an invalid/empty file as deletion.
func (r *CPADirectReader) ReadEntry(ctx context.Context, path string) (*Entry, time.Time, error) {
	if r == nil {
		return nil, time.Time{}, errors.New("CPA direct reader is nil")
	}
	if err := contextErr(ctx); err != nil {
		return nil, time.Time{}, err
	}
	path, err := r.safePath(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, time.Time{}, errors.New("CPA auth entry is not a regular file")
	}
	if info.Size() > cpaDirectMaxFileSize {
		return nil, time.Time{}, errors.New("CPA auth file exceeds 2 MiB")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	if len(raw) == 0 {
		return nil, time.Time{}, errors.New("CPA auth file is empty")
	}
	latest, err := os.Lstat(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	if latest.Size() != info.Size() || !latest.ModTime().Equal(info.ModTime()) {
		return nil, time.Time{}, errors.New("CPA auth file changed while reading")
	}
	entry, err := parseCPAAuthFile(raw)
	if err != nil {
		return nil, time.Time{}, err
	}
	return entry, latest.ModTime(), nil
}

func (r *CPADirectReader) safePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("CPA auth path is invalid")
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(r.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", errors.New("CPA auth path is outside configured directory")
	}
	rootInfo, err := os.Lstat(r.root)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return "", errors.New("CPA auth directory is unavailable")
	}
	resolvedRoot, err := filepath.EvalSymlinks(r.root)
	if err != nil {
		return "", errors.New("CPA auth directory is unavailable")
	}
	resolvedPath, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", errors.New("CPA auth path is unavailable")
	}
	resolvedRel, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(os.PathSeparator)) || filepath.IsAbs(resolvedRel) {
		return "", errors.New("CPA auth path is outside configured directory")
	}
	return abs, nil
}
