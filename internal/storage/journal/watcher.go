package journal

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultWatchInterval is the periodic fallback cadence (design §2 ~60s): even
// if every fs event is dropped, or a subtree could not be watched (descriptor
// limit), a snapshot is still requested at least this often, so a change can
// never be stranded longer than one interval.
const DefaultWatchInterval = 60 * time.Second

// SnapshotTrigger is invoked by a Watcher when a mount's tree changes or on the
// periodic fallback tick. It must be cheap and non-blocking: the journal's
// adaptive debounce + serial queue do the coalescing (a flood of events
// collapses into at most one in-flight + one pending snapshot). In production it
// is a closure over Manager.RequestSnapshot bound to (spawnID, gen, mount).
type SnapshotTrigger func()

// fsEvent is a platform-neutral file-change notification emitted by a notifier.
type fsEvent struct {
	Path     string // the affected path
	IsCreate bool   // a create op — may be a new directory that needs its own watch
}

// notifier is the platform file-change source behind a Watcher. Linux/macOS use
// fsnotify (inotify/kqueue); other platforms get a periodic-only notifier.
// Injectable so tests can drive events + simulate the descriptor limit.
type notifier interface {
	// Add starts watching dir. A descriptor-limit error (ENOSPC/EMFILE) is
	// surfaced so the Watcher can fall back to periodic rescan for that subtree.
	Add(dir string) error
	Events() <-chan fsEvent
	Errors() <-chan error
	Close() error
}

// Watcher drives RequestSnapshot for one journaled mount (design §2): a
// recursive file watcher over the mount host dir, coalesced downstream by the
// journal's adaptive debounce. It always runs a periodic fallback so a dropped
// event or an unwatched (descriptor-limited) subtree cannot strand changes.
type Watcher struct {
	root    string
	period  time.Duration
	trigger SnapshotTrigger
	n       notifier

	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	watched  map[string]struct{}
	degraded bool // hit a descriptor limit; relying on periodic rescan for some subtree
}

// NewWatcher builds a Watcher over root with the platform notifier. period<=0
// uses DefaultWatchInterval.
func NewWatcher(root string, period time.Duration, trigger SnapshotTrigger) (*Watcher, error) {
	n, err := newNotifier()
	if err != nil {
		return nil, err
	}
	return newWatcherWith(root, period, trigger, n), nil
}

// newWatcherWith builds a Watcher around an explicit notifier — the test seam
// (fake notifier to drive events + simulate the descriptor limit).
func newWatcherWith(root string, period time.Duration, trigger SnapshotTrigger, n notifier) *Watcher {
	if period <= 0 {
		period = DefaultWatchInterval
	}
	return &Watcher{root: root, period: period, trigger: trigger, n: n, watched: map[string]struct{}{}}
}

// Start begins watching: add the initial recursive watch set, then run the event
// + periodic loop in the background until Stop (or ctx cancellation).
func (w *Watcher) Start(ctx context.Context) {
	ctx, w.cancel = context.WithCancel(ctx)
	w.done = make(chan struct{})
	w.addTree(w.root)
	go w.loop(ctx)
}

// Stop cancels the loop and waits for it to exit (closing the notifier). Safe to
// call once; a never-Started Watcher is a no-op.
func (w *Watcher) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	if w.done != nil {
		<-w.done
	}
}

// Degraded reports whether some subtree could not be watched (descriptor limit)
// and is covered only by the periodic rescan. Exposed for telemetry/tests.
func (w *Watcher) Degraded() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.degraded
}

// addTree walks root and adds a watch for every directory, best-effort.
func (w *Watcher) addTree(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // intentional: skip unreadable entries, let periodic rescan retry
		}
		if d.IsDir() {
			w.add(path)
		}
		return nil
	})
}

// add watches a single directory once. A failure (typically the inotify
// descriptor limit on a node_modules-scale tree) marks the watcher degraded and
// is NOT recorded as watched, so a later rescan retries it — the Mutagen-style
// hybrid: hot-path watches + periodic rescan when descriptors run out (design §2).
func (w *Watcher) add(dir string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.watched[dir]; ok {
		return
	}
	if err := w.n.Add(dir); err != nil {
		w.degraded = true
		return
	}
	w.watched[dir] = struct{}{}
}

func (w *Watcher) loop(ctx context.Context) {
	defer close(w.done)
	defer func() { _ = w.n.Close() }()
	t := time.NewTicker(w.period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.n.Events():
			if !ok {
				return
			}
			if ev.IsCreate {
				if fi, err := os.Stat(ev.Path); err == nil && fi.IsDir() {
					w.addTree(ev.Path) // recursive coverage for a newly created dir
				}
			}
			w.trigger()
		case _, ok := <-w.n.Errors():
			if !ok {
				return
			}
			// Best-effort: an overflow/dropped-event error is exactly what the
			// periodic fallback below exists to cover.
		case <-t.C:
			// Periodic fallback (design §2): re-add watches for any new or
			// previously unwatched dirs (covers the degraded path) and request a
			// snapshot unconditionally, so a dropped event cannot strand changes.
			w.addTree(w.root)
			w.trigger()
		}
	}
}
