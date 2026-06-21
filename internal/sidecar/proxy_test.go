package sidecar

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// mustHandler is a test helper that constructs a NewHandler and fatals on error.
func mustHandler(t *testing.T, upstream, key string, ov *Override, trackers ...*Inflight) http.Handler {
	t.Helper()
	h, err := NewHandler(upstream, key, ov, trackers...)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

func TestNewHandlerBadUpstream(t *testing.T) {
	// url.Parse rejects control characters; \x7f (DEL) reliably triggers
	// "net/url: invalid control character in URL".
	badURL := "http://\x7f"
	h, err := NewHandler(badURL, "k", &Override{})
	if err == nil {
		t.Fatal("expected error for control-character URL, got nil")
	}
	if h != nil {
		t.Fatal("expected nil handler on error, got non-nil")
	}
}

func TestProxyInjectsKeyAndRewritesUpstream(t *testing.T) {
	var gotAuth, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	h := mustHandler(t, upstream.URL, "secret-key", &Override{})
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

	srv := httptest.NewServer(mustHandler(t, upstream.URL, "k", &Override{}))
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

func TestProxyRedactsCredentialFromUpstreamError(t *testing.T) {
	const upstreamBody = `{"error":{"message":"provider echoed Bearer byok-key and byok-key"}}`
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()

	ov := &Override{}
	if err := ov.SetCredentials(upstream.URL, "byok-key"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mustHandler(t, "http://default.invalid", "default-key", ov))
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
	if strings.Contains(string(got), "byok-key") {
		t.Fatalf("response body leaked credential: %s", got)
	}
	if strings.Contains(logs.String(), "byok-key") {
		t.Fatalf("logs leaked credential: %s", logs.String())
	}
}

// Override unset: the body the upstream receives is byte-identical to what the agent sent.
func TestProxyOverrideUnsetByteIdentical(t *testing.T) {
	const sent = `{"model":"agent/model","messages":[{"role":"user","content":"hi"}]}`
	var got []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	srv := httptest.NewServer(mustHandler(t, upstream.URL, "k", &Override{}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(sent))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if string(got) != sent {
		t.Fatalf("body not byte-identical:\n got %q\nwant %q", got, sent)
	}
}

// Override set: top-level model is rewritten, other fields preserved, Content-Length consistent.
func TestProxyOverrideSetRewritesModel(t *testing.T) {
	var got []byte
	var gotCL int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		gotCL = r.ContentLength
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	ov := &Override{}
	ov.Set("override/model")
	srv := httptest.NewServer(mustHandler(t, upstream.URL, "k", ov))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"agent/model","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("upstream body not JSON: %v (%s)", err, got)
	}
	if obj["model"] != "override/model" {
		t.Fatalf("model = %v, want override/model", obj["model"])
	}
	if _, ok := obj["messages"]; !ok {
		t.Fatalf("messages field dropped: %s", got)
	}
	if gotCL != int64(len(got)) {
		t.Fatalf("Content-Length %d != body len %d", gotCL, len(got))
	}
}

// Override set on a Codex-style /v1/responses request: the catch-all proxy rewrites the
// top-level model exactly as it does for /v1/chat/completions — Responses API fields
// (input/instructions) are preserved and the path forwards unchanged.
func TestProxyOverrideAppliesToResponsesAPI(t *testing.T) {
	var got []byte
	var gotPath string
	var gotCL int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		gotPath = r.URL.Path
		gotCL = r.ContentLength
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	ov := &Override{}
	ov.Set("override/model")
	srv := httptest.NewServer(mustHandler(t, upstream.URL, "k", ov))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"agent/model","instructions":"be terse","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if gotPath != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", gotPath)
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("upstream body not JSON: %v (%s)", err, got)
	}
	if obj["model"] != "override/model" {
		t.Fatalf("model = %v, want override/model", obj["model"])
	}
	if obj["instructions"] != "be terse" {
		t.Fatalf("instructions field not preserved: %s", got)
	}
	if _, ok := obj["input"]; !ok {
		t.Fatalf("input field dropped: %s", got)
	}
	if gotCL != int64(len(got)) {
		t.Fatalf("Content-Length %d != body len %d", gotCL, len(got))
	}
}

// A streaming response still flows back after a rewritten request.
func TestProxyStreamingPassthroughAfterRewrite(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"x\":1}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	ov := &Override{}
	ov.Set("override/model")
	srv := httptest.NewServer(mustHandler(t, upstream.URL, "k", ov))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"agent/model","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "[DONE]") {
		t.Fatalf("streamed body not passed through: %q", b)
	}
}

func TestProxyCredentialsOverrideAppliesPerRequest(t *testing.T) {
	var defaultHit bool
	defaultUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultHit = true
		io.WriteString(w, `{"ok":true}`)
	}))
	defer defaultUpstream.Close()

	var gotAuth, gotPath string
	overrideUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		io.WriteString(w, `{"ok":true}`)
	}))
	defer overrideUpstream.Close()

	ov := &Override{}
	if err := ov.SetCredentials(overrideUpstream.URL, "byok-key"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mustHandler(t, defaultUpstream.URL, "default-key", ov))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"agent/model"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if defaultHit {
		t.Fatalf("request unexpectedly reached default upstream")
	}
	if gotAuth != "Bearer byok-key" {
		t.Fatalf("auth = %q, want BYOK bearer", gotAuth)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", gotPath)
	}
}

func TestProxyTracksInflightUntilResponseCompletes(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(reached)
		<-release
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	inflight := NewInflight()
	srv := httptest.NewServer(mustHandler(t, upstream.URL, "k", &Override{}, inflight))
	defer srv.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Post(srv.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"m"}`))
		if err != nil {
			t.Errorf("POST: %v", err)
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
	}()
	<-reached
	if got := inflight.Active(); got != 1 {
		t.Fatalf("inflight while upstream response is blocked = %d, want 1", got)
	}
	close(release)
	wg.Wait()
	if got := inflight.Active(); got != 0 {
		t.Fatalf("inflight after response completes = %d, want 0", got)
	}
}
