package spawnlet

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// GitEnvMountPath is where a spawn's WRITABLE git environment dir is bind-mounted in the agent. It is
// a SIBLING of SecretsMountPath (not under it): the secrets mount is node-owned/read-only, whereas
// git-env is chowned to the agent's mapped uid so the agent owns GIT_CONFIG_GLOBAL and can run
// `git config --global ...` (sp-7amh). Render targets (identity, cred helper) are layered on by
// sp-m859.1 / sp-n7iy.
const GitEnvMountPath = "/run/spawnery/git-env"

// GitConfigName is the global gitconfig file inside the git-env dir; GIT_CONFIG_GLOBAL points here.
const GitConfigName = "gitconfig"

// gitEnvChown is a seam for hermetic tests to force the chown / EPERM-degraded path.
var gitEnvChown = os.Chown

// GitEnv owns a per-node root of per-spawn WRITABLE git-environment dirs (parallel to SecretInjector /
// ArtifactStager). Unlike the secrets tmpfs (node-owned, read-only to the agent), this dir is chowned
// to the agent's mapped uid so the agent can write its own global git config. Root should be a tmpfs
// in production.
type GitEnv struct{ Root string }

// DirFor returns the per-spawn host dir (the bind-mount source for GitEnvMountPath).
func (g GitEnv) DirFor(spawnID string) string { return filepath.Join(g.Root, spawnID) }

// Prepare creates the per-spawn git-env dir and makes it writable by the in-container agent-root.
// agentUID is the host uid the agent-root maps to (userns-remap base on Docker, 0 on runsc/native);
// agentUID < 0 is the degraded lane (no userns) where the dir is made world-writable instead. Mirrors
// storage.Scratch.Prepare's chown + EPERM/0777 degraded fallback. Idempotent (MkdirAll), non-
// destructive so a same-node resume preserves the dir. Returns the absolute host dir.
func (g GitEnv) Prepare(spawnID string, agentUID int) (string, error) {
	dir := g.DirFor(spawnID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("git-env: mkdir %s: %w", dir, err)
	}
	degraded := agentUID < 0
	if !degraded {
		if err := gitEnvChown(dir, agentUID, agentUID); err != nil {
			if errors.Is(err, os.ErrPermission) {
				log.Printf("git-env: chown %s to uid %d failed (%v); falling back to world-writable", dir, agentUID, err)
				degraded = true
			} else {
				return "", fmt.Errorf("git-env: chown %s: %w", dir, err)
			}
		}
	}
	mode := os.FileMode(0o755)
	if degraded {
		mode = 0o777
	}
	if err := os.Chmod(dir, mode); err != nil { // MkdirAll is umask-masked; chmod explicitly
		return "", fmt.Errorf("git-env: chmod %s: %w", dir, err)
	}
	return dir, nil
}

// Remove deletes a spawn's whole git-env subdir (teardown). Missing dir is ok.
func (g GitEnv) Remove(spawnID string) error {
	if err := os.RemoveAll(g.DirFor(spawnID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
