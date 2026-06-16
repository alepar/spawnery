package store

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func testCipher(t *testing.T) TokenCipher {
	t.Helper()
	c, err := NewAESGCMTokenCipher(bytes.Repeat([]byte{0x2a}, 32))
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	return c
}

func TestTokenCipherRoundTrip(t *testing.T) {
	c := testCipher(t)
	ct, err := c.Encrypt("ghr_secret", "github_link/refresh/v1/gh-main")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if ct == "ghr_secret" || ct == "" {
		t.Fatalf("ciphertext must differ from plaintext and be non-empty: %q", ct)
	}
	pt, err := c.Decrypt(ct, "github_link/refresh/v1/gh-main")
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if pt != "ghr_secret" {
		t.Fatalf("round-trip mismatch: %q", pt)
	}
}

func TestTokenCipherEmptyPassThrough(t *testing.T) {
	c := testCipher(t)
	ct, err := c.Encrypt("", "aad")
	if err != nil || ct != "" {
		t.Fatalf("empty plaintext must encrypt to empty: %q %v", ct, err)
	}
	pt, err := c.Decrypt("", "aad")
	if err != nil || pt != "" {
		t.Fatalf("empty ciphertext must decrypt to empty: %q %v", pt, err)
	}
}

func TestTokenCipherAADMismatchFails(t *testing.T) {
	c := testCipher(t)
	ct, _ := c.Encrypt("ghr_secret", "github_link/refresh/v1/gh-main")
	if _, err := c.Decrypt(ct, "github_link/access/v1/gh-main"); err == nil {
		t.Fatal("decrypt with wrong AAD must fail (no field/column splice)")
	}
}

func TestTokenCipherTamperFails(t *testing.T) {
	c := testCipher(t)
	ct, _ := c.Encrypt("ghr_secret", "aad")
	raw, _ := base64.RawStdEncoding.DecodeString(ct)
	raw[len(raw)-1] ^= 0xff
	if _, err := c.Decrypt(base64.RawStdEncoding.EncodeToString(raw), "aad"); err == nil {
		t.Fatal("decrypt of tampered ciphertext must fail")
	}
}

func TestTokenCipherNonceIsRandom(t *testing.T) {
	c := testCipher(t)
	a, _ := c.Encrypt("ghr_secret", "aad")
	b, _ := c.Encrypt("ghr_secret", "aad")
	if a == b {
		t.Fatal("same plaintext+aad must produce distinct ciphertext (random nonce)")
	}
}

func TestNewAESGCMTokenCipherRejectsShortKey(t *testing.T) {
	if _, err := NewAESGCMTokenCipher(make([]byte, 16)); err == nil {
		t.Fatal("must reject non-32-byte key")
	}
}

func TestParseTokenCipherKey(t *testing.T) {
	key := bytes.Repeat([]byte{0x07}, 32)
	c, err := ParseTokenCipherKey(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}
	ct, _ := c.Encrypt("x", "aad")
	if pt, _ := c.Decrypt(ct, "aad"); pt != "x" {
		t.Fatal("parsed-key cipher must round-trip")
	}
	if _, err := ParseTokenCipherKey("not-base64!!"); err == nil {
		t.Fatal("must reject non-base64 key")
	}
	if _, err := ParseTokenCipherKey(base64.StdEncoding.EncodeToString(make([]byte, 16))); err == nil {
		t.Fatal("must reject wrong-length decoded key")
	}
}
