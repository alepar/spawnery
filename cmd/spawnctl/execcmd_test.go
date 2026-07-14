package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"spawnery/internal/client"
)

type fakeAuthenticatedExecClient struct {
	spawn string
	argv  []string
	code  int
	err   error
}

func (f *fakeAuthenticatedExecClient) Exec(_ context.Context, spawn string, argv []string, stdout, stderr io.Writer) (int, error) {
	f.spawn = spawn
	f.argv = append([]string(nil), argv...)
	_, _ = io.WriteString(stdout, "hello\n")
	_, _ = io.WriteString(stderr, "warn\n")
	return f.code, f.err
}

func TestRunExecUsesAuthenticatedClientAndPreservesExit(t *testing.T) {
	fake := &fakeAuthenticatedExecClient{code: 37}
	var stdout, stderr bytes.Buffer
	code, err := runExec(context.Background(), fake, "sp-123", []string{"echo", "hi"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if code != 37 || fake.spawn != "sp-123" || strings.Join(fake.argv, " ") != "echo hi" {
		t.Fatalf("exec = code %d spawn %q argv %v", code, fake.spawn, fake.argv)
	}
	if stdout.String() != "hello\n" || stderr.String() != "warn\n" {
		t.Fatalf("output = %q / %q", stdout.String(), stderr.String())
	}
}

func TestExecCommandHasNoDirectNodeAddress(t *testing.T) {
	cmd := execCmd()
	for _, flag := range cmd.Flags {
		if flag.Names()[0] == "addr" {
			t.Fatal("production exec still exposes direct node -addr")
		}
	}
}

func TestExecClientFactoryRequiresNodeAuthorization(t *testing.T) {
	_, err := buildAuthenticatedExecClient("http://cp.invalid", &cpTokenSource{staticToken: "cp-only"}, client.TargetTrust{})
	if err == nil || !strings.Contains(err.Error(), "node authorization") {
		t.Fatalf("CP-only client error = %v", err)
	}
}
