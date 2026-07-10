package storage

import (
	"errors"
	"fmt"
	"strings"
)

var ErrUnsupportedBackend = errors.New("storage: unsupported backend")

type UnsupportedBackendError struct {
	BackendURI string
	Scheme     string
}

func (e *UnsupportedBackendError) Error() string {
	if e == nil {
		return ErrUnsupportedBackend.Error()
	}
	if e.Scheme == "" {
		return ErrUnsupportedBackend.Error()
	}
	if e.BackendURI == "" {
		return fmt.Sprintf("%s: %s", ErrUnsupportedBackend, e.Scheme)
	}
	return fmt.Sprintf("%s: %s (%s)", ErrUnsupportedBackend, e.Scheme, e.BackendURI)
}

func (e *UnsupportedBackendError) Unwrap() error { return ErrUnsupportedBackend }

type BackendResolver interface {
	Resolve(backendURI string) (Backend, error)
}

type BackendBinding struct {
	Name               string
	BackendURI         string
	CredentialSecretID string
	CreateIfMissing    bool
	RepositoryID       string
}

type BindingResolver interface {
	ResolveBinding(binding BackendBinding) (Backend, error)
}

type SchemeResolver struct {
	scratchRoot       string
	githubCredentials GitHubCredentialProvider
	githubRepos       GitHubRepoService
	gitRunner         GitRunner

	// githubHost overrides the resolved github mount Host when non-empty (default: github.com from
	// ParseGitHubURI). githubAllowInsecureHost relaxes the host/https restriction so a local git
	// server (e.g. Gitea over HTTP) can back a github: mount. Both flip ONLY from explicit node
	// config (SetGitHubHostOverride); production leaves them zero (github.com, secure).
	githubHost              string
	githubAllowInsecureHost bool
}

func NewSchemeResolver(scratchRoot string) *SchemeResolver {
	return &SchemeResolver{scratchRoot: scratchRoot}
}

func NewSchemeResolverWithGitHub(scratchRoot string, creds GitHubCredentialProvider) *SchemeResolver {
	return &SchemeResolver{scratchRoot: scratchRoot, githubCredentials: creds}
}

func (r *SchemeResolver) SetGitHubCredentials(creds GitHubCredentialProvider) {
	r.githubCredentials = creds
}

func (r *SchemeResolver) SetGitHubServices(repos GitHubRepoService, runner GitRunner) {
	r.githubRepos = repos
	r.gitRunner = runner
}

// SetGitHubHostOverride points github: mounts at a non-github.com git host. host replaces the
// default Host on every resolved GitHubConfig; allowInsecure permits the HTTP clone URLs a local
// test git server (Gitea) serves. Leaving host empty preserves the production default (github.com,
// secure) — this is only wired from explicit node config (GITHUB_HOST / GITHUB_ALLOW_INSECURE_HOST).
func (r *SchemeResolver) SetGitHubHostOverride(host string, allowInsecure bool) {
	r.githubHost = strings.TrimSpace(host)
	r.githubAllowInsecureHost = allowInsecure
}

func (r *SchemeResolver) Resolve(backendURI string) (Backend, error) {
	return r.ResolveBinding(BackendBinding{BackendURI: backendURI})
}

func (r *SchemeResolver) ResolveBinding(binding BackendBinding) (Backend, error) {
	backendURI := binding.BackendURI
	scheme, _, hasScheme := strings.Cut(backendURI, ":")
	if !hasScheme || scheme == "" || scheme == "scratch" {
		return NewScratch(r.scratchRoot), nil
	}
	if scheme == "github" {
		cfg, err := ParseGitHubURI(backendURI)
		if err != nil {
			return nil, err
		}
		cfg.MountName = binding.Name
		cfg.CredentialSecretID = binding.CredentialSecretID
		cfg.CreateIfMissing = binding.CreateIfMissing
		cfg.RepositoryID = binding.RepositoryID
		// Node-config host override (default: github.com, secure — set by ParseGitHubURI). Only an
		// explicit GITHUB_HOST/GITHUB_ALLOW_INSECURE_HOST flips this to a local git server (Gitea).
		if r.githubHost != "" {
			cfg.Host = r.githubHost
		}
		cfg.AllowInsecureHost = r.githubAllowInsecureHost
		gh := NewGitHub(r.scratchRoot, cfg)
		gh.Credentials = r.githubCredentials
		gh.Repos = r.githubRepos
		gh.Git = r.gitRunner
		return gh, nil
	}
	return nil, &UnsupportedBackendError{BackendURI: backendURI, Scheme: scheme}
}

func IsGitHubBackendURI(backendURI string) bool {
	scheme, _, hasScheme := strings.Cut(backendURI, ":")
	return hasScheme && scheme == "github"
}
