//go:build linux || darwin

package journal

import "github.com/fsnotify/fsnotify"

// fsnotifyNotifier adapts github.com/fsnotify/fsnotify (inotify on Linux, kqueue
// on macOS) to the notifier interface. fsnotify is the maintained cross-platform
// lib named in the design (§2). It is NOT recursive — the Watcher walks the tree
// and adds each directory — and it surfaces the descriptor-limit error from Add
// so the Watcher can fall back to periodic rescan for the unwatched subtree.
type fsnotifyNotifier struct {
	w    *fsnotify.Watcher
	out  chan fsEvent
	errc chan error
	done chan struct{}
}

func newFsnotifyNotifier() (*fsnotifyNotifier, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	n := &fsnotifyNotifier{
		w:    w,
		out:  make(chan fsEvent),
		errc: make(chan error),
		done: make(chan struct{}),
	}
	go n.pump()
	return n, nil
}

// pump translates fsnotify's native events to platform-neutral fsEvents until
// Close. The select-on-done ensures a forward never blocks past Close.
func (n *fsnotifyNotifier) pump() {
	defer close(n.out)
	defer close(n.errc)
	for {
		select {
		case ev, ok := <-n.w.Events:
			if !ok {
				return
			}
			select {
			case n.out <- fsEvent{Path: ev.Name, IsCreate: ev.Op&fsnotify.Create != 0}:
			case <-n.done:
				return
			}
		case err, ok := <-n.w.Errors:
			if !ok {
				return
			}
			select {
			case n.errc <- err:
			case <-n.done:
				return
			}
		case <-n.done:
			return
		}
	}
}

func (n *fsnotifyNotifier) Add(dir string) error   { return n.w.Add(dir) }
func (n *fsnotifyNotifier) Events() <-chan fsEvent { return n.out }
func (n *fsnotifyNotifier) Errors() <-chan error   { return n.errc }

func (n *fsnotifyNotifier) Close() error {
	close(n.done)
	return n.w.Close()
}
