// Package manifest parses an App's spawneryapp.yml.
package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Mount is one named data mount the app declares.
type Mount struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"` // relative to /app
	Seed string `yaml:"seed"` // relative to /app
	// Durability is the per-mount durability class (transient-tier journal,
	// design §1a): "ephemeral" (default, unset), "node-local", or "owner-sealed".
	// Empty/ephemeral preserves today's scratch contract — the journaler is a
	// no-op until a mount opts in. See internal/storage/journal.ParseDurability.
	Durability string `yaml:"durability"`
}

type Storage struct {
	Mounts []Mount `yaml:"mounts"`
}

type Agents struct {
	Support     []string `yaml:"support"`
	Exclude     []string `yaml:"exclude"`
	RequiresAcp []string `yaml:"requiresAcp"`
}

type Model struct {
	Requires struct {
		ToolUse          bool  `yaml:"toolUse"`
		MinContextTokens int64 `yaml:"minContextTokens"`
		Vision           bool  `yaml:"vision"`
	} `yaml:"requires"`
	RecommendedDefault string `yaml:"recommendedDefault"`
}

type Manifest struct {
	APIVersion  string   `yaml:"apiVersion"`
	Kind        string   `yaml:"kind"`
	ID          string   `yaml:"id"`
	Title       string   `yaml:"title"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags"`
	Visibility  string   `yaml:"visibility"`
	Agents      Agents   `yaml:"agents"`
	Tools       []string `yaml:"tools"`
	Persona     string   `yaml:"persona"`
	Skills      []string `yaml:"skills"`
	Model       Model    `yaml:"model"`
	Runtime     struct {
		BaseVersion string `yaml:"baseVersion"`
	} `yaml:"runtime"`
	Storage Storage `yaml:"storage"`
}

// Parse reads <appPath>/spawneryapp.yml.
func Parse(appPath string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(appPath, "spawneryapp.yml"))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// protoManifest is a minimal struct to extract durability from the CP-stored protojson blob.
// The protojson representation uses camelCase keys (proto field names are camelCase in JSON).
type protoManifest struct {
	Mounts []struct {
		Name       string `json:"name"`
		Durability string `json:"durability"`
	} `json:"mounts"`
}

// MountDurabilityFromJSON parses the protojson AppManifest string stored in the CP store and
// returns a map of mount-name -> durability class ("ephemeral" | "node-local" | "owner-sealed").
// Absent/empty durability defaults to "ephemeral" (the no-journaling contract).
// This is used by the CP durability guard (internal/cp/durability.go) to classify mounts
// without requiring a durability column on spawn_mounts.
func MountDurabilityFromJSON(manifestJSON string) (map[string]string, error) {
	if manifestJSON == "" {
		return map[string]string{}, nil
	}
	var pm protoManifest
	if err := json.Unmarshal([]byte(manifestJSON), &pm); err != nil {
		return nil, fmt.Errorf("manifest: parse protojson: %w", err)
	}
	out := make(map[string]string, len(pm.Mounts))
	for _, m := range pm.Mounts {
		d := m.Durability
		if d == "" {
			d = "ephemeral"
		}
		out[m.Name] = d
	}
	return out, nil
}
