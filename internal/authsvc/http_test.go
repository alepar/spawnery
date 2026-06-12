package authsvc_test

// Full vertical test over httptest.NewServer(s.Handler()) with fake provider (Phase 8).
// Proves Handler() composes correctly: authorize→callback→refresh→logout.

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"spawnery/internal/authsvc"
	"spawnery/internal/authsvc/githubfake"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/pki"
)

// reverseProxy mux: allows swapping the real handler after the server is started.
type lazyMux struct {
	real http.Handler
}

func (l *lazyMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if l.real == nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	l.real.ServeHTTP(w, r)
}

func TestHandlerFullVertical(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	fake.SetUser(88001, "e2euser")

	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	_, sigKey, _ := ed25519.GenerateKey(rand.Reader)
	sessKey, spkiDER := genP256key(t)

	root, err := pki.NewRootCA("Test Root")
	if err != nil {
		t.Fatal(err)
	}
	inter, err := root.NewIntermediate(pki.ClassSelfHosted)
	if err != nil {
		t.Fatal(err)
	}

	// Start server with a lazy mux so we know the URL before wiring the IdP.
	lazy := &lazyMux{}
	srv := httptest.NewServer(lazy)
	defer srv.Close()

	callbackURI := srv.URL + "/oauth/callback"
	cfg := authsvc.IdPConfig{
		Store:               st,
		GitHub:              authsvc.NewGitHubProvider(fake.URL(), fake.URL(), fake.ClientID, fake.ClientSecret),
		SigningKey:           sigKey,
		GitHubRedirectURI:   callbackURI,
		SPAOrigin:           "http://localhost:3000",
		RedirectURIs:        []string{"http://localhost:3000/callback"},
		RegistrationEnabled: true,
		Now:                 func() time.Time { return now },
	}
	idp, err := authsvc.NewIdP(cfg)
	if err != nil {
		t.Fatal(err)
	}
	svc := authsvc.New(root.Cert, inter, authsvc.WithIdP(idp))
	lazy.real = svc.Handler()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// 1. Authorize.
	authResp, err := client.Get(srv.URL + "/oauth/authorize?" + url.Values{
		"redirect_uri":   {"http://localhost:3000/callback"},
		"state":          {"flow-state"},
		"session_pubkey": {base64.StdEncoding.EncodeToString(spkiDER)},
	}.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if authResp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: want 302, got %d", authResp.StatusCode)
	}
	flowCookie := ""
	for _, c := range authResp.Cookies() {
		if c.Name == "as_flow" {
			flowCookie = c.Value
		}
	}
	if flowCookie == "" {
		t.Fatal("no flow cookie after authorize")
	}
	ghURL := authResp.Header.Get("Location")

	// 2. Follow fake GitHub redirect.
	ghResp, err := client.Get(ghURL)
	if err != nil || ghResp == nil {
		t.Fatalf("fake github: %v", err)
	}
	cbURL := ghResp.Header.Get("Location")
	if cbURL == "" {
		t.Fatalf("no callback URL from fake GitHub (status=%d)", ghResp.StatusCode)
	}

	// 3. Callback with flow cookie.
	cbReq, _ := http.NewRequest("GET", cbURL, nil)
	cbReq.AddCookie(&http.Cookie{Name: "as_flow", Value: flowCookie})
	cbResp, err := client.Do(cbReq)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if cbResp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(cbResp.Body)
		t.Fatalf("callback: want 302, got %d: %s", cbResp.StatusCode, body)
	}
	location := cbResp.Header.Get("Location")
	accessToken := urlQP(location, "access_token")
	if accessToken == "" {
		t.Fatalf("no access_token in callback redirect: %q", location)
	}
	refreshCookieVal := ""
	for _, c := range cbResp.Cookies() {
		if c.Name == "refresh_token" {
			refreshCookieVal = c.Value
		}
	}
	if refreshCookieVal == "" {
		t.Fatal("no refresh_token cookie after callback")
	}

	// 4. /refresh with PoP.
	ts := now.Unix()
	tsStr, nonceB64, sigB64 := makePoP(t, sessKey, refreshCookieVal, ts)
	refreshReq, _ := http.NewRequest("POST", srv.URL+"/refresh", nil)
	refreshReq.AddCookie(&http.Cookie{Name: "refresh_token", Value: refreshCookieVal})
	refreshReq.Header.Set("X-PoP-Timestamp", tsStr)
	refreshReq.Header.Set("X-PoP-Nonce", nonceB64)
	refreshReq.Header.Set("X-PoP-Sig", sigB64)
	refreshResp, err := client.Do(refreshReq)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if refreshResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(refreshResp.Body)
		t.Fatalf("refresh: want 200, got %d: %s", refreshResp.StatusCode, body)
	}
	var refreshOut struct{ AccessToken string `json:"access_token"` }
	body2, _ := io.ReadAll(refreshResp.Body)
	_ = json.Unmarshal(body2, &refreshOut)
	if refreshOut.AccessToken == "" {
		t.Fatalf("refresh: no access_token in response: %s", body2)
	}
	newRefreshVal := ""
	for _, c := range refreshResp.Cookies() {
		if c.Name == "refresh_token" {
			newRefreshVal = c.Value
		}
	}
	if newRefreshVal == "" {
		t.Fatal("no new refresh_token cookie after refresh")
	}

	// 5. /logout — use the logout_session mirror cookie (Path=/logout).
	// refresh_token lives at Path=/refresh; a browser would NOT send it to /logout.
	logoutReq, _ := http.NewRequest("POST", srv.URL+"/logout", nil)
	logoutReq.AddCookie(&http.Cookie{Name: "logout_session", Value: newRefreshVal})
	logoutResp, err := client.Do(logoutReq)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	if logoutResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(logoutResp.Body)
		t.Fatalf("logout: want 200, got %d: %s", logoutResp.StatusCode, body)
	}

	// 6. /revocations should contain the logout event.
	revResp, _ := client.Get(srv.URL + "/revocations?since=0")
	revBody, _ := io.ReadAll(revResp.Body)
	var entries []authsvc.SignedRevocationEntry
	_ = json.Unmarshal(revBody, &entries)
	if len(entries) == 0 {
		t.Fatal("no revocation events after logout")
	}
}

func genP256key(t *testing.T) (*ecdsa.PrivateKey, []byte) {
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

func makePoP(t *testing.T, priv *ecdsa.PrivateKey, rawToken string, ts int64) (tsStr, nonceB64, sigB64 string) {
	t.Helper()
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)

	hash := sha256.Sum256([]byte(rawToken))
	refreshHash := hash[:]
	var tsBytes [8]byte
	binary.BigEndian.PutUint64(tsBytes[:], uint64(ts))

	msg := []byte("spawnery/refresh-pop/v1")
	msg = append(msg, refreshHash...)
	msg = append(msg, tsBytes[:]...)
	msg = append(msg, nonce...)
	digest := sha256.Sum256(msg)

	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	copy(sig[32-len(r.Bytes()):32], r.Bytes())
	copy(sig[64-len(s.Bytes()):64], s.Bytes())

	rawURL := base64.RawURLEncoding.EncodeToString
	return fmt.Sprintf("%d", ts), rawURL(nonce), rawURL(sig)
}

func urlQP(rawURL, key string) string {
	u, _ := url.Parse(rawURL)
	return u.Query().Get(key)
}
