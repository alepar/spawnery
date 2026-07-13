package node

// fakebackend_test.go: the node package's seam onto internal/runtime/fakepod, replacing the
// hand-forwarded ad-hoc pod-backend fake that used to live in attach_lifecycle_test.go (sp-2tx8.1.4).

import (
	"strings"
	"testing"

	"spawnery/internal/runtime/fakepod"
)

// fakeBackend builds a fakepod.Backend and joins its background writers at test end. Pass
// fakepod.WithAttachScript(scriptGoose) for the scripted-agent tests; without it Attach returns a
// loopback pipe (what the old ad-hoc fake did with a nil script).
func fakeBackend(t *testing.T, opts ...fakepod.Option) *fakepod.Backend {
	t.Helper()
	b := fakepod.New(opts...)
	t.Cleanup(b.Close)
	return b
}

// lastOf returns the final element of s, or "".
func lastOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

// podWasStopped reports whether Stop was called on the backend, read from fakepod's ops log.
// (Replaces the old scriptedPodBackend.wasStopped(), removed when tests moved onto fakepod.)
func podWasStopped(b *fakepod.Backend) bool {
	for _, op := range b.Ops() {
		if strings.HasPrefix(op, "stop:") {
			return true
		}
	}
	return false
}
