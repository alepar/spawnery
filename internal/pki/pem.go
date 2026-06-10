package pki

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

// MarshalCertPEM encodes a certificate as PEM.
func MarshalCertPEM(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

// MarshalKeyPEM encodes an EC private key as PKCS#8 PEM.
func MarshalKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParseCertPEM decodes a single PEM-encoded certificate.
func ParseCertPEM(b []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("pki: no CERTIFICATE PEM block")
	}
	return x509.ParseCertificate(blk.Bytes)
}

// ParseKeyPEM decodes a PKCS#8 PEM-encoded EC private key.
func ParseKeyPEM(b []byte) (*ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("pki: no PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("pki: key is %T, want *ecdsa.PrivateKey", k)
	}
	return ec, nil
}

// TLSCertificate builds an mTLS identity (leaf + intermediate chain + private key) for dial/serve.
func (n *Node) TLSCertificate() (tls.Certificate, error) {
	if n.Key == nil {
		return tls.Certificate{}, errors.New("pki: node has no private key")
	}
	raw := [][]byte{n.Cert.Raw}
	for _, c := range n.Chain {
		raw = append(raw, c.Raw)
	}
	return tls.Certificate{Certificate: raw, PrivateKey: n.Key, Leaf: n.Cert}, nil
}
