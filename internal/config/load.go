// Package config is the layered, schema-defined configuration loader shared across the spawnery
// binaries. A binary defines its config as a Go struct (the schema), supplies its embedded config
// files and an env-name→key table, and calls Load. See
// docs/superpowers/specs/2026-06-20-config-framework-design.md.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

// Options carries everything Load needs to build a config for one binary. Only Embedded is
// strictly required; the rest default to empty/no-op.
type Options struct {
	// Args is the binary's CLI args (os.Args[1:]); scanned for --env during the bootstrap phase.
	Args []string
	// Getenv looks up environment variables; defaults to os.LookupEnv. Injected in tests.
	Getenv func(string) (string, bool)
	// Embedded is the //go:embed'd config file tree (common.yaml, <svc>.yaml, *.<env>.yaml).
	Embedded fs.FS
	// SecretsFS, if set, is searched for secrets.<env>.sops.yaml; when present that file is wired
	// as the ${sops:} resolver for the active env (typically the same embed.FS as Embedded).
	SecretsFS fs.FS
	// ExternalDir is an optional on-disk override dir; defaults to $SPAWNERY_CONFIG_DIR.
	ExternalDir string
	// Defaults is an optional pointer to a struct of in-code defaults (layer 0).
	Defaults any
	// EnvAliases maps full env-var names to dotted config keys (layer 5).
	EnvAliases map[string]string
	// FlagProvider is an optional layer-6 provider (cliflagv3), supplied post-parse by the binary.
	FlagProvider koanf.Provider
	// Sets are the raw --set key=value strings (layer 7).
	Sets []string
	// Resolvers are extra ${scheme:arg} backends (e.g. sops) beyond the built-in env/file ones.
	Resolvers []Resolver
	// DefaultEnv, if set, is the environment used when neither --env nor SPAWNERY_ENV is set.
	// Servers leave it empty (fail-closed); a client CLI sets it (e.g. "dev") to stay usable.
	DefaultEnv string
}

// Load builds the typed config T for the named service by layering, in precedence order:
// in-code defaults < embedded files (+ external dir) < env vars < flags < --set, then decoding
// with weakly-typed coercion. The environment is resolved fail-closed first (see resolveEnv).
func Load[T any](svc string, opts Options) (*T, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.LookupEnv
	}
	externalDir := opts.ExternalDir
	if externalDir == "" {
		if v, ok := getenv("SPAWNERY_CONFIG_DIR"); ok {
			externalDir = v
		}
	}

	env, err := resolveEnv(opts.Args, getenv, opts.DefaultEnv)
	if err != nil {
		return nil, err
	}

	k := koanf.New(".")

	// Layer 0: in-code defaults, so every key is present before higher layers load.
	if opts.Defaults != nil {
		if err := k.Load(structs.Provider(opts.Defaults, "koanf"), nil); err != nil {
			return nil, fmt.Errorf("loading defaults: %w", err)
		}
	}

	// Layers 1–4: embedded files, then external-dir overlay.
	if opts.Embedded != nil {
		if err := loadFiles(k, svc, env, opts.Embedded, externalDir); err != nil {
			return nil, err
		}
	}

	// Layer 5: explicitly-aliased env vars that are set.
	if len(opts.EnvAliases) > 0 {
		if err := k.Load(confmap.Provider(buildEnvLayer(opts.EnvAliases, getenv), "."), nil); err != nil {
			return nil, fmt.Errorf("loading env layer: %w", err)
		}
	}

	// Layer 6: curated flags (cliflagv3), if the binary supplied them.
	if opts.FlagProvider != nil {
		if err := k.Load(opts.FlagProvider, nil); err != nil {
			return nil, fmt.Errorf("loading flag layer: %w", err)
		}
	}

	// Layer 7: --set overrides win over everything.
	if len(opts.Sets) > 0 {
		sets, err := parseSets(opts.Sets)
		if err != nil {
			return nil, err
		}
		if err := k.Load(confmap.Provider(sets, "."), nil); err != nil {
			return nil, fmt.Errorf("loading --set layer: %w", err)
		}
	}

	// Resolve ${scheme:arg} references on the merged map, before decode, so a resolved value is
	// coerced to the field type like any other.
	resolvers := map[string]Resolver{
		"env":  newEnvResolver(getenv),
		"file": newFileResolver(),
	}
	if opts.SecretsFS != nil {
		name := "secrets." + env + ".sops.yaml"
		b, err := fs.ReadFile(opts.SecretsFS, name)
		switch {
		case err == nil:
			resolvers["sops"] = newSopsResolver(b)
		case !errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
	}
	for _, r := range opts.Resolvers {
		resolvers[r.Scheme()] = r
	}
	if err := resolveRefs(k, resolvers); err != nil {
		return nil, err
	}

	var out T
	if err := decodeInto(k, &out); err != nil {
		return nil, fmt.Errorf("decoding config for %q (env %s): %w", svc, env, err)
	}
	if err := validateConfig(&out); err != nil {
		return nil, fmt.Errorf("config for %q (env %s): %w", svc, env, err)
	}
	return &out, nil
}
