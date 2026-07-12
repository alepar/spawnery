package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	configfiles "spawnery/config"
	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/config"
	"spawnery/internal/pki"
)

func loadCPTest(t *testing.T, env string, getenv map[string]string, sets ...string) (*CP, error) {
	t.Helper()
	return config.Load[CP]("cp", config.Options{
		Args:       []string{"--env=" + env},
		Getenv:     func(k string) (string, bool) { v, ok := getenv[k]; return v, ok },
		Embedded:   configfiles.FS,
		SecretsFS:  configfiles.FS,
		EnvAliases: cpEnvAliases,
		Sets:       sets,
	})
}

func TestCPConfig_Defaults(t *testing.T) {
	cfg, err := loadCPTest(t, "dev", nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8080" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.Store.Driver != "sqlite" || string(cfg.Store.DSN) != sqliteDefaultDSN {
		t.Errorf("Store = %s/%s", cfg.Store.Driver, string(cfg.Store.DSN))
	}
	if !cfg.DevMode() {
		t.Error("expected dev mode by default")
	}
	if cfg.MaxSpawnsPerOwner != 5 {
		t.Errorf("MaxSpawnsPerOwner = %d, want 5", cfg.MaxSpawnsPerOwner)
	}
	if cfg.Evaluator.IdleDetached != 15*time.Minute || cfg.Evaluator.IdleAttached != 60*time.Minute {
		t.Errorf("evaluator idle defaults = %s/%s", cfg.Evaluator.IdleDetached, cfg.Evaluator.IdleAttached)
	}
	if cfg.Auth.RevocationPollInterval != 30*time.Second {
		t.Errorf("revocation_poll_interval = %s, want 30s", cfg.Auth.RevocationPollInterval)
	}
	if cfg.Node.AuthMode != "insecure" || cfg.Node.Listen != "127.0.0.1:8081" {
		t.Errorf("node = %s/%s", cfg.Node.AuthMode, cfg.Node.Listen)
	}
}

func TestCPConfig_EnvAliasOverride(t *testing.T) {
	cfg, err := loadCPTest(t, "dev", map[string]string{
		"CP_LISTEN":               "0.0.0.0:9000",
		"CP_MAX_SPAWNS_PER_OWNER": "9",
		"EVALUATOR_IDLE_DETACHED": "5m",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != "0.0.0.0:9000" {
		t.Errorf("Listen = %q (env alias should win over file)", cfg.Listen)
	}
	if cfg.MaxSpawnsPerOwner != 9 {
		t.Errorf("MaxSpawnsPerOwner = %d, want 9 (string env coerced to int)", cfg.MaxSpawnsPerOwner)
	}
	if cfg.Evaluator.IdleDetached != 5*time.Minute {
		t.Errorf("IdleDetached = %s, want 5m", cfg.Evaluator.IdleDetached)
	}
}

func TestCPConfig_SetOverride(t *testing.T) {
	cfg, err := loadCPTest(t, "dev", nil, "node.auth_mode=enforced")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Node.AuthMode != "enforced" {
		t.Errorf("node.auth_mode = %q, want enforced (--set)", cfg.Node.AuthMode)
	}
}

func TestCPConfig_ProdModeRequiresRootArtifactTrust(t *testing.T) {
	tests := []struct {
		name string
		sets []string
		want string
	}{
		{name: "all missing", sets: []string{"auth.mode=prod"}, want: "auth.environment"},
		{name: "root missing", sets: []string{"auth.mode=prod", "auth.environment=prod"}, want: "auth.root_ca"},
		{name: "state missing", sets: []string{"auth.mode=prod", "auth.environment=prod", "auth.root_ca=/root.pem"}, want: "auth.signer_revocation_state"},
		{name: "legacy raw key does not count", sets: []string{"auth.mode=prod", "auth.as_session_" + "pubkeys=/as.pub"}, want: "auth.environment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadCPTest(t, "dev", nil, tt.sets...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %s validation error, got %v", tt.want, err)
			}
		})
	}

	if _, err := loadCPTest(t, "dev", nil,
		"auth.mode=prod",
		"auth.environment=prod",
		"auth.root_ca=/root.pem",
		"auth.signer_revocation_state=/state/revocations.json",
	); err != nil {
		t.Fatalf("valid root-only production config: %v", err)
	}
}

func TestCPConfig_ArtifactTrustEnvAliases(t *testing.T) {
	cfg, err := loadCPTest(t, "dev", map[string]string{
		"CP_AUTH_ENVIRONMENT":                 "prod",
		"CP_AUTH_ROOT_CA":                     "/root.pem",
		"CP_AUTH_SIGNER_REVOCATION_STATEMENT": "/statement",
		"CP_AUTH_SIGNER_REVOCATION_STATE":     "/state",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Environment != "prod" || cfg.Auth.RootCA != "/root.pem" ||
		cfg.Auth.SignerRevocationStatement != "/statement" || cfg.Auth.SignerRevocationState != "/state" {
		t.Fatalf("artifact trust aliases were not applied: %+v", cfg.Auth)
	}
}

func TestLoadArtifactVerifierGenerationZeroAndStoreLifetime(t *testing.T) {
	root, err := pki.NewRootCA("CP artifact test root")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "root.pem")
	statePath := filepath.Join(dir, "state", "signer-revocations.json")
	if err := os.WriteFile(rootPath, pki.MarshalCertPEM(root.Cert), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := CP{}
	cfg.Auth.Environment = "prod"
	cfg.Auth.RootCA = rootPath
	cfg.Auth.SignerRevocationState = statePath
	verifier, store, err := loadArtifactVerifier(cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if verifier == nil || store == nil || store.Generation() != 0 {
		t.Fatalf("verifier=%v store=%v", verifier, store)
	}
	if _, _, err := loadArtifactVerifier(cfg, time.Now()); err == nil {
		t.Fatal("second store owner should be rejected")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, reopened, err := loadArtifactVerifier(cfg, time.Now())
	if err != nil {
		t.Fatalf("reopen after Close: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadArtifactVerifierRejectsMalformedRoot(t *testing.T) {
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "root.pem")
	if err := os.WriteFile(rootPath, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := CP{}
	cfg.Auth.Environment = "prod"
	cfg.Auth.RootCA = rootPath
	cfg.Auth.SignerRevocationState = filepath.Join(dir, "state")
	if _, _, err := loadArtifactVerifier(cfg, time.Now()); err == nil || !strings.Contains(err.Error(), "parse root") {
		t.Fatalf("expected malformed root rejection, got %v", err)
	}
}

func TestParseSingleRootCertificateRejectsMultipleCertificates(t *testing.T) {
	root, err := pki.NewRootCA("root")
	if err != nil {
		t.Fatal(err)
	}
	pem := pki.MarshalCertPEM(root.Cert)
	if _, err := parseSingleRootCertificate(append(append([]byte(nil), pem...), pem...)); err == nil {
		t.Fatal("accepted multiple root certificates")
	}
}

func TestLoadArtifactVerifierRejectsWrongEnvironmentAndRollbackStatement(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	root, err := pki.NewRootCA("root")
	if err != nil {
		t.Fatal(err)
	}
	intermediateKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "auth signing"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true, IsCA: true,
		Policies: []x509.OID{pki.AuthSigningIntermediatePolicyOID},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, root.Cert, &intermediateKey.PublicKey, root.Key)
	if err != nil {
		t.Fatal(err)
	}
	intermediate, _ := x509.ParseCertificate(der)
	sign := func(environment string, generation uint64) string {
		payload, err := proto.Marshal(&authv1.SignerRevocationStatement{Environment: environment, Generation: generation, IssuedAt: now.Unix()})
		if err != nil {
			t.Fatal(err)
		}
		wire, err := token.SignSignerRevocationStatement(intermediate, intermediateKey, payload)
		if err != nil {
			t.Fatal(err)
		}
		return wire
	}
	dir := t.TempDir()
	rootPath, statePath, statementPath := filepath.Join(dir, "root.pem"), filepath.Join(dir, "state", "revocations.json"), filepath.Join(dir, "statement")
	if err := os.WriteFile(rootPath, pki.MarshalCertPEM(root.Cert), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := CP{}
	cfg.Auth.Environment, cfg.Auth.RootCA, cfg.Auth.SignerRevocationState, cfg.Auth.SignerRevocationStatement = "prod", rootPath, statePath, statementPath
	if err := os.WriteFile(statementPath, []byte(sign("staging", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadArtifactVerifier(cfg, now); err == nil {
		t.Fatal("accepted wrong-environment statement")
	}

	if err := os.WriteFile(statementPath, []byte(sign("prod", 2)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, store, err := loadArtifactVerifier(cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statementPath, []byte(sign("prod", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadArtifactVerifier(cfg, now); err == nil {
		t.Fatal("accepted deployed statement rollback")
	}
}

func TestCPConfig_PostgresRequiresDSN(t *testing.T) {
	_, err := loadCPTest(t, "dev", nil, "store.driver=postgres") // dsn still the sqlite default
	if err == nil || !strings.Contains(err.Error(), "store.dsn") {
		t.Fatalf("expected store.dsn validation error, got %v", err)
	}
}

func TestCPConfig_InvalidEnumIsFatal(t *testing.T) {
	if _, err := loadCPTest(t, "dev", nil, "auth.mode=bogus"); err == nil {
		t.Fatal("expected validation error for auth.mode=bogus")
	}
}
