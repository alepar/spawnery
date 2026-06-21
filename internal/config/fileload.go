package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	kfs "github.com/knadh/koanf/providers/fs"
	"github.com/knadh/koanf/v2"
)

// loadFiles layers the YAML config files for svc+env into k. For each of the four logical layers
// (common base, common env-delta, svc base, svc env-delta) the embedded file is loaded first and
// then, if an external dir is configured, a same-named file there is deep-merged on top — so an
// external override stays within its own layer (external common cannot outrank embedded svc).
//
// The two base files (common.yaml, <svc>.yaml) are required; the *.<env>.yaml deltas are optional
// and skipped when absent. External files are always optional.
func loadFiles(k *koanf.Koanf, svc, env string, embedded fs.FS, externalDir string) error {
	layers := []struct {
		name     string
		required bool
	}{
		{"common.yaml", true},
		{"common." + env + ".yaml", false},
		{svc + ".yaml", true},
		{svc + "." + env + ".yaml", false},
	}
	for _, l := range layers {
		if err := mergeEmbedded(k, embedded, l.name, l.required); err != nil {
			return err
		}
		if externalDir != "" {
			if err := mergeExternal(k, externalDir, l.name); err != nil {
				return err
			}
		}
	}
	return nil
}

// mergeEmbedded loads name from the embedded FS. A missing required file is an error; a missing
// optional file is skipped.
func mergeEmbedded(k *koanf.Koanf, embedded fs.FS, name string, required bool) error {
	if _, err := fs.Stat(embedded, name); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if required {
				return fmt.Errorf("required config file %q missing from embedded config", name)
			}
			return nil // optional delta file simply absent
		}
		return fmt.Errorf("stat embedded %q: %w", name, err)
	}
	if err := k.Load(kfs.Provider(embedded, name), yaml.Parser()); err != nil {
		return fmt.Errorf("loading embedded %q: %w", name, err)
	}
	return nil
}

// mergeExternal deep-merges name from the external dir if it exists there (always optional).
func mergeExternal(k *koanf.Koanf, dir, name string) error {
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // no external override for this layer
		}
		return fmt.Errorf("stat external %q: %w", path, err)
	}
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return fmt.Errorf("loading external %q: %w", path, err)
	}
	return nil
}
