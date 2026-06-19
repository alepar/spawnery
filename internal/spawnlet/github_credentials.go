package spawnlet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"spawnery/internal/githubcred"
	"spawnery/internal/storage"
)

// stageMountNameSuffix is appended to a mount's logical name when it is root-materialized into a
// staging dir (userns-remap lane). It is a filesystem-staging detail only and must not change which
// per-mount github credential a mount resolves to (see githubCredentialTargetDir).
const stageMountNameSuffix = ".stage"

type GitHubCredentialStore struct {
	Root string
}

func (s GitHubCredentialStore) DirFor(spawnID string) string {
	return filepath.Join(s.Root, spawnID)
}

func (s GitHubCredentialStore) Remove(spawnID string) error {
	if err := os.RemoveAll(s.DirFor(spawnID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func githubCredentialTargetDir(mountName string) (string, error) {
	if mountName == "" {
		return "", fmt.Errorf("github credential mount name is required")
	}
	// Key the credential by the LOGICAL mount name. Root-materialized mounts (userns-remap) are
	// prepared under "<name>.stage" (see stageMountNameSuffix); the at-provision render keys by the
	// logical name, so strip the staging suffix here to keep render and Prepare-time lookup aligned.
	name := strings.TrimSuffix(mountName, stageMountNameSuffix)
	sum := sha256.Sum256([]byte(name))
	return filepath.Join("github-node", hex.EncodeToString(sum[:8])), nil
}

func (m *Manager) RenderGitHubNodeCredential(spawnID, mountName string, req githubcred.RenderRequest) (githubcred.Rendered, error) {
	targetDir, err := githubCredentialTargetDir(mountName)
	if err != nil {
		return githubcred.Rendered{}, err
	}
	req.TargetDir = targetDir
	return githubcred.Render(m.githubCreds.DirFor(spawnID), req)
}

func (m *Manager) RemoveGitHubNodeCredentials(spawnID string) error {
	return m.githubCreds.Remove(spawnID)
}

func (m *Manager) TokenForGitHubMount(_ context.Context, spawnID, mountName string, cfg storage.GitHubConfig) (storage.GitHubCredential, error) {
	targetDir, err := githubCredentialTargetDir(mountName)
	if err != nil {
		return storage.GitHubCredential{}, err
	}
	root := m.githubCreds.DirFor(spawnID)
	tokenPath := filepath.Join(root, targetDir, "token")
	helperPath := filepath.Join(root, targetDir, "git-credential-spawnery")
	if _, err := os.Stat(tokenPath); err != nil {
		return storage.GitHubCredential{}, fmt.Errorf("missing node github token for mount %q secret %q: %w", mountName, cfg.CredentialSecretID, err)
	}
	return storage.GitHubCredential{TokenPath: tokenPath, CredentialHelperPath: helperPath}, nil
}
