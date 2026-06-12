package weborigin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFromEnvDevMode(t *testing.T) {
	a := FromEnv("")
	if !a.Dev() {
		t.Fatal("empty env must mean dev mode")
	}
	for _, o := range []string{
		"http://localhost:5173",
		"http://127.0.0.1:1234",
		"https://localhost",
		"http://[::1]:8080",
		// LAN origins: `just web` serves --host, so dev browsing happens from private IPs.
		"http://192.168.1.10:5173",
		"http://10.0.0.7:5173",
		"http://172.16.4.2:5173",
	} {
		if !a.Allowed(o) {
			t.Errorf("dev mode should allow %q", o)
		}
	}
	for _, o := range []string{
		"https://evil.example",
		"http://localhost.evil.example",
		"ftp://localhost",
		"null",
		"http://8.8.8.8:5173",           // public IP — never implicit, even in dev
		"http://192.168.1.evil.example", // name, not a private IP
	} {
		if a.Allowed(o) {
			t.Errorf("dev mode should deny %q", o)
		}
	}
}

func TestFromEnvExactMatch(t *testing.T) {
	a := FromEnv("https://app.spawnery.dev, https://staging.spawnery.dev")
	if a.Dev() {
		t.Fatal("non-empty env must not be dev mode")
	}
	if !a.Allowed("https://app.spawnery.dev") {
		t.Error("listed origin must be allowed")
	}
	if !a.Allowed("HTTPS://APP.SPAWNERY.DEV") {
		t.Error("origin match must be case-insensitive")
	}
	if !a.Allowed("https://staging.spawnery.dev") {
		t.Error("second listed origin must be allowed")
	}
	if a.Allowed("http://app.spawnery.dev") {
		t.Error("scheme mismatch must be denied")
	}
	if a.Allowed("https://app.spawnery.dev:8443") {
		t.Error("port mismatch must be denied")
	}
	// [WL5]: localhost is never implicit in a production allowlist.
	for _, o := range []string{"http://localhost:5173", "http://127.0.0.1:8080"} {
		if a.Allowed(o) {
			t.Errorf("prod allowlist must deny %q", o)
		}
	}
}

func TestEmptyOriginAlwaysAllowed(t *testing.T) {
	for _, env := range []string{"", "https://app.spawnery.dev"} {
		if !FromEnv(env).Allowed("") {
			t.Errorf("empty Origin (non-browser client) must be allowed for env=%q", env)
		}
	}
}

func corsHandler(env string) http.Handler {
	return FromEnv(env).CORS(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
}

func TestCORSPreflightAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodOptions, "/cp.v1.SpawnService/ListSpawns", nil)
	req.Header.Set("Origin", "https://app.spawnery.dev")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	corsHandler("https://app.spawnery.dev").ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.spawnery.dev" {
		t.Errorf("ACAO = %q", got)
	}
	if rec.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Error("missing Access-Control-Allow-Methods")
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Error("missing Access-Control-Allow-Headers")
	}
}

func TestCORSActualRequestAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/cp.v1.SpawnService/ListSpawns", nil)
	req.Header.Set("Origin", "https://app.spawnery.dev")
	rec := httptest.NewRecorder()
	corsHandler("https://app.spawnery.dev").ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want passthrough 418", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.spawnery.dev" {
		t.Errorf("ACAO = %q", got)
	}
	if got := rec.Header().Values("Vary"); len(got) != 1 || got[0] != "Origin" {
		t.Errorf("Vary = %v", got)
	}
}

func TestCORSDeniedOriginGetsNoHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	corsHandler("https://app.spawnery.dev").ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want passthrough 418 (deny = no ACAO, not a block)", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("denied origin must get no ACAO, got %q", got)
	}
}

func TestCORSNoOriginPassesThroughUntouched(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	rec := httptest.NewRecorder()
	corsHandler("https://app.spawnery.dev").ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("no-Origin request must get no ACAO, got %q", got)
	}
}

func TestCORSDevModeLocalhost(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()
	corsHandler("").ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("dev mode localhost ACAO = %q", got)
	}
}
