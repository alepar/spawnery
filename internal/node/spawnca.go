package node

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// caStore is the on-disk, node-private store for a spawn's MITM CA keypair
// (<dir>/<spawnID>/{ca.crt,ca.key}, dir 0700, ca.key 0600). It exists so a restarted node returns
// the SAME CA for a spawn it did not create — otherwise the CA is regenerated on the first cache
// miss while the agent still trusts the old cert (see githubcontrol.go's caForLocked).
//
// D1 (bead sp-2tx8.3.5): this is deliberately NOT the git-env dir renderGitProxy writes spawn-ca.crt
// into. That dir is chowned to the untrusted agent's uid and bind-mounted into it; writing the
// private key there would hand the agent the MITM CA key and let it delete the file. caStore's dir
// is node-only and never bind-mounted, mirroring DataRoot/github-static.
//
// The zero value (dir == "") is a memory-only no-op: Load always reports "absent", Save and Remove
// are no-ops. This is what a node with no configured SpawnCARoot gets — the CA does not survive a
// restart, but nothing errors.
type caStore struct {
	dir string
}

// safeSpawnID rejects a spawn id that could escape dir — it comes off a container label, so this is
// defence in depth, not the primary trust boundary.
func safeSpawnID(spawnID string) error {
	if spawnID == "" || strings.ContainsAny(spawnID, "/\x00") || strings.Contains(spawnID, "..") {
		return fmt.Errorf("caStore: unsafe spawn id %q", spawnID)
	}
	return nil
}

// Load returns the persisted CA pair for spawnID. ok=false (nil err) means no pair is on disk yet —
// including a torn write (cert present, key missing), which is treated as absent so the caller
// regenerates rather than serving a keyless CA.
func (s caStore) Load(spawnID string) (caPair, bool, error) {
	if s.dir == "" {
		return caPair{}, false, nil
	}
	if err := safeSpawnID(spawnID); err != nil {
		return caPair{}, false, err
	}
	spawnDir := filepath.Join(s.dir, spawnID)
	certPEM, certErr := os.ReadFile(filepath.Join(spawnDir, "ca.crt"))
	if os.IsNotExist(certErr) {
		return caPair{}, false, nil
	}
	if certErr != nil {
		return caPair{}, false, fmt.Errorf("caStore: read ca.crt for %s: %w", spawnID, certErr)
	}
	keyPEM, keyErr := os.ReadFile(filepath.Join(spawnDir, "ca.key"))
	if os.IsNotExist(keyErr) {
		return caPair{}, false, nil // torn write: cert without key => absent, not an error
	}
	if keyErr != nil {
		return caPair{}, false, fmt.Errorf("caStore: read ca.key for %s: %w", spawnID, keyErr)
	}
	return caPair{certPEM: certPEM, keyPEM: keyPEM}, true, nil
}

// Save persists p for spawnID, atomically (temp file + rename) so a crash mid-write cannot leave a
// torn cert-without-key (or vice versa) that Load would need to reason about beyond "absent".
func (s caStore) Save(spawnID string, p caPair) error {
	if s.dir == "" {
		return nil
	}
	if err := safeSpawnID(spawnID); err != nil {
		return err
	}
	spawnDir := filepath.Join(s.dir, spawnID)
	if err := os.MkdirAll(spawnDir, 0o700); err != nil {
		return fmt.Errorf("caStore: mkdir %s: %w", spawnDir, err)
	}
	if err := atomicWriteFile(filepath.Join(spawnDir, "ca.crt"), p.certPEM, 0o600); err != nil {
		return fmt.Errorf("caStore: write ca.crt for %s: %w", spawnID, err)
	}
	if err := atomicWriteFile(filepath.Join(spawnDir, "ca.key"), p.keyPEM, 0o600); err != nil {
		return fmt.Errorf("caStore: write ca.key for %s: %w", spawnID, err)
	}
	return nil
}

// Remove deletes spawnID's persisted CA pair, if any. Idempotent.
func (s caStore) Remove(spawnID string) error {
	if s.dir == "" {
		return nil
	}
	if err := safeSpawnID(spawnID); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(s.dir, spawnID)); err != nil {
		return fmt.Errorf("caStore: remove %s: %w", spawnID, err)
	}
	return nil
}

// atomicWriteFile writes data to path via a temp file in the same directory, chmods it explicitly
// (does not rely on umask), then renames it into place.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
