package sidecar

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newGitHubControlTestServer starts the sidecar control handler with a GitHubState whose long-poll
// timeout is shortened so tests do not wait 60s. Pass a nil state to exercise the disabled path.
func newGitHubControlTestServer(t *testing.T, gh *GitHubState) *httptest.Server {
	t.Helper()
	if gh != nil {
		gh.eventsTimeout = 100 * time.Millisecond
	}
	srv := httptest.NewServer(NewControlHandler(&Override{}, "secret", gh))
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, method, url, bearer, body string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func pushBody(t *testing.T, certPEM, keyPEM []byte, token string, exp int64) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"ca_cert_pem":      string(certPEM),
		"ca_key_pem":       string(keyPEM),
		"token":            token,
		"token_expires_at": exp,
	})
	if err != nil {
		t.Fatalf("marshal push body: %v", err)
	}
	return string(b)
}

func TestControlGitHubPush_Applies(t *testing.T) {
	certPEM, keyPEM := makeTestCA(t)
	gh := NewGitHubState()
	srv := newGitHubControlTestServer(t, gh)

	resp := do(t, http.MethodPost, srv.URL+"/control/github", "secret", pushBody(t, certPEM, keyPEM, "ghs_pushed", 4242))
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Applied bool `json:"applied"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Applied {
		t.Errorf("applied = false, want true")
	}

	tok, exp := gh.Token()
	if tok != "ghs_pushed" || exp != 4242 {
		t.Errorf("state = (%q,%d), want (ghs_pushed,4242)", tok, exp)
	}
	if _, err := gh.LeafFor("github.com"); err != nil {
		t.Errorf("LeafFor after push: %v", err)
	}
}

func TestControlGitHubPush_Idempotent(t *testing.T) {
	certPEM, keyPEM := makeTestCA(t)
	gh := NewGitHubState()
	srv := newGitHubControlTestServer(t, gh)

	for _, tok := range []string{"ghs_a", "ghs_b"} {
		resp := do(t, http.MethodPost, srv.URL+"/control/github", "secret", pushBody(t, certPEM, keyPEM, tok, 0))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("push %s: status = %d, want 200", tok, resp.StatusCode)
		}
		resp.Body.Close() //nolint:errcheck
	}
	if tok, _ := gh.Token(); tok != "ghs_b" {
		t.Errorf("token = %q, want ghs_b (the last push wins)", tok)
	}
}

func TestControlGitHubPush_Rejects(t *testing.T) {
	certPEM, keyPEM := makeTestCA(t)
	gh := NewGitHubState()
	srv := newGitHubControlTestServer(t, gh)
	good := pushBody(t, certPEM, keyPEM, "ghs_ok", 0)

	tests := []struct {
		name   string
		method string
		bearer string
		body   string
		want   int
	}{
		{"no bearer", http.MethodPost, "", good, http.StatusUnauthorized},
		{"wrong bearer", http.MethodPost, "nope", good, http.StatusUnauthorized},
		{"wrong method", http.MethodGet, "secret", "", http.StatusMethodNotAllowed},
		{"bad json", http.MethodPost, "secret", "{", http.StatusBadRequest},
		{"empty token", http.MethodPost, "secret", pushBody(t, certPEM, keyPEM, "", 0), http.StatusBadRequest},
		{"bad ca pem", http.MethodPost, "secret", pushBody(t, []byte("nope"), keyPEM, "ghs_ok", 0), http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, tc.method, srv.URL+"/control/github", tc.bearer, tc.body)
			defer resp.Body.Close() //nolint:errcheck
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
	if tok, _ := gh.Token(); tok != "" {
		t.Errorf("a rejected push mutated the state: token = %q", tok)
	}
}

func TestControlGitHubEvents_PendingRejection(t *testing.T) {
	certPEM, keyPEM := makeTestCA(t)
	gh := NewGitHubState()
	if err := gh.Set(certPEM, keyPEM, "ghs_live", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	srv := newGitHubControlTestServer(t, gh)
	gh.RecordRejection("ghs_live")

	resp := do(t, http.MethodGet, srv.URL+"/control/github/events", "secret", "")
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Event string `json:"event"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Event != "token_rejected" {
		t.Errorf("event = %q, want token_rejected", out.Event)
	}
}

func TestControlGitHubEvents_TimeoutIs204(t *testing.T) {
	gh := NewGitHubState()
	srv := newGitHubControlTestServer(t, gh) // eventsTimeout = 100ms

	start := time.Now()
	resp := do(t, http.MethodGet, srv.URL+"/control/github/events", "secret", "")
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (bounded long-poll timeout)", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Errorf("returned after %v — the long-poll did not actually hold the request open", elapsed)
	}
}

func TestControlGitHubEvents_WakesOnRejectionMidPoll(t *testing.T) {
	certPEM, keyPEM := makeTestCA(t)
	gh := NewGitHubState()
	if err := gh.Set(certPEM, keyPEM, "ghs_live", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// A long timeout: the ONLY way this test returns 200 is the mid-poll wake-up.
	srv := httptest.NewServer(NewControlHandler(&Override{}, "secret", gh))
	defer srv.Close()
	gh.eventsTimeout = 10 * time.Second

	type result struct {
		status int
		event  string
	}
	res := make(chan result, 1)
	go func() {
		resp := do(t, http.MethodGet, srv.URL+"/control/github/events", "secret", "")
		defer resp.Body.Close() //nolint:errcheck
		var out struct {
			Event string `json:"event"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		res <- result{resp.StatusCode, out.Event}
	}()

	time.Sleep(50 * time.Millisecond) // let the poll register as a waiter
	gh.RecordRejection("ghs_live")

	select {
	case r := <-res:
		if r.status != http.StatusOK || r.event != "token_rejected" {
			t.Fatalf("got (%d,%q), want (200,token_rejected)", r.status, r.event)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("long-poll was not woken by a rejection")
	}
}

func TestControlGitHubEvents_Rejects(t *testing.T) {
	gh := NewGitHubState()
	srv := newGitHubControlTestServer(t, gh)

	resp := do(t, http.MethodGet, srv.URL+"/control/github/events", "", "")
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no bearer: status = %d, want 401", resp.StatusCode)
	}

	resp2 := do(t, http.MethodPost, srv.URL+"/control/github/events", "secret", "")
	defer resp2.Body.Close() //nolint:errcheck
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: status = %d, want 405", resp2.StatusCode)
	}
}

func TestControlGitHub_DisabledWhenStateNil(t *testing.T) {
	srv := newGitHubControlTestServer(t, nil)

	resp := do(t, http.MethodPost, srv.URL+"/control/github", "secret", "{}")
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("push with nil state: status = %d, want 503", resp.StatusCode)
	}

	resp2 := do(t, http.MethodGet, srv.URL+"/control/github/events", "secret", "")
	defer resp2.Body.Close() //nolint:errcheck
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("events with nil state: status = %d, want 503", resp2.StatusCode)
	}
}

// TestControlGitHub_WireContract pins the exact JSON field names and paths that the NODE's push
// client (sp-2tx8.9.3) and long-poll (sp-2tx8.9.4) code against. There is no shared type across the
// two packages, so a rename here silently breaks credential delivery — this test is the only thing
// standing between that rename and a spawn that cannot talk to GitHub.
func TestControlGitHub_WireContract(t *testing.T) {
	certPEM, keyPEM := makeTestCA(t)
	gh := NewGitHubState()
	srv := newGitHubControlTestServer(t, gh)

	// The node sends EXACTLY these keys — hand-written, not marshalled from a struct.
	raw := `{"ca_cert_pem":` + jsonQuote(t, string(certPEM)) +
		`,"ca_key_pem":` + jsonQuote(t, string(keyPEM)) +
		`,"token":"ghs_wire","token_expires_at":99}`

	resp := do(t, http.MethodPost, srv.URL+"/control/github", "secret", raw)
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /control/github with the documented field names: status = %d, want 200", resp.StatusCode)
	}
	if tok, exp := gh.Token(); tok != "ghs_wire" || exp != 99 {
		t.Fatalf("state = (%q,%d), want (ghs_wire,99)", tok, exp)
	}

	gh.RecordRejection("ghs_wire")
	ev := do(t, http.MethodGet, srv.URL+"/control/github/events", "secret", "")
	defer ev.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(ev.Body)
	if err != nil {
		t.Fatalf("read events body: %v", err)
	}
	// The node matches on this exact shape: {"event":"token_rejected"}.
	if !strings.Contains(string(body), `"event"`) || !strings.Contains(string(body), `"token_rejected"`) {
		t.Fatalf("events body = %q, want {\"event\":\"token_rejected\"}", body)
	}
}

func jsonQuote(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("jsonQuote: %v", err)
	}
	return string(b)
}
