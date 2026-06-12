package intent_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"testing"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/intent"
)

func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func testBody(op intent.Op) *authv1.IntentBody {
	return &authv1.IntentBody{
		Jti:          "test-jti-1",
		IssuedAt:     1770000000,
		SpawnId:      "sp-001",
		Generation:   1,
		TargetNodeId: "node-1",
		Op:           string(op),
		AppRef:       "app/ref@sha256:abc",
		Image:        "img@sha256:def",
		Model:        "claude-3",
	}
}

func TestBuildVerifyRoundTrip(t *testing.T) {
	priv := testKey(t)
	body := testBody(intent.OpCreateSpawn)

	si, err := intent.Build(intent.OpCreateSpawn, body, priv)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if si.Domain != intent.DomainCreateSpawn {
		t.Fatalf("domain: got %q want %q", si.Domain, intent.DomainCreateSpawn)
	}
	if len(si.Body) == 0 || len(si.Sig) != 64 || len(si.SpkiDer) == 0 {
		t.Fatalf("missing fields: body=%d sig=%d spki=%d", len(si.Body), len(si.Sig), len(si.SpkiDer))
	}
	// Verify sig over raw bytes.
	if err := intent.VerifySig(si.Domain, si.Body, si.Sig, si.SpkiDer); err != nil {
		t.Fatalf("VerifySig: %v", err)
	}
	// Parse body round-trip.
	got, err := intent.ParseBody(si.Body)
	if err != nil {
		t.Fatalf("ParseBody: %v", err)
	}
	if got.SpawnId != "sp-001" || got.Generation != 1 || got.TargetNodeId != "node-1" {
		t.Fatalf("body fields: %+v", got)
	}
}

// Mutated body must fail verification — WM9: verification is over raw received bytes, never
// a re-serialization.
func TestVerifySigMutatedBodyFails(t *testing.T) {
	priv := testKey(t)
	si, _ := intent.Build(intent.OpCreateSpawn, testBody(intent.OpCreateSpawn), priv)
	mut := append([]byte(nil), si.Body...)
	mut[0] ^= 0xff
	if err := intent.VerifySig(si.Domain, mut, si.Sig, si.SpkiDer); err == nil {
		t.Fatal("mutated body verified — WM9 violation")
	}
}

// Wrong domain must fail verification — domain-separation [MC1].
func TestVerifySigWrongDomainFails(t *testing.T) {
	priv := testKey(t)
	si, _ := intent.Build(intent.OpCreateSpawn, testBody(intent.OpCreateSpawn), priv)
	if err := intent.VerifySig(intent.DomainResumeSpawn, si.Body, si.Sig, si.SpkiDer); err == nil {
		t.Fatal("wrong domain accepted")
	}
}

// SPKI hash match [AM11]: SHA-256(spki) must equal the cnf claim.
func TestSPKIMatchesHash(t *testing.T) {
	priv := testKey(t)
	spki, err := intent.SPKIDER(priv)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(spki)
	if !intent.SPKIMatchesHash(spki, h[:]) {
		t.Fatal("SPKIMatchesHash returned false for correct hash")
	}
	bad := make([]byte, 32)
	if intent.SPKIMatchesHash(spki, bad) {
		t.Fatal("SPKIMatchesHash returned true for wrong hash")
	}
}

// Non-P256 SPKI must be rejected by VerifySig [AM11].
func TestVerifySigRejectsNonP256SPKI(t *testing.T) {
	priv := testKey(t)
	si, _ := intent.Build(intent.OpCreateSpawn, testBody(intent.OpCreateSpawn), priv)

	// Use P384 (different curve) to test SPKI rejection.
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p384spki, _ := x509.MarshalPKIXPublicKey(&p384.PublicKey)
	if err := intent.VerifySig(si.Domain, si.Body, si.Sig, p384spki); err == nil {
		t.Fatal("non-P256 SPKI accepted")
	}
}

// Domain tags must be distinct per op [AC1].
func TestDomainTagsAreDistinct(t *testing.T) {
	ops := []intent.Op{
		intent.OpCreateSpawn, intent.OpResumeSpawn, intent.OpRecreateSpawn,
		intent.OpMigrateSpawn, intent.OpSessionOpen,
	}
	seen := map[string]bool{}
	for _, op := range ops {
		d := intent.DomainFor(op)
		if seen[d] {
			t.Fatalf("duplicate domain %q for op %q", d, op)
		}
		seen[d] = true
	}
}

// Body bytes are stable across proto.Marshal calls for identical messages [WM9].
func TestBodyBytesAreStable(t *testing.T) {
	priv := testKey(t)
	body := testBody(intent.OpCreateSpawn)
	si1, _ := intent.Build(intent.OpCreateSpawn, body, priv)
	b1 := append([]byte(nil), si1.Body...)

	// Second marshal of same body must produce identical bytes.
	b2, err := proto.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("body bytes changed between Build and proto.Marshal: len %d vs %d", len(b1), len(b2))
	}
}

// SessionKeyHash must use DER SPKI (not SEC1/compressed) — cross-language interop [AM11].
func TestSessionKeyHashUsesDERSPKI(t *testing.T) {
	priv := testKey(t)
	spki, _ := intent.SPKIDER(priv)

	// DER SPKI starts with 0x30 (SEQUENCE tag).
	if len(spki) == 0 || spki[0] != 0x30 {
		t.Fatalf("SPKI DER does not start with SEQUENCE: %02x", spki[0])
	}

	h := intent.SessionKeyHash(spki)
	if len(h) != 32 {
		t.Fatalf("SessionKeyHash len: got %d want 32", len(h))
	}
	exp := sha256.Sum256(spki)
	if string(h) != string(exp[:]) {
		t.Fatal("SessionKeyHash != SHA-256(DER SPKI)")
	}
}
