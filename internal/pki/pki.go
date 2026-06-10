// Package pki implements Spawnery's node-identity certificate authority: a Root CA, name-constrained
// per-class intermediates (cloud / self-hosted), node-leaf issuance, and verification that derives a
// node's identity from its certificate SAN. Class is enforced by RFC 5280 name constraints — a
// self-hosted intermediate is cryptographically incapable of signing a cloud-subtree leaf that
// validates (see docs/superpowers/specs/2026-06-05-node-auth-unified-identity-design.md §4).
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
	"strings"
	"time"
)

// Domain is the DNS suffix under which every node identity lives. A node's SAN is
// <nodeId>.<accountId>.<class>.<Domain>; the per-class subtree is <class>.<Domain>.
const Domain = "nodes.spawnery.internal"

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

// Node is an issued node identity: its leaf cert + key, and the intermediate chain to present for
// verification against the Root.
type Node struct {
	Cert  *x509.Certificate
	Key   *ecdsa.PrivateKey
	Chain []*x509.Certificate
}

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func classSubtree(class string) string { return class + "." + Domain }

func nodeSAN(nodeID, accountID, class string) string {
	return nodeID + "." + accountID + "." + class + "." + Domain
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

// NewIntermediate issues an intermediate CA signed by this CA, name-constrained to the given class's
// subtree so it can only sign leaves under <class>.Domain.
func (ca *CA) NewIntermediate(class string) (*CA, error) {
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
		Subject:               pkix.Name{CommonName: "Spawnery " + class + " Intermediate"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(5 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		PermittedDNSDomains:   []string{classSubtree(class)},
	}
	return finishCA(tmpl, ca.Cert, key.Public(), key, ca.Key)
}

// IssueNode issues a node leaf certificate from this (intermediate) CA, with the identity encoded in
// the SAN. The chain to present is this CA's cert.
func (ca *CA) IssueNode(nodeID, accountID, class string, notAfter time.Time) (*Node, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	san := nodeSAN(nodeID, accountID, class)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: san},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{san},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, key.Public(), ca.Key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &Node{Cert: leaf, Key: key, Chain: []*x509.Certificate{ca.Cert}}, nil
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

// Verify validates leaf against the pinned root (enforcing name constraints + expiry at now) and
// returns the identity read from the verified leaf's SAN.
func Verify(leaf *x509.Certificate, intermediates []*x509.Certificate, root *x509.Certificate, now time.Time) (Identity, error) {
	roots := x509.NewCertPool()
	roots.AddCert(root)
	inter := x509.NewCertPool()
	for _, c := range intermediates {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: inter,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return Identity{}, err
	}
	return identityFromSAN(leaf)
}

func identityFromSAN(leaf *x509.Certificate) (Identity, error) {
	if len(leaf.DNSNames) == 0 {
		return Identity{}, errors.New("certificate has no DNS SAN")
	}
	san := leaf.DNSNames[0]
	head, ok := strings.CutSuffix(san, "."+Domain)
	if !ok {
		return Identity{}, fmt.Errorf("SAN %q not under %q", san, Domain)
	}
	parts := strings.Split(head, ".")
	if len(parts) != 3 {
		return Identity{}, fmt.Errorf("SAN %q: want <nodeId>.<accountId>.<class>.%s", san, Domain)
	}
	return Identity{NodeID: parts[0], AccountID: parts[1], Class: parts[2]}, nil
}
