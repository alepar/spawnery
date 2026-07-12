package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	configfiles "spawnery/config"
	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/config"
	"spawnery/internal/mtls"
	"spawnery/internal/pki"
	"spawnery/internal/spawnlet"
	"spawnery/internal/storage"
)

// loadSpawnletTest is a test helper that calls config.Load[Spawnlet] with an injected getenv map
// and optional --set overrides, bypassing SPAWNERY_ENV and the real process environment.
func loadSpawnletTest(t *testing.T, env string, getenv map[string]string, sets ...string) (*Spawnlet, error) {
	t.Helper()
	return config.Load[Spawnlet]("spawnlet", config.Options{
		Args:       []string{"--env=" + env},
		Getenv:     func(k string) (string, bool) { v, ok := getenv[k]; return v, ok },
		Embedded:   configfiles.FS,
		SecretsFS:  configfiles.FS,
		EnvAliases: spawnletEnvAliases,
		Sets:       sets,
	})
}

func TestSpawnletCertificateCRLClosesMatchingLiveConnection(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := pki.NewRootCA("root")
	issuer, _ := root.NewIntermediate(pki.IssuerService, "prod.spawnery.internal")
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "root.pem")
	issuerPath := filepath.Join(dir, "issuer.pem")
	crlPath := filepath.Join(dir, "issuer.crl")
	initial, _ := issuer.CreateCRL(big.NewInt(1), nil, now, now.Add(time.Hour))
	for path, data := range map[string][]byte{rootPath: pki.MarshalCertPEM(root.Cert), issuerPath: pki.MarshalCertPEM(issuer.Cert), crlPath: pki.MarshalCRLPEM(initial)} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &Spawnlet{}
	cfg.Node.AuthMode = "enforced"
	cfg.Node.RootCA = rootPath
	cfg.Node.TrustDomain = "prod.spawnery.internal"
	cfg.Node.CertificateRevocationState = filepath.Join(dir, "state", "certificates.json")
	cfg.Node.CertificateRevocationIssuers = issuerPath
	cfg.Node.CertificateRevocationCRLs = crlPath
	runtime, err := loadNodeCertificateRevocations(cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		runtime.unsubscribe()
		_ = runtime.state.Close()
	})
	live, cancelLive := context.WithCancel(t.Context())
	release := runtime.connections.Register(mtls.PeerCertificate{IssuerSerial: issuer.Cert.SerialNumber, LeafSerial: big.NewInt(77)}, cancelLive)
	t.Cleanup(release)
	updated, _ := issuer.CreateCRL(big.NewInt(2), []x509.RevocationListEntry{{SerialNumber: big.NewInt(77), RevocationTime: now}}, now, now.Add(time.Hour))
	if err := os.WriteFile(crlPath, pki.MarshalCRLPEM(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runtime.refresh(); err != nil {
		t.Fatal(err)
	}
	if !runtime.state.IsRevoked(issuer.Cert.SerialNumber, big.NewInt(77)) {
		t.Fatal("spawnlet fresh verification did not reject revoked serial")
	}
	select {
	case <-live.Done():
	case <-time.After(time.Second):
		t.Fatal("spawnlet accepted CRL did not close matching live connection")
	}
}

// --- config-framework tests -----------------------------------------------

func TestSpawnletConfig_Defaults(t *testing.T) {
	cfg, err := loadSpawnletTest(t, "dev", nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.AgentImage != "spawnery/stubagent:dev" {
		t.Errorf("AgentImage = %q", cfg.AgentImage)
	}
	if cfg.DataRoot != "/var/lib/spawnlet/spawns" {
		t.Errorf("DataRoot = %q", cfg.DataRoot)
	}
	if cfg.Node.ID != "node-1" || cfg.Node.Class != "cloud" {
		t.Errorf("Node = %s/%s", cfg.Node.ID, cfg.Node.Class)
	}
	if !cfg.Egress.Enforce {
		t.Error("Egress.Enforce should default to true")
	}
	if cfg.Limits.MemMB != 1024 || cfg.Limits.CPU != 1.0 || cfg.Limits.Pids != 256 {
		t.Errorf("Limits = %d/%f/%d", cfg.Limits.MemMB, cfg.Limits.CPU, cfg.Limits.Pids)
	}
	if cfg.CRI.Endpoint != "unix:///run/containerd/containerd.sock" {
		t.Errorf("CRI.Endpoint = %q", cfg.CRI.Endpoint)
	}
	if cfg.Journal.Backend != "" {
		t.Errorf("Journal.Backend should default to empty (disabled), got %q", cfg.Journal.Backend)
	}
	if cfg.Journal.S3.Region != "garage" {
		t.Errorf("Journal.S3.Region = %q, want garage", cfg.Journal.S3.Region)
	}
	if cfg.Node.AuthMode != "insecure" {
		t.Errorf("Node.AuthMode = %q, want insecure", cfg.Node.AuthMode)
	}
	if cfg.CP.Addr != "" {
		t.Errorf("CP.Addr should default to empty (standalone mode), got %q", cfg.CP.Addr)
	}
}

func TestSpawnletConfig_EnvAliasOverride(t *testing.T) {
	cfg, err := loadSpawnletTest(t, "dev", map[string]string{
		"NODE_ID":        "node-prod-1",
		"NODE_CLASS":     "self-hosted",
		"MEM_LIMIT_MB":   "2048",
		"EGRESS_ENFORCE": "false",
		"CP_ADDR":        "http://cp.example.com:8080",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Node.ID != "node-prod-1" {
		t.Errorf("Node.ID = %q (NODE_ID alias should win over file)", cfg.Node.ID)
	}
	if cfg.Node.Class != "self-hosted" {
		t.Errorf("Node.Class = %q", cfg.Node.Class)
	}
	if cfg.Limits.MemMB != 2048 {
		t.Errorf("Limits.MemMB = %d, want 2048 (string env coerced to int64)", cfg.Limits.MemMB)
	}
	if cfg.Egress.Enforce {
		t.Error("Egress.Enforce should be false when EGRESS_ENFORCE=false")
	}
	if cfg.CP.Addr != "http://cp.example.com:8080" {
		t.Errorf("CP.Addr = %q", cfg.CP.Addr)
	}
}

func TestSpawnletConfig_SetOverride(t *testing.T) {
	cfg, err := loadSpawnletTest(t, "dev", nil,
		"node.auth_mode=enforced", "node.signer_revocation_state=/tmp/spawnlet-test-state",
		"node.certificate_revocation_state=/tmp/cert-state", "node.certificate_revocation_issuers=/tmp/issuer.pem",
		"node.certificate_revocation_crls=/tmp/issuer.crl", "cp.server_name=cp.internal", "limits.pids=512")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Node.AuthMode != "enforced" {
		t.Errorf("Node.AuthMode = %q, want enforced (--set)", cfg.Node.AuthMode)
	}
	if cfg.Limits.Pids != 512 {
		t.Errorf("Limits.Pids = %d, want 512 (--set)", cfg.Limits.Pids)
	}
}

func TestSpawnletConfig_EnforcedArtifactTrustRequirements(t *testing.T) {
	for _, tc := range []struct {
		name string
		sets []string
		want string
	}{
		{name: "environment", sets: []string{"node.environment="}, want: "node.environment"},
		{name: "root", sets: []string{"node.id_dir=", "node.root_ca="}, want: "node.root_ca"},
		{name: "state", sets: nil, want: "node.signer_revocation_state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sets := append([]string{"node.auth_mode=enforced"}, tc.sets...)
			_, err := loadSpawnletTest(t, "dev", nil, sets...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSpawnletConfig_RejectsUnknownAuthMode(t *testing.T) {
	for _, mode := range []string{"enforcd", "", "production"} {
		t.Run(mode, func(t *testing.T) {
			_, err := loadSpawnletTest(t, "dev", nil, "node.auth_mode="+mode)
			if err == nil || !strings.Contains(err.Error(), "node.auth_mode") {
				t.Fatalf("mode %q error = %v, want closed-enum rejection", mode, err)
			}
		})
	}
}

func TestBuildIntentVerifierRejectsUnknownAuthMode(t *testing.T) {
	cfg := &Spawnlet{}
	cfg.Node.AuthMode = "enforcd"
	if _, err := buildIntentVerifier(cfg, nil, "node-1", ""); err == nil {
		t.Fatal("auth mode typo selected verify-log instead of failing closed")
	}
}

func TestSpawnletConfig_ArtifactTrustAliasesAndRemovedRawKeys(t *testing.T) {
	cfg, err := loadSpawnletTest(t, "dev", map[string]string{
		"NODE_AUTH_ENVIRONMENT":            "prod",
		"NODE_SIGNER_REVOCATION_STATEMENT": "/deployment/revocations",
		"NODE_SIGNER_REVOCATION_STATE":     "/state/revocations",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node.Environment != "prod" || cfg.Node.SignerRevocationStatement != "/deployment/revocations" || cfg.Node.SignerRevocationState != "/state/revocations" {
		t.Fatalf("artifact trust aliases not loaded: %+v", cfg.Node)
	}
	legacyAlias := strings.Join([]string{"NODE", "AS", "PUBKEYS"}, "_")
	if _, exists := spawnletEnvAliases[legacyAlias]; exists {
		t.Fatalf("legacy raw-key alias %q remains trusted", legacyAlias)
	}
}

func TestArtifactTrustIdentityDirectoryRootDefaultForAllNodeClasses(t *testing.T) {
	now := time.Now()
	root, err := pki.NewRootCA("root")
	if err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{"cloud", "self-hosted"} {
		t.Run(class, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "root.pem"), pki.MarshalCertPEM(root.Cert), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := &Spawnlet{}
			cfg.Node.Class = class
			cfg.Node.IDDir = dir
			cfg.Node.Environment = "prod"
			cfg.Node.SignerRevocationState = filepath.Join(dir, "revocations", "state.json")
			trust, err := loadArtifactVerifier(cfg, now)
			if err != nil {
				t.Fatal(err)
			}
			if err := trust.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestArtifactTrustLiveReloadIsMonotonicAndCancelled(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	root, err := pki.NewRootCA("test root")
	if err != nil {
		t.Fatal(err)
	}
	intermediateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	intermediateTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "auth signing"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true, IsCA: true, MaxPathLen: 0, MaxPathLenZero: true,
		Policies: []x509.OID{pki.AuthSigningIntermediatePolicyOID},
	}
	der, err := x509.CreateCertificate(rand.Reader, intermediateTemplate, root.Cert, &intermediateKey.PublicKey, root.Key)
	if err != nil {
		t.Fatal(err)
	}
	intermediate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "root.pem")
	statementPath := filepath.Join(dir, "statement")
	if err := os.WriteFile(rootPath, pki.MarshalCertPEM(root.Cert), 0o600); err != nil {
		t.Fatal(err)
	}
	writeStatement := func(generation uint64, issuedAt time.Time) {
		t.Helper()
		payload, err := proto.Marshal(&authv1.SignerRevocationStatement{Environment: "prod", Generation: generation, IssuedAt: issuedAt.Unix()})
		if err != nil {
			t.Fatal(err)
		}
		wire, err := token.SignSignerRevocationStatement(intermediate, intermediateKey, payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statementPath, []byte(wire+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeStatement(1, now)
	cfg := &Spawnlet{}
	cfg.Node.Environment = "prod"
	cfg.Node.RootCA = rootPath
	cfg.Node.SignerRevocationStatement = statementPath
	cfg.Node.SignerRevocationState = filepath.Join(dir, "state", "revocations.json")
	trust, err := loadArtifactVerifier(cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := trust.revocations.Generation(); got != 1 {
		t.Fatalf("initial generation = %d, want 1", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errorsSeen := make(chan error, 4)
	done := trust.watch(ctx, 5*time.Millisecond, func() time.Time { return now }, func(err error) { errorsSeen <- err })
	writeStatement(2, now)
	waitForGeneration(t, trust.revocations, 2)
	writeStatement(1, now.Add(-time.Second))
	select {
	case <-errorsSeen:
	case <-time.After(time.Second):
		t.Fatal("rollback did not surface an operational error")
	}
	if got := trust.revocations.Generation(); got != 2 {
		t.Fatalf("generation after rollback = %d, want 2", got)
	}
	if err := os.WriteFile(statementPath, []byte("malformed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-errorsSeen:
	case <-time.After(time.Second):
		t.Fatal("malformed replacement did not surface an operational error")
	}
	if got := trust.revocations.Generation(); got != 2 {
		t.Fatalf("generation after malformed replacement = %d, want 2", got)
	}
	cancel()
	<-done
	writeStatement(3, now)
	time.Sleep(30 * time.Millisecond)
	if got := trust.revocations.Generation(); got != 2 {
		t.Fatalf("generation after cancellation = %d, want 2", got)
	}
	if err := trust.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Node.SignerRevocationStatement = ""
	reopened, err := loadArtifactVerifier(cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.revocations.Generation(); got != 2 {
		t.Fatalf("generation after restart = %d, want 2", got)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Node.SignerRevocationStatement = statementPath
	writeStatement(1, now.Add(-time.Second))
	if _, err := loadArtifactVerifier(cfg, now); err == nil {
		t.Fatal("startup accepted rollback statement")
	}
	if err := os.WriteFile(statementPath, []byte("malformed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadArtifactVerifier(cfg, now); err == nil {
		t.Fatal("startup accepted malformed statement")
	}
}

func TestArtifactTrustShutdownIsBoundedWithBlockedLoader(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var closes atomic.Int32
	trust := &artifactTrust{
		statementPath: "configured",
		reload: func(context.Context, time.Time) error {
			close(started)
			<-release
			return nil
		},
		closeStore: func() error { closes.Add(1); return nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := trust.watch(ctx, time.Millisecond, time.Now, nil)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("reload did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(50 * time.Millisecond):
		close(release)
		t.Fatal("watch shutdown waited for blocked loader")
	}
	closeCtx, cancelClose := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelClose()
	if err := trust.CloseContext(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked reload close error = %v, want deadline", err)
	}
	if got := closes.Load(); got != 0 {
		t.Fatalf("store close called %d times during active reload", got)
	}
	close(release)
	finalCtx, cancelFinal := context.WithTimeout(context.Background(), time.Second)
	defer cancelFinal()
	if err := trust.CloseContext(finalCtx); err != nil {
		t.Fatalf("close after reload returned: %v", err)
	}
}

func waitForGeneration(t *testing.T, store *token.SignerRevocationStore, want uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if store.Generation() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("generation = %d, want %d", store.Generation(), want)
}

func TestSpawnletConfig_CSVAgentBinaries(t *testing.T) {
	cfg, err := loadSpawnletTest(t, "dev", map[string]string{
		"AGENT_BINARIES": "opencode,goose,claude-code",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.AgentBinaries) != 3 || cfg.AgentBinaries[0] != "opencode" {
		t.Errorf("AgentBinaries = %v, want [opencode goose claude-code]", cfg.AgentBinaries)
	}
}

func TestSpawnletConfig_GetTokenListenIPEnvAlias(t *testing.T) {
	cfg, err := loadSpawnletTest(t, "dev", map[string]string{"GETTOKEN_LISTEN_IP": "10.234.0.1"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.GetTokenListenIP != "10.234.0.1" {
		t.Fatalf("GetTokenListenIP = %q, want CNI gateway", cfg.GetTokenListenIP)
	}
}

func TestSpawnletConfig_GitHubOverrideEnvAliases(t *testing.T) {
	cfg, err := loadSpawnletTest(t, "dev", map[string]string{
		"GITHUB_API_BASE_URL":        "http://127.0.0.1:3000/api/v1",
		"GITHUB_HOST":                "127.0.0.1:3000",
		"GITHUB_ALLOW_INSECURE_HOST": "true",
		"GITHUB_STATIC_TOKEN":        "gitea-pat-abc",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.GitHub.APIBaseURL != "http://127.0.0.1:3000/api/v1" {
		t.Errorf("GitHub.APIBaseURL = %q", cfg.GitHub.APIBaseURL)
	}
	if cfg.GitHub.Host != "127.0.0.1:3000" {
		t.Errorf("GitHub.Host = %q", cfg.GitHub.Host)
	}
	if !cfg.GitHub.AllowInsecureHost {
		t.Error("GitHub.AllowInsecureHost = false, want true")
	}
	if string(cfg.GitHub.StaticToken) != "gitea-pat-abc" {
		t.Errorf("GitHub.StaticToken not resolved (redaction is display-only)")
	}
}

// TestSpawnletConfig_GitHubOverrideAbsentDefault pins that the github override is OFF by default:
// no env, no static token, secure. This is the production-parity invariant.
func TestSpawnletConfig_GitHubOverrideAbsentDefault(t *testing.T) {
	cfg, err := loadSpawnletTest(t, "dev", nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.GitHub.Host != "" || cfg.GitHub.APIBaseURL != "" || cfg.GitHub.AllowInsecureHost || string(cfg.GitHub.StaticToken) != "" {
		t.Errorf("github override should be empty by default, got %+v", cfg.GitHub)
	}
}

// --- configureGitHubOverride tests ----------------------------------------

// TestConfigureGitHubOverride_AbsentIsNoOp is the production-default guarantee: with no GitHub
// config, managerCfg carries no host override, no repo service, and no static credentials.
func TestConfigureGitHubOverride_AbsentIsNoOp(t *testing.T) {
	cfg := &Spawnlet{}
	cfg.DataRoot = t.TempDir()
	var mc spawnlet.ManagerConfig
	if err := configureGitHubOverride(&mc, cfg); err != nil {
		t.Fatalf("configureGitHubOverride: %v", err)
	}
	if mc.GitHubHost != "" || mc.GitHubAllowInsecureHost || mc.GitHubRepos != nil || mc.GitHubStaticCredentials != nil {
		t.Fatalf("expected no-op, got %+v", mc)
	}
}

// TestConfigureGitHubOverride_StaticToken wires the full Gitea lane: repo service targeted at the
// API base, host override + insecure flag set, and a static credential provider backed by a rendered
// credential-helper on disk.
func TestConfigureGitHubOverride_StaticToken(t *testing.T) {
	cfg := &Spawnlet{}
	cfg.DataRoot = t.TempDir()
	cfg.GitHub.APIBaseURL = "http://127.0.0.1:3000/api/v1"
	cfg.GitHub.Host = "127.0.0.1:3000"
	cfg.GitHub.AllowInsecureHost = true
	cfg.GitHub.StaticToken = "gitea-pat-abc"

	var mc spawnlet.ManagerConfig
	if err := configureGitHubOverride(&mc, cfg); err != nil {
		t.Fatalf("configureGitHubOverride: %v", err)
	}
	if mc.GitHubHost != "127.0.0.1:3000" || !mc.GitHubAllowInsecureHost {
		t.Errorf("host override = %q insecure=%v, want 127.0.0.1:3000 true", mc.GitHubHost, mc.GitHubAllowInsecureHost)
	}
	if mc.GitHubRepos == nil {
		t.Error("GitHubRepos = nil, want a repo service for the API base")
	}
	if mc.GitHubStaticCredentials == nil {
		t.Fatal("GitHubStaticCredentials = nil, want a static provider")
	}
	cred, err := mc.GitHubStaticCredentials.TokenForGitHubMount(context.Background(), "s", "repo", storage.GitHubConfig{})
	if err != nil {
		t.Fatalf("TokenForGitHubMount: %v", err)
	}
	tok, err := cred.Token()
	if err != nil || tok != "gitea-pat-abc" {
		t.Fatalf("Token = %q, %v, want gitea-pat-abc", tok, err)
	}
	// The rendered helper must exist, be executable, and reproduce the token via a sibling token file.
	info, err := os.Stat(cred.CredentialHelperPath)
	if err != nil {
		t.Fatalf("stat helper: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("helper is not executable (mode %v)", info.Mode())
	}
	tokBytes, err := os.ReadFile(filepath.Join(cfg.DataRoot, "github-static", "token"))
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if strings.TrimSpace(string(tokBytes)) != "gitea-pat-abc" {
		t.Errorf("token file = %q, want gitea-pat-abc", strings.TrimSpace(string(tokBytes)))
	}
}

// TestConfigureGitHubOverride_StaticTokenFromFile resolves the PAT from GITHUB_STATIC_TOKEN_FILE.
func TestConfigureGitHubOverride_StaticTokenFromFile(t *testing.T) {
	cfg := &Spawnlet{}
	cfg.DataRoot = t.TempDir()
	tokFile := filepath.Join(t.TempDir(), "pat")
	if err := os.WriteFile(tokFile, []byte("file-pat-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.GitHub.StaticTokenFile = tokFile

	var mc spawnlet.ManagerConfig
	if err := configureGitHubOverride(&mc, cfg); err != nil {
		t.Fatalf("configureGitHubOverride: %v", err)
	}
	if mc.GitHubStaticCredentials == nil {
		t.Fatal("GitHubStaticCredentials = nil, want provider from token file")
	}
	cred, _ := mc.GitHubStaticCredentials.TokenForGitHubMount(context.Background(), "s", "repo", storage.GitHubConfig{})
	if tok, _ := cred.Token(); tok != "file-pat-123" {
		t.Errorf("Token = %q, want file-pat-123 (trimmed)", tok)
	}
}

// TestConfigureGitHubOverride_HostOnlyNoStatic covers a host override WITHOUT a static token (e.g. a
// self-hosted GitHub still using the AS mint): the host applies, but no static provider is installed.
func TestConfigureGitHubOverride_HostOnlyNoStatic(t *testing.T) {
	cfg := &Spawnlet{}
	cfg.DataRoot = t.TempDir()
	cfg.GitHub.Host = "ghe.internal"

	var mc spawnlet.ManagerConfig
	if err := configureGitHubOverride(&mc, cfg); err != nil {
		t.Fatalf("configureGitHubOverride: %v", err)
	}
	if mc.GitHubHost != "ghe.internal" {
		t.Errorf("GitHubHost = %q, want ghe.internal", mc.GitHubHost)
	}
	if mc.GitHubStaticCredentials != nil {
		t.Error("GitHubStaticCredentials should be nil without a static token")
	}
}

// --- buildManager / applyUsernsProbe tests --------------------------------

func TestBuildManagerRunscPath(t *testing.T) {
	m, err := buildManager(spawnlet.ManagerConfig{
		ContainerRuntime: "runsc", AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	}, "", "", nil)
	if err != nil {
		t.Fatalf("runsc buildManager: %v", err)
	}
	if m == nil {
		t.Fatal("nil manager")
	}
}

func TestBuildManagerDockerDefault(t *testing.T) {
	m, err := buildManager(spawnlet.ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	}, "", "", nil)
	if err != nil {
		t.Fatalf("docker buildManager: %v", err)
	}
	if m == nil {
		t.Fatal("nil manager")
	}
}

func TestApplyUsernsProbe(t *testing.T) {
	probeErr := errors.New("daemon unreachable")
	cases := []struct {
		name     string
		base     uint32
		active   bool
		probeErr error
		wantMode string
		wantBase uint32
	}{
		// Happy path: probe succeeds, userns active, base parsed.
		{"success", 700000, true, nil, "remap", 700000},
		{"success base zero", 0, true, nil, "remap", 0},
		// Degraded: probe OK but daemon not running with userns-remap.
		{"not active", 0, false, nil, "off", 0},
		// Degraded: daemon info call failed.
		{"probe error", 0, false, probeErr, "off", 0},
		// The subtle ordering: active=true but base unparseable (err!=nil) — error-first
		// check means this still degrades rather than proceeding with a zero base.
		{"active but unparseable base", 0, true, probeErr, "off", 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			mode, base := applyUsernsProbe(tc.base, tc.active, tc.probeErr)
			if mode != tc.wantMode || base != tc.wantBase {
				t.Errorf("applyUsernsProbe(%d, %v, %v) = (%q, %d), want (%q, %d)",
					tc.base, tc.active, tc.probeErr, mode, base, tc.wantMode, tc.wantBase)
			}
		})
	}
}

// --- configureJournal tests -----------------------------------------------
// These build a typed config; configureJournal is driven by cfg.Journal, not the environment.

func TestConfigureJournalS3WithGarageAdminDoesNotRequireStaticBucketCredentials(t *testing.T) {
	cfg := &Spawnlet{}
	cfg.DataRoot = t.TempDir()
	cfg.Journal.Backend = "s3"
	cfg.Journal.S3.Endpoint = "http://127.0.0.1:3900"
	cfg.Journal.S3.GarageAdminEndpoint = "http://127.0.0.1:3903"
	cfg.Journal.S3.GarageAdminToken = "test-token"
	cfg.Journal.S3.DisableTLS = true

	m, err := buildManager(spawnlet.ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: cfg.DataRoot,
	}, "", "", nil)
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	if err := configureJournal(m, cfg); err != nil {
		t.Fatalf("configure generation-keyed s3 journal: %v", err)
	}
	if m == nil {
		t.Fatal("nil manager")
	}
}

func TestNodeGitHubMint_DisabledByDefault(t *testing.T) {
	cfg := &Spawnlet{}
	cfg.Node.AuthMode = "insecure"
	cfg.ASURL = "http://127.0.0.1:8090"
	if got := nodeGitHubMint(cfg, nil); got != nil {
		t.Fatal("insecure mode must not construct a production-capable AS client")
	}
}

func TestConfigureJournalS3FailsClosedWithoutGarageAdmin(t *testing.T) {
	cfg := &Spawnlet{}
	cfg.DataRoot = t.TempDir()
	cfg.Journal.Backend = "s3"
	cfg.Journal.S3.Endpoint = "http://127.0.0.1:3900"
	cfg.Journal.S3.DisableTLS = true

	m, err := buildManager(spawnlet.ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: cfg.DataRoot,
	}, "", "", nil)
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	err = configureJournal(m, cfg)
	if err == nil {
		t.Fatalf("configure journal = nil, want s3 generation-key manager requirement error")
	}
	if !strings.Contains(err.Error(), "garage_admin_endpoint") {
		t.Fatalf("error = %v, want Garage admin requirement", err)
	}
}
