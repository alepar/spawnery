package token

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
)

func TestSignedAuthArtifactProtoRoundTrip(t *testing.T) {
	want := &authv1.SignedAuthArtifact{
		ArtifactType: "session-token",
		Payload:      []byte{0x00, 0x01, 0xfe, 0xff},
		Signature:    bytes.Repeat([]byte{0xa5}, 64),
		SignerChain: [][]byte{
			{0x30, 0x03, 0x01},
			{0x30, 0x03, 0x02},
		},
		KeyId: bytes.Repeat([]byte{0x5a}, 32),
	}

	encoded, err := proto.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	wire := base64.RawURLEncoding.EncodeToString(encoded)
	raw, err := base64.RawURLEncoding.DecodeString(wire)
	if err != nil {
		t.Fatal(err)
	}
	var got authv1.SignedAuthArtifact
	if err := proto.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if got.ArtifactType != want.ArtifactType ||
		!bytes.Equal(got.Payload, want.Payload) ||
		!bytes.Equal(got.Signature, want.Signature) ||
		!bytes.Equal(got.KeyId, want.KeyId) ||
		len(got.SignerChain) != len(want.SignerChain) {
		t.Fatalf("round trip mismatch: got %+v want %+v", &got, want)
	}
	for i := range want.SignerChain {
		if !bytes.Equal(got.SignerChain[i], want.SignerChain[i]) {
			t.Fatalf("chain[%d] mismatch: got %x want %x", i, got.SignerChain[i], want.SignerChain[i])
		}
	}
}

func TestNewSigningCredential(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	valid := newCertTestPKI(t, nil)
	credential, err := NewSigningCredential(valid.leafEd25519Priv, valid.chain, valid.root, "prod", now)
	if err != nil {
		t.Fatalf("valid credential: %v", err)
	}
	wantSPKI, err := x509.MarshalPKIXPublicKey(valid.leaf.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	wantKeyID := sha256.Sum256(wantSPKI)
	if credential.KeyID != wantKeyID {
		t.Fatalf("key ID mismatch: got %x want %x", credential.KeyID, wantKeyID)
	}

	_, wrongPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherRoot := newCertTestPKI(t, nil).root
	graftedPriv := append(ed25519.PrivateKey(nil), wrongPriv[:ed25519.SeedSize]...)
	graftedPriv = append(graftedPriv, valid.leaf.PublicKey.(ed25519.PublicKey)...)
	tests := []struct {
		name  string
		priv  ed25519.PrivateKey
		chain []*x509.Certificate
		root  *x509.Certificate
		env   string
	}{
		{name: "mismatched private key", priv: wrongPriv, chain: valid.chain, root: valid.root, env: "prod"},
		{name: "grafted private key", priv: graftedPriv, chain: valid.chain, root: valid.root, env: "prod"},
		{name: "empty chain", priv: valid.leafEd25519Priv, root: valid.root, env: "prod"},
		{name: "reversed chain", priv: valid.leafEd25519Priv, chain: []*x509.Certificate{valid.intermediate, valid.leaf}, root: valid.root, env: "prod"},
		{name: "duplicate chain", priv: valid.leafEd25519Priv, chain: []*x509.Certificate{valid.leaf, valid.intermediate, valid.intermediate}, root: valid.root, env: "prod"},
		{name: "root included", priv: valid.leafEd25519Priv, chain: []*x509.Certificate{valid.leaf, valid.intermediate, valid.root}, root: valid.root, env: "prod"},
		{name: "wrong root", priv: valid.leafEd25519Priv, chain: valid.chain, root: otherRoot, env: "prod"},
		{name: "wrong environment", priv: valid.leafEd25519Priv, chain: valid.chain, root: valid.root, env: "staging"},
		{name: "intermediate used as leaf", priv: valid.leafEd25519Priv, chain: []*x509.Certificate{valid.intermediate}, root: valid.root, env: "prod"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewSigningCredential(tc.priv, tc.chain, tc.root, tc.env, now); err == nil {
				t.Fatal("invalid credential accepted")
			}
		})
	}
}

func TestSignerProfileRejectsInvalidCertificates(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tests := []struct {
		name   string
		mutate func(*certTestOptions)
	}{
		{name: "no URI SAN", mutate: func(o *certTestOptions) { o.leafURIs = nil }},
		{name: "multiple URI SANs", mutate: func(o *certTestOptions) {
			o.leafURIs = append(o.leafURIs, "spiffe://prod.spawnery.internal/signer/auth-artifact/signer-2")
		}},
		{name: "wrong URI scheme", mutate: func(o *certTestOptions) {
			o.leafURIs = []string{"https://prod.spawnery.internal/signer/auth-artifact/signer-1"}
		}},
		{name: "wrong URI path", mutate: func(o *certTestOptions) {
			o.leafURIs = []string{"spiffe://prod.spawnery.internal/node/auth-artifact/signer-1"}
		}},
		{name: "empty signer ID", mutate: func(o *certTestOptions) {
			o.leafURIs = []string{"spiffe://prod.spawnery.internal/signer/auth-artifact/"}
		}},
		{name: "wrong leaf policy", mutate: func(o *certTestOptions) { o.leafPolicies = []x509.OID{mustOID(t, "1.2.3")} }},
		{name: "wrong intermediate policy", mutate: func(o *certTestOptions) { o.intermediatePolicies = []x509.OID{mustOID(t, "1.2.3")} }},
		{name: "extra key usage", mutate: func(o *certTestOptions) { o.leafUsage |= x509.KeyUsageKeyEncipherment }},
		{name: "TLS EKU", mutate: func(o *certTestOptions) { o.leafExtUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth} }},
		{name: "ECDSA leaf", mutate: func(o *certTestOptions) { o.leafECDSA = true }},
		{name: "expired leaf", mutate: func(o *certTestOptions) { o.leafExpired = true }},
		{name: "node intermediate lookalike", mutate: func(o *certTestOptions) { o.useNodeIntermediate = true }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pki := newCertTestPKI(t, tc.mutate)
			if _, err := NewSigningCredential(pki.leafEd25519Priv, pki.chain, pki.root, "prod", now); err == nil {
				t.Fatal("invalid signer profile accepted")
			}
		})
	}
}

func TestSigningCredentialSignUsesExactPayloadAndDomain(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	credential, err := NewSigningCredential(pki.leafEd25519Priv, pki.chain, pki.root, "prod", now)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte{0x00, 0xff, 0x01, 0x80, 0x00}

	for _, artifactType := range []string{ArtifactTypeSession, ArtifactTypeRevocation} {
		t.Run(artifactType, func(t *testing.T) {
			wire, err := credential.Sign(artifactType, payload)
			if err != nil {
				t.Fatal(err)
			}
			envelope := mustDecodeArtifact(t, wire)
			domain, err := artifactDomain(artifactType)
			if err != nil {
				t.Fatal(err)
			}
			message := append([]byte(domain), payload...)
			if !ed25519.Verify(pki.leaf.PublicKey.(ed25519.PublicKey), message, envelope.Signature) {
				t.Fatal("signature does not cover domain and exact payload bytes")
			}
			if !bytes.Equal(envelope.Payload, payload) || envelope.ArtifactType != artifactType {
				t.Fatalf("envelope changed signed data: %+v", envelope)
			}
		})
	}
	if _, err := credential.Sign("caller-selected-domain", payload); err == nil {
		t.Fatal("unknown artifact type accepted")
	}
	if _, err := credential.Sign(ArtifactTypeSession, bytes.Repeat([]byte{1}, maxArtifactPayloadSize+1)); err == nil {
		t.Fatal("oversized payload accepted")
	}
}

func TestOnlineSignerRejectsSignerRevocationArtifacts(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	credential, err := NewSigningCredential(pki.leafEd25519Priv, pki.chain, pki.root, "prod", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := credential.Sign(ArtifactTypeSignerRevocation, []byte("offline statement")); err == nil {
		t.Fatal("online signing leaf signed a signer-revocation artifact")
	}

	domain, err := artifactDomain(ArtifactTypeSignerRevocation)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("offline statement")
	message := append([]byte(domain), payload...)
	envelope := &authv1.SignedAuthArtifact{
		ArtifactType: ArtifactTypeSignerRevocation,
		Payload:      payload,
		Signature:    ed25519.Sign(pki.leafEd25519Priv, message),
		SignerChain:  [][]byte{pki.leaf.Raw, pki.intermediate.Raw},
		KeyId:        credential.KeyID[:],
	}
	verifier, err := NewVerifier(pki.root, "prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(mustEncodeArtifact(t, envelope), ArtifactTypeSignerRevocation, now); err == nil {
		t.Fatal("online signing leaf verified a signer-revocation artifact")
	}
}

func TestVerifierVerifySignedArtifact(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	credential, err := NewSigningCredential(pki.leafEd25519Priv, pki.chain, pki.root, "prod", now)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(pki.root, "prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, artifactType := range []string{ArtifactTypeSession, ArtifactTypeRevocation} {
		payload := []byte("exact payload for " + artifactType)
		wire, err := credential.Sign(artifactType, payload)
		if err != nil {
			t.Fatal(err)
		}
		got, err := verifier.Verify(wire, artifactType, now)
		if err != nil {
			t.Fatalf("verify %s: %v", artifactType, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("payload mismatch: got %x want %x", got, payload)
		}
	}
}

func TestVerifierRejectsArtifactSubstitution(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	credential, _ := NewSigningCredential(pki.leafEd25519Priv, pki.chain, pki.root, "prod", now)
	verifier, _ := NewVerifier(pki.root, "prod", nil)
	wire, _ := credential.Sign(ArtifactTypeSession, []byte("payload"))
	original := mustDecodeArtifact(t, wire)
	other := newCertTestPKI(t, nil)

	tests := []struct {
		name     string
		expected string
		mutate   func(*authv1.SignedAuthArtifact)
	}{
		{name: "expected type", expected: ArtifactTypeRevocation},
		{name: "artifact type", expected: ArtifactTypeRevocation, mutate: func(a *authv1.SignedAuthArtifact) { a.ArtifactType = ArtifactTypeRevocation }},
		{name: "payload", mutate: func(a *authv1.SignedAuthArtifact) { a.Payload[0] ^= 1 }},
		{name: "signature", mutate: func(a *authv1.SignedAuthArtifact) { a.Signature[0] ^= 1 }},
		{name: "leaf", mutate: func(a *authv1.SignedAuthArtifact) { a.SignerChain[0] = other.leaf.Raw }},
		{name: "intermediate", mutate: func(a *authv1.SignedAuthArtifact) { a.SignerChain[1] = other.intermediate.Raw }},
		{name: "key ID", mutate: func(a *authv1.SignedAuthArtifact) { a.KeyId[0] ^= 1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := proto.Clone(original).(*authv1.SignedAuthArtifact)
			if tc.mutate != nil {
				tc.mutate(candidate)
			}
			expected := tc.expected
			if expected == "" {
				expected = ArtifactTypeSession
			}
			if _, err := verifier.Verify(mustEncodeArtifact(t, candidate), expected, now); err == nil {
				t.Fatal("substituted artifact verified")
			}
		})
	}
}

func TestVerifierRejectsMalformedArtifacts(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	credential, _ := NewSigningCredential(pki.leafEd25519Priv, pki.chain, pki.root, "prod", now)
	verifier, _ := NewVerifier(pki.root, "prod", nil)
	wire, _ := credential.Sign(ArtifactTypeSession, []byte("payload"))
	original := mustDecodeArtifact(t, wire)

	mutations := []struct {
		name   string
		mutate func(*authv1.SignedAuthArtifact)
	}{
		{name: "missing type", mutate: func(a *authv1.SignedAuthArtifact) { a.ArtifactType = "" }},
		{name: "missing payload", mutate: func(a *authv1.SignedAuthArtifact) { a.Payload = nil }},
		{name: "missing signature", mutate: func(a *authv1.SignedAuthArtifact) { a.Signature = nil }},
		{name: "missing chain", mutate: func(a *authv1.SignedAuthArtifact) { a.SignerChain = nil }},
		{name: "missing key ID", mutate: func(a *authv1.SignedAuthArtifact) { a.KeyId = nil }},
		{name: "root included", mutate: func(a *authv1.SignedAuthArtifact) { a.SignerChain = append(a.SignerChain, pki.root.Raw) }},
		{name: "too many certificates", mutate: func(a *authv1.SignedAuthArtifact) {
			a.SignerChain = append(a.SignerChain, a.SignerChain[1], a.SignerChain[1], a.SignerChain[1])
		}},
		{name: "duplicate certificate", mutate: func(a *authv1.SignedAuthArtifact) { a.SignerChain = append(a.SignerChain, a.SignerChain[1]) }},
		{name: "short signature", mutate: func(a *authv1.SignedAuthArtifact) { a.Signature = a.Signature[:63] }},
		{name: "oversized payload", mutate: func(a *authv1.SignedAuthArtifact) { a.Payload = bytes.Repeat([]byte{1}, maxArtifactPayloadSize+1) }},
		{name: "oversized certificate", mutate: func(a *authv1.SignedAuthArtifact) {
			a.SignerChain[0] = bytes.Repeat([]byte{1}, maxArtifactCertificateSize+1)
		}},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			candidate := proto.Clone(original).(*authv1.SignedAuthArtifact)
			tc.mutate(candidate)
			if _, err := verifier.Verify(mustEncodeArtifact(t, candidate), ArtifactTypeSession, now); err == nil {
				t.Fatal("malformed artifact verified")
			}
		})
	}

	unknownFieldRaw, err := proto.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	unknownFieldRaw = append(unknownFieldRaw, 0x9a, 0x06, 0x00)
	legacyPayload := []byte("legacy")
	legacyMessage := append([]byte(DomainPrefix), legacyPayload...)
	legacy := base64.RawURLEncoding.EncodeToString(legacyPayload) + "." +
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(pki.leafEd25519Priv, legacyMessage))
	for _, candidate := range []string{
		"",
		"!!!",
		base64.RawURLEncoding.EncodeToString([]byte{0xff}),
		base64.RawURLEncoding.EncodeToString(unknownFieldRaw),
		legacy,
		strings.Repeat("A", maxEncodedArtifactSize+1),
	} {
		if _, err := verifier.Verify(candidate, ArtifactTypeSession, now); !errors.Is(err, ErrMalformed) {
			t.Fatalf("malformed %q: want ErrMalformed, got %v", candidate[:min(len(candidate), 32)], err)
		}
	}
}

func mustDecodeArtifact(t *testing.T, wire string) *authv1.SignedAuthArtifact {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(wire)
	if err != nil {
		t.Fatal(err)
	}
	var artifact authv1.SignedAuthArtifact
	if err := proto.Unmarshal(raw, &artifact); err != nil {
		t.Fatal(err)
	}
	return &artifact
}

func mustEncodeArtifact(t *testing.T, artifact *authv1.SignedAuthArtifact) string {
	t.Helper()
	raw, err := proto.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

type fakeSignerRevocations struct {
	mu         sync.Mutex
	generation uint64
	rejections int
	err        error
}

func (f *fakeSignerRevocations) Generation() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.generation
}

func (f *fakeSignerRevocations) RejectSigner(*x509.Certificate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejections++
	return f.err
}

func (f *fakeSignerRevocations) setGeneration(generation uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.generation = generation
}

func (f *fakeSignerRevocations) rejectionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rejections
}

func TestVerifierChainValidationCache(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	credential, _ := NewSigningCredential(pki.leafEd25519Priv, pki.chain, pki.root, "prod", now)
	wire, _ := credential.Sign(ArtifactTypeSession, []byte("payload"))
	revocations := &fakeSignerRevocations{generation: 7}
	verifier, _ := NewVerifier(pki.root, "prod", revocations)
	validations := 0
	realValidate := verifier.validateChain
	verifier.validateChain = func(chain []*x509.Certificate, root *x509.Certificate, environment string, at time.Time) (*validatedSigner, error) {
		validations++
		return realValidate(chain, root, environment, at)
	}

	if _, err := verifier.Verify(wire, ArtifactTypeSession, now); err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(wire, ArtifactTypeSession, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if validations != 1 || revocations.rejectionCount() != 2 {
		t.Fatalf("cache hit counts: validations=%d revocations=%d", validations, revocations.rejectionCount())
	}

	tampered := mustDecodeArtifact(t, wire)
	tampered.Signature[0] ^= 1
	if _, err := verifier.Verify(mustEncodeArtifact(t, tampered), ArtifactTypeSession, now); !errors.Is(err, ErrSignature) {
		t.Fatalf("tampered signature: %v", err)
	}
	if validations != 1 {
		t.Fatalf("artifact signature result was cached: validations=%d", validations)
	}

	revocations.setGeneration(8)
	if _, err := verifier.Verify(wire, ArtifactTypeSession, now); err != nil {
		t.Fatal(err)
	}
	if validations != 2 {
		t.Fatalf("revocation generation did not miss cache: validations=%d", validations)
	}

	leaf2, priv2 := newCertTestLeaf(t, pki, 2, "signer-2")
	credential2, err := NewSigningCredential(priv2, []*x509.Certificate{leaf2, pki.intermediate}, pki.root, "prod", now)
	if err != nil {
		t.Fatal(err)
	}
	wire2, _ := credential2.Sign(ArtifactTypeSession, []byte("payload"))
	if _, err := verifier.Verify(wire2, ArtifactTypeSession, now); err != nil {
		t.Fatal(err)
	}
	if validations != 3 {
		t.Fatalf("different leaf did not miss cache: validations=%d", validations)
	}
	otherPKI := newCertTestPKI(t, nil)
	otherCredential, err := NewSigningCredential(otherPKI.leafEd25519Priv, otherPKI.chain, otherPKI.root, "prod", now)
	if err != nil {
		t.Fatal(err)
	}
	otherWire, _ := otherCredential.Sign(ArtifactTypeSession, []byte("payload"))
	verifier.root = otherPKI.root
	verifier.rootHash = sha256.Sum256(otherPKI.root.Raw)
	if _, err := verifier.Verify(otherWire, ArtifactTypeSession, now); err != nil {
		t.Fatal(err)
	}
	if validations != 4 {
		t.Fatalf("different trusted root did not miss cache: validations=%d", validations)
	}
	verifier.root = pki.root
	verifier.rootHash = sha256.Sum256(pki.root.Raw)

	if _, err := verifier.Verify(wire, ArtifactTypeSession, pki.leaf.NotAfter.Add(time.Second)); err == nil {
		t.Fatal("expired cached chain verified")
	}
	if validations != 5 {
		t.Fatalf("certificate expiry did not miss cache: validations=%d", validations)
	}

	rootFingerprints := map[[32]byte]bool{
		sha256.Sum256(pki.root.Raw):      false,
		sha256.Sum256(otherPKI.root.Raw): false,
	}
	for key := range verifier.chainCache.entries {
		if _, expected := rootFingerprints[key.root]; expected {
			rootFingerprints[key.root] = true
		}
	}
	for fingerprint, found := range rootFingerprints {
		if !found {
			t.Fatalf("cache key omitted trusted root fingerprint %x", fingerprint)
		}
	}
}

func TestVerifierChainCacheExpiresWithIntermediate(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, func(o *certTestOptions) {
		o.intermediateLifetime = time.Hour
		o.leafLifetime = 24 * time.Hour
	})
	credential, err := NewSigningCredential(pki.leafEd25519Priv, pki.chain, pki.root, "prod", now)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := credential.Sign(ArtifactTypeSession, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(pki.root, "prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	validations := 0
	realValidate := verifier.validateChain
	verifier.validateChain = func(chain []*x509.Certificate, root *x509.Certificate, environment string, at time.Time) (*validatedSigner, error) {
		validations++
		return realValidate(chain, root, environment, at)
	}
	if _, err := verifier.Verify(wire, ArtifactTypeSession, now); err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(wire, ArtifactTypeSession, pki.intermediate.NotAfter.Add(time.Second)); err == nil {
		t.Fatal("chain cached beyond intermediate expiry")
	}
	if validations != 2 {
		t.Fatalf("intermediate expiry did not miss cache: validations=%d", validations)
	}
}

func TestVerifierChainCacheExpiresWithRoot(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, func(o *certTestOptions) {
		o.rootLifetime = time.Hour
		o.intermediateLifetime = 24 * time.Hour
		o.leafLifetime = 12 * time.Hour
	})
	credential, err := NewSigningCredential(pki.leafEd25519Priv, pki.chain, pki.root, "prod", now)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := credential.Sign(ArtifactTypeSession, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(pki.root, "prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	validations := 0
	realValidate := verifier.validateChain
	verifier.validateChain = func(chain []*x509.Certificate, root *x509.Certificate, environment string, at time.Time) (*validatedSigner, error) {
		validations++
		return realValidate(chain, root, environment, at)
	}
	if _, err := verifier.Verify(wire, ArtifactTypeSession, now); err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(wire, ArtifactTypeSession, pki.root.NotAfter.Add(time.Second)); err == nil {
		t.Fatal("chain cached beyond trusted root expiry")
	}
	if validations != 2 {
		t.Fatalf("root expiry did not miss cache: validations=%d", validations)
	}
}

func TestVerifierChainCacheRejectsClockRollback(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	credential, err := NewSigningCredential(pki.leafEd25519Priv, pki.chain, pki.root, "prod", now)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := credential.Sign(ArtifactTypeSession, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(pki.root, "prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	validations := 0
	realValidate := verifier.validateChain
	verifier.validateChain = func(chain []*x509.Certificate, root *x509.Certificate, environment string, at time.Time) (*validatedSigner, error) {
		validations++
		return realValidate(chain, root, environment, at)
	}
	if _, err := verifier.Verify(wire, ArtifactTypeSession, now); err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(wire, ArtifactTypeSession, pki.leaf.NotBefore.Add(-time.Second)); err == nil {
		t.Fatal("cached chain survived clock rollback before leaf NotBefore")
	}
	if validations != 2 {
		t.Fatalf("clock rollback did not miss cache: validations=%d", validations)
	}
}

func TestVerifierDoesNotCachePayloadTimeValidation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	credential, _ := NewSigningCredential(pki.leafEd25519Priv, pki.chain, pki.root, "prod", now)
	payload, err := proto.Marshal(&authv1.SessionTokenBody{IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := credential.Sign(ArtifactTypeSession, payload)
	verifier, err := NewVerifier(pki.root, "prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	validations := 0
	realValidate := verifier.validateChain
	verifier.validateChain = func(chain []*x509.Certificate, root *x509.Certificate, environment string, at time.Time) (*validatedSigner, error) {
		validations++
		return realValidate(chain, root, environment, at)
	}

	for _, tc := range []struct {
		at        time.Time
		wantValid bool
	}{{at: now, wantValid: true}, {at: now.Add(2 * time.Minute), wantValid: false}} {
		gotPayload, err := verifier.Verify(wire, ArtifactTypeSession, tc.at)
		if err != nil {
			t.Fatal(err)
		}
		var body authv1.SessionTokenBody
		if err := proto.Unmarshal(gotPayload, &body); err != nil {
			t.Fatal(err)
		}
		valid := tc.at.Before(time.Unix(body.ExpiresAt, 0))
		if valid != tc.wantValid {
			t.Fatalf("payload validity at %v = %v, want %v", tc.at, valid, tc.wantValid)
		}
	}
	if validations != 1 {
		t.Fatalf("payload time affected chain cache: validations=%d", validations)
	}
}

func TestCertifiedSignerCurrentNextOverlap(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	current, err := NewSigningCredential(pki.leafEd25519Priv, pki.chain, pki.root, "prod", now)
	if err != nil {
		t.Fatal(err)
	}
	nextLeaf, nextPriv := newCertTestLeaf(t, pki, 77, "signer-next")
	next, err := NewSigningCredential(nextPriv, []*x509.Certificate{nextLeaf, pki.intermediate}, pki.root, "prod", now)
	if err != nil {
		t.Fatal(err)
	}
	oldWire, _ := current.Sign(ArtifactTypeSession, []byte("issued-by-current"))
	active := next
	newWire, _ := active.Sign(ArtifactTypeSession, []byte("issued-by-next"))
	verifier, _ := NewVerifier(pki.root, "prod", nil)
	for _, tc := range []struct {
		wire    string
		payload string
	}{{oldWire, "issued-by-current"}, {newWire, "issued-by-next"}, {oldWire, "issued-by-current"}} {
		got, err := verifier.Verify(tc.wire, ArtifactTypeSession, now)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.payload {
			t.Fatalf("payload = %q, want %q", got, tc.payload)
		}
	}
}

func TestVerifierDoesNotCacheValidationFailures(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	credential, _ := NewSigningCredential(pki.leafEd25519Priv, pki.chain, pki.root, "prod", now)
	wire, _ := credential.Sign(ArtifactTypeSession, []byte("payload"))
	revocations := &fakeSignerRevocations{generation: 1, err: errors.New("revoked")}
	verifier, _ := NewVerifier(pki.root, "prod", revocations)
	validations := 0
	realValidate := verifier.validateChain
	verifier.validateChain = func(chain []*x509.Certificate, root *x509.Certificate, environment string, at time.Time) (*validatedSigner, error) {
		validations++
		return realValidate(chain, root, environment, at)
	}
	for range 2 {
		if _, err := verifier.Verify(wire, ArtifactTypeSession, now); err == nil {
			t.Fatal("revoked signer verified")
		}
	}
	if validations != 2 || verifier.chainCache.lru.Len() != 0 {
		t.Fatalf("failed verification cached: validations=%d cache=%d", validations, verifier.chainCache.lru.Len())
	}
}

func TestVerifierChainCacheIsBounded(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	verifier, err := NewVerifier(pki.root, "prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxChainCacheEntries+1; i++ {
		leaf, priv := newCertTestLeaf(t, pki, int64(i+10), fmt.Sprintf("signer-%d", i))
		credential, err := NewSigningCredential(priv, []*x509.Certificate{leaf, pki.intermediate}, pki.root, "prod", now)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := credential.Sign(ArtifactTypeSession, []byte("payload"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := verifier.Verify(wire, ArtifactTypeSession, now); err != nil {
			t.Fatal(err)
		}
	}
	if got := verifier.chainCache.lru.Len(); got != maxChainCacheEntries {
		t.Fatalf("cache size = %d, want %d", got, maxChainCacheEntries)
	}
}

func TestVerifierChainCacheConcurrentContention(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	verifier, err := NewVerifier(pki.root, "prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	wires := make([]string, maxChainCacheEntries+8)
	for i := range wires {
		leaf, priv := newCertTestLeaf(t, pki, int64(i+1_000), fmt.Sprintf("concurrent-%d", i))
		credential, err := NewSigningCredential(priv, []*x509.Certificate{leaf, pki.intermediate}, pki.root, "prod", now)
		if err != nil {
			t.Fatal(err)
		}
		wires[i], err = credential.Sign(ArtifactTypeSession, []byte("payload"))
		if err != nil {
			t.Fatal(err)
		}
	}

	var failures atomic.Int64
	var wg sync.WaitGroup
	for worker := range 32 {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for i := range wires {
				wire := wires[(i+offset)%len(wires)]
				if _, err := verifier.Verify(wire, ArtifactTypeSession, now); err != nil {
					failures.Add(1)
				}
			}
		}(worker)
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("concurrent verifications failed: %d", failures.Load())
	}
	verifier.chainCache.mu.Lock()
	cacheLen := verifier.chainCache.lru.Len()
	entryLen := len(verifier.chainCache.entries)
	verifier.chainCache.mu.Unlock()
	if cacheLen != maxChainCacheEntries || entryLen != maxChainCacheEntries {
		t.Fatalf("contested cache sizes: lru=%d entries=%d want=%d", cacheLen, entryLen, maxChainCacheEntries)
	}
}
