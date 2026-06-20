package sidecar

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"
)

// spawnCA holds the parsed per-spawn CA certificate and private key, and a per-host cache of
// JIT-minted ECDSA-P256 leaf TLS certificates. It is the sidecar's only signing authority.
type spawnCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey

	mu    sync.Mutex
	cache map[string]*tls.Certificate // host -> leaf cert
}

// parseSpawnCA parses the node-delivered PEM-encoded CA certificate and EC private key.
// certPEM must be a CERTIFICATE block; keyPEM must be an EC PRIVATE KEY block.
func parseSpawnCA(certPEM, keyPEM []byte) (*spawnCA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, errors.New("parseSpawnCA: no CERTIFICATE PEM block")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parseSpawnCA: parse certificate: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, errors.New("parseSpawnCA: no EC PRIVATE KEY PEM block")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parseSpawnCA: parse EC private key: %w", err)
	}

	return &spawnCA{
		cert:  cert,
		key:   key,
		cache: make(map[string]*tls.Certificate),
	}, nil
}

// leafFor returns a *tls.Certificate for host, signed by the CA. It is cached per-host so the
// same pointer is returned on repeat calls (deterministic for the same host). The leaf:
//   - is ECDSA P-256 (fast off the hot path);
//   - has DNSNames == [host] (SAN);
//   - has ExtKeyUsage containing ExtKeyUsageServerAuth;
//   - chains to the CA cert (presentable as a combined [leaf, CA] chain);
//   - is safe to call concurrently (exactly one mint per host under the mutex).
func (ca *spawnCA) leafFor(host string) (*tls.Certificate, error) {
	ca.mu.Lock()
	if c, ok := ca.cache[host]; ok {
		ca.mu.Unlock()
		return c, nil
	}
	ca.mu.Unlock()

	// Mint outside the lock so concurrent requests for different hosts do not serialise
	// behind an expensive key generation. Re-check under the lock before caching.
	leaf, err := ca.mintLeaf(host)
	if err != nil {
		return nil, err
	}

	ca.mu.Lock()
	defer ca.mu.Unlock()
	if c, ok := ca.cache[host]; ok {
		// Another goroutine minted first; discard ours and use theirs.
		return c, nil
	}
	ca.cache[host] = leaf
	return leaf, nil
}

// mintLeaf generates a new ECDSA-P256 leaf cert for host, signed by ca.
func (ca *spawnCA) mintLeaf(host string) (*tls.Certificate, error) {
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("githubca: generate leaf key for %s: %w", host, err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("githubca: generate serial for %s: %w", host, err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: host,
		},
		DNSNames:    []string{host},
		NotBefore:   now.Add(-5 * time.Minute),
		NotAfter:    now.Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	leafDER, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &leafKey.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("githubca: sign leaf for %s: %w", host, err)
	}

	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return nil, fmt.Errorf("githubca: parse minted leaf for %s: %w", host, err)
	}

	caDER := ca.cert.Raw
	tlsCert := &tls.Certificate{
		Certificate: [][]byte{leafDER, caDER},
		PrivateKey:  leafKey,
		Leaf:        leaf,
	}
	return tlsCert, nil
}
