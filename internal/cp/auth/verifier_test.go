package auth

import (
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
)

var testNow = time.Unix(1_770_000_000, 0)

func mintToken(t *testing.T, credential *token.SigningCredential, audience, accountID, tokenID string, now time.Time) string {
	t.Helper()
	payload, err := proto.Marshal(&authv1.SessionTokenBody{
		AccountId: accountID, Handle: "tester", TokenId: tokenID, Audience: audience,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(15 * time.Minute).Unix(), KeyId: hex.EncodeToString(credential.KeyID[:]),
	})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := credential.Sign(token.ArtifactTypeSession, payload)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestVerifierCertifiedSession(t *testing.T) {
	fixture := newArtifactFixture(t)
	v := NewVerifier(VerifierConfig{Artifacts: fixture.verifier, Now: func() time.Time { return testNow }})
	id, err := v.Verify(mintToken(t, fixture.credential, "cp", "acct-1", "tok-1", testNow))
	if err != nil {
		t.Fatal(err)
	}
	if id.Owner != "acct-1" || id.TokenID != "tok-1" || id.IssuedAt != testNow.Unix() {
		t.Fatalf("identity = %+v", id)
	}
}

func TestVerifierEnforcesPayloadSemanticsAfterArtifactVerification(t *testing.T) {
	fixture := newArtifactFixture(t)
	for _, tc := range []struct {
		name, audience string
		issued, now    time.Time
	}{
		{name: "wrong audience", audience: "node", issued: testNow, now: testNow},
		{name: "expired", audience: "cp", issued: testNow, now: testNow.Add(20 * time.Minute)},
		{name: "issued in future", audience: "cp", issued: testNow.Add(2 * time.Minute), now: testNow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := NewVerifier(VerifierConfig{Artifacts: fixture.verifier, Now: func() time.Time { return tc.now }})
			if _, err := v.Verify(mintToken(t, fixture.credential, tc.audience, "acct", "tok", tc.issued)); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestVerifierRejectsWrongArtifactTypeAndLegacyToken(t *testing.T) {
	fixture := newArtifactFixture(t)
	other := newArtifactFixture(t)
	v := NewVerifier(VerifierConfig{Artifacts: fixture.verifier, Now: func() time.Time { return testNow }})
	payload, _ := proto.Marshal(&authv1.SessionTokenBody{Audience: "cp", IssuedAt: testNow.Unix(), ExpiresAt: testNow.Add(time.Minute).Unix()})
	revocation, _ := fixture.credential.Sign(token.ArtifactTypeRevocation, payload)
	for _, wire := range []string{revocation, "body.signature", mintToken(t, other.credential, "cp", "acct", "tok", testNow)} {
		if _, err := v.Verify(wire); err == nil {
			t.Fatalf("accepted %q", wire)
		}
	}
}

func TestVerifierDevTokensAreExactOpaqueMatches(t *testing.T) {
	fixture := newArtifactFixture(t)
	dev := NewVerifier(VerifierConfig{Artifacts: fixture.verifier, DevMode: true, DevTokens: map[string]string{"opaque.dev.token": "alice"}, Now: func() time.Time { return testNow }})
	id, err := dev.Verify("opaque.dev.token")
	if err != nil || id.Owner != "alice" {
		t.Fatalf("exact dev token: id=%+v err=%v", id, err)
	}
	for _, wire := range []string{"opaque.dev.token.", "body.signature", "opaque.dev.toke"} {
		if _, err := dev.Verify(wire); err == nil {
			t.Fatalf("accepted non-matching dev token %q", wire)
		}
	}
	prod := NewVerifier(VerifierConfig{Artifacts: fixture.verifier, DevMode: false, DevTokens: map[string]string{"opaque.dev.token": "alice"}, Now: func() time.Time { return testNow }})
	if _, err := prod.Verify("opaque.dev.token"); err == nil {
		t.Fatal("production accepted dev token")
	}
}

func TestVerifierUserRevocationRemainsDistinct(t *testing.T) {
	fixture := newArtifactFixture(t)
	revocations := NewRevocationRegistry(nil)
	v := NewVerifier(VerifierConfig{Artifacts: fixture.verifier, Revoked: revocations, Now: func() time.Time { return testNow }})
	wire := mintToken(t, fixture.credential, "cp", "acct", "tok", testNow)
	if _, err := v.Verify(wire); err != nil {
		t.Fatal(err)
	}
	revocations.mu.Lock()
	revocations.revokedTokens["tok"] = testNow.Add(time.Minute).Unix()
	revocations.mu.Unlock()
	if _, err := v.Verify(wire); !errors.Is(err, ErrRevoked) {
		t.Fatalf("want ErrRevoked, got %v", err)
	}
}

func TestVerifierUsesSignedIssuedAtForExclusiveAccountCutoff(t *testing.T) {
	fixture := newArtifactFixture(t)
	revocations := NewRevocationRegistry(nil)
	cutoff := &authv1.RevocationEntry{
		Seq: 1, AccountId: "acct", RevokedAt: testNow.Unix() - 1, RevokeTokensIssuedBefore: testNow.Unix(),
	}
	if _, err := revocations.ApplyPage([]SignedFeedEntry{signedEntry(t, fixture.credential, cutoff)}, fixture.verifier, testNow, 0); err != nil {
		t.Fatal(err)
	}
	verifier := NewVerifier(VerifierConfig{Artifacts: fixture.verifier, Revoked: revocations, Now: func() time.Time { return testNow }})
	if _, err := verifier.Verify(mintToken(t, fixture.credential, "cp", "acct", "old", testNow.Add(-time.Second))); !errors.Is(err, ErrRevoked) {
		t.Fatalf("pre-cutoff token error=%v", err)
	}
	id, err := verifier.Verify(mintToken(t, fixture.credential, "cp", "acct", "equal", testNow))
	if err != nil {
		t.Fatal(err)
	}
	if id.IssuedAt != testNow.Unix() {
		t.Fatalf("identity issued_at=%d want=%d", id.IssuedAt, testNow.Unix())
	}
}

func TestVerifierDevModeWithoutArtifactTrust(t *testing.T) {
	v := NewVerifier(VerifierConfig{DevMode: true, DevTokens: map[string]string{"tok": "owner"}})
	if id, err := v.Verify("tok"); err != nil || id.Owner != "owner" {
		t.Fatalf("id=%+v err=%v", id, err)
	}
}
