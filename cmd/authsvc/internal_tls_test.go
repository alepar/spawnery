package main

import (
	"crypto/tls"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	crlPath := filepath.Join(dir, "issuer.crl")
	if err := os.WriteFile(issuerPath, pki.MarshalCertPEM(issuer.Cert), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(crlPath, pki.MarshalCRLPEM(crl), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &AS{}
	cfg.Internal.RevocationState = filepath.Join(dir, "state", "certificates.json")
	cfg.Internal.RevocationIssuers = issuerPath
	cfg.Internal.RevocationCRLs = crlPath
	state, err := loadCertificateRevocations(cfg.Internal, func() time.Time { return now })
	if err != nil {
		t.Fatalf("load current CRL state: %v", err)
	}
	t.Cleanup(func() { _ = state.Close() })
	if state.IsRevoked(issuer.Cert.SerialNumber, big.NewInt(99)) {
		t.Fatal("current non-revoked serial failed closed")
	}

	cfg.Internal.RevocationCRLs = filepath.Join(dir, "missing.crl")
	cfg.Internal.RevocationState = filepath.Join(dir, "missing-state", "certificates.json")
	if _, err := loadCertificateRevocations(cfg.Internal, func() time.Time { return now }); err == nil {
		t.Fatal("missing configured CRL did not fail startup")
	}
}
