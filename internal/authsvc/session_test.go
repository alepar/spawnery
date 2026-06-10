package authsvc_test

import (
	"testing"
	"time"

	"spawnery/internal/sessiontoken"
)

// The AS signs session tokens with its own key; they verify against the AS's published session pubkey
// (and NOT any other key) — so a compromised CP, lacking the AS key, cannot forge session authority.
func TestSessionTokenIssuedAndVerifiable(t *testing.T) {
	s := newAS(t)
	c := sessiontoken.Claims{SpawnID: "sp1", Owner: "alice", Node: "n1", Exp: time.Now().Add(time.Hour)}
	tok, err := s.IssueSessionToken(c)
	if err != nil {
		t.Fatalf("IssueSessionToken: %v", err)
	}
	got, err := sessiontoken.Verify(tok, s.SessionPubKey(), time.Now())
	if err != nil {
		t.Fatalf("Verify against published AS pubkey: %v", err)
	}
	if got.SpawnID != "sp1" || got.Owner != "alice" || got.Node != "n1" {
		t.Fatalf("claims = %+v", got)
	}
}
