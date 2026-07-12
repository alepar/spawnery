package cp

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/url"
	"testing"
	"time"

	"spawnery/internal/authsvc/token"
	"spawnery/internal/pki"
)

func cpArtifactFixture(t *testing.T, now time.Time) (*token.SigningCredential, *token.Verifier) {
	t.Helper()
	rootKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	root := cpIssueCert(t, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "root"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour), KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, BasicConstraintsValid: true, IsCA: true, MaxPathLen: 2}, nil, &rootKey.PublicKey, rootKey)
	intermediateKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	intermediate := cpIssueCert(t, &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "auth signing"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(180 * 24 * time.Hour), KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true, IsCA: true, MaxPathLen: 0, Policies: []x509.OID{pki.AuthSigningIntermediatePolicyOID}}, root, &intermediateKey.PublicKey, rootKey)
	_, leafKey, _ := ed25519.GenerateKey(rand.Reader)
	uri, _ := url.Parse("spiffe://test.spawnery.internal/signer/auth-artifact/cp-test")
	leaf := cpIssueCert(t, &x509.Certificate{SerialNumber: new(big.Int).Lsh(big.NewInt(1), 127), Subject: pkix.Name{CommonName: "signer"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(90 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, Policies: []x509.OID{pki.AuthArtifactSignerPolicyOID}, URIs: []*url.URL{uri}}, intermediate, leafKey.Public(), intermediateKey)
	credential, err := token.NewSigningCredential(leafKey, []*x509.Certificate{leaf, intermediate}, root, "test", now)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := token.NewVerifier(root, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	return credential, verifier
}

func cpIssueCert(t *testing.T, template, parent *x509.Certificate, public any, signer crypto.Signer) *x509.Certificate {
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
