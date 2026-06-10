//go:build darwin

package journal

// newNotifier builds the macOS file-change notifier. fsnotify uses kqueue on
// Darwin, which needs one open file descriptor PER watched file/dir — far more
// descriptor pressure than Linux inotify — so the periodic rescan fallback
// (DefaultWatchInterval) is load-bearing here, not just a safety net. FSEvents
// (path-based, recursive, no per-file fd) is the design's preferred macOS
// capture path (§2); adopting a cgo FSEvents binding is a deferred upgrade (§9).
// This file exists so the macOS build selects + documents the right notifier; it
// is cross-compile-checked (GOOS=darwin go build/vet) but cannot be run here.
func newNotifier() (notifier, error) { return newFsnotifyNotifier() }
