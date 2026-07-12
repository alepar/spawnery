package sidecar

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file is sp-wwtc.3's negative test: proof that the sidecar's STRICT upstream transport
// (newDefaultUpstreamTransport — unmodified, no InsecureSkipVerify, Proxy=nil, HTTP/1.1 only) really
// does trust the golden CA via a MERGED SSL_CERT_FILE bundle, and really does reject the same
// upstream once that CA is removed from the bundle. Without this, "wire SSL_CERT_FILE in the e2e/VM
// profile" would be an unverified claim.
//
// Go caches the process-wide system root pool behind a sync.Once (crypto/x509/root_unix.go): once
// any code path in this process resolves a nil-RootCAs tls.Config (which newDefaultUpstreamTransport
// does), the SAME pool is reused for the rest of the process regardless of a later SSL_CERT_FILE
// change. Flipping SSL_CERT_FILE between two sub-tests in one process would therefore silently reuse
// whichever bundle was read first — so each case below re-execs this test binary as a subprocess
// (the standard os/exec-test "helper process" pattern), giving each SSL_CERT_FILE value its own,
// untouched process.

// mintTestGoldenCAAndLeaf mints a throwaway CA (standing in for the VM's golden CA) and a leaf cert
// for 127.0.0.1 signed by it (standing in for Gitea's github.com cert, sp-wwtc.1). Returns the CA
// cert PEM (what an SSL_CERT_FILE bundle would need to include) and the tls.Certificate the test
// upstream server presents.
func mintTestGoldenCAAndLeaf(t *testing.T) (caPEM []byte, leaf tls.Certificate) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caSerial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("CA serial: %v", err)
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "test-golden-CA"},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(30 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	leafSerial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("leaf serial: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: leafSerial,
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	leaf = tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
	return caPEM, leaf
}

// runHelperUpstreamFetch re-execs this test binary as a subprocess (GO_WANT_HELPER_PROCESS=1) with
// SSL_CERT_FILE=sslCertFile, and has it perform a real HTTPS GET of targetURL through
// newDefaultUpstreamTransport() — the sidecar's actual, unmodified upstream transport. Returns the
// combined output and the process error (non-nil on any failure, including a TLS verification error).
func runHelperUpstreamFetch(t *testing.T, sslCertFile, targetURL string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperUpstreamFetch", "-test.v") //nolint:gosec
	cmd.Env = append(os.Environ(),
		"GO_WANT_HELPER_PROCESS=1",
		"SSL_CERT_FILE="+sslCertFile,
		"HELPER_TARGET_URL="+targetURL,
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestHelperUpstreamFetch is NOT a real test — it is a no-op under a normal `go test` run (the
// GO_WANT_HELPER_PROCESS gate below returns immediately) and only does real work when re-exec'd by
// runHelperUpstreamFetch, exactly the os/exec_test.go "helper process" idiom. It fetches
// HELPER_TARGET_URL using the sidecar's real newDefaultUpstreamTransport() and reports success/
// failure via exit code + stderr, so the parent process can assert on both.
func TestHelperUpstreamFetch(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	defer os.Exit(0) // this process's own PASS/FAIL is irrelevant; only the exit code below matters

	tr := newDefaultUpstreamTransport()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get(os.Getenv("HELPER_TARGET_URL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "FETCH-ERROR:", err)
		os.Exit(1)
	}
	_ = resp.Body.Close()
	fmt.Println("FETCH-OK", resp.StatusCode)
	os.Exit(0)
}

// TestUpstreamTransportTrustsSSLCertFileBundle is sp-wwtc.3's deliverable: proof (not assertion)
// that the sidecar's strict, UNMODIFIED upstream transport trusts an upstream whose cert chains to a
// CA present in the SSL_CERT_FILE bundle — and the NEGATIVE property required by the bead: with that
// CA removed from the bundle, the identical transport against the identical upstream fails with a
// certificate error. This is what makes "wire a merged SSL_CERT_FILE bundle in the e2e/VM profile"
// more than an unverified claim.
func TestUpstreamTransportTrustsSSLCertFileBundle(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		t.Skip("helper-process invocation; see TestHelperUpstreamFetch")
	}

	goldenCAPEM, leaf := mintTestGoldenCAAndLeaf(t)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	srv.StartTLS()
	defer srv.Close()

	dir := t.TempDir()

	// An unrelated CA stands in for "system roots" — proves the bundle must be a MERGE (§4.1: the
	// golden CA ALONE is not the point; SSL_CERT_FILE replaces the pool, so what matters is whether
	// the golden CA is present in whatever bundle is supplied), not that any non-empty file passes.
	fillerPEM, _ := makeTestCA(t)

	withGoldenCA := filepath.Join(dir, "with-golden-ca.crt")
	merged := append(append([]byte{}, fillerPEM...), goldenCAPEM...)
	if err := os.WriteFile(withGoldenCA, merged, 0o644); err != nil {
		t.Fatal(err)
	}

	withoutGoldenCA := filepath.Join(dir, "without-golden-ca.crt")
	if err := os.WriteFile(withoutGoldenCA, fillerPEM, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("merged bundle WITH the golden CA: upstream trust succeeds", func(t *testing.T) {
		out, err := runHelperUpstreamFetch(t, withGoldenCA, srv.URL)
		if err != nil {
			t.Fatalf("expected the strict upstream transport to trust the merged bundle; err=%v\n%s", err, out)
		}
		if !strings.Contains(out, "FETCH-OK") {
			t.Errorf("expected FETCH-OK in helper output, got:\n%s", out)
		}
	})

	t.Run("bundle WITHOUT the golden CA: upstream trust fails with a certificate error (negative test)", func(t *testing.T) {
		out, err := runHelperUpstreamFetch(t, withoutGoldenCA, srv.URL)
		if err == nil {
			t.Fatalf("expected the strict upstream transport to REJECT the upstream once the golden CA is removed from the bundle; helper succeeded:\n%s", out)
		}
		if !strings.Contains(out, "FETCH-ERROR") {
			t.Errorf("expected a FETCH-ERROR line in helper output, got:\n%s", out)
		}
		lower := strings.ToLower(out)
		if !strings.Contains(lower, "certificate") && !strings.Contains(lower, "x509") {
			t.Errorf("expected a certificate/x509 verification error, got:\n%s", out)
		}
	})
}
