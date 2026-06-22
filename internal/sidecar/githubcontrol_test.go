package sidecar

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	sidecarv1 "spawnery/gen/sidecar/v1"
)

// buildRotatingControlServer returns a test HTTP server for 401-retry tests. On each gettoken
// request it returns normalToken when force_refresh=false, or forceToken when force_refresh=true.
// It increments *normalCalls or *forceCalls accordingly. certPEM/keyPEM are served on /control/spawnca.
func buildRotatingControlServer(t *testing.T, normalToken, forceToken string, expiry int64, certPEM, keyPEM []byte, normalCalls, forceCalls *int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/control/gettoken", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		var req sidecarv1.GetTokenRequest
		_ = protojson.Unmarshal(body, &req)

		tok := normalToken
		if req.GetForceRefresh() {
			atomic.AddInt32(forceCalls, 1)
			tok = forceToken
		} else {
			atomic.AddInt32(normalCalls, 1)
		}
		resp := &sidecarv1.GetTokenResponse{Token: tok, AccessExpiresAtUnix: expiry}
		out, _ := protojson.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
	mux.HandleFunc("/control/spawnca", func(w http.ResponseWriter, r *http.Request) {
		resp := &sidecarv1.SpawnCADelivery{CaCertPem: certPEM, CaKeyPem: keyPEM}
		out, _ := protojson.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// buildControlServer returns a test HTTP server that handles /control/gettoken and /control/spawnca.
// It increments *getTokenCalls on each gettoken request.
func buildControlServer(t *testing.T, token string, expiresAtUnix int64, certPEM, keyPEM []byte, getTokenCalls *int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/control/gettoken", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		atomic.AddInt32(getTokenCalls, 1)
		resp := &sidecarv1.GetTokenResponse{
			Token:               token,
			AccessExpiresAtUnix: expiresAtUnix,
		}
		out, _ := protojson.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
	mux.HandleFunc("/control/spawnca", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		resp := &sidecarv1.SpawnCADelivery{
			CaCertPem: certPEM,
			CaKeyPem:  keyPEM,
		}
		out, _ := protojson.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
	return httptest.NewServer(mux)
}

func TestGitHubControl_TCP_FetchToken(t *testing.T) {
	var calls int32
	expiry := time.Now().Add(8 * time.Hour).Unix()
	certPEM, keyPEM := makeTestCA(t)
	srv := buildControlServer(t, "real-token-abc", expiry, certPEM, keyPEM, &calls)
	defer srv.Close()

	cfg := ControlConfig{
		Network: "tcp",
		Address: srv.Listener.Addr().String(),
		Bearer:  "test-bearer-123",
		SpawnID: "spawn-001",
	}

	// Capture what the server sees to verify bearer + spawn_id.
	var sawBearer, sawSpawnID string
	origMux := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawBearer = r.Header.Get("Authorization")
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		var req sidecarv1.GetTokenRequest
		_ = protojson.Unmarshal(body[:n], &req)
		sawSpawnID = req.GetSpawnId()
		origMux.ServeHTTP(w, r)
	})

	ctrl := newGitHubControl(cfg)
	tok, exp, err := ctrl.FetchToken(context.Background(), false)
	if err != nil {
		t.Fatalf("FetchToken: %v", err)
	}
	if tok != "real-token-abc" {
		t.Errorf("token = %q, want real-token-abc", tok)
	}
	if exp != expiry {
		t.Errorf("expiry = %d, want %d", exp, expiry)
	}
	if sawBearer != "Bearer test-bearer-123" {
		t.Errorf("Authorization header = %q, want 'Bearer test-bearer-123'", sawBearer)
	}
	if sawSpawnID != "spawn-001" {
		t.Errorf("spawn_id = %q, want spawn-001", sawSpawnID)
	}
}

func TestGitHubControl_UDS_FetchToken(t *testing.T) {
	var calls int32
	expiry := time.Now().Add(8 * time.Hour).Unix()
	certPEM, keyPEM := makeTestCA(t)

	// Start a server on a unix domain socket.
	sockPath := t.TempDir() + "/test.sock"
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/control/gettoken", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		resp := &sidecarv1.GetTokenResponse{
			Token:               "uds-token",
			AccessExpiresAtUnix: expiry,
		}
		out, _ := protojson.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
	mux.HandleFunc("/control/spawnca", func(w http.ResponseWriter, r *http.Request) {
		resp := &sidecarv1.SpawnCADelivery{CaCertPem: certPEM, CaKeyPem: keyPEM}
		out, _ := protojson.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln) //nolint:errcheck
	defer srv.Close()

	cfg := ControlConfig{
		Network: "unix",
		Address: sockPath,
		SpawnID: "spawn-uds",
	}
	ctrl := newGitHubControl(cfg)

	tok, _, err := ctrl.FetchToken(context.Background(), false)
	if err != nil {
		t.Fatalf("UDS FetchToken: %v", err)
	}
	if tok != "uds-token" {
		t.Errorf("token = %q, want uds-token", tok)
	}
}

func TestGitHubControl_Token_Cache(t *testing.T) {
	var calls int32
	// Token expires 1 hour from now — well within the cache window.
	expiry := time.Now().Add(1 * time.Hour).Unix()
	certPEM, keyPEM := makeTestCA(t)
	srv := buildControlServer(t, "cached-token", expiry, certPEM, keyPEM, &calls)
	defer srv.Close()

	cfg := ControlConfig{
		Network: "tcp",
		Address: srv.Listener.Addr().String(),
		SpawnID: "spawn-cache",
	}
	ctrl := newGitHubControl(cfg)

	tok1, err := ctrl.Token(context.Background())
	if err != nil {
		t.Fatalf("Token (1st): %v", err)
	}
	tok2, err := ctrl.Token(context.Background())
	if err != nil {
		t.Fatalf("Token (2nd): %v", err)
	}
	if tok1 != "cached-token" || tok2 != "cached-token" {
		t.Errorf("unexpected tokens %q %q", tok1, tok2)
	}
	// Only one server hit: the second call returned cached.
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("server called %d times, want 1 (cache hit expected)", calls)
	}
}

func TestGitHubControl_Token_RefetchNearExpiry(t *testing.T) {
	var calls int32
	// Token expires in less than minRemainingSeconds.
	expiry := time.Now().Add(60 * time.Second).Unix()
	certPEM, keyPEM := makeTestCA(t)
	srv := buildControlServer(t, "near-expiry-token", expiry, certPEM, keyPEM, &calls)
	defer srv.Close()

	cfg := ControlConfig{
		Network: "tcp",
		Address: srv.Listener.Addr().String(),
		SpawnID: "spawn-nearexp",
	}
	ctrl := newGitHubControl(cfg)

	// Seed the cache with a near-expiry token.
	ctrl.mu.Lock()
	ctrl.cachedToken = "old-token"
	ctrl.cachedExpiry = time.Now().Add(60 * time.Second).Unix() // < minRemainingSeconds=300
	ctrl.mu.Unlock()

	tok, err := ctrl.Token(context.Background())
	if err != nil {
		t.Fatalf("Token near expiry: %v", err)
	}
	if tok != "near-expiry-token" {
		t.Errorf("token = %q, want near-expiry-token", tok)
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Error("expected a server hit on near-expiry refresh")
	}
}

func TestGitHubControl_FetchCA(t *testing.T) {
	var calls int32
	certPEM, keyPEM := makeTestCA(t)
	srv := buildControlServer(t, "tok", time.Now().Add(time.Hour).Unix(), certPEM, keyPEM, &calls)
	defer srv.Close()

	cfg := ControlConfig{
		Network: "tcp",
		Address: srv.Listener.Addr().String(),
		SpawnID: "spawn-ca",
	}
	ctrl := newGitHubControl(cfg)

	delivery, err := ctrl.FetchCA(context.Background())
	if err != nil {
		t.Fatalf("FetchCA: %v", err)
	}
	if string(delivery.GetCaCertPem()) != string(certPEM) {
		t.Error("cert PEM mismatch")
	}
	if string(delivery.GetCaKeyPem()) != string(keyPEM) {
		t.Error("key PEM mismatch")
	}
}

func TestGitHubControl_ErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{"403 forbidden", http.StatusForbidden},
		{"429 rate-limited", http.StatusTooManyRequests},
		{"502 bad gateway", http.StatusBadGateway},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "error body", tc.statusCode)
			}))
			defer srv.Close()

			cfg := ControlConfig{
				Network: "tcp",
				Address: srv.Listener.Addr().String(),
				SpawnID: "spawn-err",
			}
			ctrl := newGitHubControl(cfg)

			_, _, err := ctrl.FetchToken(context.Background(), false)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var ctrlErr *ControlTokenError
			if !errors.As(err, &ctrlErr) {
				t.Fatalf("error type = %T, want *ControlTokenError", err)
			}
			if ctrlErr.StatusCode != tc.statusCode {
				t.Errorf("status = %d, want %d", ctrlErr.StatusCode, tc.statusCode)
			}
		})
	}
}

// Ensure ControlConfig round-trips JSON (used in tests; also validates the struct is exported correctly).
func TestControlConfig_JSON(t *testing.T) {
	cfg := ControlConfig{
		Network: "unix",
		Address: "/tmp/test.sock",
		Bearer:  "secret",
		SpawnID: "s1",
	}
	b, _ := json.Marshal(cfg)
	var got ControlConfig
	_ = json.Unmarshal(b, &got)
	if got != cfg {
		t.Errorf("round-trip: got %+v, want %+v", got, cfg)
	}
}
