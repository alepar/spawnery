package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/node/nodeid"
	"spawnery/internal/pki"
)

func TestGenDevInternalServiceIdentitiesAndCurrentCRLs(t *testing.T) {
	dir := t.TempDir()
	if err := genDev(dir); err != nil {
		t.Fatalf("genDev: %v", err)
	}
	root := readCertificate(t, filepath.Join(dir, "root.pem"))
	serviceIssuer := readCertificate(t, filepath.Join(dir, "service-intermediate.pem"))
	for _, test := range []struct {
		cert, role, name string
	}{
		{"cp-service.pem", pki.RoleCP, "cp.internal"},
		{"authsvc-service.pem", pki.RoleAuthService, "authsvc.internal"},
	} {
		leaf := readCertificate(t, filepath.Join(dir, test.cert))
		principal, err := pki.VerifyPrincipal(leaf, []*x509.Certificate{serviceIssuer}, pki.VerifyOptions{
			Root: root, TrustDomain: pki.DefaultTrustDomain, CurrentTime: time.Now(),
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsRevoked: allowNoCertificateRevocations,
		})
		if err != nil {
			t.Fatalf("verify %s: %v", test.cert, err)
		}
		if principal.Kind != pki.KindService || principal.Role != test.role {
			t.Fatalf("%s principal=%+v", test.cert, principal)
		}
		if err := leaf.VerifyHostname(test.name); err != nil {
			t.Fatalf("%s hostname %s: %v", test.cert, test.name, err)
		}
	}
	for _, name := range []string{"service.crl.pem", "cloud-node.crl.pem", "self-hosted-node.crl.pem"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		list, err := pki.ParseCRLPEM(raw)
		if err != nil || !list.NextUpdate.After(time.Now()) {
			t.Fatalf("current %s: list=%v err=%v", name, list, err)
		}
	}
}

func TestMintAuthTokenUsesCertifiedSignerCredential(t *testing.T) {
	dir := t.TempDir()
	if err := genDev(dir); err != nil {
		t.Fatal(err)
	}
	spki := []byte("session-key-spki")
	wire, err := mintAuthToken(
		filepath.Join(dir, "root.pem"),
		filepath.Join(dir, "auth-signer-current-key.pem"),
		filepath.Join(dir, "auth-signer-current-chain.pem"),
		"dev", "node", "acct-1", spki, time.Now(),
	)
	if err != nil {
		t.Fatalf("mintAuthToken: %v", err)
	}
	root := readCertificate(t, filepath.Join(dir, "root.pem"))
	verifier, err := token.NewVerifier(root, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := verifier.Verify(wire, token.ArtifactTypeSession, time.Now())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	var body authv1.SessionTokenBody
	if err := proto.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256(spki)
	if body.Audience != "node" || body.AccountId != "acct-1" || string(body.SessionKeyHash) != string(wantHash[:]) {
		t.Fatalf("body = %+v", &body)
	}
}

func TestSignSignerRevocationRevokesCertifiedLeaf(t *testing.T) {
	dir := t.TempDir()
	if err := genDev(dir); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	wire, err := signSignerRevocation(
		filepath.Join(dir, "auth-signing-intermediate.pem"),
		filepath.Join(dir, "auth-signing-intermediate-key.pem"),
		filepath.Join(dir, "auth-signer-current-chain.pem"),
		"dev", 7, now,
	)
	if err != nil {
		t.Fatalf("signSignerRevocation: %v", err)
	}
	statement, err := token.ParseSignerRevocationStatement(wire, readCertificate(t, filepath.Join(dir, "root.pem")), "dev", now)
	if err != nil {
		t.Fatalf("ParseSignerRevocationStatement: %v", err)
	}
	if statement.Generation() != 7 {
		t.Fatalf("generation = %d", statement.Generation())
	}
	leaf := readCertificateChain(t, filepath.Join(dir, "auth-signer-current-chain.pem"))[0]
	state, _ := token.NewSignerRevocationState(readCertificate(t, filepath.Join(dir, "root.pem")), "dev")
	if err := state.Apply(statement); err != nil {
		t.Fatal(err)
	}
	if err := state.RejectSigner(leaf); err == nil {
		t.Fatal("revoked leaf remained accepted")
	}
}

func TestGenDevEmitsCertifiedAuthArtifactSigners(t *testing.T) {
	dir := t.TempDir()
	before := time.Now()
	if err := genDev(dir); err != nil {
		t.Fatalf("genDev: %v", err)
	}

	root := readCertificate(t, filepath.Join(dir, "root.pem"))
	intermediate := readCertificate(t, filepath.Join(dir, "auth-signing-intermediate.pem"))
	if !pki.HasPolicy(intermediate, pki.AuthSigningIntermediatePolicyOID) {
		t.Fatalf("auth-signing intermediate policies = %v", intermediate.Policies)
	}
	if _, err := os.Stat(filepath.Join(dir, "auth-signing-intermediate-key.pem")); err != nil {
		t.Fatalf("offline intermediate key: %v", err)
	}

	var leaves []*x509.Certificate
	for _, name := range []string{"current", "next"} {
		keyBytes, err := os.ReadFile(filepath.Join(dir, "auth-signer-"+name+"-key.pem"))
		if err != nil {
			t.Fatalf("read %s key: %v", name, err)
		}
		keyBlock, rest := pem.Decode(keyBytes)
		if keyBlock == nil || len(rest) != 0 {
			t.Fatalf("decode %s key", name)
		}
		parsedKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
		if err != nil {
			t.Fatalf("parse %s key: %v", name, err)
		}
		privateKey, ok := parsedKey.(ed25519.PrivateKey)
		if !ok {
			t.Fatalf("%s key = %T, want Ed25519", name, parsedKey)
		}
		chain := readCertificateChain(t, filepath.Join(dir, "auth-signer-"+name+"-chain.pem"))
		if len(chain) != 2 || !chain[1].Equal(intermediate) || chain[0].Equal(root) || chain[1].Equal(root) {
			t.Fatalf("%s chain must be leaf, intermediate and omit root", name)
		}
		if _, err := token.NewSigningCredential(privateKey, chain, root, "dev", before.Add(time.Minute)); err != nil {
			t.Fatalf("load %s signing credential: %v", name, err)
		}
		leaves = append(leaves, chain[0])
	}
	if overlapStart := maxTime(leaves[0].NotBefore, leaves[1].NotBefore); minTime(leaves[0].NotAfter, leaves[1].NotAfter).Sub(overlapStart) < 24*time.Hour {
		t.Fatal("current and next auth signers overlap by less than 24 hours")
	}
	for _, legacy := range []string{"session-key.pem", "session-pub.pem"} {
		if _, err := os.Stat(filepath.Join(dir, legacy)); !os.IsNotExist(err) {
			t.Fatalf("legacy raw-key file %s must not be emitted", legacy)
		}
	}
}

func readCertificate(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := pki.ParseCertPEM(data)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func readCertificateChain(t *testing.T, path string) []*x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var chain []*x509.Certificate
	for len(data) > 0 {
		block, rest := pem.Decode(data)
		if block == nil || block.Type != "CERTIFICATE" {
			t.Fatalf("invalid certificate chain %s", path)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		chain = append(chain, cert)
		data = rest
	}
	return chain
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// TestGenDevCloudNodeIdentity verifies that genDev emits a cloud node identity under node-cloud/
// that chains to the dev root CA and reports class=cloud, nodeID=node-1, accountID=spawnery-system.
func TestGenDevCloudNodeIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := genDev(dir); err != nil {
		t.Fatalf("genDev: %v", err)
	}

	id, err := nodeid.Load(filepath.Join(dir, "node-cloud"))
	if err != nil {
		t.Fatalf("nodeid.Load(node-cloud): %v", err)
	}

	leaf, err := pki.ParseCertPEM(id.CertPEM)
	if err != nil {
		t.Fatalf("parse leaf cert: %v", err)
	}
	inter, err := pki.ParseCertPEM(id.ChainPEM)
	if err != nil {
		t.Fatalf("parse chain cert: %v", err)
	}
	root, err := pki.ParseCertPEM(id.RootPEM)
	if err != nil {
		t.Fatalf("parse root cert: %v", err)
	}

	identity, err := pki.Verify(leaf, []*x509.Certificate{inter}, root, pki.DefaultTrustDomain, time.Now(), allowNoCertificateRevocations)
	if err != nil {
		t.Fatalf("Verify cloud node identity: %v", err)
	}
	if identity.Class != pki.ClassCloud {
		t.Errorf("class = %q, want %q", identity.Class, pki.ClassCloud)
	}
	if identity.AccountID != "spawnery-system" {
		t.Errorf("accountID = %q, want %q", identity.AccountID, "spawnery-system")
	}
	if identity.NodeID != "node-1" {
		t.Errorf("nodeID = %q, want %q", identity.NodeID, "node-1")
	}
}

// TestGenDevSelfHostedNodeIdentityStillPresent verifies that the self-hosted node identity
// (used by dev-enforced / node-enforced) is still emitted by genDev unchanged.
func TestGenDevSelfHostedNodeIdentityStillPresent(t *testing.T) {
	dir := t.TempDir()
	if err := genDev(dir); err != nil {
		t.Fatalf("genDev: %v", err)
	}

	id, err := nodeid.Load(filepath.Join(dir, "node"))
	if err != nil {
		t.Fatalf("nodeid.Load(node): %v", err)
	}

	leaf, err := pki.ParseCertPEM(id.CertPEM)
	if err != nil {
		t.Fatalf("parse leaf cert: %v", err)
	}
	inter, err := pki.ParseCertPEM(id.ChainPEM)
	if err != nil {
		t.Fatalf("parse chain cert: %v", err)
	}
	root, err := pki.ParseCertPEM(id.RootPEM)
	if err != nil {
		t.Fatalf("parse root cert: %v", err)
	}

	identity, err := pki.Verify(leaf, []*x509.Certificate{inter}, root, pki.DefaultTrustDomain, time.Now(), allowNoCertificateRevocations)
	if err != nil {
		t.Fatalf("Verify self-hosted node identity: %v", err)
	}
	if identity.Class != pki.ClassSelfHosted {
		t.Errorf("class = %q, want %q", identity.Class, pki.ClassSelfHosted)
	}
	if identity.AccountID != "alice" {
		t.Errorf("accountID = %q, want %q", identity.AccountID, "alice")
	}
	if identity.NodeID != "node-1" {
		t.Errorf("nodeID = %q, want %q", identity.NodeID, "node-1")
	}
}
