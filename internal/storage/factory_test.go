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
