package nodeauth_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"spawnery/internal/authsvc"
	"spawnery/internal/clientverify"
	"spawnery/internal/cp/nodeauth"
	"spawnery/internal/pki"
	"spawnery/internal/sessiontoken"
)

// PKI-soundness e2e (sp-aad): a REAL mTLS handshake against the enforced-mode middleware (the same
// RequireAnyClientCert + middleware-verify topology cmd/cp uses) must ACCEPT a valid node cert and
// REJECT every bad one — forged cloud, expired, wrong root, and no cert.
func TestEnforcedMTLSEndToEnd(t *testing.T) {
	root, _ := pki.NewRootCA("Test Root")
	selfHosted, _ := root.NewIntermediate(pki.ClassSelfHosted)
	cloud, _ := root.NewIntermediate(pki.ClassCloud)
	otherRoot, _ := pki.NewRootCA("Other Root")
	otherInter, _ := otherRoot.NewIntermediate(pki.ClassSelfHosted)

	var mu sync.Mutex
	var lastID pki.Identity
	var reached bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastID, _ = nodeauth.IdentityFromContext(r.Context())
		reached = true
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	ts := httptest.NewUnstartedServer(nodeauth.Middleware(nodeauth.ModeEnforced, root.Cert, next))
	ts.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert} // present required; middleware verifies
	ts.StartTLS()
	defer ts.Close()

	// do connects presenting node's cert (or none if node is nil) and returns the HTTP status, or -1 if
	// the TLS handshake itself fails.
	do := func(node *pki.Node) int {
		mu.Lock()
		reached = false
		mu.Unlock()
		// Fresh transport per call (clone the trust of the httptest server cert; no keep-alive) so each
		// request opens a new connection presenting THIS node's cert — otherwise pooling reuses the first.
		tlsCfg := ts.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		if node != nil {
			tc, err := node.TLSCertificate()
			if err != nil {
				t.Fatalf("TLSCertificate: %v", err)
			}
			tlsCfg.Certificates = []tls.Certificate{tc}
		}
		tr := &http.Transport{TLSClientConfig: tlsCfg, DisableKeepAlives: true}
		defer tr.CloseIdleConnections()
		resp, err := (&http.Client{Transport: tr}).Get(ts.URL)
		if err != nil {
			return -1 // handshake refused (e.g. no client cert under RequireAnyClientCert)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	hour := time.Now().Add(time.Hour)

	// 1) valid self-hosted -> accepted, identity derived from the cert.
	good, _ := selfHosted.IssueNode("n1", "alice", pki.ClassSelfHosted, hour)
	if code := do(good); code != http.StatusOK || !reached || lastID.AccountID != "alice" || lastID.Class != pki.ClassSelfHosted {
		t.Fatalf("valid self-hosted: code=%d reached=%v id=%+v", code, reached, lastID)
	}
	// 2) valid cloud -> accepted.
	cnode, _ := cloud.IssueNode("c1", "spawnery-system", pki.ClassCloud, hour)
	if code := do(cnode); code != http.StatusOK || lastID.Class != pki.ClassCloud {
		t.Fatalf("valid cloud: code=%d id=%+v", code, lastID)
	}
	// 3) forged cloud (self-hosted intermediate, cloud SAN) -> rejected by name constraints.
	forged, _ := selfHosted.IssueNode("evil", "victim", pki.ClassCloud, hour)
	if code := do(forged); code != http.StatusUnauthorized || reached {
		t.Fatalf("forged cloud must be 401 and not reach handler: code=%d reached=%v", code, reached)
	}
	// 4) expired -> rejected.
	expired, _ := selfHosted.IssueNode("n", "a", pki.ClassSelfHosted, time.Now().Add(-time.Hour))
	if code := do(expired); code != http.StatusUnauthorized {
		t.Fatalf("expired cert must be 401: code=%d", code)
	}
	// 5) wrong root -> rejected.
	wrong, _ := otherInter.IssueNode("n", "a", pki.ClassSelfHosted, hour)
	if code := do(wrong); code != http.StatusUnauthorized {
		t.Fatalf("cert from a foreign root must be 401: code=%d", code)
	}
	// 6) no client cert -> rejected at the TLS layer.
	if code := do(nil); code != -1 {
		t.Fatalf("no client cert must fail the handshake: code=%d", code)
	}
}

// A forged session token (signed by a non-AS key, e.g. a compromised CP) is rejected, while the
// genuine AS-signed token verifies — the offline check a node performs.
func TestForgedSessionTokenRejectedE2E(t *testing.T) {
	root, _ := pki.NewRootCA("R")
	inter, _ := root.NewIntermediate(pki.ClassSelfHosted)
	as := authsvc.New(root.Cert, inter)

	tok, _ := as.IssueSessionToken(sessiontoken.Claims{SpawnID: "s", Owner: "alice", Node: "n", Exp: time.Now().Add(time.Hour)})
	if _, err := sessiontoken.Verify(tok, as.SessionPubKey(), time.Now()); err != nil {
		t.Fatalf("genuine AS token must verify: %v", err)
	}
	// A "compromised CP" mints its own token with a different key.
	_, cpKey, _ := ed25519.GenerateKey(rand.Reader)
	forged, _ := sessiontoken.Sign(sessiontoken.Claims{SpawnID: "s", Owner: "attacker", Exp: time.Now().Add(time.Hour)}, cpKey)
	if _, err := sessiontoken.Verify(forged, as.SessionPubKey(), time.Now()); err == nil {
		t.Fatal("a session token not signed by the AS key must be rejected")
	}

	// Bonus: clientverify accepts the AS-enrolled host node but rejects a foreign-account one.
	host, _ := inter.IssueNode("n", "alice", pki.ClassSelfHosted, time.Now().Add(time.Hour))
	leaf, chain, rootPEM := pki.MarshalCertPEM(host.Cert), pki.MarshalCertPEM(inter.Cert), pki.MarshalCertPEM(root.Cert)
	if _, err := clientverify.VerifyHost(leaf, chain, rootPEM, clientverify.Expectation{Tenancy: pki.ClassSelfHosted, AccountID: "alice"}, time.Now()); err != nil {
		t.Fatalf("alice's own host must verify: %v", err)
	}
	if _, err := clientverify.VerifyHost(leaf, chain, rootPEM, clientverify.Expectation{Tenancy: pki.ClassSelfHosted, AccountID: "bob"}, time.Now()); err == nil {
		t.Fatal("a host bound to alice must not satisfy bob")
	}
}
