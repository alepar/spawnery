package config

import (
	"fmt"
	"os"
	"strings"
)

// Resolver turns the argument of a ${scheme:arg} config reference into cleartext. Resolvers are
// registered by scheme (the scheme prefix selects the backend: env, file, sops, … vault later).
// Resolution is fail-closed: a non-nil error from Resolve aborts the load.
type Resolver interface {
	Scheme() string
	Resolve(arg string) (string, error)
}

// envResolver resolves ${env:NAME} from the process environment.
type envResolver struct {
	getenv func(string) (string, bool)
}

func newEnvResolver(getenv func(string) (string, bool)) Resolver { return envResolver{getenv: getenv} }

func (envResolver) Scheme() string { return "env" }

func (r envResolver) Resolve(name string) (string, error) {
	v, ok := r.getenv(name)
	if !ok {
		return "", fmt.Errorf("environment variable %q is not set", name)
	}
	return v, nil
}

// fileResolver resolves ${file:/path} from the file's (trimmed) contents.
type fileResolver struct{}

func newFileResolver() Resolver { return fileResolver{} }

func (fileResolver) Scheme() string { return "file" }

func (fileResolver) Resolve(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading file reference: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}
