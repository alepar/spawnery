package node

// fakebackend_test.go: the node package's seam onto internal/runtime/fakepod, replacing the
// hand-forwarded ad-hoc pod-backend fake that used to live in attach_lifecycle_test.go (sp-2tx8.1.4).

import (
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
