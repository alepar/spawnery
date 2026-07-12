package node

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	sidecarv1 "spawnery/gen/sidecar/v1"
	"spawnery/internal/spawnlet"
)

// newTestControlServer creates a githubControlServer with a fake refresher and a memory-only CA
// store (dir == ""). Use newTestControlServerWithCADir for tests that need the CA to persist.
func newTestControlServer(fake *fakeMintClient) (*githubControlServer, *githubRefresher) {
	return newTestControlServerWithCADir(fake, "")
}

// newTestControlServerWithCADir is newTestControlServer with an explicit on-disk CA store dir.
func newTestControlServerWithCADir(fake *fakeMintClient, caDir string) (*githubControlServer, *githubRefresher) {
	r := newGitHubRefresher(fake)
	return newGitHubControlServer(r, caStore{dir: caDir}), r
}

// udsClient builds an http.Client that dials the given Unix socket.
func udsClient(sockPath string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		},
	}}
}

// parseCA is a test helper that PEM-decodes and x509-parses a CA certificate, failing the test
// with a descriptive message if either step fails.
func parseCA(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("SpawnCACert: PEM decode returned nil (not a valid PEM block)")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("SpawnCACert: x509.ParseCertificate: %v", err)
	}
	return cert
}

// TestGitHubControlServerCAStability verifies that SpawnCACert returns the same CA on repeated
// calls for the same spawnID (generated once, stable across calls for the lifetime of the server).
// Also verifies the generated cert is a proper ECDSA-P256 CA (IsCA=true, correct key type).
func TestGitHubControlServerCAStability(t *testing.T) {
	s, _ := newTestControlServer(&fakeMintClient{})

	cert1PEM, err := s.SpawnCACert("sp-1")
	if err != nil {
		t.Fatalf("first SpawnCACert: %v", err)
	}
	cert2PEM, err := s.SpawnCACert("sp-1")
	if err != nil {
		t.Fatalf("second SpawnCACert: %v", err)
	}
	if !bytes.Equal(cert1PEM, cert2PEM) {
		t.Fatal("SpawnCACert returned different certificates for the same spawn on two calls")
	}

	// Parse the certificate and verify its properties (spec §2.5 + TDD step 3).
	cert := parseCA(t, cert1PEM)
	if !cert.IsCA {
		t.Fatal("SpawnCACert: certificate IsCA=false, want true")
	}
	if _, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Fatalf("SpawnCACert: public key type %T, want *ecdsa.PublicKey (ECDSA P-256)", cert.PublicKey)
	}

	// Different spawns get different CAs.
	cert3PEM, err := s.SpawnCACert("sp-2")
	if err != nil {
		t.Fatalf("other spawn SpawnCACert: %v", err)
	}
	if bytes.Equal(cert1PEM, cert3PEM) {
		t.Fatal("different spawns must not share a CA")
	}
}

// TestGitHubControlServerCAForgottenAfterStop verifies that Stop purges the CA so a subsequent
// SpawnCACert call generates a fresh (different) certificate.
func TestGitHubControlServerCAForgottenAfterStop(t *testing.T) {
	s, _ := newTestControlServer(&fakeMintClient{})

	cert1, err := s.SpawnCACert("sp-1")
	if err != nil {
		t.Fatalf("pre-stop SpawnCACert: %v", err)
	}

	s.Stop("sp-1")

	cert2, err := s.SpawnCACert("sp-1")
	if err != nil {
		t.Fatalf("post-stop SpawnCACert: %v", err)
	}
	if bytes.Equal(cert1, cert2) {
		t.Fatal("after Stop, SpawnCACert must generate a new CA (old one must be purged)")
	}
}

// TestStatusForGetToken verifies the typed-error → HTTP-status mapping without going through the
// full HTTP server. This is a pure unit test of the statusForGetToken function.
func TestStatusForGetToken(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"not_linked", ErrGitHubNotLinked, http.StatusForbidden},
		{"relink_required", ErrGitHubRelinkRequired, http.StatusForbidden},
		{"rate_limited", ErrGitHubMintRateLimited, http.StatusTooManyRequests},
		{"other_error", errors.New("some upstream error"), http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := statusForGetToken(tc.err)
			if got != tc.wantStatus {
				t.Fatalf("statusForGetToken(%v) = %d, want %d", tc.err, got, tc.wantStatus)
			}
		})
	}
}

// TestGitHubControlServerUDSGetTokenRoundTrip verifies the full UDS GetToken path: Serve binds a
// unix socket, and an HTTP POST to it returns the cached token as a protojson GetTokenResponse.
func TestGitHubControlServerUDSGetTokenRoundTrip(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	fake := &fakeMintClient{}
	s, r := newTestControlServer(fake)
	r.now = func() time.Time { return base }

	// Seed a long-lived token so no mint is needed.
	seedEntry(r, "sp-1", "sec-1", "uds-token", base.Add(2*time.Hour).Unix(), base)

	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "gettoken.sock")

	if err := s.Serve(spawnlet.ControlTransport{SpawnID: "sp-1", Network: "unix", Address: sockPath}); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer s.Stop("sp-1")

	// Verify socket exists with 0666 perms.
	fi, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("socket not created: %v", err)
	}
	if fi.Mode().Perm()&0o666 != 0o666 {
		t.Fatalf("socket perms = %o, want at least 0666", fi.Mode().Perm())
	}

	reqMsg := &sidecarv1.GetTokenRequest{SpawnId: "sp-1", MinRemainingSeconds: 300}
	body, _ := protojson.Marshal(reqMsg)

	resp, err := udsClient(sockPath).Post("http://unix/control/gettoken", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST gettoken: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	respBytes, _ := io.ReadAll(resp.Body)
	var tokResp sidecarv1.GetTokenResponse
	if err := protojson.Unmarshal(respBytes, &tokResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if tokResp.GetToken() != "uds-token" {
		t.Fatalf("token = %q, want uds-token", tokResp.GetToken())
	}
}

// TestGitHubControlServerUDSNotLinked verifies that a spawn with no entry receives 403.
func TestGitHubControlServerUDSNotLinked(t *testing.T) {
	s, _ := newTestControlServer(&fakeMintClient{})

	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "gettoken.sock")

	if err := s.Serve(spawnlet.ControlTransport{SpawnID: "sp-nope", Network: "unix", Address: sockPath}); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer s.Stop("sp-nope")

	reqMsg := &sidecarv1.GetTokenRequest{SpawnId: "sp-nope", MinRemainingSeconds: 300}
	body, _ := protojson.Marshal(reqMsg)

	resp, err := udsClient(sockPath).Post("http://unix/control/gettoken", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 403: %s", resp.StatusCode, b)
	}
}

// TestGitHubControlServerSpawnCARoundTrip verifies the full UDS GetSpawnCA path: the server
// returns a protojson SpawnCADelivery with both cert and key PEM blocks, and the returned cert
// matches what SpawnCACert(spawnID) returns (re-delivery stability guarantee, §2.5).
func TestGitHubControlServerSpawnCARoundTrip(t *testing.T) {
	s, _ := newTestControlServer(&fakeMintClient{})

	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "gettoken.sock")

	if err := s.Serve(spawnlet.ControlTransport{SpawnID: "sp-1", Network: "unix", Address: sockPath}); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer s.Stop("sp-1")

	reqMsg := &sidecarv1.GetSpawnCARequest{SpawnId: "sp-1"}
	body, _ := protojson.Marshal(reqMsg)

	resp, err := udsClient(sockPath).Post("http://unix/control/spawnca", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST spawnca: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	respBytes, _ := io.ReadAll(resp.Body)
	var caResp sidecarv1.SpawnCADelivery
	if err := protojson.Unmarshal(respBytes, &caResp); err != nil {
		t.Fatalf("unmarshal SpawnCADelivery: %v", err)
	}
	if len(caResp.GetCaCertPem()) == 0 {
		t.Fatal("CaCertPem is empty")
	}
	if len(caResp.GetCaKeyPem()) == 0 {
		t.Fatal("CaKeyPem is empty")
	}
	if !strings.Contains(string(caResp.GetCaCertPem()), "BEGIN CERTIFICATE") {
		t.Fatalf("CaCertPem does not look like PEM cert: %.80s", caResp.GetCaCertPem())
	}
	if !strings.Contains(string(caResp.GetCaKeyPem()), "EC PRIVATE KEY") {
		t.Fatalf("CaKeyPem does not look like PEM EC key: %.80s", caResp.GetCaKeyPem())
	}

	// Verify that the returned cert is identical to what SpawnCACert returns (§2.5 re-delivery
	// stability: the sidecar can call SpawnCA again on an app-restart and get the same CA).
	directCertPEM, err := s.SpawnCACert("sp-1")
	if err != nil {
		t.Fatalf("SpawnCACert after round-trip: %v", err)
	}
	if !bytes.Equal(caResp.GetCaCertPem(), directCertPEM) {
		t.Fatal("SpawnCADelivery cert does not match SpawnCACert; re-delivery stability broken")
	}
}

// TestGitHubControlServerTCPBearerRejected verifies that TCP requests with an incorrect bearer
// token receive 401 Unauthorized (constant-time comparison).
func TestGitHubControlServerTCPBearerRejected(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	fake := &fakeMintClient{}
	s, r := newTestControlServer(fake)
	r.now = func() time.Time { return base }
	seedEntry(r, "sp-1", "sec-1", "tok", base.Add(2*time.Hour).Unix(), base)

	// Reserve a port then release it for Serve to bind.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	if err := s.Serve(spawnlet.ControlTransport{
		SpawnID: "sp-1", Network: "tcp", Address: addr,
		Bearer: "correct-bearer", PodIP: "127.0.0.1",
	}); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer s.Stop("sp-1")

	reqMsg := &sidecarv1.GetTokenRequest{SpawnId: "sp-1", MinRemainingSeconds: 300}
	body, _ := protojson.Marshal(reqMsg)

	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/control/gettoken", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer wrong-bearer")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for bad bearer", resp.StatusCode)
	}
}

// TestGitHubControlServerTCPWrongIPRejected verifies that TCP requests from an unexpected source
// IP receive 403 Forbidden, even with the correct bearer.
func TestGitHubControlServerTCPWrongIPRejected(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	fake := &fakeMintClient{}
	s, r := newTestControlServer(fake)
	r.now = func() time.Time { return base }
	seedEntry(r, "sp-1", "sec-1", "tok", base.Add(2*time.Hour).Unix(), base)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	if err := s.Serve(spawnlet.ControlTransport{
		SpawnID: "sp-1", Network: "tcp", Address: addr,
		Bearer: "the-bearer",
		PodIP:  "10.99.99.99", // test client connects from 127.0.0.1, not this IP
	}); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer s.Stop("sp-1")

	reqMsg := &sidecarv1.GetTokenRequest{SpawnId: "sp-1", MinRemainingSeconds: 300}
	body, _ := protojson.Marshal(reqMsg)

	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/control/gettoken", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer the-bearer")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for unexpected source IP", resp.StatusCode)
	}
}

// TestGitHubControlServerCAPersistsAcrossRestart is the bug this bead exists to fix: two
// githubControlServers over the SAME caStore dir (simulating a spawnlet restart — the process
// died, a new one starts, the spawn's agent is still running and still trusts the old cert) must
// return byte-identical CAs for the same spawn.
func TestGitHubControlServerCAPersistsAcrossRestart(t *testing.T) {
	caDir := t.TempDir()

	s1, _ := newTestControlServerWithCADir(&fakeMintClient{}, caDir)
	cert1, err := s1.SpawnCACert("sp-1")
	if err != nil {
		t.Fatalf("server 1 SpawnCACert: %v", err)
	}

	// A fresh server, as if the node process restarted, over the SAME on-disk store.
	s2, _ := newTestControlServerWithCADir(&fakeMintClient{}, caDir)
	cert2, err := s2.SpawnCACert("sp-1")
	if err != nil {
		t.Fatalf("server 2 SpawnCACert: %v", err)
	}

	if !bytes.Equal(cert1, cert2) {
		t.Fatal("SpawnCACert across a restart (same caStore dir) returned different certs — " +
			"the agent's cached trust bundle would now fail to verify the MITM proxy")
	}
}

// TestGitHubControlServerStopRemovesPersistedCA verifies Stop purges the on-disk keypair too, not
// just the memory cache: a subsequent SpawnCACert (even via a later "restart") must mint fresh.
func TestGitHubControlServerStopRemovesPersistedCA(t *testing.T) {
	caDir := t.TempDir()
	s, _ := newTestControlServerWithCADir(&fakeMintClient{}, caDir)

	cert1, err := s.SpawnCACert("sp-1")
	if err != nil {
		t.Fatalf("SpawnCACert: %v", err)
	}
	s.Stop("sp-1")

	spawnDir := filepath.Join(caDir, "sp-1")
	if _, err := os.Stat(spawnDir); !os.IsNotExist(err) {
		t.Fatalf("Stop must remove the on-disk CA dir %s, stat err = %v", spawnDir, err)
	}

	// A "restart" (fresh server, same dir) now mints a DIFFERENT CA, because Stop removed the old one.
	s2, _ := newTestControlServerWithCADir(&fakeMintClient{}, caDir)
	cert2, err := s2.SpawnCACert("sp-1")
	if err != nil {
		t.Fatalf("post-stop SpawnCACert: %v", err)
	}
	if bytes.Equal(cert1, cert2) {
		t.Fatal("after Stop removed the persisted CA, a fresh server must mint a new one")
	}
}

// TestGitHubControlServerServeOverStaleUDSSocket verifies Serve succeeds when a UDS path already
// holds a stale socket file — the file a previous process's listener left behind on disk (its
// deferred Close/removal never ran) after gracefulDetachAll left the pod running. Without the
// unlink-before-Listen fix this fails EADDRINUSE, and every re-adopt on the UDS lane would fail to
// bind its control server.
func TestGitHubControlServerServeOverStaleUDSSocket(t *testing.T) {
	s, _ := newTestControlServer(&fakeMintClient{})

	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "gettoken.sock")

	// Simulate the stale socket file a prior process's net.Listen("unix", ...) created and never
	// cleaned up (a plain regular file suffices to trigger Listen's EADDRINUSE — the fix must not
	// rely on it being a real socket).
	if err := os.WriteFile(sockPath, []byte{}, 0o666); err != nil {
		t.Fatalf("seed stale socket file: %v", err)
	}

	if err := s.Serve(spawnlet.ControlTransport{SpawnID: "sp-1", Network: "unix", Address: sockPath}); err != nil {
		t.Fatalf("Serve over a stale socket file: %v", err)
	}
	defer s.Stop("sp-1")

	// Confirm the listener actually works: dial it.
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial the re-served socket: %v", err)
	}
	_ = conn.Close()
}
