package cp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"spawnery/internal/cp/auth"
	"spawnery/internal/weborigin"
)

// dialWS attempts a WS upgrade against the test server with the given Origin header
// (empty = no Origin, i.e. a non-browser client).
func dialWS(t *testing.T, url, origin string) (*websocket.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	if origin != "" {
		hdr.Set("Origin", origin)
	}
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr})
	return conn, err
}

func newWSOriginTestServer(t *testing.T, allowEnv string) string {
	t.Helper()
	s, _, _ := newTestServer(t)
	v := auth.NewVerifier(auth.VerifierConfig{
		DevTokens: map[string]string{"dev-token": "alice"},
		DevMode:   true,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/session", s.HandleWS(v, weborigin.FromEnv(allowEnv)))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return "ws" + ts.URL[len("http"):] + "/ws/session"
}

func TestHandleWSAllowedOriginUpgrades(t *testing.T) {
	url := newWSOriginTestServer(t, "https://app.spawnery.dev")
	conn, err := dialWS(t, url, "https://app.spawnery.dev")
	if err != nil {
		t.Fatalf("allowed origin must upgrade: %v", err)
	}
	defer conn.CloseNow() //nolint:errcheck

	// The bind path past the upgrade works: a bad bind frame gets the protocol close,
	// proving HandleWS proceeded beyond the Origin gate.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte("not json")); err != nil {
		t.Fatalf("write bind frame: %v", err)
	}
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusUnsupportedData {
		t.Fatalf("expected bad-bind close StatusUnsupportedData, got %v", err)
	}
}

func TestHandleWSDisallowedOriginRejected(t *testing.T) {
	url := newWSOriginTestServer(t, "https://app.spawnery.dev")
	conn, err := dialWS(t, url, "https://evil.example")
	if err == nil {
		conn.CloseNow() //nolint:errcheck
		t.Fatal("disallowed origin must not upgrade")
	}
}

func TestHandleWSAbsentOriginAllowed(t *testing.T) {
	url := newWSOriginTestServer(t, "https://app.spawnery.dev")
	conn, err := dialWS(t, url, "")
	if err != nil {
		t.Fatalf("absent Origin (non-browser client) must upgrade: %v", err)
	}
	conn.CloseNow() //nolint:errcheck
}

func TestHandleWSDevModeAllowsLocalhost(t *testing.T) {
	url := newWSOriginTestServer(t, "")
	conn, err := dialWS(t, url, "http://localhost:5173")
	if err != nil {
		t.Fatalf("dev mode must allow localhost origin: %v", err)
	}
	conn.CloseNow() //nolint:errcheck

	if c, err := dialWS(t, url, "https://evil.example"); err == nil {
		c.CloseNow() //nolint:errcheck
		t.Fatal("dev mode must still deny non-localhost origins")
	}
}
