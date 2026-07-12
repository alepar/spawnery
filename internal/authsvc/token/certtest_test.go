package token

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

	"spawnery/internal/pki"
)

type certTestOptions struct {
	environment          string
	intermediatePolicies []x509.OID
	leafPolicies         []x509.OID
	leafURIs             []string
	leafUsage            x509.KeyUsage
	leafExtUsage         []x509.ExtKeyUsage
	leafECDSA            bool
	leafExpired          bool
	useNodeIntermediate  bool
	intermediateLifetime time.Duration
	leafLifetime         time.Duration
	rootLifetime         time.Duration
}

type certTestPKI struct {
	root            *x509.Certificate
	intermediate    *x509.Certificate
	intermediateKey *ecdsa.PrivateKey
	leaf            *x509.Certificate
	leafEd25519Priv ed25519.PrivateKey
	chain           []*x509.Certificate
}

func newCertTestPKI(t *testing.T, mutate func(*certTestOptions)) certTestPKI {
	t.Helper()
	opts := certTestOptions{
		environment:          "prod",
		intermediatePolicies: []x509.OID{pki.AuthSigningIntermediatePolicyOID},
		leafPolicies:         []x509.OID{pki.AuthArtifactSignerPolicyOID},
		leafURIs:             []string{"spiffe://prod.spawnery.internal/signer/auth-artifact/signer-1"},
		leafUsage:            x509.KeyUsageDigitalSignature,
	}
	if mutate != nil {
		mutate(&opts)
	}
	now := time.Unix(1_800_000_000, 0)
	if opts.intermediateLifetime == 0 {
		opts.intermediateLifetime = 180 * 24 * time.Hour
	}
	if opts.leafLifetime == 0 {
		opts.leafLifetime = 90 * 24 * time.Hour
	}
	if opts.rootLifetime == 0 {
		opts.rootLifetime = 365 * 24 * time.Hour
	}

	rootKey := mustP256Key(t)
	rootTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Spawnery test root"},
		NotBefore:             now.Add(-24 * time.Hour),
		NotAfter:              now.Add(opts.rootLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            2,
	}
	root := mustCreateCertificate(t, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)

	intermediateKey := mustP256Key(t)
	intermediatePolicies := opts.intermediatePolicies
	if opts.useNodeIntermediate {
		intermediatePolicies = []x509.OID{mustOID(t, "1.2.3.4")}
	}
	intermediateTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "Spawnery auth signing intermediate"},
		NotBefore:             now.Add(-12 * time.Hour),
		NotAfter:              now.Add(opts.intermediateLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		Policies:              intermediatePolicies,
	}
	intermediate := mustCreateCertificate(t, intermediateTemplate, root, &intermediateKey.PublicKey, rootKey)

	var leafPublic crypto.PublicKey
	var leafEdPriv ed25519.PrivateKey
	if opts.leafECDSA {
		leafKey := mustP256Key(t)
		leafPublic = &leafKey.PublicKey
	} else {
		var err error
		_, leafEdPriv, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leafPublic = leafEdPriv.Public()
	}
	leafNotAfter := now.Add(opts.leafLifetime)
	if opts.leafExpired {
		leafNotAfter = now.Add(-time.Hour)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: new(big.Int).Lsh(big.NewInt(1), 127),
		Subject:      pkix.Name{CommonName: "Spawnery artifact signer"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     leafNotAfter,
		KeyUsage:     opts.leafUsage,
		ExtKeyUsage:  opts.leafExtUsage,
		Policies:     opts.leafPolicies,
		URIs:         mustParseURIs(t, opts.leafURIs),
	}
	leaf := mustCreateCertificate(t, leafTemplate, intermediate, leafPublic, intermediateKey)
	return certTestPKI{
		root:            root,
		intermediate:    intermediate,
		intermediateKey: intermediateKey,
		leaf:            leaf,
		leafEd25519Priv: leafEdPriv,
		chain:           []*x509.Certificate{leaf, intermediate},
	}
}

func newCertTestLeaf(t *testing.T, fixture certTestPKI, serial int64, signerID string) (*x509.Certificate, ed25519.PrivateKey) {
	t.Helper()
	now := time.Unix(1_800_000_000, 0)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 127), big.NewInt(serial)),
		Subject:      pkix.Name{CommonName: "Spawnery artifact signer"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		Policies:     []x509.OID{pki.AuthArtifactSignerPolicyOID},
		URIs:         mustParseURIs(t, []string{"spiffe://prod.spawnery.internal/signer/auth-artifact/" + signerID}),
	}
	return mustCreateCertificate(t, template, fixture.intermediate, priv.Public(), fixture.intermediateKey), priv
}

func mustP256Key(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func mustCreateCertificate(t *testing.T, template, parent *x509.Certificate, pub any, signer crypto.Signer) *x509.Certificate {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, template, parent, pub, signer)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func mustParseURIs(t *testing.T, values []string) []*url.URL {
	t.Helper()
	result := make([]*url.URL, 0, len(values))
	for _, value := range values {
		u, err := url.Parse(value)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, u)
	}
	return result
}

func mustOID(t *testing.T, value string) x509.OID {
	t.Helper()
	oid, err := x509.ParseOID(value)
	if err != nil {
		t.Fatal(err)
	}
	return oid
}
