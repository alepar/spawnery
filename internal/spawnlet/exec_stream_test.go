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
		[]string{"sh", "-c", "printf out; printf err 1>&2; exit 7"}, &stdout, &stderr, false)
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
		[]string{"sh", "-c", "printf done"}, &stdout, &stderr, false)
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
		[]string{"this-binary-does-not-exist-sp8v39"}, &bytes.Buffer{}, &bytes.Buffer{}, false)
	if err == nil {
		t.Fatalf("runExecStream returned nil error for a missing binary")
	}
	if code == 0 {
		t.Fatalf("exit code = 0 for a launch failure, want non-zero")
	}
}

// TestRunExecStreamParsesCrictlExitCode covers the runsc/CRI lane: crictl exec exits 1 for any
// non-zero inner status and reports the real code only on a stderr line "command terminated with
// exit code N". With parseCrictlExit=true, that N must be propagated as the returned code — here we
// simulate crictl by emitting its exact stderr line and exiting 1.
func TestRunExecStreamParsesCrictlExitCode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := runExecStream(context.Background(),
		[]string{"sh", "-c", "printf inner-out; printf 'execing command in container abc123: command terminated with exit code 3\\n' 1>&2; exit 1"},
		&stdout, &stderr, true)
	if err != nil {
		t.Fatalf("runExecStream: %v", err)
	}
	if code != 3 {
		t.Fatalf("exit code = %d, want 3 (parsed from crictl stderr)", code)
	}
	if stdout.String() != "inner-out" {
		t.Fatalf("stdout = %q, want %q", stdout.String(), "inner-out")
	}
}

func TestParseCrictlExitCode(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantOK  bool
	}{
		{"execing command in container abc: command terminated with exit code 3\n", 3, true},
		{"command terminated with exit code 137", 137, true},
		{"some other error\n", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := parseCrictlExitCode([]byte(c.in))
		if ok != c.wantOK || got != c.want {
			t.Fatalf("parseCrictlExitCode(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}
