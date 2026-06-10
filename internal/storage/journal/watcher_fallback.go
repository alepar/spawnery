//go:build !linux && !darwin

package journal

// newNotifier on platforms with no fsnotify backend wired here returns a
// no-event notifier: the Watcher then relies entirely on its periodic fallback
// (DefaultWatchInterval). Keeps the package building everywhere.
type nullNotifier struct {
	events chan fsEvent
	errs   chan error
}

func newNotifier() (notifier, error) {
	return &nullNotifier{events: make(chan fsEvent), errs: make(chan error)}, nil
}

func (n *nullNotifier) Add(string) error       { return nil }
func (n *nullNotifier) Events() <-chan fsEvent { return n.events }
func (n *nullNotifier) Errors() <-chan error   { return n.errs }
func (n *nullNotifier) Close() error           { close(n.events); close(n.errs); return nil }
