package spawnlet

// fakebackend_test.go: the shared seam onto internal/runtime/fakepod. It replaces the hand-forwarded
// fakePodBackend/noSizeFakeBackend that used to live in manager_sandbox_test.go / manager_quota_test.go
// (sp-2tx8.1.4) — one maintained fake instead of three copies that silently broke on every interface
// change.

import (
	"strings"
	"testing"

	"spawnery/internal/runtime/fakepod"
)

// fakeBackend builds a fakepod.Backend and joins its background writers at test end.
func fakeBackend(t *testing.T, opts ...fakepod.Option) *fakepod.Backend {
	t.Helper()
	b := fakepod.New(opts...)
	t.Cleanup(b.Close)
	return b
}

// lastOf returns the final element of s, or "" — the fakepod accessors return the whole call history
// where the old fake kept only the most recent value.
func lastOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

// lifecycleOps filters fakepod's full ops log down to the delta/teardown ops the old fake recorded
// (capture, capture-as, release, export, import, stop), preserving order — so the ordering assertions
// ported from the old fake keep their meaning. Entries look like "capture:sp1" / "stop:sp1"; a failed
// call carries a trailing "!".
func lifecycleOps(b *fakepod.Backend) []string {
	want := []fakepod.Op{
		fakepod.OpCaptureDelta, fakepod.OpCaptureDeltaAs, fakepod.OpReleaseDelta,
		fakepod.OpExportDelta, fakepod.OpImportDelta, fakepod.OpStop,
	}
	var out []string
	for _, entry := range b.Ops() {
		for _, op := range want {
			if strings.HasPrefix(entry, string(op)+":") {
				out = append(out, entry)
				break
			}
		}
	}
	return out
}
