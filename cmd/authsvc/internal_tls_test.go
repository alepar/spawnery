package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"spawnery/internal/mtls"
	"spawnery/internal/pki"
)

func TestNewInternalHTTPServerUsesDirectTLSAndHTTP2(t *testing.T) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13}
	server, err := newInternalHTTPServer("127.0.0.1:0", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), tlsConfig)
	if err != nil {
		t.Fatalf("new internal server: %v", err)
	}
	if server.TLSConfig != tlsConfig {
		t.Fatal("internal server did not retain the direct TLS configuration")
	}
	if _, ok := server.TLSNextProto["h2"]; !ok {
		t.Fatal("internal server did not enable HTTP/2")
	}
}

func TestValidateInternalServerNameRequiresLeafHostname(t *testing.T) {
	now := time.Now().UTC()
	root, _ := pki.NewRootCA("root")
	issuer, _ := root.NewIntermediate(pki.IssuerService, "prod.spawnery.internal")
	leaf, _ := issuer.IssueService(pki.RoleAuthService, "as-1", "prod.spawnery.internal", []string{"authsvc.internal"}, nil, now.Add(time.Hour))
	identity, _ := leaf.TLSCertificate()

	if err := validateInternalServerName(identity, "authsvc.internal"); err != nil {
		t.Fatalf("valid server name: %v", err)
	}
	for _, name := range []string{"", "wrong.internal"} {
		if err := validateInternalServerName(identity, name); err == nil {
			t.Fatalf("server name %q accepted", name)
		}
	}
}

func TestLoadCertificateRevocationsRequiresCurrentConfiguredCRL(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := pki.NewRootCA("root")
	issuer, _ := root.NewIntermediate(pki.IssuerService, "prod.spawnery.internal")
	crl, err := issuer.CreateCRL(big.NewInt(1), nil, now.Add(-time.Minute), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	issuerPath := filepath.Join(dir, "issuer.pem")
	rootPath := filepath.Join(dir, "root.pem")
	crlPath := filepath.Join(dir, "issuer.crl")
	if err := os.WriteFile(issuerPath, pki.MarshalCertPEM(issuer.Cert), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootPath, pki.MarshalCertPEM(root.Cert), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(crlPath, pki.MarshalCRLPEM(crl), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &AS{}
	cfg.Internal.RootCA = rootPath
	cfg.Internal.TrustDomain = "prod.spawnery.internal"
	cfg.Internal.RevocationState = filepath.Join(dir, "state", "certificates.json")
	cfg.Internal.RevocationIssuers = issuerPath
	cfg.Internal.RevocationCRLs = crlPath
	state, refresher, err := loadCertificateRevocations(cfg.Internal, func() time.Time { return now })
	if err != nil {
		t.Fatalf("load current CRL state: %v", err)
	}
	t.Cleanup(func() { _ = state.Close() })
	if state.IsRevoked(issuer.Cert.SerialNumber, big.NewInt(99)) {
		t.Fatal("current non-revoked serial failed closed")
	}
	connections := mtls.NewConnectionRegistry()
	unsubscribe := mtls.SubscribeConnectionRegistry(state, connections)
	t.Cleanup(unsubscribe)
	live, cancelLive := context.WithCancel(t.Context())
	release := connections.Register(mtls.PeerCertificate{IssuerSerial: issuer.Cert.SerialNumber, LeafSerial: big.NewInt(99)}, cancelLive)
	t.Cleanup(release)
	updated, err := issuer.CreateCRL(big.NewInt(2), []x509.RevocationListEntry{{SerialNumber: big.NewInt(99), RevocationTime: now}}, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(crlPath, pki.MarshalCRLPEM(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := refresher.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !state.IsRevoked(issuer.Cert.SerialNumber, big.NewInt(99)) {
		t.Fatal("authsvc fresh verification did not reject revoked serial")
	}
	select {
	case <-live.Done():
	case <-time.After(time.Second):
		t.Fatal("authsvc accepted CRL did not cancel matching live connection")
	}

	cfg.Internal.RevocationCRLs = filepath.Join(dir, "missing.crl")
	cfg.Internal.RevocationState = filepath.Join(dir, "missing-state", "certificates.json")
	if _, _, err := loadCertificateRevocations(cfg.Internal, func() time.Time { return now }); err == nil {
		t.Fatal("missing configured CRL did not fail startup")
	}
}

func TestLoadCertificateRevocationsRecoversExpiredCheckpointOnlyWithHigherCurrentSource(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	recoveryTime := base.Add(2 * time.Minute)
	root, _ := pki.NewRootCA("root")
	issuer, _ := root.NewIntermediate(pki.IssuerService, "prod.spawnery.internal")
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "root.pem")
	issuerPath := filepath.Join(dir, "issuer.pem")
	crlPath := filepath.Join(dir, "issuer.crl")
	for path, data := range map[string][]byte{rootPath: pki.MarshalCertPEM(root.Cert), issuerPath: pki.MarshalCertPEM(issuer.Cert)} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	expired, _ := issuer.CreateCRL(big.NewInt(5), nil, base, base.Add(time.Minute))
	makeExpiredState := func(name string) string {
		path := filepath.Join(dir, name, "state.json")
		state, err := pki.OpenRevocationState(path, []*x509.Certificate{issuer.Cert}, func() time.Time { return base })
		if err != nil {
			t.Fatal(err)
		}
		if err := state.ApplyPEM(pki.MarshalCRLPEM(expired)); err != nil {
			t.Fatal(err)
		}
		if err := state.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	config := func(statePath string) ASInternalTLS {
		return ASInternalTLS{RootCA: rootPath, TrustDomain: "prod.spawnery.internal", RevocationState: statePath, RevocationIssuers: issuerPath, RevocationCRLs: crlPath}
	}

	fresh, _ := issuer.CreateCRL(big.NewInt(6), nil, recoveryTime, recoveryTime.Add(time.Hour))
	if err := os.WriteFile(crlPath, pki.MarshalCRLPEM(fresh), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := loadCertificateRevocations(config(makeExpiredState("recover")), func() time.Time { return recoveryTime })
	if err != nil {
		t.Fatalf("higher source did not recover expired checkpoint: %v", err)
	}
	if got, ok := recovered.HighestNumber(issuer.Cert.SerialNumber); !ok || got.Cmp(big.NewInt(6)) != 0 {
		t.Fatalf("recovered floor = %v, %v", got, ok)
	}
	_ = recovered.Close()

	if err := os.WriteFile(crlPath, pki.MarshalCRLPEM(expired), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadCertificateRevocations(config(makeExpiredState("stale")), func() time.Time { return recoveryTime }); err == nil {
		t.Fatal("expired source made startup ready")
	}
	lower, _ := issuer.CreateCRL(big.NewInt(4), nil, recoveryTime, recoveryTime.Add(time.Hour))
	if err := os.WriteFile(crlPath, pki.MarshalCRLPEM(lower), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadCertificateRevocations(config(makeExpiredState("rollback")), func() time.Time { return recoveryTime }); !errors.Is(err, pki.ErrCRLRollback) {
		t.Fatalf("restart rollback error = %v", err)
	}
	noSource := config(makeExpiredState("no-source"))
	noSource.RevocationCRLs = ""
	if _, _, err := loadCertificateRevocations(noSource, func() time.Time { return recoveryTime }); err == nil {
		t.Fatal("restart without current source became ready")
	}
}
