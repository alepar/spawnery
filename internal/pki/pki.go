// Package pki implements Spawnery's SPIFFE X.509-SVID authority and typed principal verification.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"time"
)

// DefaultTrustDomain is for explicit development and test fixtures only.
// Deprecated: production callers must provide their configured environment trust domain.
const DefaultTrustDomain = "dev.spawnery.internal"

const (
	ClassCloud      = "cloud"
	ClassSelfHosted = "self-hosted"
)

// Identity is the node identity carried in a verified leaf certificate's SAN.
type Identity struct {
	NodeID    string
	AccountID string
	Class     string
}

// CA is a signing authority (Root or intermediate): its certificate plus the private key that signs
// certs below it.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
}

// Leaf is issued identity material: a leaf certificate and private key plus its presented chain.
type Leaf struct {
	Cert  *x509.Certificate
	Key   *ecdsa.PrivateKey
	Chain []*x509.Certificate
}

// Node is retained as a compatibility alias while callers migrate to identity-neutral Leaf.
type Node = Leaf

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

// NewRootCA generates a self-signed Root CA.
func NewRootCA(commonName string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	return finishCA(tmpl, tmpl, key.Public(), key, key)
}

// NewIntermediate issues a non-delegating, role-bearing intermediate signed by this CA. Omitting
// trustDomains selects DefaultTrustDomain for legacy development fixtures only.
func (ca *CA) NewIntermediate(role IssuerRole, trustDomains ...string) (*CA, error) {
	role, err := normalizeIssuerRole(role)
	if err != nil {
		return nil, err
	}
	trustDomain := DefaultTrustDomain
	if len(trustDomains) > 1 {
		return nil, errors.New("pki: expected at most one trust domain")
	}
	if len(trustDomains) == 1 {
		trustDomain = trustDomains[0]
	}
	rootID, err := principalIDForTrustDomain(trustDomain)
	if err != nil {
		return nil, err
	}
	policy, err := issuerRolePolicy(role)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Spawnery " + string(role) + " Intermediate"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(5 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		URIs:                  []*url.URL{rootID},
		Policies:              []x509.OID{policy},
	}
	return finishCA(tmpl, ca.Cert, key.Public(), key, ca.Key)
}

// IssueNode issues a node X.509-SVID. The fourth argument accepts either trustDomain followed by
// notAfter, or a legacy notAfter value which selects DefaultTrustDomain for test fixtures only.
func (ca *CA) IssueNode(nodeID, accountID, role string, trustDomainOrNotAfter any, notAfterValues ...time.Time) (*Leaf, error) {
	trustDomain, notAfter, err := issuanceWindow(trustDomainOrNotAfter, notAfterValues)
	if err != nil {
		return nil, err
	}
	id, err := NodeID(trustDomain, role, accountID, nodeID)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	leaf, err := ca.issueLeaf(key.Public(), id, nil, nil, notAfter)
	if err != nil {
		return nil, err
	}
	return &Leaf{Cert: leaf, Key: key, Chain: []*x509.Certificate{ca.Cert}}, nil
}

// IssueService issues a service X.509-SVID with optional endpoint DNS and IP SANs.
func (ca *CA) IssueService(role, instanceID, trustDomain string, dns []string, ips []net.IP, notAfter time.Time) (*Leaf, error) {
	id, err := ServiceID(trustDomain, role, instanceID)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	leaf, err := ca.issueLeaf(key.Public(), id, dns, ips, notAfter)
	if err != nil {
		return nil, err
	}
	return &Leaf{Cert: leaf, Key: key, Chain: []*x509.Certificate{ca.Cert}}, nil
}

func (ca *CA) issueLeaf(publicKey any, id *url.URL, dns []string, ips []net.IP, notAfter time.Time) (*x509.Certificate, error) {
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		DNSNames:              dns,
		IPAddresses:           ips,
		URIs:                  []*url.URL{id},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, publicKey, ca.Key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

func issuanceWindow(trustDomainOrNotAfter any, notAfterValues []time.Time) (string, time.Time, error) {
	switch value := trustDomainOrNotAfter.(type) {
	case string:
		if len(notAfterValues) != 1 {
			return "", time.Time{}, errors.New("pki: trust domain requires one notAfter value")
		}
		return value, notAfterValues[0], nil
	case time.Time:
		if len(notAfterValues) != 0 {
			return "", time.Time{}, errors.New("pki: unexpected additional notAfter value")
		}
		return DefaultTrustDomain, value, nil
	default:
		return "", time.Time{}, fmt.Errorf("pki: issuance argument is %T, want trust domain or time.Time", trustDomainOrNotAfter)
	}
}

// finishCA creates a CA cert from tmpl signed by parent (signerKey), parses it, and returns the CA.
func finishCA(tmpl, parent *x509.Certificate, pub any, ownKey, signerKey *ecdsa.PrivateKey) (*CA, error) {
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signerKey)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: ownKey}, nil
}

// Verify is the compatibility node-result verifier. The caller must provide its configured trust
// domain; identity is never derived from an untrusted leaf before verification.
func Verify(leaf *x509.Certificate, intermediates []*x509.Certificate, root *x509.Certificate, trustDomain string, now time.Time, isRevoked CertificateRevocationChecker) (Identity, error) {
	if leaf == nil || len(leaf.URIs) != 1 {
		return Identity{}, errors.New("pki: leaf must contain exactly one URI SAN")
	}
	principal, err := VerifyPrincipal(leaf, intermediates, VerifyOptions{
		Root:        root,
		TrustDomain: trustDomain,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		IsRevoked:   isRevoked,
	})
	if err != nil {
		return Identity{}, err
	}
	if principal.Kind != KindNode {
		return Identity{}, errors.New("pki: principal is not a node")
	}
	return Identity{NodeID: principal.NodeID, AccountID: principal.AccountID, Class: principal.Role}, nil
}
