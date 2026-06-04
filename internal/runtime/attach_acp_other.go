//go:build !linux

package runtime

import (
	"context"
	"fmt"
)

// AttachACP requires setns(CLONE_NEWNET), which is Linux-only. The CRI/UDS transport runs solely on
// Linux nodes; this stub lets the module build (and tooling/tests run) on macOS/CI. Calling it off
// Linux is a programming error, so it returns an explanatory error rather than silently no-op'ing.
func AttachACP(_ context.Context, _ string) (*AttachedStream, error) {
	return nil, fmt.Errorf("AttachACP: unsupported on non-Linux (requires setns(CLONE_NEWNET))")
}
