package sidecar

import (
	"encoding/json"
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
