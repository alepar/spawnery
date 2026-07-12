package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
	cpauth "spawnery/internal/cp/auth"
	"spawnery/internal/pki"
)

func TestSignerRevocationReloaderAppliesHigherGenerationWithoutRestart(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	root, err := pki.NewRootCA("reload root")
	if err != nil {
		t.Fatal(err)
	}
	intermediateKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	intermediate := reloadIssueCert(t, &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "auth signing"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true, IsCA: true, Policies: []x509.OID{pki.AuthSigningIntermediatePolicyOID},
	}, root.Cert, &intermediateKey.PublicKey, root.Key)
	_, leafKey, _ := ed25519.GenerateKey(rand.Reader)
	uri, _ := url.Parse("spiffe://prod.spawnery.internal/signer/auth-artifact/reload-test")
	leaf := reloadIssueCert(t, &x509.Certificate{
		SerialNumber: new(big.Int).Lsh(big.NewInt(1), 127), Subject: pkix.Name{CommonName: "signer"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(12 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		Policies: []x509.OID{pki.AuthArtifactSignerPolicyOID}, URIs: []*url.URL{uri},
	}, intermediate, leafKey.Public(), intermediateKey)
	credential, err := token.NewSigningCredential(leafKey, []*x509.Certificate{leaf, intermediate}, root.Cert, "prod", now)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	statementPath := filepath.Join(dir, "statement")
	statePath := filepath.Join(dir, "state", "revocations.json")
	sign := func(generation uint64, serials [][]byte) string {
		payload, err := proto.Marshal(&authv1.SignerRevocationStatement{Environment: "prod", Generation: generation, IssuedAt: now.Unix(), RevokedSerials: serials})
		if err != nil {
			t.Fatal(err)
		}
		wire, err := token.SignSignerRevocationStatement(intermediate, intermediateKey, payload)
		if err != nil {
			t.Fatal(err)
		}
		return wire
	}
	atomicReplaceForTest(t, statementPath, []byte(sign(1, nil)))
	store, err := token.OpenSignerRevocationStore(statePath, root.Cert, "prod", now)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.LoadAndApply(statementPath, now); err != nil {
		t.Fatal(err)
	}
	verifier, err := token.NewVerifier(root.Cert, "prod", store)
	if err != nil {
		t.Fatal(err)
	}
	sessionPayload, err := proto.Marshal(&authv1.SessionTokenBody{
		AccountId: "account", TokenId: "token", Audience: "cp", IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := credential.Sign(token.ArtifactTypeSession, sessionPayload)
	if err != nil {
		t.Fatal(err)
	}
	authVerifier := cpauth.NewVerifier(cpauth.VerifierConfig{Artifacts: verifier, Now: func() time.Time { return now }})
	if _, err := authVerifier.Verify(wire); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	reloader := newSignerRevocationReloader(store, statementPath)
	reloader.interval = 5 * time.Millisecond
	reloader.now = func() time.Time { return now }
	reloader.onError = func(err error) {
		select {
		case errCh <- err:
		default:
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); reloader.Run(ctx) }()

	atomicReplaceForTest(t, statementPath, []byte(sign(2, [][]byte{leaf.SerialNumber.Bytes()})))
	waitForGeneration(t, store, 2)
	if _, err := verifier.Verify(wire, token.ArtifactTypeSession, now); !errors.Is(err, token.ErrSignerRevoked) {
		t.Fatalf("cached artifact after signer revocation: %v", err)
	}
	if _, err := authVerifier.Verify(wire); err == nil {
		t.Fatal("CP auth accepted artifact from reloaded revoked signer")
	}

	atomicReplaceForTest(t, statementPath, []byte(sign(1, nil)))
	select {
	case err := <-errCh:
		if !errors.Is(err, token.ErrRevocationRollback) {
			t.Fatalf("reload error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("rollback reload did not surface an operational error")
	}
	if store.Generation() != 2 {
		t.Fatalf("rollback changed generation to %d", store.Generation())
	}
	atomicReplaceForTest(t, statementPath, []byte("not an artifact"))
	deadline := time.After(time.Second)
	for {
		select {
		case err := <-errCh:
			if !errors.Is(err, token.ErrRevocationRollback) {
				if store.Generation() != 2 {
					t.Fatalf("malformed reload changed generation to %d", store.Generation())
				}
				goto malformedObserved
			}
		case <-deadline:
			t.Fatal("malformed reload did not surface an operational error")
		}
	}

malformedObserved:
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reloader did not stop with context")
	}
}

func TestSignerRevocationReloaderCancelsBlockedLoad(t *testing.T) {
	started := make(chan struct{})
	unblock := make(chan struct{})
	var startedOnce sync.Once
	reloader := &signerRevocationReloader{interval: time.Millisecond}
	reloader.load = func(context.Context) error {
		startedOnce.Do(func() { close(started) })
		<-unblock
		return nil
	}
	reloader.onError = func(error) {}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); reloader.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("load did not start")
	}
	begin := time.Now()
	if stopSignerRevocationReloader(cancel, done, 20*time.Millisecond) {
		t.Fatal("blocked loader unexpectedly reported a clean stop")
	}
	if elapsed := time.Since(begin); elapsed > 100*time.Millisecond {
		t.Fatalf("bounded shutdown took %s", elapsed)
	}
	close(unblock)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reloader did not exit after load unblocked")
	}
}

func reloadIssueCert(t *testing.T, template, parent *x509.Certificate, public, signer any) *x509.Certificate {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, template, parent, public, signer)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func atomicReplaceForTest(t *testing.T, path string, data []byte) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func waitForGeneration(t *testing.T, store *token.SignerRevocationStore, generation uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for store.Generation() != generation && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if store.Generation() != generation {
		t.Fatalf("generation = %d, want %d", store.Generation(), generation)
	}
}
