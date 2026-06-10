// Package nodeid persists a node's enrolled mTLS identity (leaf cert + chain + key + the pinned CP
// root) and builds the HTTP/2 client a node uses to dial the CP over mTLS in enforced mode (sp-ova
// design §3.3/§5). insecure mode does not use this — it keeps the plaintext h2c client.
package nodeid

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"golang.org/x/net/http2"
)

// Identity is a node's on-disk mTLS material (all PEM).
type Identity struct {
	CertPEM  []byte // the node leaf
	ChainPEM []byte // intermediate(s)
	KeyPEM   []byte // the node private key
	RootPEM  []byte // the pinned CP root CA (out-of-band; used to verify the CP server)
}

const (
	certFile  = "cert.pem"
	chainFile = "chain.pem"
	keyFile   = "key.pem"
	rootFile  = "root.pem"
)

// Save writes the identity to dir (key mode 0600).
func Save(dir string, id Identity) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{certFile, id.CertPEM, 0o644},
		{chainFile, id.ChainPEM, 0o644},
		{keyFile, id.KeyPEM, 0o600},
		{rootFile, id.RootPEM, 0o644},
	} {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.data, f.mode); err != nil {
			return err
		}
	}
	return nil
}

// Load reads an identity previously written by Save.
func Load(dir string) (Identity, error) {
	read := func(n string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, n)) }
	var id Identity
	var err error
	if id.CertPEM, err = read(certFile); err != nil {
		return Identity{}, err
	}
	if id.ChainPEM, err = read(chainFile); err != nil {
		return Identity{}, err
	}
	if id.KeyPEM, err = read(keyFile); err != nil {
		return Identity{}, err
	}
	if id.RootPEM, err = read(rootFile); err != nil {
		return Identity{}, err
	}
	return id, nil
}

// MTLSClient builds an HTTP/2 client that presents the node's client certificate (leaf + chain) and
// verifies the CP server against the pinned root.
func (id Identity) MTLSClient() (*http.Client, error) {
	chain := append(append([]byte{}, id.CertPEM...), id.ChainPEM...)
	cert, err := tls.X509KeyPair(chain, id.KeyPEM)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(id.RootPEM) {
		return nil, errors.New("nodeid: no usable certificate in pinned root PEM")
	}
	return &http.Client{
		Transport: &http2.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      pool,
				MinVersion:   tls.VersionTLS12,
			},
		},
	}, nil
}
