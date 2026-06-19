package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"spawnery/internal/execstream"
)

func TestRunExecDemuxesAndReturnsExitCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/exec" {
			t.Errorf("path = %q, want /exec", r.URL.Path)
		}
		if got := r.URL.Query().Get("spawn"); got != "sp-123" {
			t.Errorf("spawn = %q, want sp-123", got)
		}
		var body struct {
			Cmd []string `json:"cmd"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if len(body.Cmd) != 2 || body.Cmd[0] != "echo" {
			t.Errorf("cmd = %v, want [echo hi]", body.Cmd)
		}
		w.WriteHeader(http.StatusOK)
		_ = execstream.WriteFrame(w, execstream.Stdout, []byte("hello\n"))
		_ = execstream.WriteFrame(w, execstream.Stderr, []byte("warn\n"))
		_ = execstream.WriteExit(w, 3)
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code, err := runExec(srv.URL, "sp-123", []string{"echo", "hi"}, &out, &errb)
	if err != nil {
		t.Fatalf("runExec: %v", err)
	}
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	if out.String() != "hello\n" {
		t.Fatalf("stdout = %q, want %q", out.String(), "hello\n")
	}
	if errb.String() != "warn\n" {
		t.Fatalf("stderr = %q, want %q", errb.String(), "warn\n")
	}
}

func TestRunExecNon200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "spawn not found: sp-x", http.StatusNotFound)
	}))
	defer srv.Close()

	code, err := runExec(srv.URL, "sp-x", []string{"true"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatalf("runExec returned nil error for a non-200 response")
	}
	if code == 0 {
		t.Fatalf("exit code = 0 for a non-200 response, want non-zero")
	}
	if !strings.Contains(err.Error(), "spawn not found") {
		t.Fatalf("error = %v, want it to contain the node's message", err)
	}
}
