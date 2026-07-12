package main

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spawnery/internal/pki"
)

func TestPrepareSelfHostedCRLBootstrapsFreshDBBeforeConsumersLoad(t *testing.T) {
	cfg, selfHosted, now := nodeCRLPreflightFixture(t)
	if _, err := os.Stat(cfg.CA.RevocationCRL); !os.IsNotExist(err) {
		t.Fatalf("self-hosted CRL source was preseeded: %v", err)
	}
	if err := prepareSelfHostedCRL(context.Background(), cfg, func() time.Time { return now }); err != nil {
		t.Fatalf("prepare self-hosted CRL: %v", err)
	}
	raw, err := os.ReadFile(cfg.CA.RevocationCRL)
	if err != nil {
		t.Fatal(err)
	}
	list, err := pki.ParseCRLPEM(raw)
	if err != nil || list.Number.Cmp(big.NewInt(1)) != 0 || len(list.RevokedCertificateEntries) != 0 {
		t.Fatalf("bootstrap CRL = %+v, %v", list, err)
	}
	state, _, err := loadCertificateRevocations(cfg.Internal, func() time.Time { return now })
	if err != nil {
		t.Fatalf("consumer load after bootstrap: %v", err)
	}
	t.Cleanup(func() { _ = state.Close() })
	if got, ok := state.HighestNumber(selfHosted.Cert.SerialNumber); !ok || got.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("consumer self-hosted floor = %v, %v", got, ok)
	}
	serviceIssuerRaw, err := os.ReadFile(splitCSV(cfg.Internal.RevocationIssuers)[0])
	if err != nil {
		t.Fatal(err)
	}
	serviceIssuer, err := pki.ParseCertPEM(serviceIssuerRaw)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := state.HighestNumber(serviceIssuer.SerialNumber); !ok || got.Cmp(big.NewInt(7)) != 0 {
		t.Fatalf("pre-existing service CRL floor = %v, %v", got, ok)
	}
}

func TestPrepareSelfHostedCRLReissuesExpiredCheckpointBeforeConsumersLoad(t *testing.T) {
	cfg, selfHosted, now := nodeCRLPreflightFixture(t)
	clock := now
	if err := prepareSelfHostedCRL(t.Context(), cfg, func() time.Time { return clock }); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(25 * time.Hour)
	if err := prepareSelfHostedCRL(t.Context(), cfg, func() time.Time { return clock }); err != nil {
		t.Fatalf("restart renewal: %v", err)
	}
	raw, err := os.ReadFile(cfg.CA.RevocationCRL)
	if err != nil {
		t.Fatal(err)
	}
	list, err := pki.ParseCRLPEM(raw)
	if err != nil || list.Number.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("restart CRL = %+v, %v", list, err)
	}
	state, _, err := loadCertificateRevocations(cfg.Internal, func() time.Time { return clock })
	if err != nil {
		t.Fatalf("consumer load after restart renewal: %v", err)
	}
	t.Cleanup(func() { _ = state.Close() })
	if got, ok := state.HighestNumber(selfHosted.Cert.SerialNumber); !ok || got.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("consumer renewed floor = %v, %v", got, ok)
	}
}

func TestPrepareSelfHostedCRLFailsBeforeServingForMissingIssuerOrBadSink(t *testing.T) {
	t.Run("missing sink", func(t *testing.T) {
		cfg, _, now := nodeCRLPreflightFixture(t)
		cfg.CA.RevocationCRL = ""
		err := prepareSelfHostedCRL(t.Context(), cfg, func() time.Time { return now })
		if err == nil || !strings.Contains(err.Error(), "ca.revocation_crl") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("missing self-hosted issuer", func(t *testing.T) {
		cfg, _, now := nodeCRLPreflightFixture(t)
		issuers := splitCSV(cfg.Internal.RevocationIssuers)
		crls := splitCSV(cfg.Internal.RevocationCRLs)
		cfg.Internal.RevocationIssuers = issuers[0]
		cfg.Internal.RevocationCRLs = crls[0]
		err := prepareSelfHostedCRL(t.Context(), cfg, func() time.Time { return now })
		if err == nil || !strings.Contains(err.Error(), "self-hosted revocation issuer") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("unwritable sink", func(t *testing.T) {
		cfg, _, now := nodeCRLPreflightFixture(t)
		badSink := filepath.Join(t.TempDir(), "sink-is-a-directory")
		if err := os.Mkdir(badSink, 0o700); err != nil {
			t.Fatal(err)
		}
		crls := splitCSV(cfg.Internal.RevocationCRLs)
		cfg.CA.RevocationCRL = badSink
		cfg.Internal.RevocationCRLs = strings.Join([]string{crls[0], badSink}, ",")
		err := prepareSelfHostedCRL(t.Context(), cfg, func() time.Time { return now })
		if err == nil || !strings.Contains(err.Error(), "publish") {
			t.Fatalf("error = %v", err)
		}
	})
}

func nodeCRLPreflightFixture(t *testing.T) (*AS, *pki.CA, time.Time) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	root, err := pki.NewRootCA("root")
	if err != nil {
		t.Fatal(err)
	}
	service, err := root.NewIntermediate(pki.IssuerService, "prod.spawnery.internal")
	if err != nil {
		t.Fatal(err)
	}
	selfHosted, err := root.NewIntermediate(pki.IssuerSelfHostedNode, "prod.spawnery.internal")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "root.pem")
	serviceIssuerPath := filepath.Join(dir, "service-issuer.pem")
	selfHostedIssuerPath := filepath.Join(dir, "self-hosted-issuer.pem")
	selfHostedKeyPath := filepath.Join(dir, "self-hosted-key.pem")
	serviceCRLPath := filepath.Join(dir, "service.crl")
	selfHostedCRLPath := filepath.Join(dir, "self-hosted.crl")
	revocationStateDir := filepath.Join(dir, "revocation-state")
	if err := os.Mkdir(revocationStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPEM, err := pki.MarshalKeyPEM(selfHosted.Key)
	if err != nil {
		t.Fatal(err)
	}
	serviceCRL, err := service.CreateCRL(big.NewInt(7), nil, now, now.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{
		rootPath:             pki.MarshalCertPEM(root.Cert),
		serviceIssuerPath:    pki.MarshalCertPEM(service.Cert),
		selfHostedIssuerPath: pki.MarshalCertPEM(selfHosted.Cert),
		selfHostedKeyPath:    keyPEM,
		serviceCRLPath:       pki.MarshalCRLPEM(serviceCRL),
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &AS{Dev: true}
	cfg.CA.TrustDomain = "prod.spawnery.internal"
	cfg.CA.RootPEM = rootPath
	cfg.CA.IntermediateCert = selfHostedIssuerPath
	cfg.CA.IntermediateKey = selfHostedKeyPath
	cfg.CA.RevocationCRL = selfHostedCRLPath
	cfg.CA.RevocationRenewBefore = 6 * time.Hour
	cfg.DB.Driver = "sqlite"
	cfg.DB.DSN = "file:" + filepath.Join(dir, "identity.db")
	cfg.Internal = ASInternalTLS{
		RootCA: rootPath, TrustDomain: "prod.spawnery.internal",
		RevocationState:   filepath.Join(revocationStateDir, "state.json"),
		RevocationIssuers: strings.Join([]string{serviceIssuerPath, selfHostedIssuerPath}, ","),
		RevocationCRLs:    strings.Join([]string{serviceCRLPath, selfHostedCRLPath}, ","),
	}
	return cfg, selfHosted, now
}
