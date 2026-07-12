package main

import (
	"crypto/x509"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"spawnery/internal/pki"
)

func TestLoadMoveOptionsSuppliesCurrentCertificateRevocations(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := pki.NewRootCA("root")
	issuer, _ := root.NewIntermediate(pki.IssuerSelfHostedNode, "prod.spawnery.internal")
	revoked := big.NewInt(700)
	list, err := issuer.CreateCRL(big.NewInt(1), []x509.RevocationListEntry{{SerialNumber: revoked, RevocationTime: now.Add(-time.Minute)}}, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rootPath := writeMovePKIFile(t, dir, "root.pem", pki.MarshalCertPEM(root.Cert))
	issuerPath := writeMovePKIFile(t, dir, "issuer.pem", pki.MarshalCertPEM(issuer.Cert))
	crlPath := writeMovePKIFile(t, dir, "current.crl", pki.MarshalCRLPEM(list))
	statePath := filepath.Join(dir, "revocations", "state.json")
	current := now
	clock := func() time.Time { return current }
	opts, err := loadMoveOptions(dir, "dev-token", "", rootPath, "prod.spawnery.internal", statePath, []string{issuerPath}, []string{crlPath}, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if opts.CloseCertificateRevocations != nil {
			_ = opts.CloseCertificateRevocations()
		}
	})
	if opts.CertificateRevocations == nil || opts.CertificateRevocations(issuer.Cert.SerialNumber, big.NewInt(701)) {
		t.Fatal("current CRL did not permit an unlisted certificate")
	}
	if !opts.CertificateRevocations(issuer.Cert.SerialNumber, revoked) {
		t.Fatal("revoked certificate was permitted")
	}
	current = now.Add(time.Hour)
	if !opts.CertificateRevocations(issuer.Cert.SerialNumber, big.NewInt(701)) {
		t.Fatal("live spawnctl checker did not fail closed when CRL expired")
	}
	current = now
	if err := opts.CloseCertificateRevocations(); err != nil {
		t.Fatal(err)
	}
	opts.CloseCertificateRevocations = nil
	reloaded, err := loadMoveOptions(dir, "dev-token", "", rootPath, "prod.spawnery.internal", statePath, []string{issuerPath}, nil, clock)
	if err != nil {
		t.Fatalf("reload persisted current CRL: %v", err)
	}
	t.Cleanup(func() { _ = reloaded.CloseCertificateRevocations() })
	if !reloaded.CertificateRevocations(issuer.Cert.SerialNumber, revoked) {
		t.Fatal("persisted revoked certificate was permitted after reload")
	}
}

func TestLoadMoveOptionsFailsClosedWithoutOrWithStaleCRLState(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := pki.NewRootCA("root")
	issuer, _ := root.NewIntermediate(pki.IssuerService, "prod.spawnery.internal")
	dir := t.TempDir()
	rootPath := writeMovePKIFile(t, dir, "root.pem", pki.MarshalCertPEM(root.Cert))
	issuerPath := writeMovePKIFile(t, dir, "issuer.pem", pki.MarshalCertPEM(issuer.Cert))
	clock := func() time.Time { return now }
	if _, err := loadMoveOptions(dir, "dev-token", "", rootPath, "prod.spawnery.internal", "", nil, nil, clock); err == nil {
		t.Fatal("production options accepted missing revocation state")
	}
	if opts, err := loadMoveOptions(dir, "dev-token", "", rootPath, "prod.spawnery.internal", filepath.Join(dir, "empty-state", "state.json"), []string{issuerPath}, nil, clock); err == nil {
		if opts.CloseCertificateRevocations != nil {
			_ = opts.CloseCertificateRevocations()
		}
		t.Fatal("empty revocation checkpoint accepted")
	}
	list, err := issuer.CreateCRL(big.NewInt(1), nil, now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	crlPath := writeMovePKIFile(t, dir, "stale.crl", pki.MarshalCRLPEM(list))
	if _, err := loadMoveOptions(dir, "dev-token", "", rootPath, "prod.spawnery.internal", filepath.Join(dir, "stale-state", "state.json"), []string{issuerPath}, []string{crlPath}, func() time.Time { return now.Add(time.Minute) }); err == nil {
		t.Fatal("stale CRL accepted")
	}
}

func TestLoadMoveOptionsExplicitCompatibilityModeHasNoCertificateChecker(t *testing.T) {
	opts, err := loadMoveOptions(t.TempDir(), "dev-token", "", "", "", "", nil, nil, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if opts.CertificateRevocations != nil || opts.CloseCertificateRevocations != nil {
		t.Fatal("no-root compatibility mode unexpectedly installed certificate revocation state")
	}
}

func writeMovePKIFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
