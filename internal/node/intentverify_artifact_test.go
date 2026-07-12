package node

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"log"
	"math/big"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/intent"
	"spawnery/internal/pki"
)

type artifactFixture struct {
	root            *x509.Certificate
	intermediate    *x509.Certificate
	intermediateKey *ecdsa.PrivateKey
	leaf            *x509.Certificate
	credential      *token.SigningCredential
	verifier        *token.Verifier
}

func newArtifactFixture(t *testing.T, now time.Time, environment string) artifactFixture {
	t.Helper()
	rootKey := mustArtifactP256(t)
	root := mustArtifactCert(t, &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "root"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, BasicConstraintsValid: true, IsCA: true, MaxPathLen: 2,
	}, nil, &rootKey.PublicKey, rootKey)
	intermediateKey := mustArtifactP256(t)
	intermediate := mustArtifactCert(t, &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "auth signing"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(180 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true, IsCA: true, MaxPathLen: 0, MaxPathLenZero: true,
		Policies: []x509.OID{pki.AuthSigningIntermediatePolicyOID},
	}, root, &intermediateKey.PublicKey, rootKey)
	_, leafKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signerURI, err := url.Parse("spiffe://" + environment + ".spawnery.internal/signer/auth-artifact/signer-1")
	if err != nil {
		t.Fatal(err)
	}
	leaf := mustArtifactCert(t, &x509.Certificate{
		SerialNumber: new(big.Int).Lsh(big.NewInt(1), 127), Subject: pkix.Name{CommonName: "artifact signer"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(90 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, Policies: []x509.OID{pki.AuthArtifactSignerPolicyOID}, URIs: []*url.URL{signerURI},
	}, intermediate, leafKey.Public(), intermediateKey)
	credential, err := token.NewSigningCredential(leafKey, []*x509.Certificate{leaf, intermediate}, root, environment, now)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := token.NewVerifier(root, environment, nil)
	if err != nil {
		t.Fatal(err)
	}
	return artifactFixture{root: root, intermediate: intermediate, intermediateKey: intermediateKey, leaf: leaf, credential: credential, verifier: verifier}
}

func mustArtifactP256(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func mustArtifactCert(t *testing.T, template, parent *x509.Certificate, public any, signer crypto.Signer) *x509.Certificate {
	t.Helper()
	if parent == nil {
		parent = template
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, public, signer)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func mintArtifactSession(t *testing.T, fixture artifactFixture, body *authv1.SessionTokenBody) string {
	t.Helper()
	payload, err := proto.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := fixture.credential.Sign(token.ArtifactTypeSession, payload)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestIntentVerifierRejectsLegacyTokenBeforeIntentState(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newArtifactFixture(t, now, "prod")
	v := NewIntentVerifier(fixture.verifier, "alice", "node-1", false, AuthModeEnforced, func() time.Time { return now })
	nack, _ := v.VerifyStart(&authv1.AuthEnvelope{AccessToken: "legacy.signature"}, goodStartFields("sp-1", "node-1", 1))
	if nack != NACKTokenInvalid {
		t.Fatalf("legacy token: got %q, want %q", nack, NACKTokenInvalid)
	}
}

type mutableSignerRevocations struct {
	generation atomic.Uint64
	revoked    atomic.Bool
}

func (r *mutableSignerRevocations) Generation() uint64 { return r.generation.Load() }
func (r *mutableSignerRevocations) RejectSigner(*x509.Certificate) error {
	if r.revoked.Load() {
		return errors.New("revoked signer")
	}
	return nil
}

func TestIntentVerifierRevocationGenerationInvalidatesCachedSigner(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newArtifactFixture(t, now, "prod")
	revocations := &mutableSignerRevocations{}
	artifacts, err := token.NewVerifier(fixture.root, "prod", revocations)
	if err != nil {
		t.Fatal(err)
	}
	env, fields := certifiedIntent(t, fixture, now, "jti-revoked")
	v := NewIntentVerifier(artifacts, "alice", "node-1", false, AuthModeEnforced, func() time.Time { return now })
	if nack, detail := v.VerifyStart(env, fields); nack != "" {
		t.Fatalf("initial verify: %s %s", nack, detail)
	}
	revocations.revoked.Store(true)
	revocations.generation.Store(1)
	if nack, _ := v.VerifyStart(env, fields); nack != NACKTokenInvalid {
		t.Fatalf("after revocation: got %q, want %q", nack, NACKTokenInvalid)
	}
}

func TestIntentVerifierRejectsExpiredCertifiedSessionBeforeIntent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newArtifactFixture(t, now, "prod")
	env, fields := certifiedIntent(t, fixture, now, "jti-expired")
	var artifact authv1.SignedAuthArtifact
	raw, _ := base64.RawURLEncoding.DecodeString(env.AccessToken)
	if err := proto.Unmarshal(raw, &artifact); err != nil {
		t.Fatal(err)
	}
	var body authv1.SessionTokenBody
	if err := proto.Unmarshal(artifact.Payload, &body); err != nil {
		t.Fatal(err)
	}
	body.ExpiresAt = now.Unix()
	env.AccessToken = mintArtifactSession(t, fixture, &body)
	v := NewIntentVerifier(fixture.verifier, "alice", "node-1", false, AuthModeEnforced, func() time.Time { return now })
	if nack, _ := v.VerifyStart(env, fields); nack != NACKTokenInvalid {
		t.Fatalf("expired certified session: got %q, want %q", nack, NACKTokenInvalid)
	}
}

func TestIntentVerifierCertifiedArtifactFailuresPrecedeIntentState(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newArtifactFixture(t, now, "prod")
	valid, fields := certifiedIntent(t, fixture, now, "jti-state-proof")
	wrongRoot := newArtifactFixture(t, now, "prod")
	wrongRootEnv, _ := certifiedIntent(t, wrongRoot, now, "irrelevant-root")
	wrongEnvironment := newArtifactFixture(t, now, "staging")
	wrongEnvironmentEnv, _ := certifiedIntent(t, wrongEnvironment, now, "irrelevant-env")
	unknown := mutateArtifactWire(t, valid.AccessToken, func(a *authv1.SignedAuthArtifact) { a.ArtifactType = "unknown" })
	corruptSignature := mutateArtifactWire(t, valid.AccessToken, func(a *authv1.SignedAuthArtifact) { a.Signature[0] ^= 1 })
	corruptPayload := mutateArtifactWire(t, valid.AccessToken, func(a *authv1.SignedAuthArtifact) { a.Payload[0] ^= 1 })
	corruptKeyID := mutateArtifactWire(t, valid.AccessToken, func(a *authv1.SignedAuthArtifact) { a.KeyId[0] ^= 1 })
	corruptChain := mutateArtifactWire(t, valid.AccessToken, func(a *authv1.SignedAuthArtifact) { a.SignerChain[0] = []byte("not DER") })
	wrongPathLeaf := replacementArtifactLeaf(t, fixture, now, fixture.credential.PrivateKey.Public(), func(c *x509.Certificate) {
		c.URIs = []*url.URL{{Scheme: "spiffe", Host: "prod.spawnery.internal", Path: "/service/authsvc/signer-1"}}
	})
	wrongPolicyLeaf := replacementArtifactLeaf(t, fixture, now, fixture.credential.PrivateKey.Public(), func(c *x509.Certificate) { c.Policies = nil })
	expiredLeaf := replacementArtifactLeaf(t, fixture, now, fixture.credential.PrivateKey.Public(), func(c *x509.Certificate) { c.NotAfter = now.Add(-time.Second) })
	ecdsaLeaf := replacementArtifactLeaf(t, fixture, now, &mustArtifactP256(t).PublicKey, nil)
	withLeaf := func(leaf *x509.Certificate) string {
		return mutateArtifactWire(t, valid.AccessToken, func(a *authv1.SignedAuthArtifact) { a.SignerChain[0] = leaf.Raw })
	}
	for _, tc := range []struct {
		name      string
		artifacts *token.Verifier
		wire      string
	}{
		{name: "wrong root", artifacts: fixture.verifier, wire: wrongRootEnv.AccessToken},
		{name: "wrong environment", artifacts: fixture.verifier, wire: wrongEnvironmentEnv.AccessToken},
		{name: "unknown artifact", artifacts: fixture.verifier, wire: unknown},
		{name: "wrong SPIFFE path", artifacts: fixture.verifier, wire: withLeaf(wrongPathLeaf)},
		{name: "wrong policy", artifacts: fixture.verifier, wire: withLeaf(wrongPolicyLeaf)},
		{name: "ECDSA signer", artifacts: fixture.verifier, wire: withLeaf(ecdsaLeaf)},
		{name: "expired certificate", artifacts: fixture.verifier, wire: withLeaf(expiredLeaf)},
		{name: "corrupt payload", artifacts: fixture.verifier, wire: corruptPayload},
		{name: "corrupt signature", artifacts: fixture.verifier, wire: corruptSignature},
		{name: "corrupt key ID", artifacts: fixture.verifier, wire: corruptKeyID},
		{name: "corrupt chain", artifacts: fixture.verifier, wire: corruptChain},
		{name: "legacy two part", artifacts: fixture.verifier, wire: "legacy.signature"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := NewIntentVerifier(tc.artifacts, "alice", "node-1", false, AuthModeEnforced, func() time.Time { return now })
			invalid := proto.Clone(valid).(*authv1.AuthEnvelope)
			invalid.AccessToken = tc.wire
			if nack, _ := v.VerifyStart(invalid, fields); nack != NACKTokenInvalid {
				t.Fatalf("invalid artifact: got %q, want %q", nack, NACKTokenInvalid)
			}
			if nack, detail := v.VerifyStart(valid, fields); nack != "" {
				t.Fatalf("valid intent after artifact failure: %s %s", nack, detail)
			}
		})
	}
}

func replacementArtifactLeaf(t *testing.T, fixture artifactFixture, now time.Time, public any, mutate func(*x509.Certificate)) *x509.Certificate {
	t.Helper()
	signerURI, err := url.Parse("spiffe://prod.spawnery.internal/signer/auth-artifact/replacement")
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: new(big.Int).Add(fixture.leaf.SerialNumber, big.NewInt(1)),
		Subject:      pkix.Name{CommonName: "replacement artifact signer"},
		NotBefore:    now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, Policies: []x509.OID{pki.AuthArtifactSignerPolicyOID}, URIs: []*url.URL{signerURI},
	}
	if mutate != nil {
		mutate(template)
	}
	return mustArtifactCert(t, template, fixture.intermediate, public, fixture.intermediateKey)
}

func TestIntentVerifierVerifyLogDoesNotLogCertificateMaterial(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newArtifactFixture(t, now, "prod")
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(old)
	v := NewIntentVerifier(fixture.verifier, "alice", "node-1", false, AuthModeVerifyLog, func() time.Time { return now })
	if nack, _ := v.VerifyStart(&authv1.AuthEnvelope{AccessToken: "invalid"}, goodStartFields("sp-1", "node-1", 1)); nack != "" {
		t.Fatalf("verify-log returned %q", nack)
	}
	logged := output.String()
	if !strings.Contains(logged, string(NACKTokenInvalid)) || strings.Contains(logged, "CERTIFICATE") || strings.Contains(logged, base64.RawURLEncoding.EncodeToString(fixture.leaf.Raw)) {
		t.Fatalf("unsafe or missing verify-log output: %q", logged)
	}
}

func certifiedIntent(t *testing.T, fixture artifactFixture, now time.Time, jti string) (*authv1.AuthEnvelope, StartFields) {
	t.Helper()
	sessionKey := genECDSA(t)
	spki, err := x509.MarshalPKIXPublicKey(&sessionKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	wire := mintArtifactSession(t, fixture, &authv1.SessionTokenBody{
		AccountId: "alice", TokenId: "token-1", Audience: "node", IssuedAt: now.Unix(),
		ExpiresAt: now.Add(time.Minute).Unix(), SessionKeyHash: token.SessionKeyHash(spki),
	})
	body := goodStartBody("sp-1", "node-1", 1, now)
	body.Jti = jti
	si, err := intent.Build(intent.OpCreateSpawn, body, sessionKey)
	if err != nil {
		t.Fatal(err)
	}
	return &authv1.AuthEnvelope{AccessToken: wire, Intent: si}, goodStartFields("sp-1", "node-1", 1)
}

func mutateArtifactWire(t *testing.T, wire string, mutate func(*authv1.SignedAuthArtifact)) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(wire)
	if err != nil {
		t.Fatal(err)
	}
	var artifact authv1.SignedAuthArtifact
	if err := proto.Unmarshal(raw, &artifact); err != nil {
		t.Fatal(err)
	}
	mutate(&artifact)
	raw, err = proto.Marshal(&artifact)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
