package main

import (
	"strings"
	"testing"
	"time"

	configfiles "spawnery/config"
	"spawnery/internal/config"
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
