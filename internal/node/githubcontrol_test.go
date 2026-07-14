package node

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
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
