package pki_test

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"testing"
	"time"

	"spawnery/internal/authsvc/token"
	"spawnery/internal/pki"
)

func TestIssueAuthArtifactSignerProfile(t *testing.T) {
	now := time.Now()
	root, err := pki.NewRootCA("root")
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := root.NewAuthSigningIntermediate("prod")
	if err != nil {
		t.Fatalf("NewAuthSigningIntermediate: %v", err)
	}
	signer, err := issuer.IssueAuthArtifactSigner("prod", "current", now.Add(90*24*time.Hour))
	if err != nil {
		t.Fatalf("IssueAuthArtifactSigner: %v", err)
	}

	if issuer.Cert.PublicKeyAlgorithm != x509.ECDSA || !pki.HasPolicy(issuer.Cert, pki.AuthSigningIntermediatePolicyOID) {
		t.Fatalf("intermediate profile = algorithm %v policies %v", issuer.Cert.PublicKeyAlgorithm, issuer.Cert.Policies)
	}
	if signer.Cert.PublicKeyAlgorithm != x509.Ed25519 {
		t.Fatalf("leaf algorithm = %v, want Ed25519", signer.Cert.PublicKeyAlgorithm)
	}
	if signer.Cert.SerialNumber.Sign() <= 0 || signer.Cert.SerialNumber.BitLen() < 128 {
		t.Fatalf("leaf serial = %v (%d bits), want positive >=128-bit", signer.Cert.SerialNumber, signer.Cert.SerialNumber.BitLen())
	}
	if signer.Cert.KeyUsage != x509.KeyUsageDigitalSignature || len(signer.Cert.ExtKeyUsage) != 0 || len(signer.Cert.UnknownExtKeyUsage) != 0 {
		t.Fatalf("leaf usages = key %v extended %v unknown %v", signer.Cert.KeyUsage, signer.Cert.ExtKeyUsage, signer.Cert.UnknownExtKeyUsage)
	}
	if len(signer.Cert.Policies) != 1 || !signer.Cert.Policies[0].Equal(pki.AuthArtifactSignerPolicyOID) {
		t.Fatalf("leaf policies = %v", signer.Cert.Policies)
	}
	wantURI := "spiffe://prod.spawnery.internal/signer/auth-artifact/current"
	if len(signer.Cert.URIs) != 1 || signer.Cert.URIs[0].String() != wantURI {
		t.Fatalf("leaf URI SANs = %v, want %q", signer.Cert.URIs, wantURI)
	}
	if len(signer.Chain) != 1 || !signer.Chain[0].Equal(issuer.Cert) {
		t.Fatalf("leaf chain = %v, want auth-signing intermediate only", signer.Chain)
	}
	if _, err := token.NewSigningCredential(signer.Key, []*x509.Certificate{signer.Cert, issuer.Cert}, root.Cert, "prod", now); err != nil {
		t.Fatalf("NewSigningCredential: %v", err)
	}
}

func TestUnrelatedIssuersCannotMintAuthArtifactCredentials(t *testing.T) {
	now := time.Now()
	root, _ := pki.NewRootCA("root")
	nodeIssuer, _ := root.NewIntermediate(pki.IssuerSelfHostedNode, "prod.spawnery.internal")
	node, _ := nodeIssuer.IssueNode("node-1", "acct", pki.RoleSelfHosted, "prod.spawnery.internal", now.Add(time.Hour))
	serviceIssuer, _ := root.NewIntermediate(pki.IssuerService, "prod.spawnery.internal")
	service, _ := serviceIssuer.IssueService(pki.RoleCP, "cp-1", "prod.spawnery.internal", nil, nil, now.Add(time.Hour))

	for name, cert := range map[string]*x509.Certificate{"node": node.Cert, "service": service.Cert} {
		if pki.HasPolicy(cert, pki.AuthArtifactSignerPolicyOID) {
			t.Fatalf("%s leaf unexpectedly has auth-artifact signer policy", name)
		}
	}

	_, forgedKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	forgedSPKI, _ := x509.MarshalPKIXPublicKey(forgedKey.Public())
	if sha256.Sum256(forgedSPKI) == ([32]byte{}) {
		t.Fatal("unexpected empty SPKI hash")
	}
	if _, err := token.NewSigningCredential(forgedKey, []*x509.Certificate{node.Cert, nodeIssuer.Cert}, root.Cert, "prod", now); err == nil {
		t.Fatal("node intermediate chain must not authorize an auth-artifact signing credential")
	}
}
