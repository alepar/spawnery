package authsvc

// Test helpers for identity-core tests: IdP factory, fake provider adapter, fake GitHub, and
// a P-256 session-key helper. All tests run hermetically (in-memory store, no network, no keys
// except generated ones).

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"net/url"
	"testing"
	"time"

	"spawnery/internal/authsvc/githubfake"
	"spawnery/internal/authsvc/store"
)

// fakeProvider wraps githubfake.Fake as a GitHubProvider by delegating to the fake's real HTTP
// server via githubClient (so the EXACT production code path is exercised).
type fakeProvider struct {
	gh GitHubProvider
}

func newFakeProvider(fake *githubfake.Fake) *fakeProvider {
	return &fakeProvider{
		gh: NewGitHubProvider(fake.URL(), fake.URL(), fake.ClientID, fake.ClientSecret),
	}
}

func (fp *fakeProvider) AuthorizeURL(state, challenge, redirectURI string) string {
	return fp.gh.AuthorizeURL(state, challenge, redirectURI)
}
func (fp *fakeProvider) Exchange(ctx context.Context, code, verifier, redirectURI string) (string, error) {
	return fp.gh.Exchange(ctx, code, verifier, redirectURI)
}
func (fp *fakeProvider) FetchUser(ctx context.Context, token string) (GitHubUser, error) {
	return fp.gh.FetchUser(ctx, token)
}
func (fp *fakeProvider) RefreshUserAccessToken(ctx context.Context, refreshToken string) (GitHubUserToken, error) {
	return fp.gh.RefreshUserAccessToken(ctx, refreshToken)
}

// newTestIdP builds an IdP backed by in-memory store and the given fake GitHub.
// The clock is fixed at `now`.
func newTestIdP(t *testing.T, fake *githubfake.Fake, now time.Time, opts ...func(*IdPConfig)) (*IdP, store.Store, ed25519.PrivateKey) {
	t.Helper()
	st := store.NewTestStore(t)
	_, sigKey, _ := ed25519.GenerateKey(rand.Reader)

	cfg := IdPConfig{
		Store:               st,
		GitHub:              newFakeProvider(fake),
		SigningKey:           sigKey,
		GitHubRedirectURI:   "http://127.0.0.1:8090/oauth/callback",
		SPAOrigin:           "http://localhost:3000",
		RedirectURIs:        []string{"http://localhost:3000/callback"},
		VerificationURI:     "http://localhost:8090/device/verify",
		RegistrationEnabled: true,
		Now:                 func() time.Time { return now },
	}
	for _, o := range opts {
		o(&cfg)
	}
	idp, err := NewIdP(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return idp, st, sigKey
}

// newTestP256 generates a P-256 keypair and returns (privKey, spkiDER).
func newTestP256(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return k, der
}

// newTestP384 generates a P-384 keypair and returns the SPKI DER bytes.
// Used to verify that non-P256 keys are rejected by parseSessionSPKI.
func newTestP384(t *testing.T) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// spkiB64 encodes DER SPKI to base64 standard encoding.
func spkiB64(der []byte) string {
	return base64.StdEncoding.EncodeToString(der)
}

// extractQueryParam extracts a query param from a URL string.
func extractQueryParam(rawURL, key string) string {
	u, _ := url.Parse(rawURL)
	return u.Query().Get(key)
}
