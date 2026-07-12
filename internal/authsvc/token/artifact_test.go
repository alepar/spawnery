package token

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
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
	tests := []struct {
		name  string
		priv  ed25519.PrivateKey
		chain []*x509.Certificate
		root  *x509.Certificate
		env   string
	}{
		{name: "mismatched private key", priv: wrongPriv, chain: valid.chain, root: valid.root, env: "prod"},
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
		{name: "wrong leaf policy", mutate: func(o *certTestOptions) { o.leafPolicies = []asn1.ObjectIdentifier{{1, 2, 3}} }},
		{name: "wrong intermediate policy", mutate: func(o *certTestOptions) { o.intermediatePolicies = []asn1.ObjectIdentifier{{1, 2, 3}} }},
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
