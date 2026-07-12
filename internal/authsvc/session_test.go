package authsvc

import (
	"encoding/hex"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/githubfake"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/authsvc/token"
)

func TestMintAccessTokenCertifiedEnvelope(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1_800_000_000, 0)
	pki := newTestArtifactPKI(t, now, "prod")
	signer := pki.signer(t, now, "current")
	idp, _, _ := newTestIdP(t, fake, now, func(cfg *IdPConfig) { cfg.Signer = signer })
	wire, _, err := idp.mintAccess(store.User{AccountID: "acct-1", Handle: "alice"}, []byte("session-spki"), now)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := token.NewVerifier(pki.root, "prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := verifier.Verify(wire, token.ArtifactTypeSession, now)
	if err != nil {
		t.Fatal(err)
	}
	var body authv1.SessionTokenBody
	if err := proto.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	if body.GetAccountId() != "acct-1" || body.GetHandle() != "alice" {
		t.Fatalf("body = %+v", &body)
	}
	if body.GetKeyId() != hex.EncodeToString(signer.KeyID[:]) {
		t.Fatalf("key_id = %q, want full SPKI hash %x", body.GetKeyId(), signer.KeyID)
	}
}

func TestSignerRotationNeedsNoVerifierBundle(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1_800_000_000, 0)
	pki := newTestArtifactPKI(t, now, "prod")
	oldSigner := pki.signer(t, now, "old")
	newSigner := pki.signer(t, now, "new")
	idp, _, _ := newTestIdP(t, fake, now, func(cfg *IdPConfig) { cfg.Signer = oldSigner })
	oldWire, _, err := idp.mintAccess(store.User{AccountID: "acct"}, []byte("spki"), now)
	if err != nil {
		t.Fatal(err)
	}
	idp.signer = newSigner
	newWire, _, err := idp.mintAccess(store.User{AccountID: "acct"}, []byte("spki"), now)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := token.NewVerifier(pki.root, "prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, wire := range []string{oldWire, newWire} {
		if _, err := verifier.Verify(wire, token.ArtifactTypeSession, now); err != nil {
			t.Fatalf("artifact failed after rotation: %v", err)
		}
	}
}
