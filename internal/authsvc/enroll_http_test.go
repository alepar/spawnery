package authsvc_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"spawnery/internal/authsvc"
	"spawnery/internal/mtls"
	"spawnery/internal/pki"
)

func TestRunEnrollWithKeyTLSAuthenticatesServerBeforeSendingCredentials(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, _ := pki.NewRootCA("root")
	serviceIssuer, _ := root.NewIntermediate(pki.IssuerService, "prod.spawnery.internal")
	asLeaf, _ := serviceIssuer.IssueService(pki.RoleAuthService, "as-1", "prod.spawnery.internal", []string{"authsvc.internal"}, []net.IP{net.ParseIP("127.0.0.1")}, now.Add(time.Hour))
	asTLS, _ := asLeaf.TLSCertificate()
	crl, _ := serviceIssuer.CreateCRL(big.NewInt(1), nil, now.Add(-time.Minute), now.Add(time.Hour))
	state, err := pki.OpenRevocationState(filepath.Join(t.TempDir(), "revocations", "state.json"), []*x509.Certificate{serviceIssuer.Cert}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	if err := state.ApplyPEM(pki.MarshalCRLPEM(crl)); err != nil {
		t.Fatal(err)
	}

	svc := newAS(t)
	key, _ := pki.NewNodeKey()
	fp, _ := pki.PublicKeyFingerprint(key.Public())
	token, _ := svc.IssueBoundEnrollmentToken("acct", pki.ClassSelfHosted, fp)
	var requests atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		svc.InternalHandler(mtls.Policy{"anonymous": {"authsvc.enroll": {}}}).ServeHTTP(w, r)
	})
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{Certificates: []tls.Certificate{asTLS}, MinVersion: tls.VersionTLS13}
	server.StartTLS()
	defer server.Close()

	transport := authsvc.EnrollTransport{Root: root.Cert, TrustDomain: "prod.spawnery.internal", ServerName: "authsvc.internal", IsRevoked: state.IsRevoked}
	if _, err := authsvc.RunEnrollWithKey(context.Background(), server.URL, token, "node-1", key, transport); err != nil {
		t.Fatalf("pinned enrollment: %v", err)
	}
	before := requests.Load()
	transport.ServerName = "wrong.internal"
	if _, err := authsvc.RunEnrollWithKey(context.Background(), server.URL, token, "node-1", key, transport); err == nil {
		t.Fatal("wrong AS server name was accepted")
	}
	if requests.Load() != before {
		t.Fatal("credentials were sent to a server rejected during TLS authentication")
	}
}
