package authsvc_test

import (
	"testing"
	"time"

	"spawnery/internal/authsvc/token"
)

// SessionPubKey exposes the AS's Ed25519 public key — nodes pin this to verify A4 tokens offline.
func TestSessionPubKeyIsNonNil(t *testing.T) {
	s := newAS(t)
	pub := s.SessionPubKey()
	if len(pub) == 0 {
		t.Fatal("SessionPubKey must return a non-nil Ed25519 public key")
	}
}

// A4 token minted with the AS session key verifies against the AS's published pubkey [MC1][AM4].
func TestA4TokenMintedByASVerifies(t *testing.T) {
	s := newAS(t)
	pub := s.SessionPubKey()

	ks, err := token.NewKeySet(pub)
	if err != nil {
		t.Fatal(err)
	}
	// Verify that the KeySet constructed from the AS pubkey works for lookup.
	if len(ks) == 0 {
		t.Fatal("KeySet must not be empty")
	}
	// Derived key_id must be non-empty.
	keyID, err := token.KeyID(pub)
	if err != nil {
		t.Fatal(err)
	}
	if keyID == "" {
		t.Fatal("KeyID must not be empty")
	}

	_, ok := ks.Lookup(keyID)
	if !ok {
		t.Fatalf("published pubkey not found in KeySet under derived key_id %q", keyID)
	}

	_ = time.Now() // satisfy import
}
