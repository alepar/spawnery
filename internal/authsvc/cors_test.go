package authsvc

// Tests for credentialed CORS [AM2] on /refresh and /logout.

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"spawnery/internal/authsvc/githubfake"
)

func TestCORSPreflight(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	srv, _, _ := testAS(t, fake, now)
	client := &http.Client{}

	// OPTIONS /refresh from allowed origin → 204 + CORS headers.
	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/refresh", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight: want 204, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
		t.Fatalf("ACAO header: %q", resp.Header.Get("Access-Control-Allow-Origin"))
	}
	if resp.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("ACAC header: %q", resp.Header.Get("Access-Control-Allow-Credentials"))
	}
}

func TestCORSCredentialedResponseAllowed(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	srv, _, _ := testAS(t, fake, now)
	client := &http.Client{}

	// POST /refresh from allowed SPA origin — CORS headers should be present (even if 401 for missing cookie).
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/refresh", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
		t.Fatalf("ACAO missing on credentialed response: %q", resp.Header.Get("Access-Control-Allow-Origin"))
	}
	if resp.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("ACAC missing: %q", resp.Header.Get("Access-Control-Allow-Credentials"))
	}
}

func TestCORSOffOriginRejected(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	srv, _, _ := testAS(t, fake, now)
	client := &http.Client{}

	// POST /refresh from a FOREIGN origin → 403 (hard reject, not just no CORS headers).
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/refresh",
		strings.NewReader(""))
	req.Header.Set("Origin", "https://evil.example.com")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("off-origin: want 403, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "origin") {
		t.Fatalf("no origin error in body: %s", body)
	}
}

func TestCORSNoOriginHeaderPassesThrough(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	srv, _, _ := testAS(t, fake, now)
	client := &http.Client{}

	// POST /refresh with NO Origin header (CLI, curl) → no CORS headers but request passes.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/refresh", nil)
	// No Origin header — simulates CLI/curl access.
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// No 403; the request reaches the handler (which will 401 for missing cookie).
	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("no-origin request rejected as CORS violation")
	}
	// No CORS response headers on non-Origin requests.
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unexpected ACAO on non-CORS request: %q", resp.Header.Get("Access-Control-Allow-Origin"))
	}
}
