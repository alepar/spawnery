package spawnlet

import (
	"fmt"

	"spawnery/internal/manifest"
	"spawnery/internal/storage"
)

type MountBinding struct {
	Name               string
	BackendURI         string
	CredentialSecretID string
	CreateIfMissing    bool
	RepositoryID       string
}

func resolveMountBackend(resolver storage.BackendResolver, binding MountBinding) (storage.Backend, error) {
	if typed, ok := resolver.(storage.BindingResolver); ok {
		return typed.ResolveBinding(storage.BackendBinding{
			Name:               binding.Name,
			BackendURI:         binding.BackendURI,
			CredentialSecretID: binding.CredentialSecretID,
			CreateIfMissing:    binding.CreateIfMissing,
			RepositoryID:       binding.RepositoryID,
		})
	}
	return resolver.Resolve(binding.BackendURI)
}

// applyRestoreHint tells a restore-aware backend (github) whether a journal restore will repopulate
// this mount on resume, so it can skip a fresh network clone (spec §16.7). No-op for plain backends.
func applyRestoreHint(backend storage.Backend, restorePending bool) {
	if ra, ok := backend.(storage.RestoreAware); ok {
		ra.SetRestorePending(restorePending)
	}
}

func mountBindingsByName(manifestMounts []manifest.Mount, bindings []MountBinding) (map[string]MountBinding, error) {
	manifestNames := make(map[string]struct{}, len(manifestMounts))
	for _, mount := range manifestMounts {
		manifestNames[mount.Name] = struct{}{}
	}

	out := make(map[string]MountBinding, len(bindings))
	for _, binding := range bindings {
		if binding.Name == "" {
			return nil, fmt.Errorf("mount binding name must not be empty")
		}
		if _, ok := manifestNames[binding.Name]; !ok {
			return nil, fmt.Errorf("mount binding %q does not match any manifest mount", binding.Name)
		}
		if _, dup := out[binding.Name]; dup {
			return nil, fmt.Errorf("duplicate mount binding %q", binding.Name)
		}
		out[binding.Name] = binding
	}
	return out, nil
}
