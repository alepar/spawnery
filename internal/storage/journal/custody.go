package journal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// NodeLocalCustody implements PasswordProvider for the node-local durability
// class (design §4): the node generates a per-spawn random Kopia repo password
// and stores it encrypted-at-rest under a node key. The CP and the object-store
// operator only ever see Kopia ciphertext; same-node suspend/resume and crash
// recovery read the local sealed password.
//
// Sealing: a node master key (read from a keyfile) is run through HKDF-SHA256
// with a per-spawn salt to derive a per-spawn AES-256-GCM key; the password is
// sealed as nonce||ciphertext and written to a node-local file. This is the
// node-held custody seam — an owner-sealed provider implements PasswordProvider
// differently (sealing to the owner's keys) without changing the repo code.
type NodeLocalCustody struct {
	nodeKey []byte // node master key material (>= 16 bytes)
	dir     string // directory holding per-spawn sealed-password files

	mu    sync.Mutex
	cache map[string]string // spawnID -> plaintext password (process-lifetime cache)
}

const (
	hkdfInfoPassword = "spawnery/journal/repo-password/v1"
	passwordBytes    = 32 // 256-bit repo password, hex-encoded
)

// NewNodeLocalCustody builds custody from a node keyfile. The keyfile holds the
// node master key (raw bytes; >= 16). sealDir is where per-spawn sealed
// passwords live (created if absent, 0700). The keyfile is never copied off the
// node; node death loses node-local journals by construction (design §4).
func NewNodeLocalCustody(keyfilePath, sealDir string) (*NodeLocalCustody, error) {
	key, err := os.ReadFile(keyfilePath)
	if err != nil {
		return nil, fmt.Errorf("journal custody: read node keyfile: %w", err)
	}
	if len(key) < 16 {
		return nil, fmt.Errorf("journal custody: node key too short (%d bytes; need >= 16)", len(key))
	}
	if err := os.MkdirAll(sealDir, 0o700); err != nil {
		return nil, fmt.Errorf("journal custody: mkdir seal dir: %w", err)
	}
	return &NodeLocalCustody{nodeKey: key, dir: sealDir, cache: map[string]string{}}, nil
}

// GenerateNodeKeyfile writes a fresh 32-byte random node master key to path
// (0600) if it does not already exist, and returns its path. Idempotent: an
// existing keyfile is left untouched so the node's custody survives restarts.
func GenerateNodeKeyfile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("journal custody: stat keyfile: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("journal custody: mkdir keyfile dir: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("journal custody: gen node key: %w", err)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return fmt.Errorf("journal custody: write keyfile: %w", err)
	}
	return nil
}

func (c *NodeLocalCustody) sealPath(spawnID string) string {
	// hex the spawn id so an arbitrary id is a safe filename.
	return filepath.Join(c.dir, hex.EncodeToString([]byte(spawnID))+".seal")
}

// aead derives the per-spawn AES-256-GCM AEAD from the node key + spawn salt.
func (c *NodeLocalCustody) aead(spawnID string) (cipher.AEAD, error) {
	dk, err := hkdf.Key(sha256.New, c.nodeKey, []byte("spawn:"+spawnID), hkdfInfoPassword, 32)
	if err != nil {
		return nil, fmt.Errorf("hkdf: %w", err)
	}
	block, err := aes.NewCipher(dk)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return gcm, nil
}

// PasswordFor implements PasswordProvider: return the existing sealed password,
// or generate+seal a fresh one on first call.
func (c *NodeLocalCustody) PasswordFor(spawnID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if pw, ok := c.cache[spawnID]; ok {
		return pw, nil
	}
	// Try to unseal an existing password (same-node resume / crash recovery).
	if pw, err := c.unseal(spawnID); err == nil {
		c.cache[spawnID] = pw
		return pw, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	// First call for this spawn: mint + seal.
	raw := make([]byte, passwordBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("journal custody: gen password: %w", err)
	}
	pw := hex.EncodeToString(raw)
	if err := c.seal(spawnID, pw); err != nil {
		return "", err
	}
	c.cache[spawnID] = pw
	return pw, nil
}

func (c *NodeLocalCustody) seal(spawnID, pw string) error {
	gcm, err := c.aead(spawnID)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("journal custody: gen nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(pw), []byte(spawnID))
	// Write atomically: temp + rename, 0600.
	tmp := c.sealPath(spawnID) + ".tmp"
	if err := os.WriteFile(tmp, sealed, 0o600); err != nil {
		return fmt.Errorf("journal custody: write sealed: %w", err)
	}
	if err := os.Rename(tmp, c.sealPath(spawnID)); err != nil {
		return fmt.Errorf("journal custody: rename sealed: %w", err)
	}
	return nil
}

func (c *NodeLocalCustody) unseal(spawnID string) (string, error) {
	sealed, err := os.ReadFile(c.sealPath(spawnID))
	if err != nil {
		return "", err // includes fs.ErrNotExist, handled by caller
	}
	gcm, err := c.aead(spawnID)
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(sealed) < ns {
		return "", fmt.Errorf("journal custody: sealed file too short")
	}
	nonce, ct := sealed[:ns], sealed[ns:]
	pt, err := gcm.Open(nil, nonce, ct, []byte(spawnID))
	if err != nil {
		return "", fmt.Errorf("journal custody: unseal (wrong node key or corrupt): %w", err)
	}
	return string(pt), nil
}

// Forget implements PasswordProvider: drop the cached + sealed password for
// spawnID (spawn delete / migrate-away). Missing files are not an error.
func (c *NodeLocalCustody) Forget(spawnID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cache, spawnID)
	if err := os.Remove(c.sealPath(spawnID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("journal custody: remove sealed: %w", err)
	}
	return nil
}
