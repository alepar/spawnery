// Package storage realizes a spawn's data mounts via pluggable backends.
package storage

import (
	"context"
	"os"
	"path/filepath"
)

// Backend prepares and finalizes the host directory backing one data mount.
type Backend interface {
	// Prepare returns a host dir to bind read-write at the mount path.
	// seedDir is the absolute host path of the app's seed dir (may be missing).
	Prepare(ctx context.Context, spawnID, mountName, seedDir string) (hostDir string, err error)
	// Finalize runs at teardown.
	Finalize(ctx context.Context, hostDir string) error
}

// Scratch is an ephemeral backend: seed a fresh dir on Prepare, nuke it on Finalize.
type Scratch struct{ Root string }

func NewScratch(root string) *Scratch { return &Scratch{Root: root} }

func (s *Scratch) Prepare(_ context.Context, spawnID, mountName, seedDir string) (string, error) {
	hostDir := filepath.Join(s.Root, spawnID, mountName)
	if err := os.MkdirAll(hostDir, 0o777); err != nil {
		return "", err
	}
	// The agent runs --cap-drop=ALL (no CAP_DAC_OVERRIDE), so its uid is subject to real
	// permissions even as container-root; the host dir is owned by the (possibly different) node
	// uid. Make the mount world-writable so the agent can write its own data dir regardless of the
	// uid mapping (rootful / rootless / future userns). MkdirAll is umask-masked, so chmod explicitly.
	if err := os.Chmod(hostDir, 0o777); err != nil {
		return "", err
	}
	if err := copyDirFiles(seedDir, hostDir); err != nil {
		return "", err
	}
	return hostDir, nil
}

func (s *Scratch) Finalize(_ context.Context, hostDir string) error {
	return os.RemoveAll(hostDir)
}

// copyDirFiles copies top-level regular files from srcDir into dstDir.
// A missing srcDir yields an empty mount (no error).
func copyDirFiles(srcDir, dstDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(srcDir, e.Name()))
		if err != nil {
			return err
		}
		// 0o666: seeded files must be editable by the cap-dropped agent uid too (see Prepare).
		if err := os.WriteFile(filepath.Join(dstDir, e.Name()), b, 0o666); err != nil {
			return err
		}
	}
	return nil
}
