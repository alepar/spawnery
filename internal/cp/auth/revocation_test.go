package auth

import (
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
)

func signedEntry(t *testing.T, credential *token.SigningCredential, body *authv1.RevocationEntry) SignedFeedEntry {
	t.Helper()
	payload, err := (proto.MarshalOptions{Deterministic: true}).Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := credential.Sign(token.ArtifactTypeRevocation, payload)
	if err != nil {
		t.Fatal(err)
	}
	return SignedFeedEntry{Seq: body.Seq, Sig: wire}
}

func familyEntry(seq int64, account, family, tokenID string, revokedAt, retainUntil int64) *authv1.RevocationEntry {
	return &authv1.RevocationEntry{
		Seq: seq, AccountId: account, FamilyId: family, RevokedAt: revokedAt,
		RevokedTokens: []*authv1.RevokedToken{{TokenId: tokenID, RetainUntil: retainUntil}},
	}
}

func TestRevocationRegistryAppliesRetentionAndExclusiveAccountCutoff(t *testing.T) {
	fixture := newArtifactFixture(t)
	r := NewRevocationRegistry(nil)
	page := []SignedFeedEntry{
		signedEntry(t, fixture.credential, familyEntry(2, "alice", "family", "explicit", testNow.Unix()-1, testNow.Unix()+30)),
		signedEntry(t, fixture.credential, &authv1.RevocationEntry{Seq: 5, AccountId: "bob", RevokedAt: testNow.Unix() - 1, RevokeTokensIssuedBefore: testNow.Unix()}),
	}
	last, err := r.ApplyPage(page, fixture.verifier, testNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	if last != 5 || !r.IsRevoked("explicit", "nobody", testNow.Unix()+10, testNow) || !r.IsRevoked("fresh", "bob", testNow.Unix()-1, testNow) || r.IsRevoked("fresh", "bob", testNow.Unix(), testNow) || r.IsRevoked("fresh", "bob", testNow.Unix()+1, testNow) {
		t.Fatalf("last=%d explicit=%v old=%v equal=%v future=%v", last,
			r.IsRevoked("explicit", "nobody", testNow.Unix()+10, testNow),
			r.IsRevoked("fresh", "bob", testNow.Unix()-1, testNow),
			r.IsRevoked("fresh", "bob", testNow.Unix(), testNow),
			r.IsRevoked("fresh", "bob", testNow.Unix()+1, testNow))
	}
	if r.IsRevoked("explicit", "nobody", 0, testNow.Add(30*time.Second)) {
		t.Fatal("expired explicit token remained revoked")
	}
}

func TestRevocationRegistryAppliesWholePageAtomically(t *testing.T) {
	fixture := newArtifactFixture(t)
	r := NewRevocationRegistry(nil)
	valid := signedEntry(t, fixture.credential, familyEntry(2, "alice", "family", "prefix", testNow.Unix()-1, testNow.Unix()+30))
	invalid := signedEntry(t, fixture.credential, familyEntry(3, "alice", "family", "invalid", testNow.Unix()-1, testNow.Unix()+30))
	invalid.Seq = 4
	if _, err := r.ApplyPage([]SignedFeedEntry{valid, invalid}, fixture.verifier, testNow, 0); err == nil {
		t.Fatal("invalid page accepted")
	}
	if r.IsRevoked("prefix", "alice", 0, testNow) || r.IsRevoked("invalid", "alice", 0, testNow) {
		t.Fatal("invalid page partially applied")
	}
}

func TestRevocationRegistryRejectsWrongArtifactType(t *testing.T) {
	fixture := newArtifactFixture(t)
	payload, err := proto.Marshal(&authv1.SessionTokenBody{Audience: "cp"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := fixture.credential.Sign(token.ArtifactTypeSession, payload)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRevocationRegistry(nil)
	if _, err := r.ApplyPage([]SignedFeedEntry{{Seq: 1, Sig: session}}, fixture.verifier, testNow, 0); err == nil {
		t.Fatal("session artifact accepted as revocation")
	}
}

func TestRevocationRegistryFansOutExplicitTokensOnly(t *testing.T) {
	fixture := newArtifactFixture(t)
	sessions := NewSessionRegistry()
	r := NewRevocationRegistry(sessions)
	var explicitCancelled, siblingCancelled atomic.Int32
	releaseExplicit := sessions.Add("explicit", "alice", func() { explicitCancelled.Add(1) })
	defer releaseExplicit()
	releaseSibling := sessions.Add("sibling", "alice", func() { siblingCancelled.Add(1) })
	defer releaseSibling()
	body := &authv1.RevocationEntry{
		Seq: 1, AccountId: "alice", RevokedAt: testNow.Unix() - 1, RevokeTokensIssuedBefore: testNow.Unix(),
		RevokedTokens: []*authv1.RevokedToken{{TokenId: "explicit", RetainUntil: testNow.Unix() + 30}},
	}
	if _, err := r.ApplyPage([]SignedFeedEntry{signedEntry(t, fixture.credential, body)}, fixture.verifier, testNow, 0); err != nil {
		t.Fatal(err)
	}
	if explicitCancelled.Load() != 1 || siblingCancelled.Load() != 0 {
		t.Fatalf("cancellations explicit=%d sibling=%d", explicitCancelled.Load(), siblingCancelled.Load())
	}
}
