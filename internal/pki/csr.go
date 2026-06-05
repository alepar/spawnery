package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
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

// SignCSR verifies a CSR's self-signature (proof of possession) and issues a node leaf binding the
// CSR's public key to a CA-CHOSEN SAN (<nodeID>.<accountID>.<class>.Domain). Names requested inside the
// CSR are ignored — the caller (enrollment) supplies the authoritative nodeID/accountID/class.
func (ca *CA) SignCSR(csrDER []byte, nodeID, accountID, class string, notAfter time.Time) (*x509.Certificate, []*x509.Certificate, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parse csr: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, nil, fmt.Errorf("csr signature: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
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
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		return nil, nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return leaf, []*x509.Certificate{ca.Cert}, nil
}
