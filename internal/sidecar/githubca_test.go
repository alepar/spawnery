package sidecar

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"sync"
	"testing"
	"time"
)

// makeTestCA generates a throwaway ECDSA-P256 CA matching the format the node produces
// (EC PRIVATE KEY + CERTIFICATE PEM blocks), exactly like internal/node/githubcontrol.go generateCA.
func makeTestCA(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("makeTestCA: generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("makeTestCA: serial: %v", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "test-spawn-CA"},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(30 * 24 * time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("makeTestCA: create cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("makeTestCA: marshal key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return
}

func TestParseSpawnCA(t *testing.T) {
	certPEM, keyPEM := makeTestCA(t)

	ca, err := parseSpawnCA(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parseSpawnCA: %v", err)
	}
	if ca.cert == nil {
		t.Fatal("ca.cert is nil")
	}
	if ca.key == nil {
		t.Fatal("ca.key is nil")
	}
}

func TestParseSpawnCA_BadPEM(t *testing.T) {
	certPEM, keyPEM := makeTestCA(t)

	if _, err := parseSpawnCA([]byte("not pem"), keyPEM); err == nil {
		t.Error("expected error for bad cert PEM")
	}
	if _, err := parseSpawnCA(certPEM, []byte("not pem")); err == nil {
		t.Error("expected error for bad key PEM")
	}
	if _, err := parseSpawnCA(nil, keyPEM); err == nil {
		t.Error("expected error for nil cert PEM")
	}
}

func TestLeafFor_Properties(t *testing.T) {
	certPEM, keyPEM := makeTestCA(t)
	ca, err := parseSpawnCA(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parseSpawnCA: %v", err)
	}

	host := "github.com"
	leaf, err := ca.leafFor(host)
	if err != nil {
		t.Fatalf("leafFor: %v", err)
	}

	// (a) chains to the CA
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	_, verifyErr := leaf.Leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if verifyErr != nil {
		t.Errorf("leaf does not chain to CA: %v", verifyErr)
	}

	// (b) SAN matches host
	if len(leaf.Leaf.DNSNames) != 1 || leaf.Leaf.DNSNames[0] != host {
		t.Errorf("DNSNames = %v, want [%s]", leaf.Leaf.DNSNames, host)
	}

	// (c) ExtKeyUsage contains ServerAuth
	hasServerAuth := false
	for _, eku := range leaf.Leaf.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			hasServerAuth = true
			break
		}
	}
	if !hasServerAuth {
		t.Error("leaf missing ExtKeyUsageServerAuth")
	}

	// (d) public key is ECDSA P-256
	ecKey, ok := leaf.Leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("public key is %T, want *ecdsa.PublicKey", leaf.Leaf.PublicKey)
	}
	if ecKey.Curve != elliptic.P256() {
		t.Errorf("curve is %v, want P-256", ecKey.Curve)
	}

	// (e) validity window
	now := time.Now()
	if leaf.Leaf.NotAfter.Before(now) {
		t.Errorf("NotAfter %v is in the past", leaf.Leaf.NotAfter)
	}
	if leaf.Leaf.NotBefore.After(now) {
		t.Errorf("NotBefore %v is in the future", leaf.Leaf.NotBefore)
	}
}

func TestLeafFor_Cache(t *testing.T) {
	certPEM, keyPEM := makeTestCA(t)
	ca, err := parseSpawnCA(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parseSpawnCA: %v", err)
	}

	leaf1, err := ca.leafFor("github.com")
	if err != nil {
		t.Fatalf("first leafFor: %v", err)
	}
	leaf2, err := ca.leafFor("github.com")
	if err != nil {
		t.Fatalf("second leafFor: %v", err)
	}
	// Same host → same pointer (cached)
	if leaf1 != leaf2 {
		t.Error("expected cached pointer identity for same host")
	}

	// Different host → different cert
	leafOther, err := ca.leafFor("api.github.com")
	if err != nil {
		t.Fatalf("leafFor api.github.com: %v", err)
	}
	if leafOther == leaf1 {
		t.Error("expected different pointer for different host")
	}
}

func TestLeafFor_Concurrent(t *testing.T) {
	certPEM, keyPEM := makeTestCA(t)
	ca, err := parseSpawnCA(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parseSpawnCA: %v", err)
	}

	const goroutines = 50
	results := make([]*tls.Certificate, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		i := i
		go func() {
			defer wg.Done()
			c, err := ca.leafFor("github.com")
			if err != nil {
				t.Errorf("goroutine %d: leafFor error: %v", i, err)
				return
			}
			results[i] = c
		}()
	}
	wg.Wait()

	// All goroutines must get the same cached pointer.
	for i, c := range results {
		if c == nil {
			continue // error already logged
		}
		if c != results[0] {
			t.Errorf("goroutine %d: got different pointer (expected cache hit)", i)
		}
	}
}
