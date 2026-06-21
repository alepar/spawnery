package config

import (
	"os"
	"testing"
	"testing/fstest"
)

// SecretsFS auto-registers a ${sops:} resolver from secrets.<env>.sops.yaml for the active env.
func TestLoad_SecretsFSAutoRegistersSops(t *testing.T) {
	t.Setenv("SOPS_AGE_KEY_FILE", testAgeKeyFile)
	ct, err := os.ReadFile("testdata/secrets.test.sops.yaml")
	if err != nil {
		t.Fatal(err)
	}
	secretsFS := fstest.MapFS{"secrets.dev.sops.yaml": {Data: ct}}
	embed := fstest.MapFS{
		"common.yaml": {Data: []byte("{}\n")},
		"x.yaml":      {Data: []byte("store:\n  dsn: ${sops:store.dsn}\n")},
	}
	cfg, err := Load[sopsCP]("x", Options{
		Args:      []string{"--env=dev"},
		Getenv:    envFrom(nil),
		Embedded:  embed,
		SecretsFS: secretsFS,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(cfg.Store.DSN) != "supersecret" {
		t.Errorf("Store.DSN = %q, want supersecret (sops auto-registered for env dev)", string(cfg.Store.DSN))
	}
}

// Absent secrets.<env>.sops.yaml is fine (no sops resolver registered, no error).
func TestLoad_SecretsFSMissingFileIsFine(t *testing.T) {
	cfg, err := Load[loadCP]("cp", Options{
		Args:      []string{"--env=dev"},
		Getenv:    envFrom(nil),
		Embedded:  loadFS(),
		SecretsFS: fstest.MapFS{}, // no secrets.dev.sops.yaml
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
}
