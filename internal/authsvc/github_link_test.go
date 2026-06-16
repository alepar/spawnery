package authsvc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"spawnery/internal/authsvc/githubfake"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/pki"
)

// drive performs the authorize GET without following the redirect and returns the `code`
// the fake placed in the Location query — local helper so githubfake stays untouched.
func drive(t *testing.T, authURL string) string {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(authURL)
	if err != nil {
		t.Fatalf("authorize GET: %v", err)
	}
	defer resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("authorize returned no code: %s", resp.Header.Get("Location"))
	}
	return code
}

func newLinkExchanger(t *testing.T) (GitHubLinkExchanger, *githubfake.Fake) {
	t.Helper()
	f := githubfake.New()
	t.Cleanup(f.Close)
	prov := NewGitHubProvider(f.URL(), f.URL(), f.ClientID, f.ClientSecret)
	ex, ok := prov.(GitHubLinkExchanger)
	if !ok {
		t.Fatalf("provider does not implement GitHubLinkExchanger")
	}
	return ex, f
}

func TestExchangeUserTokenReturnsFullTuple(t *testing.T) {
	ex, f := newLinkExchanger(t)
	f.SetUser(424242, "octolink")
	verifier := "verifier-abc"
	redirectURI := "https://as.example.com/github/link/callback"
	authURL := ex.AppAuthorizeURL("state-1", pkceChallenge(verifier), redirectURI)
	code := drive(t, authURL)

	tok, err := ex.ExchangeUserToken(context.Background(), code, verifier, redirectURI)
	if err != nil {
		t.Fatalf("ExchangeUserToken: %v", err)
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" || tok.AccessExpiresAtUnix == 0 || tok.RefreshExpiresAtUnix == 0 {
		t.Fatalf("incomplete tuple: %+v", tok)
	}
	user, err := ex.FetchUser(context.Background(), tok.AccessToken)
	if err != nil {
		t.Fatalf("FetchUser: %v", err)
	}
	if user.Sub != 424242 || user.Login != "octolink" {
		t.Fatalf("user = %+v", user)
	}
}

func headerAccount(header string) AccountFromRequest {
	return func(r *http.Request) (string, bool) {
		v := r.Header.Get(header)
		return v, v != ""
	}
}

func newLinkAS(t *testing.T, st store.Store, ex GitHubLinkExchanger, now func() time.Time) *Service {
	t.Helper()
	root, _ := pki.NewRootCA("R")
	inter, _ := root.NewIntermediate(pki.ClassSelfHosted)
	return New(root.Cert, inter,
		WithClock(now),
		WithGitHubLink(GitHubLinkConfig{
			Exchanger:          ex,
			Store:              st,
			AppClientID:        "Iv1.spawneryapp",
			RedirectURI:        "https://as.example.com/github/link/callback",
			PostRedeemRedirect: "https://app.example.com/settings/github",
			DefaultHost:        "github.com",
			AccountFromReq:     headerAccount("X-Test-Account"),
		}),
	)
}

func TestGitHubLinkRedeemUpsertsAndReturnsTuple(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, f := newLinkExchanger(t)
	f.SetUser(990011, "octolink")
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	// Seed a state row as serveGitHubLinkAuthorize would have.
	verifier := "ver-xyz"
	state := "state-xyz"
	s.githubLinkStates[state] = githubLinkState{
		accountID:   "acct-7",
		secretID:    "gh-main",
		host:        "github.com",
		verifier:    verifier,
		redirectURI: s.githubLinkRedirectURI,
		expiresAt:   now.Add(time.Minute),
	}
	code := drive(t, ex.AppAuthorizeURL(state, pkceChallenge(verifier), s.githubLinkRedirectURI))

	// Callback: exchanges, stashes nonce, sets HttpOnly cookie, redirects with NO nonce in URL.
	cbReq := httptest.NewRequest(http.MethodGet, "/github/link/callback?state="+state+"&code="+code, nil)
	cbRes := httptest.NewRecorder()
	s.serveGitHubLinkCallback(cbRes, cbReq)
	if cbRes.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body=%s", cbRes.Code, cbRes.Body.String())
	}
	loc := cbRes.Header().Get("Location")
	if !strings.HasPrefix(loc, s.githubLinkPostRedeem) {
		t.Fatalf("callback redirect = %q", loc)
	}
	if strings.Contains(loc, "nonce") || strings.Contains(loc, "as_gh_link") {
		t.Fatalf("nonce leaked into redirect URL: %q", loc)
	}
	var nonceCookie *http.Cookie
	for _, c := range cbRes.Result().Cookies() {
		if c.Name == githubLinkCookieName {
			nonceCookie = c
		}
	}
	if nonceCookie == nil || nonceCookie.Value == "" {
		t.Fatalf("no %s cookie set", githubLinkCookieName)
	}
	if !nonceCookie.HttpOnly {
		t.Fatalf("nonce cookie must be HttpOnly")
	}

	// Redeem: owner-authenticated, pops nonce, Upserts, returns the DR tuple.
	rdReq := httptest.NewRequest(http.MethodPost, "/github/link/redeem", nil)
	rdReq.Header.Set("X-Test-Account", "acct-7")
	rdReq.AddCookie(nonceCookie)
	rdRes := httptest.NewRecorder()
	s.serveGitHubLinkRedeem(rdRes, rdReq)
	if rdRes.Code != http.StatusOK {
		t.Fatalf("redeem status = %d, body=%s", rdRes.Code, rdRes.Body.String())
	}
	body := rdRes.Body.String()
	for _, want := range []string{`"secret_id":"gh-main"`, `"login":"octolink"`, `"refresh_token":"ghr_`, `"version":1`} {
		if !strings.Contains(body, want) {
			t.Fatalf("redeem body missing %q: %s", want, body)
		}
	}

	// Durable AS-custodial row exists for mint to read.
	link, err := st.GitHubLinks().Get(t.Context(), "gh-main")
	if err != nil {
		t.Fatalf("GitHubLinks().Get: %v", err)
	}
	if link.AccountID != "acct-7" || link.Login != "octolink" || link.GithubUserID != "990011" {
		t.Fatalf("link identity = %+v", link)
	}
	if link.RefreshToken == "" || link.AccessToken == "" || link.Version != 1 {
		t.Fatalf("link tokens/version = %+v", link)
	}
	if link.DeliveryID != githubAccessDeliveryID("gh-main", 1) || link.AppClientID != "Iv1.spawneryapp" {
		t.Fatalf("link delivery/app = %+v", link)
	}
}

func linkOnce(t *testing.T, s *Service, ex GitHubLinkExchanger, secretID, account string, now time.Time) *http.Cookie {
	t.Helper()
	verifier := "ver-" + secretID + account
	state := "state-" + secretID + account
	s.githubLinkMu.Lock()
	s.githubLinkStates[state] = githubLinkState{accountID: account, secretID: secretID, host: "github.com", verifier: verifier, redirectURI: s.githubLinkRedirectURI, expiresAt: now.Add(time.Minute)}
	s.githubLinkMu.Unlock()
	code := drive(t, ex.AppAuthorizeURL(state, pkceChallenge(verifier), s.githubLinkRedirectURI))
	res := httptest.NewRecorder()
	s.serveGitHubLinkCallback(res, httptest.NewRequest(http.MethodGet, "/github/link/callback?state="+state+"&code="+code, nil))
	for _, c := range res.Result().Cookies() {
		if c.Name == githubLinkCookieName {
			return c
		}
	}
	t.Fatalf("no nonce cookie")
	return nil
}

func redeem(s *Service, account string, c *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/github/link/redeem", nil)
	if account != "" {
		req.Header.Set("X-Test-Account", account)
	}
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	s.serveGitHubLinkRedeem(rec, req)
	return rec
}

func TestGitHubLinkRedeemSingleUse(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })
	c := linkOnce(t, s, ex, "gh-main", "acct-7", now)
	if got := redeem(s, "acct-7", c).Code; got != http.StatusOK {
		t.Fatalf("first redeem = %d", got)
	}
	if got := redeem(s, "acct-7", c).Code; got != http.StatusNotFound {
		t.Fatalf("replay redeem = %d, want 404", got)
	}
}

func TestGitHubLinkRedeemAccountMismatch(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })
	c := linkOnce(t, s, ex, "gh-main", "acct-7", now)
	if got := redeem(s, "attacker", c).Code; got != http.StatusForbidden {
		t.Fatalf("cross-account redeem = %d, want 403", got)
	}
}

func TestGitHubLinkRedeemRequiresAuth(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })
	c := linkOnce(t, s, ex, "gh-main", "acct-7", now)
	if got := redeem(s, "", c).Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated redeem = %d, want 401", got)
	}
}

func TestGitHubLinkRelinkBumpsVersion(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })
	if got := redeem(s, "acct-7", linkOnce(t, s, ex, "gh-main", "acct-7", now)).Code; got != http.StatusOK {
		t.Fatalf("link v1 = %d", got)
	}
	if got := redeem(s, "acct-7", linkOnce(t, s, ex, "gh-main", "acct-7", now)).Code; got != http.StatusOK {
		t.Fatalf("relink = %d", got)
	}
	link, err := st.GitHubLinks().Get(t.Context(), "gh-main")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if link.Version != 2 || link.DeliveryID != githubAccessDeliveryID("gh-main", 2) {
		t.Fatalf("relink version/delivery = %+v", link)
	}
}

func TestRevokeAppGrantKillsChainAtGitHub(t *testing.T) {
	ex, f := newLinkExchanger(t)
	// Bootstrap a live grant via the link exchanger.
	verifier := "v-revoke"
	redirectURI := "https://as.example.com/github/link/callback"
	code := drive(t, ex.AppAuthorizeURL("st", pkceChallenge(verifier), redirectURI))
	tok, err := ex.ExchangeUserToken(context.Background(), code, verifier, redirectURI)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if _, err := ex.FetchUser(context.Background(), tok.AccessToken); err != nil {
		t.Fatalf("precondition FetchUser: %v", err)
	}
	if err := ex.RevokeAppGrant(context.Background(), tok.AccessToken); err != nil {
		t.Fatalf("RevokeAppGrant: %v", err)
	}
	// Chain is dead: the access token no longer authenticates.
	if _, err := ex.FetchUser(context.Background(), tok.AccessToken); err == nil {
		t.Fatalf("FetchUser succeeded after RevokeAppGrant; want failure")
	}
	// Idempotent: revoking an already-dead token is a no-op success (GitHub 404 → nil).
	if err := ex.RevokeAppGrant(context.Background(), tok.AccessToken); err != nil {
		t.Fatalf("second RevokeAppGrant should be idempotent, got %v", err)
	}
	_ = f
}

func TestGitHubLinkAuthorizeRequiresAccountAndSecret(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })
	rec := httptest.NewRecorder()
	s.serveGitHubLinkAuthorize(rec, httptest.NewRequest(http.MethodGet, "/github/link/authorize?secret_id=gh-main", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-account authorize = %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/github/link/authorize", nil)
	req.Header.Set("X-Test-Account", "acct-7")
	rec2 := httptest.NewRecorder()
	s.serveGitHubLinkAuthorize(rec2, req)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("no-secret authorize = %d", rec2.Code)
	}
}
