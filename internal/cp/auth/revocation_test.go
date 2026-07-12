package auth

import (
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
)

func signedEntry(t *testing.T, credential *token.SigningCredential, seq int64, accountID string, tokenIDs []string) SignedFeedEntry {
	t.Helper()
	tidJSON, _ := json.Marshal(tokenIDs)
	body, err := json.Marshal(feedEntry{Seq: seq, AccountID: accountID, FamilyID: "fam", TokenIDs: string(tidJSON), RevokedAt: testNow.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := credential.Sign(token.ArtifactTypeRevocation, body)
	if err != nil {
		t.Fatal(err)
	}
	return SignedFeedEntry{Seq: seq, AccountID: accountID, TokenIDs: string(tidJSON), RevokedAt: testNow.Unix(), Sig: wire}
}

func TestRevocationRegistryAppliesVerifiedPayloadNotUnsignedCopies(t *testing.T) {
	fixture := newArtifactFixture(t)
	r := NewRevocationRegistry(nil)
	entry := signedEntry(t, fixture.credential, 1, "acct", []string{"tok"})
	entry.AccountID, entry.TokenIDs = "attacker", `["attacker-token"]`
	if err := r.Apply(entry, fixture.verifier, testNow); err != nil {
		t.Fatal(err)
	}
	if !r.IsRevoked("tok", "acct") || r.IsRevoked("attacker-token", "attacker") {
		t.Fatal("registry acted on unsigned feed copies")
	}
}

func TestRevocationRegistryAcceptsOnlyOrdinaryRevocationArtifacts(t *testing.T) {
	fixture := newArtifactFixture(t)
	payload, _ := proto.Marshal(&authv1.SessionTokenBody{Audience: "cp"})
	session, _ := fixture.credential.Sign(token.ArtifactTypeSession, payload)
	r := NewRevocationRegistry(nil)
	if err := r.Apply(SignedFeedEntry{Sig: session}, fixture.verifier, testNow); err == nil {
		t.Fatal("accepted session artifact")
	}
	if r.IsRevoked("", "") {
		t.Fatal("unexpected mutation")
	}
}

func TestRevocationRegistryRejectsUnsignedSequenceSubstitution(t *testing.T) {
	fixture := newArtifactFixture(t)
	r := NewRevocationRegistry(nil)
	entry := signedEntry(t, fixture.credential, 1, "acct", []string{"tok"})
	entry.Seq = 1000
	if err := r.Apply(entry, fixture.verifier, testNow); err == nil {
		t.Fatal("accepted unsigned sequence substitution")
	}
	if r.IsRevoked("tok", "acct") {
		t.Fatal("substituted entry mutated registry")
	}
}

func TestRevocationRegistryFansOutToSessions(t *testing.T) {
	fixture := newArtifactFixture(t)
	sessions := NewSessionRegistry()
	r := NewRevocationRegistry(sessions)
	var cancelled atomic.Int32
	release := sessions.Add("tok", "acct", func() { cancelled.Add(1) })
	defer release()
	if err := r.Apply(signedEntry(t, fixture.credential, 1, "acct", []string{"tok"}), fixture.verifier, testNow); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for cancelled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if cancelled.Load() == 0 {
		t.Fatal("session was not cancelled")
	}
}
