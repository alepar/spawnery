package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// TokenCipher encrypts/decrypts the AS-custodial GitHub token columns at rest.
// The key is held OUTSIDE the database (env/file/KMS), so a DB-only compromise
// (backup, replica, SQLi, disk theft) does not expose live refresh/access tokens
// (design §16.2 / Decision 21; review MAJOR-2). Empty strings pass through
// unchanged so the optional access_token's NULL/empty semantics are preserved.
type TokenCipher interface {
	Encrypt(plaintext, aad string) (string, error)
	Decrypt(ciphertext, aad string) (string, error)
}

// ErrTokenDecrypt is returned when a ciphertext fails AEAD authentication
// (wrong key, wrong AAD, or tampering).
var ErrTokenDecrypt = errors.New("authsvc/store: github token decrypt failed")

type aesGCMTokenCipher struct{ aead cipher.AEAD }

// NewAESGCMTokenCipher builds an AES-256-GCM TokenCipher from a 32-byte key.
func NewAESGCMTokenCipher(key []byte) (TokenCipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("authsvc/store: github token key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &aesGCMTokenCipher{aead: aead}, nil
}

// ParseTokenCipherKey decodes a standard-base64 32-byte key into a cipher.
func ParseTokenCipherKey(b64 string) (TokenCipher, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("authsvc/store: github token key not base64: %w", err)
	}
	return NewAESGCMTokenCipher(raw)
}

func (c *aesGCMTokenCipher) Encrypt(plaintext, aad string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("authsvc/store: read nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), []byte(aad))
	return base64.RawStdEncoding.EncodeToString(sealed), nil
}

func (c *aesGCMTokenCipher) Decrypt(ciphertext, aad string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", ErrTokenDecrypt
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return "", ErrTokenDecrypt
	}
	nonce, ct := raw[:ns], raw[ns:]
	pt, err := c.aead.Open(nil, nonce, ct, []byte(aad))
	if err != nil {
		return "", ErrTokenDecrypt
	}
	return string(pt), nil
}
