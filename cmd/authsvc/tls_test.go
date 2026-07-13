package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/gen/auth/v1/authv1connect"
	"spawnery/internal/authsvc"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/node/nodeid"
	"spawnery/internal/pki"
)

// This file proves the OPTIONAL TLS listener wired in main() (buildTLSConfig + the
// ListenAndServeTLS branch) — not a test-constructed tls.Config — makes nodeIdentityMiddleware
// (internal/authsvc/node_identity.go, untouched) reachable end to end: a real *http.Server, built
// exactly the way cmd/authsvc's main() builds it, serving TLS on a real net.Listener.

// tlsWireProvider is a authsvc.GitHubProvider that fails loudly if GitHub is ever called — the
// wire tests seed a FRESH (non-expired) access token so the AS dedups without needing a refresh.
type tlsWireProvider struct {
	mu      sync.Mutex
	refresh int
}

func (p *tlsWireProvider) AuthorizeURL(string, string, string) string { return "" }
func (p *tlsWireProvider) Exchange(context.Context, string, string, string) (string, error) {
	return "", errors.New("Exchange must not be called in the tls wire tests")
}
func (p *tlsWireProvider) FetchUser(context.Context, string) (authsvc.GitHubUser, error) {
	return authsvc.GitHubUser{}, errors.New("FetchUser must not be called in the tls wire tests")
}
func (p *tlsWireProvider) RefreshUserAccessToken(context.Context, string) (authsvc.GitHubUserToken, error) {
	p.mu.Lock()
	p.refresh++
	p.mu.Unlock()
	return authsvc.GitHubUserToken{}, errors.New("RefreshUserAccessToken must not be called for a fresh token")
}

// capturedMintAuthz records the last node identity the AS extracted from a presented client cert
// and asked to be authorized — proving identity flowed through the real TLS handshake and into
// nodeIdentityMiddleware, not a stub.
type capturedMintAuthz struct {
	mu   sync.Mutex
	last authsvc.GitHubMintAuthorization
	hits int
}

func (c *capturedMintAuthz) record(a authsvc.GitHubMintAuthorization) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last, c.hits = a, c.hits+1
}

func (c *capturedMintAuthz) snapshot() (authsvc.GitHubMintAuthorization, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last, c.hits
}

const (
	tlsWireNodeID    = "tls-wire-node"
	tlsWireAccountID = "tls-wire-account"
	tlsWireSecretID  = "tls-wire-secret"
	tlsWireDelivery  = "github-access-tls-wire-secret-v1"
)

// tlsWire is a live AS reachable over real TLS, wired through cmd/authsvc's OWN buildTLSConfig +
// http.Server{TLSConfig: ...}.ServeTLS — the exact code path main() runs, not a test-rolled
// tls.Config. It exposes clients for the three presentation cases: a valid node cert, no cert, and
// an untrusted cert.
type tlsWire struct {
	addr  string
	store store.Store
	authz *capturedMintAuthz

	serverRootPEM []byte // PEM of the AS root, for building extra clients that trust the server

	nodeClient authv1connect.AuthServiceClient // presents a valid, root-chained node cert
	noCert     *http.Client                    // presents no client cert
}

func newTLSWire(t *testing.T) *tlsWire {
	t.Helper()

	root, err := pki.NewRootCA("tls-wire root")
	if err != nil {
		t.Fatalf("NewRootCA: %v", err)
	}
	selfHosted, err := root.NewIntermediate(pki.ClassSelfHosted)
	if err != nil {
		t.Fatalf("NewIntermediate: %v", err)
	}
	hour := time.Now().Add(time.Hour)
	nodeLeaf, err := selfHosted.IssueNode(tlsWireNodeID, tlsWireAccountID, pki.ClassSelfHosted, hour)
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}
	serverCert, err := root.IssueServer("tls-wire-as", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, hour)
	if err != nil {
		t.Fatalf("IssueServer: %v", err)
	}

	dir := t.TempDir()
	certPath := filepath.Join(dir, "server-cert.pem")
	keyPath := filepath.Join(dir, "server-key.pem")
	clientCAPath := filepath.Join(dir, "client-ca.pem")
	serverKeyPEM, err := pki.MarshalKeyPEM(serverCert.Key)
	if err != nil {
		t.Fatalf("MarshalKeyPEM (server): %v", err)
	}
	if err := os.WriteFile(certPath, pki.MarshalCertPEM(serverCert.Cert), 0o600); err != nil {
		t.Fatalf("write server cert: %v", err)
	}
	if err := os.WriteFile(keyPath, serverKeyPEM, 0o600); err != nil {
		t.Fatalf("write server key: %v", err)
	}
	if err := os.WriteFile(clientCAPath, pki.MarshalCertPEM(root.Cert), 0o600); err != nil {
		t.Fatalf("write client ca: %v", err)
	}

	st := store.NewTestStore(t)
	authz := &capturedMintAuthz{}
	provider := &tlsWireProvider{}
	svc := authsvc.New(root.Cert, selfHosted,
		authsvc.WithGitHubMinting(st, provider),
		authsvc.WithGitHubMintAuthorizer(authsvc.GitHubMintAuthorizerFunc(func(_ context.Context, a authsvc.GitHubMintAuthorization) error {
			authz.record(a)
			return nil
		})),
		authsvc.WithGitHubTokenRotatedNotifier(authsvc.GitHubTokenRotatedNotifierFunc(func(context.Context, authsvc.GitHubTokenRotatedSignal) error {
			return nil
		})),
	)

	// THE thing under test: cmd/authsvc's own buildTLSConfig, from an AS{} shaped exactly like a
	// real AS_TLS_CERT/AS_TLS_KEY/AS_CLIENT_CA deployment.
	cfg := &AS{}
	cfg.TLS.Cert = certPath
	cfg.TLS.Key = keyPath
	cfg.TLS.ClientCA = clientCAPath
	tlsConfig, err := buildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if tlsConfig == nil {
		t.Fatalf("buildTLSConfig returned nil with cert+key set")
	}
	if tlsConfig.ClientAuth != tls.VerifyClientCertIfGiven {
		t.Fatalf("ClientAuth = %v, want VerifyClientCertIfGiven", tlsConfig.ClientAuth)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: svc.Handler(), TLSConfig: tlsConfig}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })

	rootPool := x509.NewCertPool()
	rootPool.AddCert(root.Cert)

	keyPEM, err := pki.MarshalKeyPEM(nodeLeaf.Key)
	if err != nil {
		t.Fatalf("MarshalKeyPEM (node): %v", err)
	}
	mtlsHTTP, err := nodeid.Identity{
		CertPEM:  pki.MarshalCertPEM(nodeLeaf.Cert),
		ChainPEM: pki.MarshalCertPEM(selfHosted.Cert),
		KeyPEM:   keyPEM,
		RootPEM:  pki.MarshalCertPEM(root.Cert),
	}.MTLSClient()
	if err != nil {
		t.Fatalf("MTLSClient: %v", err)
	}

	addr := ln.Addr().String()
	noCertClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: rootPool},
	}}

	return &tlsWire{
		addr:          addr,
		store:         st,
		authz:         authz,
		serverRootPEM: pki.MarshalCertPEM(root.Cert),
		nodeClient:    authv1connect.NewAuthServiceClient(mtlsHTTP, "https://"+addr),
		noCert:        noCertClient,
	}
}

func (w *tlsWire) seedFreshLink(t *testing.T) {
	t.Helper()
	now := time.Now()
	if err := w.store.GitHubLinks().Upsert(context.Background(), store.GitHubLink{
		SecretID:             tlsWireSecretID,
		AccountID:            tlsWireAccountID,
		Host:                 "github.com",
		Login:                "tls-wire-user",
		GithubUserID:         "1",
		AppClientID:          "Iv1.tls-wire",
		RefreshToken:         "ghr_unused",
		RefreshExpiresAtUnix: now.Add(180 * 24 * time.Hour).Unix(),
		AccessToken:          "ghu_fresh",
		AccessExpiresAtUnix:  now.Add(2 * time.Hour).Unix(),
		TokenType:            "bearer",
		Version:              1,
		DeliveryID:           tlsWireDelivery,
		UpdatedAt:            now.Unix(),
	}); err != nil {
		t.Fatalf("seed github link: %v", err)
	}
}

func tlsWireMintReq() *authv1.MintGitHubAccessTokenRequest {
	return &authv1.MintGitHubAccessTokenRequest{
		RequestId:  "tls-wire-mint-" + tlsWireSecretID,
		SpawnId:    "sp-tls-wire",
		Generation: 1,
		LinkRef: &authv1.GitHubLinkRef{
			SecretId:   tlsWireSecretID,
			Version:    1,
			DeliveryId: tlsWireDelivery,
		},
	}
}

// TestTLSWire_ValidNodeCert_IdentityVerifiedThroughRealServer proves the acceptance criterion: with
// AS_TLS_CERT/AS_TLS_KEY/AS_CLIENT_CA set, a node presenting a valid client cert gets a verified
// identity through the REAL server setup (cmd/authsvc's own buildTLSConfig + ListenAndServeTLS
// code path), not a test-constructed server.
func TestTLSWire_ValidNodeCert_IdentityVerifiedThroughRealServer(t *testing.T) {
	w := newTLSWire(t)
	w.seedFreshLink(t)

	resp, err := w.nodeClient.MintGitHubAccessToken(context.Background(), connect.NewRequest(tlsWireMintReq()))
	if err != nil {
		t.Fatalf("mint over real TLS with a valid node cert: %v", err)
	}
	if resp.Msg.GetAccessToken() != "ghu_fresh" {
		t.Fatalf("access token = %q, want the seeded fresh token", resp.Msg.GetAccessToken())
	}

	authz, hits := w.authz.snapshot()
	if hits == 0 || authz.NodeID != tlsWireNodeID {
		t.Fatalf("authz NodeID = %q hits=%d, want %q — the client cert's identity did not reach the AS", authz.NodeID, hits, tlsWireNodeID)
	}
}

// TestTLSWire_NoCert_AnonymousRouteStillCompletes proves browsers/CLIs (no client cert) are not
// broken by turning TLS on: a non-node-identity route completes normally.
func TestTLSWire_NoCert_AnonymousRouteStillCompletes(t *testing.T) {
	w := newTLSWire(t)

	resp, err := w.noCert.Get("https://" + w.addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz with no client cert: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("GET /healthz body = %q, want %q", body, "ok")
	}
}

// TestTLSWire_NoCert_RejectedFromNodeIdentityGatedRoute is THE non-negotiable assertion (design
// decision 4.1 / bd sp-hsqs constraint 2): VerifyClientCertIfGiven widens who may CONNECT, never
// who is TRUSTED. An anonymous (no client cert) caller must be rejected from a node-identity-gated
// route — here, MintGitHubAccessToken, the only route in the codebase gated on
// nodeIdentityExtractor. If this test ever passes with authz hits > 0, VerifyClientCertIfGiven has
// become a privilege escalation.
func TestTLSWire_NoCert_RejectedFromNodeIdentityGatedRoute(t *testing.T) {
	w := newTLSWire(t)
	w.seedFreshLink(t)

	noCertConnect := authv1connect.NewAuthServiceClient(w.noCert, "https://"+w.addr)
	_, err := noCertConnect.MintGitHubAccessToken(context.Background(), connect.NewRequest(tlsWireMintReq()))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("mint with no client cert: code = %v err = %v, want Unauthenticated", connect.CodeOf(err), err)
	}
	if _, hits := w.authz.snapshot(); hits != 0 {
		t.Fatalf("PRIVILEGE ESCALATION: AS authorized a mint with no presented client cert (authz hits = %d)", hits)
	}
}

// TestTLSWire_UntrustedCert_HandshakeFails proves a client presenting a cert that does NOT chain to
// tls.client_ca fails the TLS HANDSHAKE outright — it must never be allowed to "fall back" to
// connecting anonymously.
func TestTLSWire_UntrustedCert_HandshakeFails(t *testing.T) {
	w := newTLSWire(t)

	// A second, entirely unrelated CA — its node cert is well-formed but chains to nobody the AS
	// trusts.
	rogueRoot, err := pki.NewRootCA("rogue root")
	if err != nil {
		t.Fatalf("NewRootCA (rogue): %v", err)
	}
	rogueInter, err := rogueRoot.NewIntermediate(pki.ClassSelfHosted)
	if err != nil {
		t.Fatalf("NewIntermediate (rogue): %v", err)
	}
	rogueLeaf, err := rogueInter.IssueNode("rogue-node", "rogue-account", pki.ClassSelfHosted, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("IssueNode (rogue): %v", err)
	}
	rogueKeyPEM, err := pki.MarshalKeyPEM(rogueLeaf.Key)
	if err != nil {
		t.Fatalf("MarshalKeyPEM (rogue): %v", err)
	}
	rogueChain := append(append([]byte{}, pki.MarshalCertPEM(rogueLeaf.Cert)...), pki.MarshalCertPEM(rogueInter.Cert)...)
	rogueCert, err := tls.X509KeyPair(rogueChain, rogueKeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair (rogue): %v", err)
	}
	serverTrust := x509.NewCertPool()
	if !serverTrust.AppendCertsFromPEM(w.serverRootPEM) {
		t.Fatal("no usable cert in serverRootPEM")
	}
	// A real MITM/attacker client does not politely honor the server's CertificateRequest CA
	// hints — but Go's crypto/tls, given a plain tls.Config.Certificates list, silently omits an
	// unacceptable cert instead of sending it (SupportsCertificate filtering). GetClientCertificate
	// bypasses that filtering and forces presentation, so this test exercises the SERVER's
	// verification, not the client's good manners.
	rogueHTTP := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:              serverTrust, // trusts the real AS server cert
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &rogueCert, nil },
		},
	}}

	rogueConnect := authv1connect.NewAuthServiceClient(rogueHTTP, "https://"+w.addr)
	_, err = rogueConnect.MintGitHubAccessToken(context.Background(), connect.NewRequest(tlsWireMintReq()))
	if err == nil {
		t.Fatal("expected the TLS handshake to fail for an untrusted client cert, got a successful call")
	}
	// Must NOT be the graceful "connected anonymously, then application-level rejected" shape
	// (connect.CodeUnauthenticated, as in the no-cert test) — this has to be a hard transport/
	// handshake failure, proving the server never let the connection complete at all.
	if connect.CodeOf(err) == connect.CodeUnauthenticated {
		t.Fatalf("got Unauthenticated (connected anonymously) instead of a handshake failure: %v", err)
	}
	if _, hits := w.authz.snapshot(); hits != 0 {
		t.Fatalf("AS authorized a mint from an untrusted client cert (authz hits = %d)", hits)
	}
}

// writeSelfSignedForTest writes a throwaway server cert+key PEM pair into dir and returns their
// paths, for tests that only need buildTLSConfig to successfully load SOME certificate (i.e. tests
// not about trust chains).
func writeSelfSignedForTest(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	root, err := pki.NewRootCA("self-signed-test root")
	if err != nil {
		t.Fatalf("NewRootCA: %v", err)
	}
	serverCert, err := root.IssueServer("self-signed-test", []string{"localhost"}, nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("IssueServer: %v", err)
	}
	keyPEM, err := pki.MarshalKeyPEM(serverCert.Key)
	if err != nil {
		t.Fatalf("MarshalKeyPEM: %v", err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pki.MarshalCertPEM(serverCert.Cert), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}
