package sidecar

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxyInjectsKeyAndRewritesUpstream(t *testing.T) {
	var gotAuth, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	h := NewHandler(upstream.URL, "secret-key")
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if gotAuth != "Bearer secret-key" {
		t.Fatalf("auth not injected: %q", gotAuth)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path not preserved: %q", gotPath)
	}
}
