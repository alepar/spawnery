package spawnlet

import (
	"bytes"
	"context"
	"testing"
)

// runExecStream is the core of Manager.ExecStream (sp-8v39): it runs an argv, streams stdout/stderr to
// the writers, and returns the exit code. These tests exercise it with a real subprocess (sh) so the
// exit-code propagation and stdout/stderr separation are covered without a container — the docker
// composition (exec prefix + agent container id) is the e2e's job.

func TestRunExecStreamSeparatesStreamsAndPropagatesExit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := runExecStream(context.Background(),
		[]string{"sh", "-c", "printf out; printf err 1>&2; exit 7"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runExecStream: %v", err)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
	if stdout.String() != "out" {
		t.Fatalf("stdout = %q, want %q", stdout.String(), "out")
	}
	if stderr.String() != "err" {
		t.Fatalf("stderr = %q, want %q", stderr.String(), "err")
	}
}

func TestRunExecStreamZeroExit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := runExecStream(context.Background(),
		[]string{"sh", "-c", "printf done"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runExecStream: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stdout.String() != "done" {
		t.Fatalf("stdout = %q, want %q", stdout.String(), "done")
	}
}

func TestRunExecStreamLaunchFailureIsError(t *testing.T) {
	code, err := runExecStream(context.Background(),
		[]string{"this-binary-does-not-exist-sp8v39"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatalf("runExecStream returned nil error for a missing binary")
	}
	if code == 0 {
		t.Fatalf("exit code = 0 for a launch failure, want non-zero")
	}
}
