package authsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"spawnery/internal/authsvc/store"
)

const (
	githubLinkStateTTL   = 10 * time.Minute // authorize→callback window
	githubLinkNonceTTL   = 2 * time.Minute  // response-wrap nonce: short, single-use
	githubLinkCookieName = "as_gh_link"     // HttpOnly nonce delivery; never in a URL
	githubLinkCookiePath = "/github/link/redeem"
)

// GitHubLinkExchanger is the AS<->GitHub App leg used by the link-bootstrap flow. It is a SEPARATE,
// narrower interface than GitHubProvider so the link flow can capture the full user-token tuple
// (the login Exchange returns only the access token) WITHOUT widening GitHubProvider — keeping the
// mint tests' hand-rolled GitHubProvider mock intact. *githubClient satisfies both.
type GitHubLinkExchanger interface {
	AppAuthorizeURL(state, challenge, redirectURI string) string
	ExchangeUserToken(ctx context.Context, code, verifier, redirectURI string) (GitHubUserToken, error)
	FetchUser(ctx context.Context, accessToken string) (GitHubUser, error)
	// RevokeAppGrant deletes the GitHub App authorization grant for the user behind accessToken
	// (DELETE /applications/{client_id}/grant). Grant-WIDE: kills the whole refresh chain. The spec
	// §16.5 / Decision 24 compromise kill switch. Idempotent: an already-deleted grant (404) is nil.
	RevokeAppGrant(ctx context.Context, accessToken string) error
}

// AppAuthorizeURL builds a GitHub App user-to-server authorize URL. Unlike the login AuthorizeURL
// it sends NO `scope` — GitHub App user tokens carry fixed App permissions, not OAuth scopes.
func (g *githubClient) AppAuthorizeURL(state, challenge, redirectURI string) string {
	q := url.Values{
		"client_id":             {g.clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return g.webURL + "/login/oauth/authorize?" + q.Encode()
}

// ExchangeUserToken redeems the auth code for the FULL user-token tuple (access + refresh + both
// expiries), unlike the login-only Exchange which discards everything but the access token.
func (g *githubClient) ExchangeUserToken(ctx context.Context, code, verifier, redirectURI string) (GitHubUserToken, error) {
	form := url.Values{
		"client_id":     {g.clientID},
		"client_secret": {g.clientSecret},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.webURL+"/login/oauth/access_token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return GitHubUserToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return GitHubUserToken{}, err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken           string `json:"access_token"`
		ExpiresIn             int64  `json:"expires_in"`
		RefreshToken          string `json:"refresh_token"`
		RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
		TokenType             string `json:"token_type"`
		Error                 string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return GitHubUserToken{}, err
	}
	if resp.StatusCode != http.StatusOK || out.Error != "" || out.AccessToken == "" || out.RefreshToken == "" {
		return GitHubUserToken{}, fmt.Errorf("github link exchange failed: status %d error %q", resp.StatusCode, out.Error)
	}
	now := time.Now()
	if out.TokenType == "" {
		out.TokenType = "bearer"
	}
	return GitHubUserToken{
		AccessToken:          out.AccessToken,
		AccessExpiresAtUnix:  now.Add(time.Duration(out.ExpiresIn) * time.Second).Unix(),
		RefreshToken:         out.RefreshToken,
		RefreshExpiresAtUnix: now.Add(time.Duration(out.RefreshTokenExpiresIn) * time.Second).Unix(),
		TokenType:            out.TokenType,
	}, nil
}

// RevokeAppGrant deletes the App authorization grant for the user behind accessToken via
// DELETE /applications/{client_id}/grant (Basic auth confidential client + {access_token} body).
// 204 = revoked, 404 = already gone (idempotent → nil). Only the access token is sent; the refresh
// token never leaves AS custody (containment invariant a).
func (g *githubClient) RevokeAppGrant(ctx context.Context, accessToken string) error {
	body, err := json.Marshal(map[string]string{"access_token": accessToken})
	if err != nil {
		return err
	}
	endpoint := g.apiURL + "/applications/" + url.PathEscape(g.clientID) + "/grant"
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(g.clientID, g.clientSecret)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		return fmt.Errorf("github grant revoke failed: status %d", resp.StatusCode)
	}
}

// githubLinkState is the authorize→callback handoff row (in-memory, account-bound, single-use).
type githubLinkState struct {
	accountID   string
	secretID    string
	host        string
	verifier    string
	redirectURI string
	expiresAt   time.Time
}

// githubLinkPending is the response-wrap tuple held behind a single-use nonce until owner redeem.
type githubLinkPending struct {
	accountID    string
	secretID     string
	host         string
	login        string
	githubUserID string
	tuple        GitHubUserToken
	expiresAt    time.Time
}

// Compile-time assertion that *githubClient implements the link exchanger.
var _ GitHubLinkExchanger = (*githubClient)(nil)

func (s *Service) serveGitHubLinkAuthorize(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.githubLinkAccountFromReq(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	secretID := strings.TrimSpace(r.URL.Query().Get("secret_id"))
	if secretID == "" {
		http.Error(w, "secret_id required", http.StatusBadRequest)
		return
	}
	host := strings.TrimSpace(r.URL.Query().Get("host"))
	if host == "" {
		host = s.githubLinkDefaultHost
	}
	state := randOpaque()
	verifier := randOpaque()
	now := s.now()
	s.githubLinkMu.Lock()
	s.githubLinkStates[state] = githubLinkState{
		accountID:   accountID,
		secretID:    secretID,
		host:        host,
		verifier:    verifier,
		redirectURI: s.githubLinkRedirectURI,
		expiresAt:   now.Add(githubLinkStateTTL),
	}
	s.githubLinkMu.Unlock()
	http.Redirect(w, r, s.githubLinkExchanger.AppAuthorizeURL(state, pkceChallenge(verifier), s.githubLinkRedirectURI), http.StatusFound)
}

func (s *Service) serveGitHubLinkCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	now := s.now()

	s.githubLinkMu.Lock()
	st, ok := s.githubLinkStates[state]
	if ok {
		delete(s.githubLinkStates, state) // single-use
	}
	s.githubLinkMu.Unlock()
	if !ok || now.After(st.expiresAt) {
		http.Error(w, "invalid or expired link state", http.StatusBadRequest)
		return
	}
	if e := q.Get("error"); e != "" {
		s.redirectLinkError(w, r, e)
		return
	}
	code := q.Get("code")
	if code == "" {
		s.redirectLinkError(w, r, "no_code")
		return
	}

	tuple, err := s.githubLinkExchanger.ExchangeUserToken(r.Context(), code, st.verifier, st.redirectURI)
	if err != nil {
		s.redirectLinkError(w, r, "exchange_failed")
		return
	}
	user, err := s.githubLinkExchanger.FetchUser(r.Context(), tuple.AccessToken)
	if err != nil {
		s.redirectLinkError(w, r, "user_fetch_failed")
		return
	}

	nonce := randOpaque()
	s.githubLinkMu.Lock()
	s.githubLinkPending[nonce] = githubLinkPending{
		accountID:    st.accountID,
		secretID:     st.secretID,
		host:         st.host,
		login:        user.Login,
		githubUserID: strconv.FormatInt(user.Sub, 10),
		tuple:        tuple,
		expiresAt:    now.Add(githubLinkNonceTTL),
	}
	s.githubLinkMu.Unlock()

	// Response-wrap delivery (§3 step 4): nonce travels ONLY in an HttpOnly, Secure, SameSite=Strict
	// cookie scoped to /github/link/redeem — never the redirect URL / history / Referer.
	http.SetCookie(w, &http.Cookie{
		Name:     githubLinkCookieName,
		Value:    nonce,
		Path:     githubLinkCookiePath,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		Expires:  now.Add(githubLinkNonceTTL),
	})
	http.Redirect(w, r, s.githubLinkPostRedeem, http.StatusFound)
}

func (s *Service) serveGitHubLinkRedeem(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.githubLinkAccountFromReq(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	cookie, err := r.Cookie(githubLinkCookieName)
	if err != nil || cookie.Value == "" {
		http.Error(w, "link nonce required", http.StatusBadRequest)
		return
	}
	now := s.now()
	s.githubLinkMu.Lock()
	pend, ok := s.githubLinkPending[cookie.Value]
	if ok {
		delete(s.githubLinkPending, cookie.Value) // single-use pop (replay resistance)
	}
	s.githubLinkMu.Unlock()
	// Always clear the nonce cookie regardless of outcome.
	http.SetCookie(w, &http.Cookie{Name: githubLinkCookieName, Value: "", Path: githubLinkCookiePath, MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})

	if !ok || now.After(pend.expiresAt) {
		http.Error(w, "invalid or expired link nonce", http.StatusNotFound)
		return
	}
	// Account-binding: the redeeming owner must be the account that initiated the link.
	if pend.accountID != accountID {
		http.Error(w, "link nonce account mismatch", http.StatusForbidden)
		return
	}

	// Monotonic version across re-links so prior deliveries never collide.
	version := uint64(1)
	if existing, err := s.githubLinkStore.GitHubLinks().Get(r.Context(), pend.secretID); err == nil {
		version = existing.Version + 1
	} else if !errors.Is(err, store.ErrNotFound) {
		http.Error(w, "link lookup failed", http.StatusInternalServerError)
		return
	}
	deliveryID := githubAccessDeliveryID(pend.secretID, version)

	link := store.GitHubLink{
		SecretID:             pend.secretID,
		AccountID:            accountID,
		Host:                 pend.host,
		Login:                pend.login,
		GithubUserID:         pend.githubUserID,
		AppClientID:          s.githubLinkAppClientID,
		RefreshToken:         pend.tuple.RefreshToken,
		RefreshExpiresAtUnix: pend.tuple.RefreshExpiresAtUnix,
		AccessToken:          pend.tuple.AccessToken,
		AccessExpiresAtUnix:  pend.tuple.AccessExpiresAtUnix,
		TokenType:            tokenTypeOrBearer(pend.tuple.TokenType),
		Version:              version,
		DeliveryID:           deliveryID,
		UpdatedAt:            now.Unix(),
	}
	if err := s.githubLinkStore.GitHubLinks().Upsert(r.Context(), link); err != nil {
		http.Error(w, "link persist failed", http.StatusInternalServerError)
		return
	}

	// Return the tuple to the OWNER as DR material to seal into the CP-blind user-secrets store
	// (owner-side seal + CP write is owned by sp-7h6.1 / web+spawnctl; the CP stays blind).
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"secret_id":               link.SecretID,
		"host":                    link.Host,
		"login":                   link.Login,
		"github_user_id":          link.GithubUserID,
		"app_client_id":           link.AppClientID,
		"access_token":            link.AccessToken,
		"access_expires_at_unix":  link.AccessExpiresAtUnix,
		"refresh_token":           link.RefreshToken,
		"refresh_expires_at_unix": link.RefreshExpiresAtUnix,
		"token_type":              link.TokenType,
		"version":                 link.Version,
		"delivery_id":             link.DeliveryID,
	})
}

func (s *Service) redirectLinkError(w http.ResponseWriter, r *http.Request, code string) {
	dest, err := url.Parse(s.githubLinkPostRedeem)
	if err != nil {
		http.Error(w, "link failed", http.StatusBadGateway)
		return
	}
	qq := dest.Query()
	qq.Set("error", code)
	dest.RawQuery = qq.Encode()
	http.Redirect(w, r, dest.String(), http.StatusFound)
}

// serveGitHubLinkRevoke is the §16.5 / Decision 24 compromise kill switch. Owner-triggered
// (account-bound): it deletes the GitHub App grant (grant-wide, kills the refresh chain at GitHub)
// AND flips the AS-side revoked flag so subsequent mints fail closed (Get filters revoked=0).
// The DB flip is authoritative for local fail-closed and is attempted even when the GitHub call
// errors (the kill switch must not hinge on GitHub reachability); a remote-teardown failure is
// surfaced as 502 while the link is already locally dead (live access tokens lapse by TTL ≤8h).
func (s *Service) serveGitHubLinkRevoke(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.githubLinkAccountFromReq(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	secretID := strings.TrimSpace(r.FormValue("secret_id"))
	if secretID == "" {
		http.Error(w, "secret_id required", http.StatusBadRequest)
		return
	}
	link, err := s.githubLinkStore.GitHubLinks().Get(r.Context(), secretID)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "link not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "link lookup failed", http.StatusInternalServerError)
		return
	}
	// Account-binding: only the owning account may revoke.
	if link.AccountID != accountID {
		http.Error(w, "link account mismatch", http.StatusForbidden)
		return
	}

	// Remote teardown (best-effort, immediate); capture token BEFORE flipping the flag.
	remoteErr := s.githubLinkExchanger.RevokeAppGrant(r.Context(), link.AccessToken)

	// Local fail-closed: authoritative, always attempted.
	if err := s.githubLinkStore.GitHubLinks().Revoke(r.Context(), secretID, s.now().Unix()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Raced with another revoke; already closed.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "link revoke persist failed", http.StatusInternalServerError)
		return
	}
	if remoteErr != nil {
		// Locally revoked (mints already fail closed) but GitHub teardown failed; operator may retry.
		http.Error(w, "github grant revoke failed; link locally revoked", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
