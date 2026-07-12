package authsvc

// Test helpers for identity-core tests: IdP factory, fake provider adapter, fake GitHub, and
// a P-256 session-key helper. All tests run hermetically (in-memory store, no network, no keys
// except generated ones).

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net/url"
	"testing"
	"time"

	"spawnery/internal/authsvc/githubfake"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/pki"
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
func newTestIdP(t *testing.T, fake *githubfake.Fake, now time.Time, opts ...func(*IdPConfig)) (*IdP, store.Store, *token.SigningCredential) {
	t.Helper()
	st := store.NewTestStore(t)
	pki := newTestArtifactPKI(t, now, "prod")
	signer := pki.signer(t, now, "current")

	cfg := IdPConfig{
		Store:               st,
		GitHub:              newFakeProvider(fake),
		Signer:              signer,
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
	return idp, st, signer
}

type testArtifactPKI struct {
	root            *x509.Certificate
	rootKey         *ecdsa.PrivateKey
	intermediate    *x509.Certificate
	intermediateKey *ecdsa.PrivateKey
	environment     string
}

func newTestArtifactPKI(t *testing.T, now time.Time, environment string) testArtifactPKI {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test root"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, BasicConstraintsValid: true, IsCA: true, MaxPathLen: 2,
	}
	root := mustIssueTestCert(t, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	intermediateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	intermediateTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "auth signing"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(180 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, BasicConstraintsValid: true, IsCA: true, MaxPathLen: 0,
		Policies: []x509.OID{pki.AuthSigningIntermediatePolicyOID},
	}
	intermediate := mustIssueTestCert(t, intermediateTemplate, root, &intermediateKey.PublicKey, rootKey)
	return testArtifactPKI{root: root, rootKey: rootKey, intermediate: intermediate, intermediateKey: intermediateKey, environment: environment}
}

func (p testArtifactPKI) signer(t *testing.T, now time.Time, id string) *token.SigningCredential {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse("spiffe://" + p.environment + ".spawnery.internal/signer/auth-artifact/" + id)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: new(big.Int).SetBytes([]byte("0123456789abcdef")), Subject: pkix.Name{CommonName: id},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(90 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, Policies: []x509.OID{pki.AuthArtifactSignerPolicyOID}, URIs: []*url.URL{uri},
	}
	leaf := mustIssueTestCert(t, leafTemplate, p.intermediate, privateKey.Public(), p.intermediateKey)
	credential, err := token.NewSigningCredential(privateKey, []*x509.Certificate{leaf, p.intermediate}, p.root, p.environment, now)
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

func mustIssueTestCert(t *testing.T, template, parent *x509.Certificate, publicKey any, signer crypto.Signer) *x509.Certificate {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, template, parent, publicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
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
