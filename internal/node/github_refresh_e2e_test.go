//go:build github_e2e

package node

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http/httptest"
	"os"
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

// Fixed link/request identifiers shared across the sub-tests.
const (
	e2eSecretID   = "gh-e2e"
	e2eDeliveryID = "github-access-gh-e2e-v1"
	e2eSpawnID    = "sp-e2e"
	e2eNodeID     = "e2e-node"
	e2eAccountID  = "e2e-account"
)

// capturedAuthz records the LAST GitHubMintAuthorization the AS asked the CP to confirm. The NodeID
// here is the identity the AS extracted from the presented client cert via the real node-identity
// middleware — so asserting it proves node identity flowed end-to-end over the wire (invariant d).
type capturedAuthz struct {
	mu   sync.Mutex
	last authsvc.GitHubMintAuthorization
	hits int
}

func (c *capturedAuthz) record(a authsvc.GitHubMintAuthorization) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last, c.hits = a, c.hits+1
}

func (c *capturedAuthz) snapshot() (authsvc.GitHubMintAuthorization, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last, c.hits
}

// capturedSignal records the LAST CP-coordinated rotation signal the AS emitted. The struct carries
// only link-level metadata (no token field at all — compile-time token-free containment invariant).
type capturedSignal struct {
	mu   sync.Mutex
	last authsvc.GitHubTokenRotatedSignal
	hits int
}

func (c *capturedSignal) record(s authsvc.GitHubTokenRotatedSignal) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last, c.hits = s, c.hits+1
}

func (c *capturedSignal) snapshot() (authsvc.GitHubTokenRotatedSignal, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last, c.hits
}

// mintWire is a live AS reachable over real mTLS, plus a node-identity mTLS client (the production
// node.GitHubMintClient) and a no-client-cert client for the negative case.
type mintWire struct {
	store      store.Store
	nodeClient authv1connect.AuthServiceClient // presents the node cert (mTLS)
	noCert     authv1connect.AuthServiceClient // no client cert
	authz      *capturedAuthz
	signal     *capturedSignal
}

// newMintWire stands up the AS over an mTLS httptest server with the given (real or fake) GitHub
// provider, returning clients and capture handles. ClientAuth is VerifyClientCertIfGiven so BOTH the
// mTLS client (verified at TLS + re-verified by the AS middleware) and the no-cert client connect;
// the no-cert client then trips the AS's node-identity gate inside the handler.
func newMintWire(t *testing.T, provider authsvc.GitHubProvider) *mintWire {
	t.Helper()
	root, err := pki.NewRootCA("github-e2e root")
	if err != nil {
		t.Fatalf("NewRootCA: %v", err)
	}
	selfHosted, err := root.NewIntermediate(pki.ClassSelfHosted)
	if err != nil {
		t.Fatalf("NewIntermediate: %v", err)
	}
	hour := time.Now().Add(time.Hour)
	nodeLeaf, err := selfHosted.IssueNode(e2eNodeID, e2eAccountID, pki.ClassSelfHosted, hour)
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}
	serverCert, err := root.IssueServer("github-e2e-as", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, hour)
	if err != nil {
		t.Fatalf("IssueServer: %v", err)
	}
	serverTLS, err := serverCert.TLSCertificate()
	if err != nil {
		t.Fatalf("server TLSCertificate: %v", err)
	}

	st := store.NewTestStore(t)
	authz := &capturedAuthz{}
	signal := &capturedSignal{}
	as := authsvc.New(root.Cert, selfHosted,
		authsvc.WithGitHubMinting(st, provider),
		authsvc.WithGitHubMintAuthorizer(authsvc.GitHubMintAuthorizerFunc(func(_ context.Context, a authsvc.GitHubMintAuthorization) error {
			authz.record(a)
			return nil
		})),
		authsvc.WithGitHubTokenRotatedNotifier(authsvc.GitHubTokenRotatedNotifierFunc(func(_ context.Context, s authsvc.GitHubTokenRotatedSignal) error {
			signal.record(s)
			return nil
		})),
	)

	rootPool := x509.NewCertPool()
	rootPool.AddCert(root.Cert)
	ts := httptest.NewUnstartedServer(as.Handler())
	ts.EnableHTTP2 = true
	ts.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverTLS},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    rootPool,
	}
	ts.StartTLS()
	t.Cleanup(ts.Close)

	keyPEM, err := pki.MarshalKeyPEM(nodeLeaf.Key)
	if err != nil {
		t.Fatalf("MarshalKeyPEM: %v", err)
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

	return &mintWire{
		store:      st,
		nodeClient: authv1connect.NewAuthServiceClient(mtlsHTTP, ts.URL),
		noCert:     authv1connect.NewAuthServiceClient(ts.Client(), ts.URL),
		authz:      authz,
		signal:     signal,
	}
}

// seedLink writes a GitHub link the mint request references. A zero accessExpiresAtUnix / empty
// accessToken forces the AS down the real refresh path; a future expiry hits the fresh-token dedup.
func seedLink(t *testing.T, st store.Store, appClientID, refreshToken, accessToken string, accessExpiresAtUnix int64) {
	t.Helper()
	now := time.Now()
	if err := st.GitHubLinks().Upsert(context.Background(), store.GitHubLink{
		SecretID:             e2eSecretID,
		AccountID:            e2eAccountID,
		Host:                 "github.com",
		Login:                "e2e-user",
		GithubUserID:         "1",
		AppClientID:          appClientID,
		RefreshToken:         refreshToken,
		RefreshExpiresAtUnix: now.Add(180 * 24 * time.Hour).Unix(),
		AccessToken:          accessToken,
		AccessExpiresAtUnix:  accessExpiresAtUnix,
		TokenType:            "bearer",
		Version:              1,
		DeliveryID:           e2eDeliveryID,
		UpdatedAt:            now.Unix(),
	}); err != nil {
		t.Fatalf("seed github link: %v", err)
	}
}

func e2eMintReq() *authv1.MintGitHubAccessTokenRequest {
	return &authv1.MintGitHubAccessTokenRequest{
		RequestId:    "e2e-mint-" + e2eSecretID,
		SpawnId:      e2eSpawnID,
		Generation:   1,
		RepositoryId: "", // installation-selection is the scope; repository_id is audit-only (invariant e)
		LinkRef: &authv1.GitHubLinkRef{
			SecretId:   e2eSecretID,
			Version:    1,
			DeliveryId: e2eDeliveryID,
		},
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// countingProvider is an authsvc.GitHubProvider that fails loudly if anything calls GitHub — used by
// the fresh-token dedup sub-test to PROVE the AS short-circuited before any network call.
type countingProvider struct {
	mu      sync.Mutex
	refresh int
}

func (p *countingProvider) AuthorizeURL(string, string, string) string { return "" }
func (p *countingProvider) Exchange(context.Context, string, string, string) (string, error) {
	return "", errors.New("Exchange must not be called in the dedup sub-test")
}
func (p *countingProvider) FetchUser(context.Context, string) (authsvc.GitHubUser, error) {
	return authsvc.GitHubUser{}, errors.New("FetchUser must not be called in the dedup sub-test")
}
func (p *countingProvider) RefreshUserAccessToken(context.Context, string) (authsvc.GitHubUserToken, error) {
	p.mu.Lock()
	p.refresh++
	p.mu.Unlock()
	return authsvc.GitHubUserToken{}, errors.New("RefreshUserAccessToken must not be called for a fresh token")
}
func (p *countingProvider) refreshCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refresh
}

// TestGitHubE2E_Rotation drives a REAL single-use GitHub refresh through the live node-mTLS->AS wire.
// Single-use rotation (spike sp-v40s.3) consumes GITHUB_E2E_REFRESH_TOKEN; the test logs its rotated
// successor so the operator can re-seed for the next run.
func TestGitHubE2E_Rotation(t *testing.T) {
	refreshToken := os.Getenv("GITHUB_E2E_REFRESH_TOKEN")
	clientID := os.Getenv("GITHUB_CLIENT_ID")
	clientSecret := os.Getenv("GITHUB_CLIENT_SECRET")
	if refreshToken == "" || clientID == "" || clientSecret == "" {
		t.Fatalf("github_e2e: set GITHUB_E2E_REFRESH_TOKEN (single-use; the test logs its successor), " +
			"GITHUB_CLIENT_ID, and GITHUB_CLIENT_SECRET for the throwaway App app_id=4065493 to run the real mint/refresh leg")
	}
	provider := authsvc.NewGitHubProvider(
		envOr("GITHUB_WEB_URL", "https://github.com"),
		envOr("GITHUB_API_URL", "https://api.github.com"),
		clientID, clientSecret,
	)
	mw := newMintWire(t, provider)
	seedLink(t, mw.store, clientID, refreshToken, "", 0) // empty access token -> forces a real refresh

	ctx := context.Background()
	resp, err := mw.nodeClient.MintGitHubAccessToken(ctx, connect.NewRequest(e2eMintReq()))
	if err != nil {
		t.Fatalf("real node-mTLS mint: %v", err)
	}
	// CONTAINMENT (a): the node-facing response carries an ACCESS token only — the proto has no
	// refresh-token field, and the returned access token is not the seeded refresh token.
	if !resp.Msg.GetRefreshed() || resp.Msg.GetAccessToken() == "" {
		t.Fatalf("expected a refreshed access token, got %+v", resp.Msg)
	}
	if resp.Msg.GetAccessToken() == refreshToken {
		t.Fatalf("CONTAINMENT VIOLATION: node response returned the refresh token as the access token")
	}

	// AS persisted the rotation (new version + a NEW refresh token, retained AS-side only).
	link, err := mw.store.GitHubLinks().Get(ctx, e2eSecretID)
	if err != nil {
		t.Fatalf("get link after rotation: %v", err)
	}
	if link.Version != 2 {
		t.Fatalf("link version = %d, want 2 (rotation)", link.Version)
	}
	if link.RefreshToken == "" || link.RefreshToken == refreshToken {
		t.Fatalf("AS did not rotate the refresh chain: have %q (seed %q)", link.RefreshToken, refreshToken)
	}
	if link.AccessToken != resp.Msg.GetAccessToken() {
		t.Fatalf("store access token %q != node response %q", link.AccessToken, resp.Msg.GetAccessToken())
	}

	// Node identity (invariant d): the AS asked the CP to confirm the node from the CLIENT CERT.
	authz, authzHits := mw.authz.snapshot()
	if authzHits == 0 || authz.NodeID != e2eNodeID {
		t.Fatalf("authZ NodeID = %q hits=%d, want %q from the presented client cert", authz.NodeID, authzHits, e2eNodeID)
	}

	// CP-coordinated signal (invariant c spirit, sp-v40s.22.1): the AS emitted a token-free rotation
	// signal; the signal struct has no token field at all (compile-time containment).
	sig, sigHits := mw.signal.snapshot()
	if sigHits == 0 || sig.SecretID != e2eSecretID || sig.Version != 2 {
		t.Fatalf("signal = %+v hits=%d, want secret %q version 2", sig, sigHits, e2eSecretID)
	}
	if sig.DeliveryID == "" {
		t.Fatalf("signal delivery_id must be non-empty")
	}

	t.Logf("ROTATED: re-seed GITHUB_E2E_REFRESH_TOKEN=%s for the next run (single-use)", link.RefreshToken)
}

// TestGitHubE2E_RefresherWiresRealMint proves the node's REAL refresher (newGitHubRefresher) drives a
// real mTLS mint that the AS authorizes by node identity — the exact gap the hermetic suite's fake
// client leaves uncovered. It uses a FRESH access token so the AS dedups WITHOUT spending a GitHub
// refresh token (deterministic; always runs, no creds).
func TestGitHubE2E_RefresherWiresRealMint(t *testing.T) {
	provider := &countingProvider{}
	mw := newMintWire(t, provider)
	seedLink(t, mw.store, "Iv1.e2e", "ghr_unused", "ghu_fresh", time.Now().Add(2*time.Hour).Unix())

	r := newGitHubRefresher(mw.nodeClient)
	base := time.Now()
	r.now = func() time.Time { return base }
	r.Note(githubRefreshEntry{
		SpawnID: e2eSpawnID, Generation: 1, SecretID: e2eSecretID, Version: 1, DeliveryID: e2eDeliveryID,
	})
	// Force the entry due, then Tick -> attempt() -> real authv1connect mint over mTLS (synchronous).
	r.Tick(context.Background(), base.Add(defaultRefreshInterval+time.Minute))

	if n := provider.refreshCalls(); n != 0 {
		t.Fatalf("fresh token must dedup without calling GitHub, refresh calls = %d", n)
	}
	authz, hits := mw.authz.snapshot()
	if hits == 0 || authz.NodeID != e2eNodeID || authz.SecretID != e2eSecretID {
		t.Fatalf("refresher's real mint did not reach AS authZ with node identity: authz=%+v hits=%d", authz, hits)
	}
}

// TestGitHubE2E_RequiresNodeIdentity asserts the node->AS refresh is node-identity-bound over the
// real wire (invariant d): a well-formed mint with NO client cert is rejected Unauthenticated.
func TestGitHubE2E_RequiresNodeIdentity(t *testing.T) {
	mw := newMintWire(t, &countingProvider{})
	seedLink(t, mw.store, "Iv1.e2e", "ghr_unused", "ghu_fresh", time.Now().Add(2*time.Hour).Unix())

	_, err := mw.noCert.MintGitHubAccessToken(context.Background(), connect.NewRequest(e2eMintReq()))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("mint without a node client cert: code = %v err = %v, want Unauthenticated", connect.CodeOf(err), err)
	}
	if hits := func() int { _, h := mw.authz.snapshot(); return h }(); hits != 0 {
		t.Fatalf("AS authorized a mint with no node identity (authz hits = %d)", hits)
	}
}
