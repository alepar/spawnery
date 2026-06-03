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

// An upstream error (e.g. OpenRouter's 503 "Provider returned error") must reach the agent intact:
// the sidecar logs it AND restores the buffered body, so the status + body pass through unchanged.
func TestProxyPassesThroughUpstreamErrorBody(t *testing.T) {
	const body = `{"error":{"message":"Provider returned error","code":503}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, body)
	}))
	defer upstream.Close()

	srv := httptest.NewServer(NewHandler(upstream.URL, "k"))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Fatalf("error body not passed through intact:\n got %q\nwant %q", got, body)
	}
}
