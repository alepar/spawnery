package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

// MarshalCSRPEM encodes a CSR DER as PEM.
func MarshalCSRPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

// ParseCSRPEM decodes a PEM-encoded CSR, returning its DER bytes.
func ParseCSRPEM(b []byte) ([]byte, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("pki: no CERTIFICATE REQUEST PEM block")
	}
	return blk.Bytes, nil
}

// NewNodeCSR generates a node keypair and a CSR over its public key (proving possession). The node
// keeps the private key; only the CSR leaves the box. The CSR carries no authoritative names — the CA
// assigns the SAN at signing time (SignCSR).
func NewNodeCSR() ([]byte, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return nil, nil, err
	}
	return der, key, nil
}

// SignNodeCSR verifies proof of possession and binds the CSR key to a CA-selected node identity.
func (ca *CA) SignNodeCSR(csrDER []byte, nodeID, accountID, role, trustDomain string, notAfter time.Time) (*x509.Certificate, []*x509.Certificate, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parse csr: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, nil, fmt.Errorf("csr signature: %w", err)
	}
	id, err := NodeID(trustDomain, role, accountID, nodeID)
	if err != nil {
		return nil, nil, err
	}
	leaf, err := ca.issueLeaf(csr.PublicKey, id, nil, nil, notAfter)
	if err != nil {
		return nil, nil, err
	}
	return leaf, []*x509.Certificate{ca.Cert}, nil
}

// SignCSR is the compatibility entry point for enrollment callers not yet carrying a trust domain.
func (ca *CA) SignCSR(csrDER []byte, nodeID, accountID, role string, notAfter time.Time) (*x509.Certificate, []*x509.Certificate, error) {
	return ca.SignNodeCSR(csrDER, nodeID, accountID, role, DefaultTrustDomain, notAfter)
}
