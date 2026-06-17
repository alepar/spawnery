package storage

import (
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
