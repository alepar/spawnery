package spawnlet

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"spawnery/internal/runtime"
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
		in     string
		want   int
		wantOK bool
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

func TestManagerExecStreamCancellationKillsInContainerProcess(t *testing.T) {
	for _, tc := range []struct {
		name        string
		runtimeKind string
		client      string
		shift       string
	}{
		{name: "docker", client: "docker", shift: "9"},
		{name: "cri", runtimeKind: "runsc", client: "crictl", shift: "3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			fakeClient := filepath.Join(binDir, tc.client)
			logFile := filepath.Join(binDir, tc.client+".log")
			if err := os.WriteFile(fakeClient, []byte(`#!/bin/sh
printf 'start %s %s\n' "$$" "$*" >> "$FAKE_DOCKER_LOG"
shift "$FAKE_RUNTIME_SHIFT"
"$@"
status=$?
printf 'done %s %s\n' "$$" "$status" >> "$FAKE_DOCKER_LOG"
exit "$status"
`), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("FAKE_DOCKER_LOG", logFile)
			t.Setenv("FAKE_RUNTIME_SHIFT", tc.shift)
			m := NewManager(runtime.NewFake(), ManagerConfig{
				AgentImage: tc.client, SidecarImage: "s", DataRoot: t.TempDir(), ContainerRuntime: tc.runtimeKind,
			})
			m.store.Put(&Spawn{ID: "sp-exec", AgentID: "agent-1"})
			pidDir := t.TempDir()
			pidFile := filepath.Join(pidDir, "target.pid")
			childPIDFile := filepath.Join(pidDir, "child.pid")
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				_, err := m.ExecStream(ctx, "sp-exec",
					[]string{"sh", "-c", `echo $$ > "$1"; sleep 600 & echo $! > "$2"; wait`, "sh", pidFile, childPIDFile},
					io.Discard, io.Discard)
				done <- err
			}()
			pid := waitForPIDFile(t, pidFile)
			childPID := waitForPIDFile(t, childPIDFile)
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("ExecStream error = %v, want context.Canceled", err)
				}
			case <-time.After(2 * time.Second):
				log, _ := os.ReadFile(logFile)
				t.Fatalf("ExecStream did not return after runtime client cancellation; fake runtime log:\n%s", log)
			}
			deadline := time.Now().Add(500 * time.Millisecond)
			for processAlive(pid) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if processAlive(pid) {
				t.Fatalf("in-container process %d remains alive after ExecStream cancellation", pid)
			}
			if processAlive(childPID) {
				t.Fatalf("in-container child process %d remains alive after ExecStream cancellation", childPID)
			}
		})
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		contents, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(contents)))
			if parseErr != nil || pid <= 0 {
				t.Fatalf("pid file %q = %q: %v", path, contents, parseErr)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid file %q was not created", path)
		}
		time.Sleep(time.Millisecond)
	}
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
