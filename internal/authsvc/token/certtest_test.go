package token

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"net/url"
	"testing"
	"time"
)

var (
	testIntermediatePolicyOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}
	testLeafPolicyOID         = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 2}
)

type certTestOptions struct {
	environment          string
	intermediatePolicies []asn1.ObjectIdentifier
	leafPolicies         []asn1.ObjectIdentifier
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
		intermediatePolicies: []asn1.ObjectIdentifier{testIntermediatePolicyOID},
		leafPolicies:         []asn1.ObjectIdentifier{testLeafPolicyOID},
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
		intermediatePolicies = []asn1.ObjectIdentifier{{1, 3, 6, 1, 4, 1, 57264, 9, 9}}
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
		Policies:              mustPolicies(t, intermediatePolicies),
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
		Policies:     mustPolicies(t, opts.leafPolicies),
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

func newCertTestLeaf(t *testing.T, pki certTestPKI, serial int64, signerID string) (*x509.Certificate, ed25519.PrivateKey) {
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
		Policies:     mustPolicies(t, []asn1.ObjectIdentifier{testLeafPolicyOID}),
		URIs:         mustParseURIs(t, []string{"spiffe://prod.spawnery.internal/signer/auth-artifact/" + signerID}),
	}
	return mustCreateCertificate(t, template, pki.intermediate, priv.Public(), pki.intermediateKey), priv
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

func mustPolicies(t *testing.T, values []asn1.ObjectIdentifier) []x509.OID {
	t.Helper()
	result := make([]x509.OID, 0, len(values))
	for _, value := range values {
		oid, err := x509.OIDFromASN1OID(value)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, oid)
	}
	return result
}
