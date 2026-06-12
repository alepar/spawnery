package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	authv1 "spawnery/gen/auth/v1"
)

func testKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func testBody(now time.Time, keyID string) *authv1.SessionTokenBody {
	return &authv1.SessionTokenBody{
		AccountId:      "acct-1",
		Handle:         "alice",
		TokenId:        "tok-1",
		Audience:       "cp",
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(15 * time.Minute).Unix(),
		SessionKeyHash: SessionKeyHash([]byte("fake-spki")),
		KeyId:          keyID,
	}
}

func mustKeySet(t *testing.T, pubs ...ed25519.PublicKey) KeySet {
	t.Helper()
	ks, err := NewKeySet(pubs...)
	if err != nil {
		t.Fatal(err)
	}
	return ks
}

func TestMintVerifyRoundTrip(t *testing.T) {
	priv := testKey(t)
	now := time.Unix(1770000000, 0)
	keyID, err := KeyID(priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := Mint(testBody(now, keyID), priv)
	if err != nil {
		t.Fatal(err)
	}
	ks := mustKeySet(t, priv.Public().(ed25519.PublicKey))
	body, err := Verify(wire, ks, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if body.AccountId != "acct-1" || body.Audience != "cp" || body.KeyId != keyID {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestVerifyTamperedBody(t *testing.T) {
	priv := testKey(t)
	now := time.Unix(1770000000, 0)
	keyID, _ := KeyID(priv.Public().(ed25519.PublicKey))
	wire, _ := Mint(testBody(now, keyID), priv)
	ks := mustKeySet(t, priv.Public().(ed25519.PublicKey))

	bodyB64, sigB64, _ := strings.Cut(wire, ".")
	bodyBytes, _ := base64.RawURLEncoding.DecodeString(bodyB64)

	// Flip a byte in the body (but not in key_id, so key selection still resolves).
	mut := append([]byte(nil), bodyBytes...)
	mut[2] ^= 0xff
	tampered := base64.RawURLEncoding.EncodeToString(mut) + "." + sigB64
	if _, err := Verify(tampered, ks, now); err == nil {
		t.Fatal("tampered body verified")
	}

	// Flip a byte in the signature.
	sig, _ := base64.RawURLEncoding.DecodeString(sigB64)
	sig[0] ^= 0xff
	tampered = bodyB64 + "." + base64.RawURLEncoding.EncodeToString(sig)
	if _, err := Verify(tampered, ks, now); err != ErrSignature {
		t.Fatalf("want ErrSignature, got %v", err)
	}
}

// A signature minted WITHOUT the domain prefix must not verify: the prefix is load-bearing [MC1].
func TestVerifyMissingDomainPrefix(t *testing.T) {
	priv := testKey(t)
	now := time.Unix(1770000000, 0)
	keyID, _ := KeyID(priv.Public().(ed25519.PublicKey))
	wire := SignArtifact("", mustMarshal(t, testBody(now, keyID)), priv)
	ks := mustKeySet(t, priv.Public().(ed25519.PublicKey))
	if _, err := Verify(wire, ks, now); err != ErrSignature {
		t.Fatalf("want ErrSignature, got %v", err)
	}
}

// A revocation-domain signature must not verify as a session token (cross-class) [MC1].
func TestDomainSeparationAcrossClasses(t *testing.T) {
	priv := testKey(t)
	now := time.Unix(1770000000, 0)
	keyID, _ := KeyID(priv.Public().(ed25519.PublicKey))
	wire := SignArtifact(RevocationDomainPrefix, mustMarshal(t, testBody(now, keyID)), priv)
	ks := mustKeySet(t, priv.Public().(ed25519.PublicKey))
	if _, err := Verify(wire, ks, now); err != ErrSignature {
		t.Fatalf("want ErrSignature, got %v", err)
	}
}

func TestVerifyExpired(t *testing.T) {
	priv := testKey(t)
	now := time.Unix(1770000000, 0)
	keyID, _ := KeyID(priv.Public().(ed25519.PublicKey))
	wire, _ := Mint(testBody(now, keyID), priv)
	ks := mustKeySet(t, priv.Public().(ed25519.PublicKey))
	if _, err := Verify(wire, ks, now.Add(15*time.Minute)); err != ErrExpired {
		t.Fatalf("want ErrExpired, got %v", err)
	}
	// future-issued refused beyond skew
	wire2, _ := Mint(testBody(now.Add(10*time.Minute), keyID), priv)
	if _, err := Verify(wire2, ks, now); err != ErrNotYet {
		t.Fatalf("want ErrNotYet, got %v", err)
	}
}

func TestVerifyMalformed(t *testing.T) {
	ks := KeySet{}
	for _, w := range []string{"", "abc", "a.b.c.d", "!!!.???", "Zm9v.!!"} {
		if _, err := Verify(w, ks, time.Now()); err == nil {
			t.Fatalf("malformed %q verified", w)
		}
	}
}

// Rotation overlap [AM4]: keyset {old,new} — tokens signed by each verify; retire old — its
// tokens are refused; unknown key_id refused.
func TestKeyRotationOverlap(t *testing.T) {
	oldPriv := testKey(t)
	newPriv := testKey(t)
	now := time.Unix(1770000000, 0)
	oldID, _ := KeyID(oldPriv.Public().(ed25519.PublicKey))
	newID, _ := KeyID(newPriv.Public().(ed25519.PublicKey))

	oldTok, _ := Mint(testBody(now, oldID), oldPriv)
	newTok, _ := Mint(testBody(now, newID), newPriv)

	overlap := mustKeySet(t, oldPriv.Public().(ed25519.PublicKey), newPriv.Public().(ed25519.PublicKey))
	if _, err := Verify(oldTok, overlap, now); err != nil {
		t.Fatalf("old token in overlap window: %v", err)
	}
	if _, err := Verify(newTok, overlap, now); err != nil {
		t.Fatalf("new token in overlap window: %v", err)
	}

	retired := mustKeySet(t, newPriv.Public().(ed25519.PublicKey))
	if _, err := Verify(oldTok, retired, now); err != ErrUnknownKey {
		t.Fatalf("retired key token: want ErrUnknownKey, got %v", err)
	}

	// Forged key_id selecting the WRONG key still fails: signature runs over received bytes.
	b := testBody(now, newID)
	forged, _ := Mint(b, oldPriv) // claims newID, signed by old key
	if _, err := Verify(forged, overlap, now); err != ErrSignature {
		t.Fatalf("forged key_id: want ErrSignature, got %v", err)
	}
}

func TestSigningKeyPEMRoundTrip(t *testing.T) {
	priv := testKey(t)
	pemBytes, err := MarshalSigningKeyPEM(priv)
	if err != nil {
		t.Fatal(err)
	}
	got, gotID, err := LoadSigningKey(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	wantID, _ := KeyID(priv.Public().(ed25519.PublicKey))
	if gotID != wantID {
		t.Fatalf("key_id mismatch: %s vs %s", gotID, wantID)
	}
	if !got.Equal(priv) {
		t.Fatal("key mismatch after PEM round trip")
	}
}
