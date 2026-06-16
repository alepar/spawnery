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
