//go:build linux

package journal

// newNotifier builds the Linux file-change notifier: inotify via fsnotify
// (design §2). On a node_modules-scale tree the per-watch inotify descriptor
// budget (fs.inotify.max_user_watches) can be exhausted — Add then errors, the
// Watcher marks itself degraded, and the periodic rescan (DefaultWatchInterval)
// becomes the safety net for the unwatched subtree (the Mutagen-style hybrid).
// Production should also raise max_user_watches; fanotify (whole-mount, root
// cloud Linux) is a deferred capture upgrade (design §2 / §9).
func newNotifier() (notifier, error) { return newFsnotifyNotifier() }
