package main

import (
	"testing"

	configfiles "spawnery/config"
	"spawnery/internal/authsvc/githubfake"
	"spawnery/internal/config"
)

// loadASTest is a test helper that calls config.Load[AS] with an injected getenv map, bypassing
// SPAWNERY_ENV and the real process environment (mirrors cmd/spawnlet's loadSpawnletTest).
func loadASTest(t *testing.T, env string, getenv map[string]string) (*AS, error) {
	t.Helper()
	return config.Load[AS]("authsvc", config.Options{
		Args:       []string{"--env=" + env},
		Getenv:     func(k string) (string, bool) { v, ok := getenv[k]; return v, ok },
		Embedded:   configfiles.FS,
		SecretsFS:  configfiles.FS,
		EnvAliases: asEnvAliases,
	})
}

func TestASConfig_FakeGitHubEnvAliases(t *testing.T) {
	cfg, err := loadASTest(t, "dev", map[string]string{
		"AS_DEV":                  "1",
		"AS_FAKE_GITHUB_ADDR":     "0.0.0.0:9099",
		"AS_FAKE_GITHUB_BASE_URL": "http://fake.test.example:9099",
		"AS_FAKE_GITHUB_USERS":    "alice:2000001,bob",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.FakeGitHubAddr != "0.0.0.0:9099" {
		t.Errorf("FakeGitHubAddr = %q, want 0.0.0.0:9099", cfg.FakeGitHubAddr)
	}
	if cfg.FakeGitHubBaseURL != "http://fake.test.example:9099" {
		t.Errorf("FakeGitHubBaseURL = %q, want http://fake.test.example:9099", cfg.FakeGitHubBaseURL)
	}
	if cfg.FakeGitHubUsers != "alice:2000001,bob" {
		t.Errorf("FakeGitHubUsers = %q, want alice:2000001,bob", cfg.FakeGitHubUsers)
	}
}

// TestASConfig_FakeGitHubDefaultsEmpty pins the back-compat contract: omitting the three new env
// vars leaves the fake in loopback/default-user mode (AS_FAKE_GITHUB=1 with nothing else set is
// today's behavior exactly).
func TestASConfig_FakeGitHubDefaultsEmpty(t *testing.T) {
	cfg, err := loadASTest(t, "dev", map[string]string{"AS_DEV": "1", "AS_FAKE_GITHUB": "1"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.FakeGitHubAddr != "" || cfg.FakeGitHubBaseURL != "" || cfg.FakeGitHubUsers != "" {
		t.Errorf("expected empty defaults, got addr=%q base_url=%q users=%q",
			cfg.FakeGitHubAddr, cfg.FakeGitHubBaseURL, cfg.FakeGitHubUsers)
	}
}

func TestParseFakeGitHubUsers(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		users, err := parseFakeGitHubUsers("")
		if err != nil || users != nil {
			t.Fatalf("users=%v err=%v, want nil,nil", users, err)
		}
	})

	t.Run("explicit and derived ids", func(t *testing.T) {
		users, err := parseFakeGitHubUsers("alice:2000001, bob ")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		want := []githubfake.User{
			{ID: 2000001, Login: "alice"},
			{ID: githubfake.DeriveUserID("bob"), Login: "bob"},
		}
		if len(users) != len(want) || users[0] != want[0] || users[1] != want[1] {
			t.Fatalf("users = %+v, want %+v", users, want)
		}
	})

	t.Run("bad id", func(t *testing.T) {
		if _, err := parseFakeGitHubUsers("alice:not-a-number"); err == nil {
			t.Fatal("expected error for non-numeric id")
		}
	})

	t.Run("empty login", func(t *testing.T) {
		if _, err := parseFakeGitHubUsers(":123"); err == nil {
			t.Fatal("expected error for empty login")
		}
	})
}
