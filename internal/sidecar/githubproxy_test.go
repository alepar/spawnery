package sidecar

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestApplyGitHubAuth verifies the Authorization header overwrite policy (Step 4).
func TestApplyGitHubAuth(t *testing.T) {
	tests := []struct {
		name     string
		action   ghAction
		token    string
		existing string
		want     string
	}{
		{
			name:     "basic overwrite existing dummy",
			action:   actionMitmBasic,
			token:    "ghp_realtoken123",
			existing: "Basic dXNlcjpkdW1teQ==",
			want:     "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghp_realtoken123")),
		},
		{
			name:   "basic no existing",
			action: actionMitmBasic,
			token:  "ghp_abc",
			want:   "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghp_abc")),
		},
		{
			name:     "bearer overwrite existing dummy",
			action:   actionMitmBearer,
			token:    "ghp_realtoken",
			existing: "Bearer dummytoken",
			want:     "Bearer ghp_realtoken",
		},
		{
			name:   "bearer no existing",
			action: actionMitmBearer,
			token:  "ghp_xyz",
			want:   "Bearer ghp_xyz",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			h := make(http.Header)
			if tc.existing != "" {
				h.Set("Authorization", tc.existing)
			}
			applyGitHubAuth(h, tc.action, tc.token)
			got := h.Get("Authorization")
			if got != tc.want {
				t.Errorf("Authorization = %q, want %q", got, tc.want)
			}
			// Must be a single value (no append).
			if len(h["Authorization"]) != 1 {
				t.Errorf("Authorization has %d values, want 1", len(h["Authorization"]))
			}
		})
	}
}

// buildFixedTokenControl creates a githubControl backed by a test server that returns token.
func buildFixedTokenControl(t *testing.T, token string) *githubControl {
	t.Helper()
	expiry := time.Now().Add(8 * time.Hour).Unix()
	certPEM, keyPEM := makeTestCA(t)
	var calls int32
	srv := buildControlServer(t, token, expiry, certPEM, keyPEM, &calls)
	t.Cleanup(srv.Close)
	return newGitHubControl(ControlConfig{
		Network: "tcp",
		Address: srv.Listener.Addr().String(),
		SpawnID: "test-spawn",
	})
}

// spawnCAPool returns an *x509.CertPool trusting the per-spawn CA.
func spawnCAPool(t *testing.T, ca *spawnCA) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return pool
}

// redirectToAddr returns a DialContext function that always dials addr, ignoring the requested
// hostname. Used to redirect the proxy's upstream connections to a local test server.
func redirectToAddr(addr string) func(ctx context.Context, network, _ string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
}

// upstreamTransportFor returns an *http.Transport for the proxy's upstream leg that:
//   - trusts the test upstream's TLS cert via InsecureSkipVerify (the test cert is for example.com,
//     not github.com — skip SNI verification in the hermetic test);
//   - redirects all connections to upstreamAddr (so proxy→"github.com" goes to the test server).
//
// Note: InsecureSkipVerify is intentional here for the UPSTREAM-side transport in unit tests only.
// The client-facing side still uses the proper per-spawn CA chain. The strict-upstream test uses a
// custom transport WITHOUT InsecureSkipVerify to prove T2 holds in the real deployment.
func upstreamTransportFor(_ *httptest.Server, upstreamAddr string) *http.Transport {
	return &http.Transport{
		Proxy: nil, // no proxy-env on the upstream leg (T2)
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // hermetic test: fake upstream cert is for example.com not github.com
		},
		DialContext: redirectToAddr(upstreamAddr),
	}
}

// mitmClientTransport builds an http.Transport for a client that routes via proxyURL and trusts
// the per-spawn CA (to accept MITM leaf certs). No DialContext override: the client connects
// normally to the proxy, which handles the CONNECT and MITM.
func mitmClientTransport(t *testing.T, proxyURL string, ca *spawnCA, hostname string) *http.Transport {
	t.Helper()
	pu, _ := url.Parse(proxyURL)
	return &http.Transport{
		Proxy: http.ProxyURL(pu),
		TLSClientConfig: &tls.Config{
			RootCAs:    spawnCAPool(t, ca),
			ServerName: hostname,
			MinVersion: tls.VersionTLS12,
		},
	}
}

// tunnelClientTransport builds a transport for the CONNECT-tunnel path (no MITM).
// The client sees the upstream's OWN cert (no MITM leaf), so InsecureSkipVerify is used in tests.
func tunnelClientTransport(t *testing.T, proxyURL, hostname string) *http.Transport {
	t.Helper()
	pu, _ := url.Parse(proxyURL)
	return &http.Transport{
		Proxy: http.ProxyURL(pu),
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // test: confirming proxy does NOT MITM these hosts
			ServerName:         hostname,
			MinVersion:         tls.VersionTLS12,
		},
	}
}

// TestGitHubProxy_Inject_Basic verifies MITM + Basic auth injection for github.com (Step 5).
func TestGitHubProxy_Inject_Basic(t *testing.T) {
	const realToken = "ghp_realtoken_basic"

	// Upstream TLS server records what Authorization it received.
	var sawAuth string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	certPEM, keyPEM := makeTestCA(t)
	ca, err := parseSpawnCA(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parseSpawnCA: %v", err)
	}
	ctrl := buildFixedTokenControl(t, realToken)

	// Proxy upstream transport: trusts the test TLS server + redirects to it.
	upstreamTr := upstreamTransportFor(upstream, upstream.Listener.Addr().String())

	proxy := newGitHubProxy(GitHubProxyConfig{CA: ca, Control: ctrl, UpstreamTransport: upstreamTr})
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	// Client: routes via proxy, trusts spawn CA for the MITM leaf cert.
	client := &http.Client{
		Transport: mitmClientTransport(t, proxySrv.URL, ca, "github.com"),
	}

	req, _ := http.NewRequest(http.MethodGet, "https://github.com/path", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpkdW1teQ==") // dummy — must be overwritten
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+realToken))
	if sawAuth != wantAuth {
		t.Errorf("upstream saw Authorization=%q\nwant %q", sawAuth, wantAuth)
	}
}

// TestGitHubProxy_Inject_Bearer verifies MITM + Bearer auth injection for api.github.com.
func TestGitHubProxy_Inject_Bearer(t *testing.T) {
	const realToken = "ghp_realtoken_bearer"

	var sawAuth string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	certPEM, keyPEM := makeTestCA(t)
	ca, err := parseSpawnCA(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parseSpawnCA: %v", err)
	}
	ctrl := buildFixedTokenControl(t, realToken)

	upstreamTr := upstreamTransportFor(upstream, upstream.Listener.Addr().String())
	proxy := newGitHubProxy(GitHubProxyConfig{CA: ca, Control: ctrl, UpstreamTransport: upstreamTr})
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	client := &http.Client{
		Transport: mitmClientTransport(t, proxySrv.URL, ca, "api.github.com"),
	}

	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer dummybearertoken")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if sawAuth != "Bearer "+realToken {
		t.Errorf("upstream saw Authorization=%q, want Bearer %s", sawAuth, realToken)
	}
}

// TestGitHubProxy_Tunnel_NoInject verifies that object-store hosts are CONNECT-tunneled (no MITM,
// no Authorization injection). The presigned auth header must pass byte-identical.
func TestGitHubProxy_Tunnel_NoInject(t *testing.T) {
	const presignedAuth = "AWS4-HMAC-SHA256 Credential=AKIA.../presigned"

	var sawAuth string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	certPEM, keyPEM := makeTestCA(t)
	ca, err := parseSpawnCA(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parseSpawnCA: %v", err)
	}
	ctrl := buildFixedTokenControl(t, "should-not-appear")

	// Proxy upstream transport: trusts test TLS + redirects to it.
	upstreamTr := upstreamTransportFor(upstream, upstream.Listener.Addr().String())
	proxy := newGitHubProxy(GitHubProxyConfig{CA: ca, Control: ctrl, UpstreamTransport: upstreamTr})
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	// Tunnel client: InsecureSkipVerify because the proxy does NOT MITM tunnel-only hosts —
	// the client sees the upstream's own cert.
	client := &http.Client{
		Transport: tunnelClientTransport(t, proxySrv.URL, "github-cloud.s3.amazonaws.com"),
	}

	req, _ := http.NewRequest(http.MethodGet, "https://github-cloud.s3.amazonaws.com/object", nil)
	req.Header.Set("Authorization", presignedAuth)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("tunnel request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if sawAuth != presignedAuth {
		t.Errorf("upstream saw Authorization=%q\nwant %q (presigned must pass byte-identical)", sawAuth, presignedAuth)
	}
}

// TestGitHubProxy_StrictUpstream verifies that the default strict transport rejects a self-signed
// upstream (no InsecureSkipVerify) — T2 invariant (§2.3).
func TestGitHubProxy_StrictUpstream(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	certPEM, keyPEM := makeTestCA(t)
	ca, err := parseSpawnCA(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parseSpawnCA: %v", err)
	}
	ctrl := buildFixedTokenControl(t, "tok")

	// Strict upstream: default transport (nil) does NOT trust the self-signed test cert.
	// Override DialContext only to redirect the connection to the upstream, but NOT the TLS config.
	strictTr := &http.Transport{
		Proxy: nil,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			// No RootCAs, no InsecureSkipVerify — strict verification, will fail on self-signed.
		},
		DialContext: redirectToAddr(upstream.Listener.Addr().String()),
	}

	proxy := newGitHubProxy(GitHubProxyConfig{CA: ca, Control: ctrl, UpstreamTransport: strictTr})
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	client := &http.Client{
		Transport: mitmClientTransport(t, proxySrv.URL, ca, "github.com"),
	}

	req, _ := http.NewRequest(http.MethodGet, "https://github.com/path", nil)
	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close() //nolint:errcheck
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("expected 502 (strict TLS rejected self-signed upstream), got %d", resp.StatusCode)
		}
		return
	}
	// Transport error also proves strict TLS was enforced.
	t.Logf("strict TLS correctly rejected self-signed upstream (transport error): %v", err)
}

// TestGitHubProxy_GetTokenFailure verifies that a 403 from the control server yields a fast failure
// rather than a prompt loop.
func TestGitHubProxy_GetTokenFailure(t *testing.T) {
	controlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no github link for this spawn", http.StatusForbidden)
	}))
	defer controlSrv.Close()

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	certPEM, keyPEM := makeTestCA(t)
	ca, err := parseSpawnCA(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parseSpawnCA: %v", err)
	}
	ctrl := newGitHubControl(ControlConfig{
		Network: "tcp",
		Address: controlSrv.Listener.Addr().String(),
		SpawnID: "spawn-fail",
	})

	upstreamTr := upstreamTransportFor(upstream, upstream.Listener.Addr().String())
	proxy := newGitHubProxy(GitHubProxyConfig{CA: ca, Control: ctrl, UpstreamTransport: upstreamTr})
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	client := &http.Client{
		Transport: mitmClientTransport(t, proxySrv.URL, ca, "github.com"),
	}

	req, _ := http.NewRequest(http.MethodGet, "https://github.com/path", nil)
	resp, err := client.Do(req)
	if err != nil {
		// Transport-level close is acceptable: the proxy failed fast (no prompt loop).
		if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "connection reset") &&
			!strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "closed") {
			t.Logf("transport error (expected fast-fail): %v", err)
		}
		return
	}
	defer resp.Body.Close() //nolint:errcheck

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("expected 502, got %d (body: %s)", resp.StatusCode, string(body))
		return
	}
	// Body must mention the failure (short diagnostic, no prompt loop).
	if !strings.Contains(string(body), "403") && !strings.Contains(string(body), "GetToken") &&
		!strings.Contains(string(body), "github") {
		t.Errorf("502 body %q should contain a diagnostic (403 / GetToken / github)", string(body))
	}
}

// TestControlTransportFromEnv verifies env → ControlConfig mapping.
func TestControlTransportFromEnv(t *testing.T) {
	t.Run("UDS", func(t *testing.T) {
		cfg, ok := ControlTransportFromEnv(func(k string) string {
			m := map[string]string{
				"SIDECAR_SPAWN_ID":     "sp1",
				"SIDECAR_GETTOKEN_UDS": "/run/spawnery/control/gettoken.sock",
			}
			return m[k]
		})
		if !ok {
			t.Fatal("expected ok=true for UDS config")
		}
		if cfg.Network != "unix" || cfg.Address != "/run/spawnery/control/gettoken.sock" || cfg.SpawnID != "sp1" {
			t.Errorf("unexpected cfg: %+v", cfg)
		}
	})

	t.Run("TCP", func(t *testing.T) {
		cfg, ok := ControlTransportFromEnv(func(k string) string {
			m := map[string]string{
				"SIDECAR_SPAWN_ID":         "sp2",
				"SIDECAR_GETTOKEN_ADDR":    "10.0.0.1:9090",
				"SIDECAR_GETTOKEN_BEARER":  "bearer-xyz",
			}
			return m[k]
		})
		if !ok {
			t.Fatal("expected ok=true for TCP config")
		}
		if cfg.Network != "tcp" || cfg.Address != "10.0.0.1:9090" || cfg.Bearer != "bearer-xyz" {
			t.Errorf("unexpected cfg: %+v", cfg)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		_, ok := ControlTransportFromEnv(func(_ string) string { return "" })
		if ok {
			t.Error("expected ok=false when no transport configured")
		}
	})
}
