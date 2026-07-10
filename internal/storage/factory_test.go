package storage

import (
	"context"
	"errors"
	"testing"
)

func TestSchemeResolverResolvesScratchBackend(t *testing.T) {
	t.Parallel()

	resolver := NewSchemeResolver(t.TempDir())
	for _, backendURI := range []string{"", "scratch:", "scratch:/"} {
		backend, err := resolver.Resolve(backendURI)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", backendURI, err)
		}
		if _, ok := backend.(*Scratch); !ok {
			t.Fatalf("Resolve(%q) returned %T, want *Scratch", backendURI, backend)
		}
	}
}

func TestSchemeResolverRejectsUnsupportedBackends(t *testing.T) {
	t.Parallel()

	resolver := NewSchemeResolver(t.TempDir())
	for _, tc := range []struct {
		name       string
		backendURI string
		scheme     string
	}{
		{name: "unknown", backendURI: "mystery:thing", scheme: "mystery"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := resolver.Resolve(tc.backendURI)
			if !errors.Is(err, ErrUnsupportedBackend) {
				t.Fatalf("Resolve(%q) error = %v, want ErrUnsupportedBackend", tc.backendURI, err)
			}

			var unsupported *UnsupportedBackendError
			if !errors.As(err, &unsupported) {
				t.Fatalf("Resolve(%q) error type = %T, want *UnsupportedBackendError", tc.backendURI, err)
			}
			if unsupported.Scheme != tc.scheme {
				t.Fatalf("Resolve(%q) scheme = %q, want %q", tc.backendURI, unsupported.Scheme, tc.scheme)
			}
		})
	}
}

func TestSchemeResolverResolvesGitHubBackend(t *testing.T) {
	t.Parallel()

	resolver := NewSchemeResolver(t.TempDir())
	backend, err := resolver.Resolve("github:octo-org/demo")
	if err != nil {
		t.Fatalf("Resolve(github): %v", err)
	}
	if _, ok := backend.(*GitHub); !ok {
		t.Fatalf("Resolve(github) returned %T, want *GitHub", backend)
	}
}

func TestSchemeResolverResolveBindingFieldPassthrough(t *testing.T) {
	t.Parallel()

	resolver := NewSchemeResolverWithGitHub(t.TempDir(), nil)
	backend, err := resolver.ResolveBinding(BackendBinding{
		Name:               "main",
		BackendURI:         "github:octo-org/demo",
		CredentialSecretID: "sec-123",
		CreateIfMissing:    true,
		RepositoryID:       "42",
	})
	if err != nil {
		t.Fatalf("ResolveBinding: %v", err)
	}
	gh, ok := backend.(*GitHub)
	if !ok {
		t.Fatalf("ResolveBinding returned %T, want *GitHub", backend)
	}
	if gh.Config.MountName != "main" {
		t.Errorf("Config.MountName = %q, want %q", gh.Config.MountName, "main")
	}
	if gh.Config.CredentialSecretID != "sec-123" {
		t.Errorf("Config.CredentialSecretID = %q, want %q", gh.Config.CredentialSecretID, "sec-123")
	}
	if !gh.Config.CreateIfMissing {
		t.Errorf("Config.CreateIfMissing = false, want true")
	}
	if gh.Config.RepositoryID != "42" {
		t.Errorf("Config.RepositoryID = %q, want %q", gh.Config.RepositoryID, "42")
	}
	if gh.Config.Owner != "octo-org" {
		t.Errorf("Config.Owner = %q, want %q", gh.Config.Owner, "octo-org")
	}
	if gh.Config.Repo != "demo" {
		t.Errorf("Config.Repo = %q, want %q", gh.Config.Repo, "demo")
	}
	if gh.Config.Host != "github.com" {
		t.Errorf("Config.Host = %q, want %q", gh.Config.Host, "github.com")
	}
}

// TestSchemeResolverGitHubProductionDefault pins the production default: with no host override, a
// resolved github mount targets github.com with AllowInsecureHost=false (secure, https-only). This
// is the invariant the new node config must never weaken unless explicitly configured.
func TestSchemeResolverGitHubProductionDefault(t *testing.T) {
	t.Parallel()

	resolver := NewSchemeResolver(t.TempDir())
	backend, err := resolver.Resolve("github:octo-org/demo")
	if err != nil {
		t.Fatalf("Resolve(github): %v", err)
	}
	gh := backend.(*GitHub)
	if gh.Config.Host != "github.com" {
		t.Errorf("Config.Host = %q, want github.com", gh.Config.Host)
	}
	if gh.Config.AllowInsecureHost {
		t.Error("Config.AllowInsecureHost = true, want false (production default must be secure)")
	}
}

// TestSchemeResolverGitHubHostOverride verifies SetGitHubHostOverride flips the resolved mount to a
// local git host with insecure (http) clone URLs allowed — the Gitea lane. Only an explicit override
// does this; the owner/repo still come from the backend URI.
func TestSchemeResolverGitHubHostOverride(t *testing.T) {
	t.Parallel()

	resolver := NewSchemeResolver(t.TempDir())
	resolver.SetGitHubHostOverride("127.0.0.1:3000", true)
	backend, err := resolver.Resolve("github:octo-org/demo")
	if err != nil {
		t.Fatalf("Resolve(github): %v", err)
	}
	gh := backend.(*GitHub)
	if gh.Config.Host != "127.0.0.1:3000" {
		t.Errorf("Config.Host = %q, want 127.0.0.1:3000", gh.Config.Host)
	}
	if !gh.Config.AllowInsecureHost {
		t.Error("Config.AllowInsecureHost = false, want true after override")
	}
	// owner/repo are unaffected by the host override.
	if gh.Config.Owner != "octo-org" || gh.Config.Repo != "demo" {
		t.Errorf("owner/repo = %q/%q, want octo-org/demo", gh.Config.Owner, gh.Config.Repo)
	}
}

// TestStaticGitHubCredentials verifies the fixed-token provider echoes its token + helper path for
// any mount (the AS-mint bypass used by the Gitea lane).
func TestStaticGitHubCredentials(t *testing.T) {
	t.Parallel()

	p := StaticGitHubCredentials{AccessToken: "gitea-pat-xyz", CredentialHelperPath: "/n/git-credential-static"}
	cred, err := p.TokenForGitHubMount(context.Background(), "spawn-1", "repo", GitHubConfig{})
	if err != nil {
		t.Fatalf("TokenForGitHubMount: %v", err)
	}
	tok, err := cred.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "gitea-pat-xyz" {
		t.Errorf("Token = %q, want gitea-pat-xyz", tok)
	}
	if cred.CredentialHelperPath != "/n/git-credential-static" {
		t.Errorf("CredentialHelperPath = %q, want /n/git-credential-static", cred.CredentialHelperPath)
	}
}
