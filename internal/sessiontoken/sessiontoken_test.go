package sessiontoken_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"spawnery/internal/sessiontoken"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	c := sessiontoken.Claims{SpawnID: "s1", Owner: "alice", Node: "n1", Exp: time.Now().Add(time.Hour)}
	tok, err := sessiontoken.Sign(c, priv)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	got, err := sessiontoken.Verify(tok, pub, time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.SpawnID != "s1" || got.Owner != "alice" || got.Node != "n1" || got.Exp.Unix() != c.Exp.Unix() {
		t.Fatalf("claims = %+v", got)
	}
}

// A tampered token fails signature verification.
func TestVerifyRejectsTamper(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tok, _ := sessiontoken.Sign(sessiontoken.Claims{SpawnID: "s1", Owner: "alice", Exp: time.Now().Add(time.Hour)}, priv)
	bad := tok[:len(tok)-2] + "xy"
	if _, err := sessiontoken.Verify(bad, pub, time.Now()); err == nil {
		t.Fatal("tampered token must be rejected")
	}
}

// A token signed by a different key (e.g. a compromised CP forging session authority) is rejected.
func TestVerifyRejectsWrongKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	tok, _ := sessiontoken.Sign(sessiontoken.Claims{SpawnID: "s1", Exp: time.Now().Add(time.Hour)}, priv)
	if _, err := sessiontoken.Verify(tok, otherPub, time.Now()); err == nil {
		t.Fatal("a token not signed by the trusted AS key must be rejected")
	}
}

// An expired token is rejected.
func TestVerifyRejectsExpired(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tok, _ := sessiontoken.Sign(sessiontoken.Claims{SpawnID: "s1", Exp: time.Now().Add(-time.Minute)}, priv)
	if _, err := sessiontoken.Verify(tok, pub, time.Now()); err == nil {
		t.Fatal("expired token must be rejected")
	}
}
