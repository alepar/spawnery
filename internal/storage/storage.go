// Package storage realizes a spawn's data mounts via pluggable backends.
package storage

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// osChown is a seam for hermetic tests to force the EPERM fallback path.
var osChown = os.Chown

// Backend prepares and finalizes the host directory backing one data mount.
type Backend interface {
	// Prepare returns a host dir to bind read-write at the mount path.
	// seedDir is the absolute host path of the app's seed dir (may be missing).
	// agentUID is the host uid the in-container agent-root maps to (userns-remap base on
	// the Docker lane, 0 on the runsc/native lane); -1 means unknown/degraded, in which
	// case the backend keeps the world-writable fallback.
	Prepare(ctx context.Context, spawnID, mountName, seedDir string, agentUID int) (hostDir string, err error)
	// Finalize runs at teardown.
	Finalize(ctx context.Context, hostDir string) error
}

// Scratch is an ephemeral backend: seed a fresh dir on Prepare, nuke it on Finalize.
type Scratch struct{ Root string }

func NewScratch(root string) *Scratch { return &Scratch{Root: root} }

func (s *Scratch) Prepare(_ context.Context, spawnID, mountName, seedDir string, agentUID int) (string, error) {
	hostDir := filepath.Join(s.Root, spawnID, mountName)
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		return "", err
	}
	// Ownership (spec §5): under userns-remap (Docker) or the runsc sentry the agent's
	// container-root maps to a known host uid (agentUID); chown the mount into that uid so
	// 0755/0644 grants the agent write access WITHOUT world-writable perms. agentUID < 0 is
	// the degraded lane (no userns, agent runs cap-drop=ALL): keep the historical 0777/0666
	// workaround so the cap-dropped agent uid can still write regardless of the host mapping.
	degraded := agentUID < 0
	if !degraded {
		// chown to an arbitrary uid needs root or CAP_CHOWN. A rootless dev node has neither
		// (EPERM) — fall back to world-writable so the agent can still write its data dir.
		if err := osChown(hostDir, agentUID, agentUID); err != nil {
			if errors.Is(err, os.ErrPermission) {
				log.Printf("storage: chown %s to uid %d failed (%v); falling back to world-writable", hostDir, agentUID, err)
				degraded = true
			} else {
				return "", fmt.Errorf("chown mount dir: %w", err)
			}
		}
	}
	dirPerm, filePerm := os.FileMode(0o755), os.FileMode(0o644)
	if degraded {
		dirPerm, filePerm = 0o777, 0o666
	}
	// MkdirAll is umask-masked, so chmod explicitly to the intended mode.
	if err := os.Chmod(hostDir, dirPerm); err != nil {
		return "", err
	}
	// In the non-degraded lane chown seeds to agentUID too (files created by root-node would
	// otherwise stay root-owned); pass -1 to skip chown in the degraded lane.
	chownTo := agentUID
	if degraded {
		chownTo = -1
	}
	if err := copyDirFiles(seedDir, hostDir, filePerm, chownTo); err != nil {
		return "", err
	}
	return hostDir, nil
}

func (s *Scratch) Finalize(_ context.Context, hostDir string) error {
	return os.RemoveAll(hostDir)
}

// copyDirFiles copies top-level regular files from srcDir into dstDir.
// A missing srcDir yields an empty mount (no error).
// filePerm is the mode to write each file with; chownUID >= 0 causes each file to be
// chowned to that uid (same uid as the dir's owner in the non-degraded lane).
func copyDirFiles(srcDir, dstDir string, filePerm os.FileMode, chownUID int) error {
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
		dst := filepath.Join(dstDir, e.Name())
		// filePerm: 0644 owned by agentUID in the userns lane, 0666 in the degraded lane so
		// the cap-dropped agent uid can edit seeds regardless of mapping (see Prepare).
		if err := os.WriteFile(dst, b, filePerm); err != nil {
			return err
		}
		// WriteFile is umask-masked; chmod explicitly to guarantee the intended mode.
		if err := os.Chmod(dst, filePerm); err != nil {
			return err
		}
		if chownUID >= 0 {
			// Dir chown already succeeded above, so EPERM here would be contradictory;
			// propagate any error rather than silently leaving files root-owned.
			if err := osChown(dst, chownUID, chownUID); err != nil {
				return err
			}
		}
	}
	return nil
}
