package spawnlet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecProcessPreservesArgvStreamsAndExit(t *testing.T) {
	p := execProcess{id: strings.Repeat("a", 32)}
	touched := filepath.Join(t.TempDir(), "interpolated")
	inner := []string{
		"sh", "-c", `printf '%s|%s' "$1" "$2"; printf err >&2; exit 7`, "sh",
		"a b", "$(touch " + touched + ")",
	}
	argv, err := p.wrapArgv(inner)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code, err := runExecStreamCancelable(context.Background(), argv, &stdout, &stderr, false, p)
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 || stdout.String() != "a b|$(touch "+touched+")" || stderr.String() != "err" {
		t.Fatalf("wrapped exec = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(touched); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("user argv was shell-interpolated: %v", err)
	}
}

func TestExecProcessPreservesSignalExitWithoutSupervisorStderr(t *testing.T) {
	p := execProcess{id: strings.Repeat("e", 32)}
	argv, err := p.wrapArgv([]string{"sh", "-c", `kill -KILL $$`})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code, err := runExecStreamCancelable(context.Background(), argv, io.Discard, &stderr, false, p)
	if err != nil {
		t.Fatal(err)
	}
	if code != 137 || stderr.Len() != 0 {
		t.Fatalf("signaled exec = code %d stderr %q, want 137 and empty stderr", code, stderr.String())
	}
}

func TestExecProcessDoesNotInheritSupervisorControlFD(t *testing.T) {
	p := execProcess{id: strings.Repeat("9", 32)}
	argv, err := p.wrapArgv([]string{"sh", "-c", `test ! -e /proc/self/fd/3`})
	if err != nil {
		t.Fatal(err)
	}
	code, err := runExecStreamCancelable(context.Background(), argv, io.Discard, io.Discard, false, p)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("user command found inherited supervisor control fd: exit %d", code)
	}
}

func TestVerifyExecTerminationRejectsUnconfirmedRuntimeExit(t *testing.T) {
	if err := verifyExecTermination(execStreamResult{code: 137}, 137); err != nil {
		t.Fatalf("confirmed termination rejected: %v", err)
	}
	if err := verifyExecTermination(execStreamResult{code: 1}, 137); err == nil {
		t.Fatal("unexpected runtime exit treated as confirmed termination")
	}
	if err := verifyExecTermination(execStreamResult{err: errors.New("stream lost")}, 137); err == nil {
		t.Fatal("runtime stream failure treated as confirmed termination")
	}
}

func TestExecProcessCancellationBeforeHandshakePreventsLaunch(t *testing.T) {
	p := execProcess{id: strings.Repeat("b", 32)}
	touched := filepath.Join(t.TempDir(), "started")
	argv, err := p.wrapArgv([]string{"sh", "-c", `touch "$1"`, "sh", touched})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, err := runExecStreamCancelable(ctx, argv, &bytes.Buffer{}, &bytes.Buffer{}, false, p)
	if code == 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled exec = code %d err %v", code, err)
	}
	if _, err := os.Stat(touched); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-cancelled command ran: %v", err)
	}
}

func TestExecProcessTransportEOFKillsStartedGroup(t *testing.T) {
	p := execProcess{id: strings.Repeat("f", 32)}
	pidFile := filepath.Join(t.TempDir(), "target.pid")
	argv, err := p.wrapArgv([]string{"sh", "-c", `echo $$ > "$1"; exec sleep 600`, "sh", pidFile})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	handshake := newExecHandshakeWriter(p.id, io.Discard)
	cmd.Stderr = handshake
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	identity := <-handshake.ready
	if identity.err != nil {
		t.Fatal(identity.err)
	}
	if _, err := fmt.Fprintf(stdin, "ack %s %d\n", p.id, identity.pgid); err != nil {
		t.Fatal(err)
	}
	pid := waitForPIDFile(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("exec supervisor did not return after transport EOF")
	}
	if processAlive(pid) {
		t.Fatalf("in-container process %d remains alive after transport EOF", pid)
	}
}

func TestExecProcessRejectsInvalidIdentity(t *testing.T) {
	p := execProcess{id: "../victim"}
	if _, err := p.wrapArgv([]string{"true"}); err == nil {
		t.Fatal("invalid exec identity accepted")
	}
}

func TestExecPrefixWithStdinKeepsRuntimeInputOpen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix []string
	}{
		{name: "docker", prefix: ExecPrefixNonInteractiveFor("")},
		{name: "cri", prefix: ExecPrefixNonInteractiveFor("runsc")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := execPrefixWithStdin(tc.prefix)
			if got[len(got)-1] != "-i" {
				t.Fatalf("stdin exec prefix = %v, want trailing -i before container ID", got)
			}
		})
	}
}

func TestExecProcessFailsClosedWhenSetsidIsUnavailable(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "setsid"), []byte("#!/bin/sh\nexit 127\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	p := execProcess{id: strings.Repeat("c", 32)}
	touched := filepath.Join(t.TempDir(), "started")
	argv, err := p.wrapArgv([]string{"sh", "-c", `touch "$1"`, "sh", touched})
	if err != nil {
		t.Fatal(err)
	}
	code, err := runExecStreamCancelable(context.Background(), argv, &bytes.Buffer{}, &bytes.Buffer{}, false, p)
	if code == 0 || err == nil || !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("exec without setsid = code %d err %v", code, err)
	}
	if _, err := os.Stat(touched); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("command ran without setsid: %v", err)
	}
}

func TestParseExecHandshakeRequiresExactIdentityAndNumericPGID(t *testing.T) {
	id := strings.Repeat("d", 32)
	for _, tc := range []struct {
		name string
		line string
		ok   bool
	}{
		{name: "valid", line: execHandshakePrefix + id + " 123", ok: true},
		{name: "wrong id", line: execHandshakePrefix + strings.Repeat("e", 32) + " 123"},
		{name: "non-numeric", line: execHandshakePrefix + id + " target"},
		{name: "unsafe pgid", line: execHandshakePrefix + id + " 1"},
		{name: "extra field", line: execHandshakePrefix + id + " 123 extra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := parseExecHandshake(id, tc.line)
			if (result.err == nil) != tc.ok {
				t.Fatalf("parseExecHandshake(%q) = pgid %d err %v", tc.line, result.pgid, result.err)
			}
		})
	}
}
