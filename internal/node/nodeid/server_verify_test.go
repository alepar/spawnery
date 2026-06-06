package nodeid_test

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"spawnery/internal/node/nodeid"
	"spawnery/internal/pki"
)

// tlsServer stands up an HTTP/2 TLS server presenting srv's cert (the "CP" side of mTLS).
func tlsServer(t *testing.T, srv *pki.Node) *httptest.Server {
	t.Helper()
	tc, err := srv.TLSCertificate()
	if err != nil {
		t.Fatalf("server TLSCertificate: %v", err)
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ts.EnableHTTP2 = true
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{tc}}
	ts.StartTLS()
	return ts
}

// The OTHER direction of mTLS: a node verifies the CP's SERVER cert against its pinned root. It trusts a
// CP whose cert chains to that root, and REJECTS one served from a foreign root (defeating a rogue CP /
// MITM that can't produce a server cert under the pinned root).
func TestMTLSClientVerifiesServerAgainstPinnedRoot(t *testing.T) {
	root, _ := pki.NewRootCA("Pinned Root")
	inter, _ := root.NewIntermediate(pki.ClassSelfHosted)
	node, _ := inter.IssueNode("n", "a", pki.ClassSelfHosted, time.Now().Add(time.Hour))
	keyPEM, _ := pki.MarshalKeyPEM(node.Key)
	client, err := nodeid.Identity{
		CertPEM:  pki.MarshalCertPEM(node.Cert),
		ChainPEM: pki.MarshalCertPEM(inter.Cert),
		KeyPEM:   keyPEM,
		RootPEM:  pki.MarshalCertPEM(root.Cert), // the node pins THIS root
	}.MTLSClient()
	if err != nil {
		t.Fatalf("MTLSClient: %v", err)
	}

	ips := []net.IP{net.ParseIP("127.0.0.1")}
	hour := time.Now().Add(time.Hour)

	// Good CP: server cert from the SAME (pinned) root -> the node trusts it.
	goodSrv, _ := root.IssueServer("cp", []string{"localhost"}, ips, hour)
	good := tlsServer(t, goodSrv)
	defer good.Close()
	resp, err := client.Get(good.URL)
	if err != nil {
		t.Fatalf("node should trust a CP whose server cert chains to its pinned root: %v", err)
	}
	_ = resp.Body.Close()

	// Bad CP: server cert from a DIFFERENT root -> the node must reject the connection.
	otherRoot, _ := pki.NewRootCA("Other Root")
	badSrv, _ := otherRoot.IssueServer("cp", []string{"localhost"}, ips, hour)
	bad := tlsServer(t, badSrv)
	defer bad.Close()
	if _, err := client.Get(bad.URL); err == nil {
		t.Fatal("node must REJECT a CP whose server cert is from a foreign root")
	}
}
