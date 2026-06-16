package sidecar

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOverrideGetSet(t *testing.T) {
	var ov Override
	if got := ov.Get(); got != "" {
		t.Fatalf("zero Override Get() = %q, want empty", got)
	}
	ov.Set("anthropic/claude-3.5-sonnet")
	if got := ov.Get(); got != "anthropic/claude-3.5-sonnet" {
		t.Fatalf("Get() = %q", got)
	}
	ov.Set("") // empty clears -> passthrough
	if got := ov.Get(); got != "" {
		t.Fatalf("after clear Get() = %q, want empty", got)
	}

	var nilp *Override
	if got := nilp.Get(); got != "" {
		t.Fatalf("nil receiver Get() = %q, want empty", got)
	}
}

func TestPatchModelJSON(t *testing.T) {
	in := []byte(`{"model":"old/model","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	out, err := patchModelJSON(in, "new/model")
	if err != nil {
		t.Fatalf("patchModelJSON: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if obj["model"] != "new/model" {
		t.Fatalf("model = %v, want new/model", obj["model"])
	}
	if obj["stream"] != true {
		t.Fatalf("stream field lost: %v", obj["stream"])
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages field lost: %v", obj["messages"])
	}

	if _, err := patchModelJSON([]byte("not json"), "x"); err == nil {
		t.Fatalf("expected error on non-JSON body")
	}

	// A JSON null unmarshals into a nil map; patchModelJSON must report an error
	// rather than panicking on "assignment to entry in nil map".
	for _, body := range []string{"null", "  null\n", "[]", "[1,2]"} {
		if _, err := patchModelJSON([]byte(body), "x"); err == nil {
			t.Fatalf("expected error on non-object body %q", body)
		}
	}
}

func TestControlPostSetsOverride(t *testing.T) {
	ov := &Override{}
	srv := httptest.NewServer(NewControlHandler(ov, "secret"))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/control/model",
		strings.NewReader(`{"model":"new/model"}`))
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST status = %d, want 200", resp.StatusCode)
	}
	if ov.Get() != "new/model" {
		t.Fatalf("override = %q, want new/model", ov.Get())
	}
}

func TestControlGetReturnsOverride(t *testing.T) {
	ov := &Override{}
	ov.Set("cur/model")
	srv := httptest.NewServer(NewControlHandler(ov, "secret"))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/control/model", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}
	var obj map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		t.Fatal(err)
	}
	if obj["model"] != "cur/model" {
		t.Fatalf("GET model = %v, want cur/model", obj["model"])
	}
}

func TestControlStatusReportsInflightRequests(t *testing.T) {
	ov := &Override{}
	ov.Set("cur/model")
	inflight := NewInflight()
	inflight.Begin()
	defer inflight.End()
	srv := httptest.NewServer(NewControlHandler(ov, "secret", inflight))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/control/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}
	var obj struct {
		Model          string `json:"model"`
		Busy           bool   `json:"busy"`
		ActiveRequests int64  `json:"active_requests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		t.Fatal(err)
	}
	if obj.Model != "cur/model" || !obj.Busy || obj.ActiveRequests != 1 {
		t.Fatalf("control status = %+v, want model cur/model busy=true active_requests=1", obj)
	}
}

func TestControlRejectsBadToken(t *testing.T) {
	ov := &Override{}
	srv := httptest.NewServer(NewControlHandler(ov, "secret"))
	defer srv.Close()

	cases := []struct {
		name, auth string
	}{
		{"missing", ""},
		{"wrong", "Bearer nope"},
		{"no-bearer", "secret"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/control/model",
				strings.NewReader(`{"model":"x"}`))
			if c.auth != "" {
				req.Header.Set("Authorization", c.auth)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if ov.Get() != "" {
				t.Fatalf("override mutated on rejected request: %q", ov.Get())
			}
		})
	}
}

func TestControlPostSetsCredentialsWithoutEchoingKey(t *testing.T) {
	ov := &Override{}
	srv := httptest.NewServer(NewControlHandler(ov, "secret"))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/control/credentials",
		strings.NewReader(`{"key":"byok-key","upstream":"https://example.test/api"}`))
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "byok-key") {
		t.Fatalf("response echoed raw key: %s", body)
	}
	creds := ov.Credentials("https://default.test/api", "default-key")
	if creds.Key != "byok-key" {
		t.Fatalf("credential key = %q, want byok-key", creds.Key)
	}
	if creds.Upstream != "https://example.test/api" {
		t.Fatalf("credential upstream = %q, want override upstream", creds.Upstream)
	}
}

func TestControlCredentialsRejectsEmptyKeyAndBadUpstream(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty-key", `{"key":"","upstream":"https://example.test/api"}`},
		{"non-http-upstream", `{"key":"byok-key","upstream":"ftp://example.test/api"}`},
		{"unparsable-upstream", `{"key":"byok-key","upstream":":// bad"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ov := &Override{}
			srv := httptest.NewServer(NewControlHandler(ov, "secret"))
			defer srv.Close()

			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/control/credentials", strings.NewReader(c.body))
			req.Header.Set("Authorization", "Bearer secret")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			creds := ov.Credentials("https://default.test/api", "default-key")
			if creds.Key != "default-key" || creds.Upstream != "https://default.test/api" {
				t.Fatalf("credentials mutated on rejected request: %+v", creds)
			}
		})
	}
}
