package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
)

// helpers

func genKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func keyID(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()
	id, err := token.KeyID(priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustKeySet(t *testing.T, privs ...ed25519.PrivateKey) token.KeySet {
	t.Helper()
	pubs := make([]ed25519.PublicKey, len(privs))
	for i, p := range privs {
		pubs[i] = p.Public().(ed25519.PublicKey)
	}
	ks, err := token.NewKeySet(pubs...)
	if err != nil {
		t.Fatal(err)
	}
	return ks
}

func mintToken(t *testing.T, priv ed25519.PrivateKey, audience, accountID, tokenID string, now time.Time) string {
	t.Helper()
	kid := keyID(t, priv)
	body := &authv1.SessionTokenBody{
		AccountId: accountID,
		Handle:    "tester",
		TokenId:   tokenID,
		Audience:  audience,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(15 * time.Minute).Unix(),
		KeyId:     kid,
	}
	wire, err := token.Mint(body, priv)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

var testNow = time.Unix(1_770_000_000, 0)

func TestVerify_ValidAudCp(t *testing.T) {
	priv := genKey(t)
	ks := mustKeySet(t, priv)
	v := NewVerifier(VerifierConfig{Keys: ks, DevMode: false, Now: func() time.Time { return testNow }})
	wire := mintToken(t, priv, "cp", "acct-1", "tok-1", testNow)

	id, err := v.Verify(wire)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.Owner != "acct-1" {
		t.Errorf("owner: got %q want %q", id.Owner, "acct-1")
	}
	if id.TokenID != "tok-1" {
		t.Errorf("token_id: got %q want %q", id.TokenID, "tok-1")
	}
}

func TestVerify_WrongAudience(t *testing.T) {
	priv := genKey(t)
	ks := mustKeySet(t, priv)
	v := NewVerifier(VerifierConfig{Keys: ks, DevMode: false, Now: func() time.Time { return testNow }})
	wire := mintToken(t, priv, "node", "acct-1", "tok-1", testNow)

	_, err := v.Verify(wire)
	if err == nil {
		t.Fatal("expected error for aud=node, got nil")
	}
}

func TestVerify_ForgedSignature(t *testing.T) {
	priv := genKey(t)
	evil := genKey(t)
	ks := mustKeySet(t, priv)
	v := NewVerifier(VerifierConfig{Keys: ks, DevMode: false, Now: func() time.Time { return testNow }})
	// Mint with evil key (claims correct key_id so selection succeeds, then sig check fails).
	kid := keyID(t, priv)
	body := &authv1.SessionTokenBody{
		AccountId: "acct-x",
		Audience:  "cp",
		IssuedAt:  testNow.Unix(),
		ExpiresAt: testNow.Add(15 * time.Minute).Unix(),
		KeyId:     kid,
	}
	wire, err := token.Mint(body, evil)
	if err != nil {
		t.Fatal(err)
	}

	_, verr := v.Verify(wire)
	if verr == nil {
		t.Fatal("expected error for forged sig")
	}
	if !errors.Is(verr, token.ErrSignature) {
		t.Logf("error was: %v (acceptable)", verr)
	}
}

func TestVerify_Expired(t *testing.T) {
	priv := genKey(t)
	ks := mustKeySet(t, priv)
	v := NewVerifier(VerifierConfig{Keys: ks, DevMode: false, Now: func() time.Time {
		return testNow.Add(20 * time.Minute) // past expiry
	}})
	wire := mintToken(t, priv, "cp", "acct-1", "tok-1", testNow)
	_, err := v.Verify(wire)
	if err == nil {
		t.Fatal("expected expired error")
	}
}

func TestVerify_UnknownKeyID(t *testing.T) {
	priv := genKey(t)
	other := genKey(t)
	ks := mustKeySet(t, other) // only other, not priv
	v := NewVerifier(VerifierConfig{Keys: ks, DevMode: false, Now: func() time.Time { return testNow }})
	wire := mintToken(t, priv, "cp", "acct-1", "tok-1", testNow)
	_, err := v.Verify(wire)
	if err == nil {
		t.Fatal("expected error for unknown key_id")
	}
}

// Rotation overlap [AM4]: keyset {A,B} — tokens from A and B both verify;
// retire A (keyset {B} only) → A-tokens refused.
func TestVerify_RotationOverlap(t *testing.T) {
	keyA := genKey(t)
	keyB := genKey(t)
	now := func() time.Time { return testNow }

	overlap := mustKeySet(t, keyA, keyB)
	tokA := mintToken(t, keyA, "cp", "acct-A", "tok-A", testNow)
	tokB := mintToken(t, keyB, "cp", "acct-B", "tok-B", testNow)

	vOverlap := NewVerifier(VerifierConfig{Keys: overlap, DevMode: false, Now: now})
	if _, err := vOverlap.Verify(tokA); err != nil {
		t.Fatalf("overlap: keyA token: %v", err)
	}
	if _, err := vOverlap.Verify(tokB); err != nil {
		t.Fatalf("overlap: keyB token: %v", err)
	}

	// Retire A.
	retired := mustKeySet(t, keyB)
	vRetired := NewVerifier(VerifierConfig{Keys: retired, DevMode: false, Now: now})
	if _, err := vRetired.Verify(tokA); err == nil {
		t.Fatal("retired A: keyA token should be refused")
	}
	if _, err := vRetired.Verify(tokB); err != nil {
		t.Fatalf("retired A: keyB token still valid: %v", err)
	}
}

// Dev-token resolves in dev mode, refused in prod.
func TestVerify_DevToken(t *testing.T) {
	priv := genKey(t)
	ks := mustKeySet(t, priv)
	devTokens := map[string]string{"my-dev-token": "alice"}

	devVerifier := NewVerifier(VerifierConfig{Keys: ks, DevTokens: devTokens, DevMode: true, Now: func() time.Time { return testNow }})
	id, err := devVerifier.Verify("my-dev-token")
	if err != nil {
		t.Fatalf("dev mode: %v", err)
	}
	if id.Owner != "alice" {
		t.Errorf("dev mode: owner %q want alice", id.Owner)
	}
	if id.TokenID != "" {
		t.Errorf("dev mode: token_id should be empty, got %q", id.TokenID)
	}

	prodVerifier := NewVerifier(VerifierConfig{Keys: ks, DevTokens: devTokens, DevMode: false, Now: func() time.Time { return testNow }})
	_, err = prodVerifier.Verify("my-dev-token")
	if err == nil {
		t.Fatal("prod mode: dev token should be refused")
	}
}

// aud=cp AS token works in both dev and prod modes.
func TestVerify_ASTokenBothModes(t *testing.T) {
	priv := genKey(t)
	ks := mustKeySet(t, priv)
	now := func() time.Time { return testNow }
	wire := mintToken(t, priv, "cp", "acct-1", "tok-1", testNow)

	for _, devMode := range []bool{true, false} {
		v := NewVerifier(VerifierConfig{Keys: ks, DevMode: devMode, Now: now})
		id, err := v.Verify(wire)
		if err != nil {
			t.Errorf("devMode=%v: %v", devMode, err)
		}
		if id.Owner != "acct-1" {
			t.Errorf("devMode=%v: owner %q", devMode, id.Owner)
		}
	}
}

// Revoked token refused.
func TestVerify_RevokedToken(t *testing.T) {
	priv := genKey(t)
	ks := mustKeySet(t, priv)
	revreg := NewRevocationRegistry(nil)
	v := NewVerifier(VerifierConfig{Keys: ks, DevMode: false, Revoked: revreg, Now: func() time.Time { return testNow }})
	wire := mintToken(t, priv, "cp", "acct-1", "tok-revoked", testNow)

	// First verify succeeds.
	if _, err := v.Verify(wire); err != nil {
		t.Fatalf("pre-revocation: %v", err)
	}

	// Revoke the token_id manually.
	revreg.mu.Lock()
	revreg.revokedTokens["tok-revoked"] = struct{}{}
	revreg.mu.Unlock()

	_, err := v.Verify(wire)
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("post-revocation: want ErrRevoked, got %v", err)
	}
}

// Dev mode with no keys configured (keys empty): only dev tokens work.
func TestVerify_DevModeNoKeys(t *testing.T) {
	devTokens := map[string]string{"tok": "owner-x"}
	v := NewVerifier(VerifierConfig{Keys: token.KeySet{}, DevTokens: devTokens, DevMode: true, Now: func() time.Time { return testNow }})
	id, err := v.Verify("tok")
	if err != nil {
		t.Fatalf("dev no-keys: %v", err)
	}
	if id.Owner != "owner-x" {
		t.Errorf("owner: %q", id.Owner)
	}
}
