package auth

import (
	"crypto/rand"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
)

// signedEntry builds a valid signed revocation entry using token.SignArtifact.
func signedEntry(t *testing.T, priv ed25519.PrivateKey, seq int64, accountID string, tokenIDs []string) SignedFeedEntry {
	t.Helper()
	tidJSON, err := json.Marshal(tokenIDs)
	if err != nil {
		t.Fatal(err)
	}
	type entryBody struct {
		Seq       int64  `json:"seq"`
		AccountID string `json:"account_id"`
		FamilyID  string `json:"family_id"`
		TokenIDs  string `json:"token_ids"`
		RevokedAt int64  `json:"revoked_at"`
	}
	body := entryBody{Seq: seq, AccountID: accountID, FamilyID: "fam-1", TokenIDs: string(tidJSON), RevokedAt: time.Now().Unix()}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	wire := token.SignArtifact(token.RevocationDomainPrefix, bodyBytes, priv)
	return SignedFeedEntry{
		Seq: seq, AccountID: accountID, FamilyID: "fam-1",
		TokenIDs: string(tidJSON), RevokedAt: body.RevokedAt,
		Sig: wire,
	}
}

func TestRevocationRegistry_Apply_ValidEntry(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	ks, _ := token.NewKeySet(priv.Public().(ed25519.PublicKey))
	revreg := NewRevocationRegistry(nil)

	entry := signedEntry(t, priv, 1, "acct-1", []string{"tok-A", "tok-B"})
	if err := revreg.Apply(entry, ks); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !revreg.IsRevoked("tok-A", "") {
		t.Error("tok-A should be revoked")
	}
	if !revreg.IsRevoked("tok-B", "") {
		t.Error("tok-B should be revoked")
	}
	if !revreg.IsRevoked("", "acct-1") {
		t.Error("acct-1 should be revoked")
	}
	if revreg.IsRevoked("tok-C", "") {
		t.Error("tok-C should NOT be revoked")
	}
}

func TestRevocationRegistry_Apply_ForgedSig(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, evil, _ := ed25519.GenerateKey(rand.Reader)
	ks, _ := token.NewKeySet(priv.Public().(ed25519.PublicKey))
	revreg := NewRevocationRegistry(nil)

	entry := signedEntry(t, evil, 1, "acct-bad", []string{"tok-bad"})
	err := revreg.Apply(entry, ks)
	if err == nil {
		t.Fatal("expected error for forged sig entry")
	}
	// State must NOT have changed.
	if revreg.IsRevoked("tok-bad", "") || revreg.IsRevoked("", "acct-bad") {
		t.Error("forged entry must not modify state")
	}
}

func TestRevocationRegistry_Apply_FansOutToSessions(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	ks, _ := token.NewKeySet(priv.Public().(ed25519.PublicKey))
	sessions := NewSessionRegistry()
	revreg := NewRevocationRegistry(sessions)

	var cancelled int32
	release := sessions.Add("tok-live", "acct-live", func() { atomic.AddInt32(&cancelled, 1) })
	defer release()

	entry := signedEntry(t, priv, 2, "acct-live", []string{"tok-live"})
	if err := revreg.Apply(entry, ks); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&cancelled) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("session not cancelled after Apply")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRevocationRegistry_AccountRevocation_CancelsSession(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	ks, _ := token.NewKeySet(priv.Public().(ed25519.PublicKey))
	sessions := NewSessionRegistry()
	revreg := NewRevocationRegistry(sessions)

	var cancelled int32
	release := sessions.Add("tok-x", "acct-victim", func() { atomic.AddInt32(&cancelled, 1) })
	defer release()

	entry := signedEntry(t, priv, 3, "acct-victim", []string{"tok-x"})
	if err := revreg.Apply(entry, ks); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&cancelled) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("account revocation did not cancel session")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRevocationRegistry_IsRevoked_AfterApply(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	ks, _ := token.NewKeySet(priv.Public().(ed25519.PublicKey))
	revreg := NewRevocationRegistry(nil)

	if revreg.IsRevoked("tok-pre", "") {
		t.Error("should not be revoked before Apply")
	}

	// Mint an AS token and verify revocation works with the same Verifier.
	now := time.Unix(1_770_000_000, 0)
	kid, _ := token.KeyID(priv.Public().(ed25519.PublicKey))
	body := &authv1.SessionTokenBody{
		AccountId: "acct-2",
		Audience:  "cp",
		TokenId:   "tok-pre",
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(15 * time.Minute).Unix(),
		KeyId:     kid,
	}
	wire, _ := token.Mint(body, priv)

	v := NewVerifier(VerifierConfig{Keys: ks, DevMode: false, Revoked: revreg, Now: func() time.Time { return now }})
	if _, err := v.Verify(wire); err != nil {
		t.Fatalf("pre-revocation verify: %v", err)
	}

	entry := signedEntry(t, priv, 1, "acct-2", []string{"tok-pre"})
	if err := revreg.Apply(entry, ks); err != nil {
		t.Fatal(err)
	}

	_, err := v.Verify(wire)
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("post-revocation: want ErrRevoked, got %v", err)
	}
}
