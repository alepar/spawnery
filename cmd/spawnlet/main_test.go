package main

import (
	"errors"
	"strings"
	"testing"

	"spawnery/internal/spawnlet"
)

func TestBuildManagerRunscPath(t *testing.T) {
	m, err := buildManager(spawnlet.ManagerConfig{
		ContainerRuntime: "runsc", AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
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
	})
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

func TestConfigureJournalS3WithGarageAdminDoesNotRequireStaticBucketCredentials(t *testing.T) {
	t.Setenv("JOURNAL_BACKEND", "s3")
	t.Setenv("JOURNAL_S3_ENDPOINT", "http://127.0.0.1:3900")
	t.Setenv("JOURNAL_GARAGE_ADMIN_ENDPOINT", "http://127.0.0.1:3903")
	t.Setenv("JOURNAL_GARAGE_ADMIN_TOKEN", "test-token")
	t.Setenv("JOURNAL_S3_DISABLE_TLS", "true")

	m, err := buildManager(spawnlet.ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	if err := configureJournal(m, t.TempDir()); err != nil {
		t.Fatalf("configure generation-keyed s3 journal: %v", err)
	}
	if m == nil {
		t.Fatal("nil manager")
	}
}

func TestNodeGitHubMint_RelaxedDevClient(t *testing.T) {
	t.Setenv("NODE_GITHUB_MINT_DEV_NODE_ID", "node-1")
	t.Setenv("AS_URL", "http://127.0.0.1:8090")
	t.Setenv("NODE_AUTH_MODE", "insecure") // relaxed path must NOT require enforced mode
	if got := nodeGitHubMint(); got == nil {
		t.Fatal("relaxed dev mint client must be non-nil when NODE_GITHUB_MINT_DEV_NODE_ID + AS_URL are set")
	}
}

func TestNodeGitHubMint_RelaxedRequiresASURL(t *testing.T) {
	t.Setenv("NODE_GITHUB_MINT_DEV_NODE_ID", "node-1")
	t.Setenv("AS_URL", "")
	if got := nodeGitHubMint(); got != nil {
		t.Fatal("relaxed dev mint must be nil without AS_URL")
	}
}

func TestNodeGitHubMint_DisabledByDefault(t *testing.T) {
	t.Setenv("NODE_GITHUB_MINT_DEV_NODE_ID", "")
	t.Setenv("NODE_AUTH_MODE", "insecure")
	t.Setenv("AS_URL", "http://127.0.0.1:8090")
	if got := nodeGitHubMint(); got != nil {
		t.Fatal("without dev env and in insecure mode, mint client must be nil (unchanged behavior)")
	}
}

func TestConfigureJournalS3FailsClosedWithoutGarageAdmin(t *testing.T) {
	t.Setenv("JOURNAL_BACKEND", "s3")
	t.Setenv("JOURNAL_S3_ENDPOINT", "http://127.0.0.1:3900")
	t.Setenv("JOURNAL_S3_DISABLE_TLS", "true")

	m, err := buildManager(spawnlet.ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	err = configureJournal(m, t.TempDir())
	if err == nil {
		t.Fatalf("configure journal = nil, want s3 generation-key manager requirement error")
	}
	if !strings.Contains(err.Error(), "JOURNAL_GARAGE_ADMIN_ENDPOINT") {
		t.Fatalf("error = %v, want Garage admin requirement", err)
	}
}
