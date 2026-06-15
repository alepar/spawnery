package spawnlet

import (
	"fmt"

	"spawnery/internal/manifest"
)

type MountBinding struct {
	Name       string
	BackendURI string
}

func mountBindingsByName(manifestMounts []manifest.Mount, bindings []MountBinding) (map[string]string, error) {
	manifestNames := make(map[string]struct{}, len(manifestMounts))
	for _, mount := range manifestMounts {
		manifestNames[mount.Name] = struct{}{}
	}

	out := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		if _, ok := manifestNames[binding.Name]; !ok {
			return nil, fmt.Errorf("mount binding %q does not match any manifest mount", binding.Name)
		}
		if _, dup := out[binding.Name]; dup {
			return nil, fmt.Errorf("duplicate mount binding %q", binding.Name)
		}
		out[binding.Name] = binding.BackendURI
	}
	return out, nil
}
