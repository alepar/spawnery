package spawnlet

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SecretInjector writes Spawnery-delivered secret plaintext to a per-spawn directory tree rooted at
// Root, at the consuming tool's declared path, mode 0600 (owner-sealed-secrets design §6).
//
// §6 invariant — never-persist on durable disk: Root is a tmpfs (memory-backed) in production, so
// plaintext never lands on the node's durable disk; the per-spawn subdir is bind-mounted into the
// agent container at the secrets mount point (SecretsMountPath), and the journaler excludes it by
// mount. The node (which holds the HPKE sub-key private half) unseals the CP-relayed ciphertext and
// calls Write; the agent reads the file in place and re-reads freely (the file persists for the
// episode). NOT env: env is persisted by the runtime and inherited by every child (§6, roast M10).
type SecretInjector struct {
	Root string // per-node secrets root; a tmpfs in production
}

// SecretsMountPath is where a spawn's secrets subdir is bind-mounted inside the agent container. A
// SealedSecret.target_path is interpreted RELATIVE to this mount, so a tool reads its credential at
// SecretsMountPath/<target_path> (e.g. SecretsMountPath/gh/hosts.yml), pointed there by a path-valued
// env var (GH_CONFIG_DIR, AWS_SHARED_CREDENTIALS_FILE, …) — never by the secret value itself.
const SecretsMountPath = "/run/spawnery/secrets"

// DirFor returns the per-spawn host directory under Root. It is the bind-mount source for the agent's
// SecretsMountPath (the documented pod-spec mount seam).
func (s SecretInjector) DirFor(spawnID string) string {
	return filepath.Join(s.Root, spawnID)
}

// Write decrypts-target a single secret: it places plaintext at DirFor(spawnID)/<rel> with mode 0600,
// creating parent dirs (0700). target is sanitized to stay within the per-spawn dir — an absolute path
// is treated as relative to the mount root and any ".." traversal is rejected — so a malicious target
// can never escape the secrets tmpfs and clobber a node file. It returns the absolute host path written.
func (s SecretInjector) Write(spawnID, target string, plaintext []byte) (string, error) {
	rel, err := safeRel(target)
	if err != nil {
		return "", err
	}
	dir := s.DirFor(spawnID)
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return "", fmt.Errorf("secrets: mkdir for %s: %w", target, err)
	}
	// O_TRUNC + explicit 0600 (re-chmod to defeat a permissive umask), so a re-delivery overwrites and
	// the file is never group/world readable.
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("secrets: open %s: %w", target, err)
	}
	if _, werr := f.Write(plaintext); werr != nil {
		_ = f.Close()
		return "", fmt.Errorf("secrets: write %s: %w", target, werr)
	}
	if cerr := f.Close(); cerr != nil {
		return "", fmt.Errorf("secrets: close %s: %w", target, cerr)
	}
	if cerr := os.Chmod(full, 0o600); cerr != nil {
		return "", fmt.Errorf("secrets: chmod %s: %w", target, cerr)
	}
	return full, nil
}

// Remove deletes a spawn's whole secrets subdir (called on teardown so plaintext does not outlive the
// episode). Best-effort: a missing dir is not an error.
func (s SecretInjector) Remove(spawnID string) error {
	if err := os.RemoveAll(s.DirFor(spawnID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// safeRel normalizes a delivery target into a clean relative path that cannot escape the per-spawn
// secrets dir: a leading "/" is stripped (absolute targets are mount-relative), the result is cleaned,
// and any ".." component (or an empty/dot target) is rejected.
func safeRel(target string) (string, error) {
	t := strings.TrimSpace(target)
	if t == "" {
		return "", fmt.Errorf("secrets: empty target_path")
	}
	t = strings.TrimPrefix(t, "/")
	clean := filepath.Clean(t)
	if clean == "." || clean == "" {
		return "", fmt.Errorf("secrets: invalid target_path %q", target)
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("secrets: target_path %q escapes the secrets dir", target)
	}
	return clean, nil
}
