package config

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

const testAgeKeyFile = "testdata/age-test.key"

func testSopsCiphertext(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/secrets.test.sops.yaml")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return b
}

func TestSopsResolver(t *testing.T) {
	t.Setenv("SOPS_AGE_KEY_FILE", testAgeKeyFile)
	r := newSopsResolver(testSopsCiphertext(t))

	if r.Scheme() != "sops" {
		t.Errorf("scheme = %q, want sops", r.Scheme())
	}
	// Nested dotted key.
	if v, err := r.Resolve("store.dsn"); err != nil || v != "supersecret" {
		t.Errorf("Resolve(store.dsn) = (%q,%v), want (supersecret,nil)", v, err)
	}
	// Second key proves the decrypted map is cached and reused.
	if v, err := r.Resolve("token"); err != nil || v != "abc123" {
		t.Errorf("Resolve(token) = (%q,%v), want (abc123,nil)", v, err)
	}
	// Missing key is fail-closed.
	if _, err := r.Resolve("nope"); err == nil {
		t.Error("expected error for absent sops key")
	}
}

func TestSopsResolver_BadKeyIsFatal(t *testing.T) {
	t.Setenv("SOPS_AGE_KEY_FILE", filepath.Join(t.TempDir(), "nonexistent.key"))
	r := newSopsResolver(testSopsCiphertext(t))
	if _, err := r.Resolve("store.dsn"); err == nil {
		t.Error("expected decrypt failure with a missing age key, got nil")
	}
}

// End-to-end through Load: a ${sops:} reference in a config file resolves into a redacted Secret.
type sopsCP struct {
	Store struct {
		DSN Secret `koanf:"dsn"`
	} `koanf:"store"`
}

func TestLoad_WithSopsResolver(t *testing.T) {
	t.Setenv("SOPS_AGE_KEY_FILE", testAgeKeyFile)
	fsys := fstest.MapFS{
		"common.yaml": {Data: []byte("{}\n")},
		"scp.yaml":    {Data: []byte("store:\n  dsn: ${sops:store.dsn}\n")},
	}
	cfg, err := Load[sopsCP]("scp", Options{
		Args:      []string{"--env=dev"},
		Getenv:    envFrom(nil),
		Embedded:  fsys,
		Resolvers: []Resolver{newSopsResolver(testSopsCiphertext(t))},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(cfg.Store.DSN) != "supersecret" {
		t.Errorf("Store.DSN = %q, want supersecret", string(cfg.Store.DSN))
	}
}
