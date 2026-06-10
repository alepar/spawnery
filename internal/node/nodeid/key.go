package nodeid

import (
	"crypto/ecdsa"
	"errors"
	"os"
	"path/filepath"

	"spawnery/internal/pki"
)

// LoadOrGenerateKey returns the node's persistent private key, generating and persisting it (0600) on
// first call. The key must outlive a single process run so its SPKI fingerprint is STABLE across restarts
// — the fingerprint-bound enrollment flow pins this key at token-issuance time, so a regenerated key on
// every run would never match the token. It is written to the same key.pem that Save uses, so a later
// Save (post-enrollment) overwrites it in place with identical bytes.
func LoadOrGenerateKey(dir string) (*ecdsa.PrivateKey, error) {
	path := filepath.Join(dir, keyFile)
	if pem, err := os.ReadFile(path); err == nil {
		return pki.ParseKeyPEM(pem)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := pki.NewNodeKey()
	if err != nil {
		return nil, err
	}
	pem, err := pki.MarshalKeyPEM(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}
