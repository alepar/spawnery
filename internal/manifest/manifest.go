// Package manifest parses an App's spawneryapp.yml.
package manifest

import (
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
}

type Storage struct {
	Mounts []Mount `yaml:"mounts"`
}

type Manifest struct {
	ID      string  `yaml:"id"`
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
