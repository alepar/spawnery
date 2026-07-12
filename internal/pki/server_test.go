package pki

import (
	"crypto/x509"
	"net"
	"testing"
	"time"
)

// IssueServer mints a TLS server cert (e.g. for the CP's node mTLS listener) with DNS/IP SANs, signed
// by the CA and chaining to the root — so a node dialing it verifies the server against the pinned root.
func TestIssueServer(t *testing.T) {
	root, _ := NewRootCA("R")
	srv, err := root.IssueServer("cp-node-listener", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("IssueServer: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(root.Cert)
	if _, err := srv.Cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost"}); err != nil {
		t.Fatalf("server cert should chain to root + match localhost: %v", err)
	}
	if _, err := srv.Cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: "127.0.0.1"}); err != nil {
		t.Fatalf("server cert should match the 127.0.0.1 IP SAN: %v", err)
	}
	hasServerAuth := false
	for _, eku := range srv.Cert.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			hasServerAuth = true
		}
	}
	if !hasServerAuth {
		t.Fatal("server cert missing ServerAuth EKU")
	}
}

func TestIssueServicePreservesServerNames(t *testing.T) {
	root, _ := NewRootCA("R")
	intermediate, err := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	if err != nil {
		t.Fatal(err)
	}
	service, err := intermediate.IssueService(RoleAuthService, "as-a", "prod.spawnery.internal", []string{"authsvc.internal"}, []net.IP{net.ParseIP("10.0.0.2")}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("IssueService: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(root.Cert)
	intermediates := x509.NewCertPool()
	intermediates.AddCert(intermediate.Cert)
	if _, err := service.Cert.Verify(x509.VerifyOptions{Roots: pool, Intermediates: intermediates, DNSName: "authsvc.internal"}); err != nil {
		t.Fatalf("service certificate does not preserve endpoint DNS SAN: %v", err)
	}
}
