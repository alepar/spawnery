package journal

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeNotifier is a notifier double: tests push events through it and can make
// Add fail (the descriptor-limit fallback). It records every Add target.
type fakeNotifier struct {
	events chan fsEvent
	errs   chan error
	addErr error

	mu    sync.Mutex
	added []string
}

func newFakeNotifier(addErr error) *fakeNotifier {
	return &fakeNotifier{events: make(chan fsEvent), errs: make(chan error), addErr: addErr}
}

func (f *fakeNotifier) Add(dir string) error {
	f.mu.Lock()
	f.added = append(f.added, dir)
	f.mu.Unlock()
	return f.addErr
}
func (f *fakeNotifier) Events() <-chan fsEvent { return f.events }
func (f *fakeNotifier) Errors() <-chan error   { return f.errs }
func (f *fakeNotifier) Close() error           { return nil }

func (f *fakeNotifier) hasAdded(dir string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.added {
		if d == dir {
			return true
		}
	}
	return false
}

// firedSignal returns a SnapshotTrigger plus a channel that receives once per
// fire (drops extras so the trigger never blocks — production triggers must be
// non-blocking too).
func firedSignal() (SnapshotTrigger, <-chan struct{}) {
	ch := make(chan struct{}, 64)
	return func() {
		select {
		case ch <- struct{}{}:
		default:
		}
	}, ch
}

// TestWatcherFiresSnapshotOnFileWrite exercises the real fsnotify path: a write
// into the watched dir drives a snapshot request. The periodic fallback is set
// far out (1h) so a fire proves it came from the fs event, not the timer.
func TestWatcherFiresSnapshotOnFileWrite(t *testing.T) {
	dir := t.TempDir()
	trigger, fired := firedSignal()
	w, err := NewWatcher(dir, time.Hour, trigger)
	if err != nil {
		t.Fatal(err)
	}
	w.Start(context.Background())
	defer w.Stop()

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not request a snapshot on a file write")
	}
}

// TestWatcherPeriodicFallbackFiresWithoutEvents proves the ~60s fallback: with a
// notifier that never emits an event, a snapshot is still requested on the
// periodic tick — a dropped/undeliverable event can never strand changes.
func TestWatcherPeriodicFallbackFiresWithoutEvents(t *testing.T) {
	dir := t.TempDir()
	trigger, fired := firedSignal()
	w := newWatcherWith(dir, 20*time.Millisecond, trigger, newFakeNotifier(nil))
	w.Start(context.Background())
	defer w.Stop()

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("periodic fallback did not request a snapshot without any fs events")
	}
}

// TestWatcherDescriptorLimitFallsBackToPeriodic simulates the inotify descriptor
// limit (Add → ENOSPC): the watcher marks itself degraded and relies on the
// periodic rescan to keep requesting snapshots (Mutagen-style hybrid, design §2).
func TestWatcherDescriptorLimitFallsBackToPeriodic(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	trigger, fired := firedSignal()
	w := newWatcherWith(dir, 20*time.Millisecond, trigger, newFakeNotifier(syscall.ENOSPC))
	w.Start(context.Background())
	defer w.Stop()

	if !w.Degraded() {
		t.Fatal("watcher should be degraded after Add returned ENOSPC")
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("degraded watcher did not fall back to periodic snapshots")
	}
}

// TestWatcherWatchesNewlyCreatedSubdir proves recursive coverage: a create event
// for a new directory makes the watcher add a watch on it (so its later contents
// are observed too). The periodic fallback is far out so the Add must come from
// the event, not a rescan.
func TestWatcherWatchesNewlyCreatedSubdir(t *testing.T) {
	dir := t.TempDir()
	trigger, fired := firedSignal()
	fn := newFakeNotifier(nil)
	w := newWatcherWith(dir, time.Hour, trigger, fn)
	w.Start(context.Background())
	defer w.Stop()

	// Create the subdir AFTER Start so the only Add(sub) can come from the event.
	sub := filepath.Join(dir, "newsub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	fn.events <- fsEvent{Path: sub, IsCreate: true}

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("create event did not request a snapshot")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fn.hasAdded(sub) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("watcher did not add a watch for the newly created subdir")
}

// TestWatcherStopIsIdempotentAndClean ensures Start→Stop exits cleanly and a
// never-Started watcher's Stop is a no-op.
func TestWatcherStopIsIdempotentAndClean(t *testing.T) {
	(&Watcher{}).Stop() // never started: no panic

	dir := t.TempDir()
	trigger, _ := firedSignal()
	w := newWatcherWith(dir, time.Hour, trigger, newFakeNotifier(nil))
	w.Start(context.Background())
	w.Stop()
}
