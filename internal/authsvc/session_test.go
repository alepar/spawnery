package authsvc

import (
	"encoding/base64"
	"encoding/hex"
	"sync"
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
	idp, _, _ := newTestIdP(t, fake, now, func(cfg *IdPConfig) {
		cfg.Signer = oldSigner
		cfg.NextSigner = newSigner
	})
	oldWire, _, err := idp.mintAccess(store.User{AccountID: "acct"}, []byte("spki"), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := idp.ActivateNextSigner(); err != nil {
		t.Fatal(err)
	}
	if err := idp.ActivateNextSigner(); err == nil {
		t.Fatal("second next-signer activation succeeded")
	}
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

func TestActivateNextSignerWithoutNextFails(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1_800_000_000, 0)
	idp, _, _ := newTestIdP(t, fake, now)
	if err := idp.ActivateNextSigner(); err == nil {
		t.Fatal("activation without a next signer succeeded")
	}
}

func TestConcurrentActivateNextSignerSucceedsOnce(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1_800_000_000, 0)
	pki := newTestArtifactPKI(t, now, "prod")
	idp, _, _ := newTestIdP(t, fake, now, func(cfg *IdPConfig) {
		cfg.NextSigner = pki.signer(t, now, "next")
	})
	const contenders = 16
	results := make(chan error, contenders)
	var wg sync.WaitGroup
	for contender := 0; contender < contenders; contender++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- idp.ActivateNextSigner()
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful activations = %d, want 1", successes)
	}
}

func TestConcurrentSignerActivationAndIssuance(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1_800_000_000, 0)
	pki := newTestArtifactPKI(t, now, "prod")
	current := pki.signer(t, now, "current")
	next := pki.signer(t, now, "next")
	idp, _, _ := newTestIdP(t, fake, now, func(cfg *IdPConfig) {
		cfg.Signer = current
		cfg.NextSigner = next
	})
	verifier, err := token.NewVerifier(pki.root, "prod", nil)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 8
	const iterations = 25
	sessions := make(chan string, workers*iterations+1)
	revocations := make(chan string, workers*iterations+1)
	errs := make(chan error, workers*iterations*2+3)
	start := make(chan struct{})
	issued := make(chan struct{}, 1)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for iteration := 0; iteration < iterations; iteration++ {
				wire, _, err := idp.mintAccess(store.User{AccountID: "acct"}, []byte("spki"), now)
				if err != nil {
					errs <- err
					continue
				}
				sessions <- wire
				select {
				case issued <- struct{}{}:
				default:
				}
				entry, err := idp.signRevocationEntry(store.RevocationEvent{Seq: int64(worker*iterations + iteration + 1), AccountID: "acct"})
				if err != nil {
					errs <- err
					continue
				}
				revocations <- entry.Sig
			}
		}(worker)
	}
	close(start)
	<-issued
	if err := idp.ActivateNextSigner(); err != nil {
		errs <- err
	}
	postRotationSession, _, err := idp.mintAccess(store.User{AccountID: "acct"}, []byte("spki"), now)
	if err != nil {
		errs <- err
	} else {
		sessions <- postRotationSession
	}
	postRotationRevocation, err := idp.signRevocationEntry(store.RevocationEvent{Seq: workers*iterations + 1, AccountID: "acct"})
	if err != nil {
		errs <- err
	} else {
		revocations <- postRotationRevocation.Sig
	}
	wg.Wait()
	close(sessions)
	close(revocations)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seenSessionSigner := make(map[string]bool)
	for wire := range sessions {
		payload, err := verifier.Verify(wire, token.ArtifactTypeSession, now)
		if err != nil {
			t.Fatal(err)
		}
		var body authv1.SessionTokenBody
		if err := proto.Unmarshal(payload, &body); err != nil {
			t.Fatal(err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(wire)
		if err != nil {
			t.Fatal(err)
		}
		var envelope authv1.SignedAuthArtifact
		if err := proto.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		if body.GetKeyId() != hex.EncodeToString(envelope.GetKeyId()) {
			t.Fatalf("payload key_id %q does not match envelope key_id %x", body.GetKeyId(), envelope.GetKeyId())
		}
		seenSessionSigner[body.GetKeyId()] = true
	}
	for name, signer := range map[string]*token.SigningCredential{"current": current, "next": next} {
		if !seenSessionSigner[hex.EncodeToString(signer.KeyID[:])] {
			t.Fatalf("no session envelope issued by %s signer", name)
		}
	}
	seenRevocationSigner := make(map[string]bool)
	for wire := range revocations {
		if _, err := verifier.Verify(wire, token.ArtifactTypeRevocation, now); err != nil {
			t.Fatal(err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(wire)
		if err != nil {
			t.Fatal(err)
		}
		var envelope authv1.SignedAuthArtifact
		if err := proto.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		seenRevocationSigner[hex.EncodeToString(envelope.GetKeyId())] = true
	}
	if !seenRevocationSigner[hex.EncodeToString(next.KeyID[:])] {
		t.Fatal("no revocation envelope issued by next signer")
	}
}
