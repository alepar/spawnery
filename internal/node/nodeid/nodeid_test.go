package nodeid_test

import (
	"bytes"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"spawnery/internal/node/nodeid"
	"spawnery/internal/pki"
)

func makeIdentity(t *testing.T) nodeid.Identity {
	t.Helper()
	root, _ := pki.NewRootCA("R")
	inter, _ := root.NewIntermediate(pki.ClassSelfHosted)
	node, _ := inter.IssueNode("n1", "acct1", pki.ClassSelfHosted, time.Now().Add(time.Hour))
	keyPEM, _ := pki.MarshalKeyPEM(node.Key)
	return nodeid.Identity{
		CertPEM:  pki.MarshalCertPEM(node.Cert),
		ChainPEM: pki.MarshalCertPEM(inter.Cert),
		KeyPEM:   keyPEM,
		RootPEM:  pki.MarshalCertPEM(root.Cert),
	}
}

// The node's enrolled identity persists to disk and loads back intact.
func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	id := makeIdentity(t)
	if err := nodeid.Save(dir, id); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := nodeid.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Equal(got.CertPEM, id.CertPEM) || !bytes.Equal(got.KeyPEM, id.KeyPEM) ||
		!bytes.Equal(got.ChainPEM, id.ChainPEM) || !bytes.Equal(got.RootPEM, id.RootPEM) {
		t.Fatal("loaded identity does not match saved")
	}
}

// MTLSClient builds an HTTP/2 client carrying the node's client cert + pinning the CP root.
func TestMTLSClientWiring(t *testing.T) {
	c, err := makeIdentity(t).MTLSClient()
	if err != nil {
		t.Fatalf("MTLSClient: %v", err)
	}
	tr, ok := c.Transport.(*http2.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http2.Transport", c.Transport)
	}
	if n := len(tr.TLSClientConfig.Certificates); n != 1 {
		t.Fatalf("client certs = %d, want 1 (the node's mTLS identity)", n)
	}
	if tr.TLSClientConfig.RootCAs == nil {
		t.Fatal("RootCAs not pinned (the CP root)")
	}
}
