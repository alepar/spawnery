package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	configfiles "spawnery/config"
	"spawnery/internal/config"
	"spawnery/internal/runtime/fakepod"
	"spawnery/internal/spawnlet"
	"spawnery/internal/spawnlet/firewall"
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
	cfg, err := loadSpawnletTest(t, "dev", nil, "node.auth_mode=enforced", "limits.pids=512")
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

// --- nodeGitHubMint tests -------------------------------------------------
// These build a typed config; nodeGitHubMint is driven by cfg.Node, not the environment.

func TestNodeGitHubMint_RelaxedDevClient(t *testing.T) {
	cfg := &Spawnlet{}
	cfg.Node.GitHubMintDevID = "node-1"
	cfg.ASURL = "http://127.0.0.1:8090"
	cfg.Node.AuthMode = "insecure" // relaxed path must NOT require enforced mode
	if got := nodeGitHubMint(cfg); got == nil {
		t.Fatal("relaxed dev mint client must be non-nil when github_mint_dev_id + as_url are set")
	}
}

func TestNodeGitHubMint_RelaxedRequiresASURL(t *testing.T) {
	cfg := &Spawnlet{}
	cfg.Node.GitHubMintDevID = "node-1"
	cfg.ASURL = ""
	if got := nodeGitHubMint(cfg); got != nil {
		t.Fatal("relaxed dev mint must be nil without as_url")
	}
}

func TestNodeGitHubMint_DisabledByDefault(t *testing.T) {
	cfg := &Spawnlet{}
	cfg.Node.GitHubMintDevID = ""
	cfg.Node.AuthMode = "insecure"
	cfg.ASURL = "http://127.0.0.1:8090"
	if got := nodeGitHubMint(cfg); got != nil {
		t.Fatal("without dev id and in insecure mode, mint client must be nil (unchanged behavior)")
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

// --- gracefulDetachAll tests -----------------------------------------------

// gracefulDetachAll is the SIGTERM path (SE3 §4.1): it must leave every pod RUNNING. This test guards
// the exact call site — a regression back to mgr.StopAll here destroys every spawn on a `systemctl
// restart`, the documented upgrade path.
func TestGracefulDetachAllLeavesPodsRunning(t *testing.T) {
	ctx := context.Background()
	be := fakepod.New()
	t.Cleanup(be.Close)
	mgr := spawnlet.NewManagerWithBackend(be, noopApplier{}, spawnlet.ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), NodeID: "n1",
	})
	if _, err := mgr.Create(ctx, "sp-x", "../../examples/secret-app", "model", "", "", 0); err != nil {
		t.Fatalf("Create: %v", err)
	}

	gracefulDetachAll(mgr)

	for _, role := range []string{"sandbox", "sidecar", "agent"} {
		if got := be.State("sp-x", role); got != fakepod.StateRunning {
			t.Fatalf("after gracefulDetachAll, sp-x/%s = %v, want %v", role, got, fakepod.StateRunning)
		}
	}
	if h := be.LastStopHandle(); h != nil {
		t.Fatalf("gracefulDetachAll must not stop pods, got pod.Stop(%+v)", h)
	}
}

// noopApplier is a firewall.Applier stand-in — egress is not enforced in this test (NodeClass unset,
// EgressEnforce false) but the Manager needs a non-nil applier.
type noopApplier struct{}

func (noopApplier) Apply(context.Context, []firewall.Rule) error  { return nil }
func (noopApplier) Remove(context.Context, []firewall.Rule) error { return nil }
