package main

import (
	"strings"
	"testing"

	configfiles "spawnery/config"
	"spawnery/internal/authsvc/githubfake"
	"spawnery/internal/config"
)

func loadASTestSets(t *testing.T, env string, sets ...string) (*AS, error) {
	t.Helper()
	return config.Load[AS]("authsvc", config.Options{
		Args:       []string{"--env=" + env},
		Getenv:     func(string) (string, bool) { return "", false },
		Embedded:   configfiles.FS,
		SecretsFS:  configfiles.FS,
		EnvAliases: asEnvAliases,
		Sets:       sets,
	})
}

func TestASConfig_ProductionRequiresInternalMTLS(t *testing.T) {
	required := []struct {
		set  string
		want string
	}{
		{"internal.listen=127.0.0.1:8091", "internal.listen"},
		{"internal.trust_domain=prod.spawnery.internal", "internal.trust_domain"},
		{"internal.root_ca=/root.pem", "internal.root_ca"},
		{"internal.cert=/authsvc.pem", "internal.cert"},
		{"internal.chain=/service-issuer.pem", "internal.chain"},
		{"internal.key=/authsvc-key.pem", "internal.key"},
		{"internal.server_name=authsvc.internal", "internal.server_name"},
		{"internal.revocation_state=/state/certificates.json", "internal.revocation_state"},
		{"internal.revocation_issuers=/service-issuer.pem,/cloud-issuer.pem,/self-hosted-issuer.pem", "internal.revocation_issuers"},
		{"internal.revocation_crls=/service.crl,/cloud.crl,/self-hosted.crl", "internal.revocation_crls"},
		{"cp.url=https://cp.internal:8081", "cp.url"},
		{"cp.server_name=cp.internal", "cp.server_name"},
	}
	sets := []string{
		"ca.trust_domain=prod.spawnery.internal",
		"signing.environment=prod",
		"signing.root_pem=/root.pem",
		"signing.current_key_pem=/signer-key.pem",
		"signing.current_chain_pem=/signer-chain.pem",
		"github.token_enc_key_file=/github-key",
		"fake_github=true",
	}
	for _, step := range required {
		_, err := loadASTestSets(t, "dev", sets...)
		if err == nil || !strings.Contains(err.Error(), step.want) {
			t.Fatalf("before %s: got %v", step.want, err)
		}
		sets = append(sets, step.set)
	}
	if _, err := loadASTestSets(t, "dev", sets...); err != nil {
		t.Fatalf("complete internal mTLS config: %v", err)
	}
}

func TestASConfig_LegacyServiceSecretsAreNotRepresented(t *testing.T) {
	for _, name := range []string{"AS_CP_RPC_SECRET", "AS_CP_SECRET"} {
		if _, ok := asEnvAliases[name]; ok {
			t.Fatalf("legacy alias %s is still represented", name)
		}
	}
	var cfg AS
	if strings.Contains(strings.ToLower(strings.Join([]string{cfg.CP.URL, cfg.CP.ServerName}, " ")), "secret") {
		t.Fatal("CP config still represents a shared service secret")
	}
}

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
