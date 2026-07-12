package token

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"math/big"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
)

func TestSignerRevocationStatementRoundTrip(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	payload := mustRevocationPayload(t, &authv1.SignerRevocationStatement{
		Environment:            "prod",
		Generation:             7,
		IssuedAt:               now.Unix(),
		RevokedSerials:         [][]byte{{0, 0x0a}, {0xff}},
		RevokedSpkiSha256:      [][]byte{make([]byte, sha256.Size)},
		MinimumSignerNotBefore: now.Add(-time.Hour).Unix(),
	})
	wire, err := SignSignerRevocationStatement(pki.intermediate, pki.intermediateKey, payload)
	if err != nil {
		t.Fatal(err)
	}
	statement, err := ParseSignerRevocationStatement(wire, pki.root, "prod", now)
	if err != nil {
		t.Fatal(err)
	}
	serials := statement.RevokedSerials()
	if statement.Generation() != 7 || statement.Environment() != "prod" || serials[0] != "a" || serials[1] != "ff" {
		t.Fatalf("unexpected statement: %#v", statement)
	}
	if !statement.IssuedAt().Equal(now) || !statement.MinimumSignerNotBefore().Equal(now.Add(-time.Hour)) {
		t.Fatalf("unexpected statement times: issued=%v minimum=%v", statement.IssuedAt(), statement.MinimumSignerNotBefore())
	}
}

func TestSignerRevocationStatementRejectsUnauthorizedOrMalformedInput(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	valid := &authv1.SignerRevocationStatement{Environment: "prod", Generation: 1, IssuedAt: now.Unix()}

	t.Run("wrong root", func(t *testing.T) {
		wire := mustSignRevocation(t, pki, valid)
		other := newCertTestPKI(t, nil)
		if _, err := ParseSignerRevocationStatement(wire, other.root, "prod", now); err == nil {
			t.Fatal("wrong root accepted")
		}
	})
	t.Run("wrong environment", func(t *testing.T) {
		wire := mustSignRevocation(t, pki, valid)
		if _, err := ParseSignerRevocationStatement(wire, pki.root, "staging", now); err == nil {
			t.Fatal("wrong environment accepted")
		}
	})
	t.Run("active leaf", func(t *testing.T) {
		payload := mustRevocationPayload(t, valid)
		if _, err := SignSignerRevocationStatement(pki.leaf, pki.leafEd25519Priv, payload); err == nil {
			t.Fatal("active leaf signed offline statement")
		}
	})
	t.Run("node intermediate", func(t *testing.T) {
		node := newCertTestPKI(t, func(o *certTestOptions) { o.useNodeIntermediate = true })
		wire, err := SignSignerRevocationStatement(node.intermediate, node.intermediateKey, mustRevocationPayload(t, valid))
		if err != nil {
			return
		}
		if _, err := ParseSignerRevocationStatement(wire, node.root, "prod", now); err == nil {
			t.Fatal("node intermediate authorized statement")
		}
	})
	t.Run("intermediate missing digital signature usage", func(t *testing.T) {
		candidate := newCertTestPKI(t, func(o *certTestOptions) {
			o.intermediateUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
		})
		payload := mustRevocationPayload(t, valid)
		if _, err := SignSignerRevocationStatement(candidate.intermediate, candidate.intermediateKey, payload); err == nil {
			t.Fatal("intermediate without digital-signature usage signed statement")
		}
		domain, _ := artifactDomain(ArtifactTypeSignerRevocation)
		digest := sha256.Sum256(appendDomainPayload(domain, payload))
		signature, err := ecdsa.SignASN1(rand.Reader, candidate.intermediateKey, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		spki, err := x509.MarshalPKIXPublicKey(candidate.intermediate.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		keyID := sha256.Sum256(spki)
		envelope := &authv1.SignedAuthArtifact{
			ArtifactType: ArtifactTypeSignerRevocation,
			Payload:      payload, Signature: signature,
			SignerChain: [][]byte{candidate.intermediate.Raw}, KeyId: keyID[:],
		}
		if _, err := ParseSignerRevocationStatement(mustEncodeArtifact(t, envelope), candidate.root, "prod", now); err == nil {
			t.Fatal("intermediate without digital-signature usage verified statement")
		}
	})
	t.Run("future issued at", func(t *testing.T) {
		candidate := proto.Clone(valid).(*authv1.SignerRevocationStatement)
		candidate.IssuedAt = now.Add(61 * time.Second).Unix()
		if _, err := ParseSignerRevocationStatement(mustSignRevocation(t, pki, candidate), pki.root, "prod", now); err == nil {
			t.Fatal("far-future statement accepted")
		}
	})

	for _, tc := range []struct {
		name   string
		mutate func(*authv1.SignerRevocationStatement)
	}{
		{name: "zero generation", mutate: func(s *authv1.SignerRevocationStatement) { s.Generation = 0 }},
		{name: "missing environment", mutate: func(s *authv1.SignerRevocationStatement) { s.Environment = "" }},
		{name: "missing issued at", mutate: func(s *authv1.SignerRevocationStatement) { s.IssuedAt = 0 }},
		{name: "zero serial", mutate: func(s *authv1.SignerRevocationStatement) { s.RevokedSerials = [][]byte{{0}} }},
		{name: "duplicate normalized serial", mutate: func(s *authv1.SignerRevocationStatement) { s.RevokedSerials = [][]byte{{1}, {0, 1}} }},
		{name: "short spki", mutate: func(s *authv1.SignerRevocationStatement) { s.RevokedSpkiSha256 = [][]byte{{1}} }},
		{name: "duplicate spki", mutate: func(s *authv1.SignerRevocationStatement) {
			h := make([]byte, sha256.Size)
			s.RevokedSpkiSha256 = [][]byte{h, h}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := proto.Clone(valid).(*authv1.SignerRevocationStatement)
			tc.mutate(candidate)
			if _, err := ParseSignerRevocationStatement(mustSignRevocation(t, pki, candidate), pki.root, "prod", now); err == nil {
				t.Fatal("invalid payload accepted")
			}
		})
	}
}

func TestSignerRevocationStatementRejectsEnvelopeSubstitution(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	wire := mustSignRevocation(t, pki, &authv1.SignerRevocationStatement{Environment: "prod", Generation: 1, IssuedAt: now.Unix()})
	for _, tc := range []struct {
		name   string
		mutate func(*authv1.SignedAuthArtifact)
	}{
		{name: "wrong domain", mutate: func(a *authv1.SignedAuthArtifact) { a.ArtifactType = ArtifactTypeSession }},
		{name: "payload", mutate: func(a *authv1.SignedAuthArtifact) { a.Payload[len(a.Payload)-1] ^= 1 }},
		{name: "malformed chain", mutate: func(a *authv1.SignedAuthArtifact) { a.SignerChain[0] = []byte("not DER") }},
		{name: "duplicate chain", mutate: func(a *authv1.SignedAuthArtifact) { a.SignerChain = append(a.SignerChain, a.SignerChain[0]) }},
		{name: "reordered leaf chain", mutate: func(a *authv1.SignedAuthArtifact) { a.SignerChain = [][]byte{pki.leaf.Raw, pki.intermediate.Raw} }},
		{name: "key id", mutate: func(a *authv1.SignedAuthArtifact) { a.KeyId[0] ^= 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelope := mustDecodeArtifact(t, wire)
			tc.mutate(envelope)
			if _, err := ParseSignerRevocationStatement(mustEncodeArtifact(t, envelope), pki.root, "prod", now); err == nil {
				t.Fatal("substituted envelope accepted")
			}
		})
	}
}

func TestSignerRevocationStatementRejectsExpiredIntermediate(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, func(o *certTestOptions) { o.intermediateLifetime = -time.Hour })
	payload := mustRevocationPayload(t, &authv1.SignerRevocationStatement{Environment: "prod", Generation: 1, IssuedAt: now.Unix()})
	wire, err := SignSignerRevocationStatement(pki.intermediate, pki.intermediateKey, payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSignerRevocationStatement(wire, pki.root, "prod", now); err == nil {
		t.Fatal("expired intermediate accepted")
	}
}

func TestSignerRevocationStatementAuthenticatesPayloadBeforeParsing(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	wire := mustSignRevocation(t, pki, &authv1.SignerRevocationStatement{Environment: "prod", Generation: 1, IssuedAt: now.Unix()})
	envelope := mustDecodeArtifact(t, wire)
	envelope.Payload = []byte{0xff}
	domain, _ := artifactDomain(ArtifactTypeSignerRevocation)
	digest := sha256.Sum256(appendDomainPayload(domain, envelope.Payload))
	signature, err := ecdsa.SignASN1(rand.Reader, pki.intermediateKey, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	envelope.Signature = signature
	if _, err := ParseSignerRevocationStatement(mustEncodeArtifact(t, envelope), pki.root, "prod", now); !errors.Is(err, ErrMalformed) {
		t.Fatalf("authenticated malformed payload = %v", err)
	}
}

func TestSignerRevocationStateMonotonicAndRejectsSigners(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	spki, err := x509.MarshalPKIXPublicKey(pki.leaf.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	spkiHash := sha256.Sum256(spki)
	state, err := NewSignerRevocationState(pki.root, "prod")
	if err != nil {
		t.Fatal(err)
	}
	first := mustParseRevocation(t, pki, &authv1.SignerRevocationStatement{
		Environment: "prod", Generation: 1, IssuedAt: now.Unix(), RevokedSerials: [][]byte{pki.leaf.SerialNumber.Bytes()},
	})
	if err := state.Apply(first); err != nil {
		t.Fatal(err)
	}
	if err := state.Apply(first); err != nil {
		t.Fatalf("identical statement was not idempotent: %v", err)
	}
	if state.Generation() != 1 || !errors.Is(state.RejectSigner(pki.leaf), ErrSignerRevoked) {
		t.Fatal("serial revocation not applied")
	}

	rollback := mustParseRevocation(t, pki, &authv1.SignerRevocationStatement{Environment: "prod", Generation: 1, IssuedAt: now.Add(-time.Second).Unix()})
	if err := state.Apply(rollback); !errors.Is(err, ErrRevocationEquivocation) {
		t.Fatalf("same-generation conflict = %v", err)
	}
	higher := mustParseRevocation(t, pki, &authv1.SignerRevocationStatement{
		Environment: "prod", Generation: 2, IssuedAt: now.Unix(), RevokedSpkiSha256: [][]byte{spkiHash[:]},
	})
	if err := state.Apply(higher); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(state.Apply(first), ErrRevocationRollback) {
		t.Fatal("rollback accepted")
	}
	if state.Generation() != 2 || !errors.Is(state.RejectSigner(pki.leaf), ErrSignerRevoked) {
		t.Fatal("SPKI revocation not applied")
	}

	other, _ := newCertTestLeaf(t, pki, 44, "unrelated")
	if err := state.RejectSigner(other); err != nil {
		t.Fatalf("unrelated signer rejected: %v", err)
	}
	minimum := mustParseRevocation(t, pki, &authv1.SignerRevocationStatement{
		Environment: "prod", Generation: 3, IssuedAt: now.Unix(), MinimumSignerNotBefore: now.Unix(),
	})
	if err := state.Apply(minimum); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(state.RejectSigner(other), ErrSignerRevoked) {
		t.Fatal("signer older than minimum NotBefore accepted")
	}
}

func TestArtifactCacheInvalidatedBySignerRevocation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	state, _ := NewSignerRevocationState(pki.root, "prod")
	verifier, _ := NewVerifier(pki.root, "prod", state)
	credential, _ := NewSigningCredential(pki.leafEd25519Priv, pki.chain, pki.root, "prod", now)
	wire, _ := credential.Sign(ArtifactTypeSession, []byte("session-token-id-is-not-signer-state"))
	if _, err := verifier.Verify(wire, ArtifactTypeSession, now); err != nil {
		t.Fatal(err)
	}
	spki, _ := x509.MarshalPKIXPublicKey(pki.leaf.PublicKey)
	hash := sha256.Sum256(spki)
	statement := mustParseRevocation(t, pki, &authv1.SignerRevocationStatement{
		Environment: "prod", Generation: 1, IssuedAt: now.Unix(), RevokedSpkiSha256: [][]byte{hash[:]},
	})
	if err := state.Apply(statement); err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(wire, ArtifactTypeSession, now); !errors.Is(err, ErrSignerRevoked) {
		t.Fatalf("cached signer survived revocation: %v", err)
	}
}

func mustRevocationPayload(t *testing.T, statement *authv1.SignerRevocationStatement) []byte {
	t.Helper()
	payload, err := proto.Marshal(statement)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func mustSignRevocation(t *testing.T, pki certTestPKI, statement *authv1.SignerRevocationStatement) string {
	t.Helper()
	wire, err := SignSignerRevocationStatement(pki.intermediate, pki.intermediateKey, mustRevocationPayload(t, statement))
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func mustParseRevocation(t *testing.T, pki certTestPKI, statement *authv1.SignerRevocationStatement) *SignerRevocationStatement {
	t.Helper()
	parsed, err := ParseSignerRevocationStatement(mustSignRevocation(t, pki, statement), pki.root, "prod", time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestSignerRevocationStateRejectsInvalidEnvironmentAndNil(t *testing.T) {
	pki := newCertTestPKI(t, nil)
	if _, err := NewSignerRevocationState(pki.root, ""); err == nil {
		t.Fatal("empty environment accepted")
	}
	if _, err := NewSignerRevocationState(nil, "prod"); err == nil {
		t.Fatal("nil root accepted")
	}
	state, _ := NewSignerRevocationState(pki.root, "prod")
	if err := state.Apply(nil); err == nil {
		t.Fatal("nil statement accepted")
	}
	if err := state.RejectSigner(&x509.Certificate{SerialNumber: big.NewInt(1)}); err != nil {
		t.Fatalf("empty state rejected signer: %v", err)
	}
}

func TestSignerRevocationStatementAccessorsReturnDefensiveCopies(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	spki, err := x509.MarshalPKIXPublicKey(pki.leaf.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(spki)
	statement := mustParseRevocation(t, pki, &authv1.SignerRevocationStatement{
		Environment: "prod", Generation: 1, IssuedAt: now.Unix(),
		RevokedSerials: [][]byte{pki.leaf.SerialNumber.Bytes()}, RevokedSpkiSha256: [][]byte{hash[:]},
	})
	serials := statement.RevokedSerials()
	hashes := statement.RevokedSPKISHA256()
	serials[0] = "1"
	hashes[0][0] ^= 1
	if statement.RevokedSerials()[0] != pki.leaf.SerialNumber.Text(16) || statement.RevokedSPKISHA256()[0] != hash {
		t.Fatal("accessor mutation changed authenticated statement")
	}
	state, _ := NewSignerRevocationState(pki.root, "prod")
	if err := state.Apply(statement); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(state.RejectSigner(pki.leaf), ErrSignerRevoked) {
		t.Fatal("accessor mutation changed applied revocation")
	}
}

func TestSignerRevocationStateRejectsStatementFromDifferentRoot(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	trusted := newCertTestPKI(t, nil)
	other := newCertTestPKI(t, nil)
	statement := mustParseRevocation(t, other, &authv1.SignerRevocationStatement{Environment: "prod", Generation: 1, IssuedAt: now.Unix()})
	state, err := NewSignerRevocationState(trusted.root, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Apply(statement); err == nil {
		t.Fatal("statement verified under a different root was applied")
	}
	if state.Generation() != 0 {
		t.Fatal("wrong-root statement changed state")
	}
}
