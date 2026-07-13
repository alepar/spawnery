package main

import (
	"strings"
	"testing"
	"time"

	configfiles "spawnery/config"
	"spawnery/internal/config"
	"spawnery/internal/cp/skillfetch"
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
	if cfg.AdminOwners != "" {
		t.Errorf("AdminOwners = %q, want empty (nobody is admin by default)", cfg.AdminOwners)
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
	// sp-mwco.4.6: skills.* cap defaults must equal skillfetch's Default* consts, so a CP running
	// on file defaults enforces (and stamps on the wire) exactly what skillfetch.New(Config{})
	// would fall back to anyway.
	if cfg.Skills.WireCapBytes != skillfetch.DefaultWireCapBytes {
		t.Errorf("Skills.WireCapBytes = %d, want %d", cfg.Skills.WireCapBytes, skillfetch.DefaultWireCapBytes)
	}
	if cfg.Skills.DecompressedCapBytes != skillfetch.DefaultDecompressedCapBytes {
		t.Errorf("Skills.DecompressedCapBytes = %d, want %d", cfg.Skills.DecompressedCapBytes, skillfetch.DefaultDecompressedCapBytes)
	}
	if cfg.Skills.PlainTarCapBytes != skillfetch.DefaultPlainTarCapBytes {
		t.Errorf("Skills.PlainTarCapBytes = %d, want %d", cfg.Skills.PlainTarCapBytes, skillfetch.DefaultPlainTarCapBytes)
	}
	if cfg.Skills.FileCountCap != skillfetch.DefaultFileCountCap {
		t.Errorf("Skills.FileCountCap = %d, want %d", cfg.Skills.FileCountCap, skillfetch.DefaultFileCountCap)
	}
	if cfg.Skills.HTTPTimeout != skillfetch.DefaultHTTPTimeout {
		t.Errorf("Skills.HTTPTimeout = %s, want %s", cfg.Skills.HTTPTimeout, skillfetch.DefaultHTTPTimeout)
	}
}

// TestSkillCapEnvAliases pins the SKILLS_* env aliases (sp-mwco.4.6): each maps to its skills.*
// dotted key and overrides the file default.
func TestSkillCapEnvAliases(t *testing.T) {
	cfg, err := loadCPTest(t, "dev", map[string]string{
		"SKILLS_WIRE_CAP_BYTES":         "1048576",
		"SKILLS_DECOMPRESSED_CAP_BYTES": "2097152",
		"SKILLS_PLAIN_TAR_CAP_BYTES":    "3145728",
		"SKILLS_FILE_COUNT_CAP":         "500",
		"SKILLS_HTTP_TIMEOUT":           "10s",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Skills.WireCapBytes != 1048576 {
		t.Errorf("Skills.WireCapBytes = %d, want 1048576", cfg.Skills.WireCapBytes)
	}
	if cfg.Skills.DecompressedCapBytes != 2097152 {
		t.Errorf("Skills.DecompressedCapBytes = %d, want 2097152", cfg.Skills.DecompressedCapBytes)
	}
	if cfg.Skills.PlainTarCapBytes != 3145728 {
		t.Errorf("Skills.PlainTarCapBytes = %d, want 3145728", cfg.Skills.PlainTarCapBytes)
	}
	if cfg.Skills.FileCountCap != 500 {
		t.Errorf("Skills.FileCountCap = %d, want 500", cfg.Skills.FileCountCap)
	}
	if cfg.Skills.HTTPTimeout != 10*time.Second {
		t.Errorf("Skills.HTTPTimeout = %s, want 10s", cfg.Skills.HTTPTimeout)
	}
}

func TestCPConfig_EnvAliasOverride(t *testing.T) {
	cfg, err := loadCPTest(t, "dev", map[string]string{
		"CP_LISTEN":               "0.0.0.0:9000",
		"CP_MAX_SPAWNS_PER_OWNER": "9",
		"EVALUATOR_IDLE_DETACHED": "5m",
		"CP_ADMIN_OWNERS":         "alice,bob",
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
	if cfg.AdminOwners != "alice,bob" {
		t.Errorf("AdminOwners = %q, want %q", cfg.AdminOwners, "alice,bob")
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

func TestCPConfig_ProdModeRequiresPubkeys(t *testing.T) {
	_, err := loadCPTest(t, "dev", nil, "auth.mode=prod") // prod mode, no as_session_pubkeys
	if err == nil || !strings.Contains(err.Error(), "as_session_pubkeys") {
		t.Fatalf("expected as_session_pubkeys validation error, got %v", err)
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
