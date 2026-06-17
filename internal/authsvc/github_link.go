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
	githubLinkFlowTTL    = 15 * time.Minute
	githubLinkCookieTTL  = 5 * time.Minute
	githubLinkCookieName = "as_gh_completer"
	githubLinkCookiePath = "/github/link/redeem"
	githubSecretIDPrefix = "gh:"
	ephemeralPortMin     = 49152
	ephemeralPortMax     = 65535
	reaperInterval       = 60 * time.Second
)

type githubLinkClientKind string

const (
	clientKindWeb      githubLinkClientKind = "web"
	clientKindLoopback githubLinkClientKind = "loopback"
	clientKindDevice   githubLinkClientKind = "device"
)

// githubLinkState is the OAuth state correlator: used ONLY by the callback to find the flow from
// GitHub's state param. Deleted on a SUCCESSFUL exchange (prefetch-DoS guard).
type githubLinkState struct {
	flowID       string
	accountID    string
	secretID     string
	host         string
	verifier     string
	redirectURI  string
	clientKind   githubLinkClientKind
	loopbackPort int
	expiresAt    time.Time
}

type githubLinkFlowStatus int

const (
	flowIssued githubLinkFlowStatus = iota
	flowReady
	flowError
)

// githubLinkFlow is the flow_id-keyed record device polling + redeem operate on.
type githubLinkFlow struct {
	accountID       string
	secretID        string
	host            string
	clientKind      githubLinkClientKind
	loopbackPort    int
	status          githubLinkFlowStatus
	errorCode       string
	completerSecret string             // web cookie / loopback rc; "" for device and until callback
	pending         *githubLinkPending // nil until READY
	consumed        bool               // single-use pop guard
	expiresAt       time.Time
}

type githubLinkPending struct {
	login        string
	githubUserID string
	tuple        GitHubUserToken
}

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
	// RevokeAppToken deletes a single access token (DELETE /applications/{client_id}/token) -- the
	// reaper's targeted teardown of an abandoned flow. Grant/refresh chain untouched. 404 -> nil.
	RevokeAppToken(ctx context.Context, accessToken string) error
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

// RevokeAppToken deletes a single access token via DELETE /applications/{client_id}/token (Basic
// auth + {access_token} body). Targeted teardown of an abandoned flow; grant/refresh chain
// untouched. 204 = revoked, 404 = already gone (idempotent → nil).
func (g *githubClient) RevokeAppToken(ctx context.Context, accessToken string) error {
	body, err := json.Marshal(map[string]string{"access_token": accessToken})
	if err != nil {
		return err
	}
	endpoint := g.apiURL + "/applications/" + url.PathEscape(g.clientID) + "/token"
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
		return fmt.Errorf("github token revoke failed: status %d", resp.StatusCode)
	}
}

// Compile-time assertion that *githubClient implements the link exchanger.
var _ GitHubLinkExchanger = (*githubClient)(nil)

func (s *Service) serveGitHubLinkStart(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.githubLinkAccountFromReq(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	var body struct {
		ClientKind string `json:"client_kind"`
		Port       int    `json:"port"`
		Host       string `json:"host"`
		// secret_id intentionally read-and-ignored: secret_id is account-derived (sec 4).
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "cannot decode body")
		return
	}
	kind := githubLinkClientKind(body.ClientKind)
	switch kind {
	case clientKindWeb, clientKindDevice:
	case clientKindLoopback:
		if body.Port < ephemeralPortMin || body.Port > ephemeralPortMax {
			writeError(w, http.StatusBadRequest, "bad_port", "loopback port must be in the ephemeral range")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "bad_client_kind", "client_kind must be web|loopback|device")
		return
	}
	secretID := githubSecretIDPrefix + accountID
	host := strings.TrimSpace(body.Host)
	if host == "" {
		host = s.githubLinkDefaultHost
	}
	state := randOpaque()
	flowID := randOpaque()
	verifier := randOpaque()
	now := s.now()
	s.githubLinkMu.Lock()
	s.githubLinkStates[state] = githubLinkState{
		flowID: flowID, accountID: accountID, secretID: secretID, host: host,
		verifier: verifier, redirectURI: s.githubLinkRedirectURI,
		clientKind: kind, loopbackPort: body.Port, expiresAt: now.Add(githubLinkFlowTTL),
	}
	s.githubLinkFlows[flowID] = &githubLinkFlow{
		accountID: accountID, secretID: secretID, host: host, clientKind: kind,
		loopbackPort: body.Port, status: flowIssued, expiresAt: now.Add(githubLinkFlowTTL),
	}
	s.githubLinkMu.Unlock()
	authURL := s.githubLinkExchanger.AppAuthorizeURL(state, pkceChallenge(verifier), s.githubLinkRedirectURI)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"authorize_url": authURL, "flow_id": flowID})
}

func (s *Service) serveGitHubLinkCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	now := s.now()

	s.githubLinkMu.Lock()
	st, ok := s.githubLinkStates[state]
	s.githubLinkMu.Unlock()
	if !ok || now.After(st.expiresAt) {
		http.Error(w, "invalid or expired link state", http.StatusBadRequest)
		return
	}

	// (1) GitHub user-denial -> terminal ERROR on the flow + browser error redirect.
	if e := q.Get("error"); e != "" {
		s.githubLinkMu.Lock()
		if fl := s.githubLinkFlows[st.flowID]; fl != nil {
			fl.status, fl.errorCode = flowError, e
		}
		delete(s.githubLinkStates, state) // denial is terminal; state spent
		s.githubLinkMu.Unlock()
		s.redirectLinkError(w, r, e)
		return
	}

	// (2) Junk/forged code that fails the exchange -> NO-OP: state+flow untouched (NOT consumed, NOT
	// ERROR) so a legitimate later completion on the same state can still succeed.
	code := q.Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	tuple, err := s.githubLinkExchanger.ExchangeUserToken(r.Context(), code, st.verifier, st.redirectURI)
	if err != nil {
		http.Error(w, "exchange failed", http.StatusBadRequest)
		return
	}
	user, err := s.githubLinkExchanger.FetchUser(r.Context(), tuple.AccessToken)
	if err != nil {
		http.Error(w, "user fetch failed", http.StatusBadGateway)
		return
	}

	// (3) Success: flow -> READY + pending tuple; mint+deliver the completer secret; consume state.
	var completer string
	if st.clientKind == clientKindWeb || st.clientKind == clientKindLoopback {
		completer = randOpaque()
	}
	s.githubLinkMu.Lock()
	fl := s.githubLinkFlows[st.flowID]
	if fl == nil {
		s.githubLinkMu.Unlock()
		http.Error(w, "flow expired", http.StatusBadRequest)
		return
	}
	fl.status = flowReady
	fl.completerSecret = completer
	fl.pending = &githubLinkPending{login: user.Login, githubUserID: strconv.FormatInt(user.Sub, 10), tuple: tuple}
	delete(s.githubLinkStates, state) // single-use: only on SUCCESSFUL exchange
	s.githubLinkMu.Unlock()

	switch st.clientKind {
	case clientKindWeb:
		http.SetCookie(w, &http.Cookie{
			Name: githubLinkCookieName, Value: completer, Path: githubLinkCookiePath,
			HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, Expires: now.Add(githubLinkCookieTTL),
		})
		http.Redirect(w, r, s.githubLinkPostRedeem, http.StatusFound)
	case clientKindLoopback:
		http.Redirect(w, r, "http://127.0.0.1:"+strconv.Itoa(st.loopbackPort)+"/done?rc="+url.QueryEscape(completer), http.StatusFound)
	default: // device
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<!doctype html><title>Spawnery</title><p>GitHub authorization received. You may close this tab and return to your terminal.</p>")
	}
}

func (s *Service) serveGitHubLinkRedeem(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.githubLinkAccountFromReq(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	var body struct {
		FlowID        string `json:"flow_id"`
		ConfirmSwitch bool   `json:"confirm_switch"`
		RC            string `json:"rc"` // loopback completer; web uses the cookie
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.FlowID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "flow_id required")
		return
	}
	now := s.now()

	s.githubLinkMu.Lock()
	fl := s.githubLinkFlows[body.FlowID]
	if fl == nil || now.After(fl.expiresAt) || fl.consumed {
		s.githubLinkMu.Unlock()
		writeError(w, http.StatusNotFound, "unknown_flow", "unknown or expired flow")
		return
	}
	snap := *fl // do NOT pop yet (confirm_switch may need to leave it READY)
	s.githubLinkMu.Unlock()

	switch snap.status {
	case flowIssued:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
		return
	case flowError:
		writeError(w, http.StatusBadRequest, "link_error", snap.errorCode)
		return
	}
	// flowReady from here.

	if snap.accountID != accountID {
		writeError(w, http.StatusForbidden, "account_mismatch", "flow belongs to a different account")
		return
	}

	// Channel rule: web/loopback REQUIRE the completer secret; device accepts flow_id+Bearer.
	switch snap.clientKind {
	case clientKindWeb:
		c, err := r.Cookie(githubLinkCookieName)
		if err != nil || c.Value == "" || !constantTimeEqual(c.Value, snap.completerSecret) {
			writeError(w, http.StatusForbidden, "channel", "completer cookie required for web flow")
			return
		}
	case clientKindLoopback:
		if body.RC == "" || !constantTimeEqual(body.RC, snap.completerSecret) {
			writeError(w, http.StatusForbidden, "channel", "rc required for loopback flow")
			return
		}
	case clientKindDevice:
		// device: flow_id+Bearer is sufficient; no additional completer secret
	}

	// Ownership guard + identity-continuity peek (no pop).
	existing, err := s.githubLinkStore.GitHubLinks().PeekMeta(r.Context(), snap.secretID)
	switch {
	case errors.Is(err, store.ErrNotFound): // first link -- no prior identity, no confirm needed
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", "link lookup failed")
		return
	default:
		if existing.AccountID != accountID {
			writeError(w, http.StatusForbidden, "account_mismatch", "existing link owned by another account")
			return
		}
		if existing.GithubUserID != snap.pending.githubUserID && !body.ConfirmSwitch {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict) // flow stays READY (not popped)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "identity_change", "old": existing.Login, "new": snap.pending.login})
			return
		}
	}

	// Atomic single-use pop (loser -> 404), then DB-side upsert.
	s.githubLinkMu.Lock()
	cur := s.githubLinkFlows[body.FlowID]
	if cur == nil || cur.consumed {
		s.githubLinkMu.Unlock()
		writeError(w, http.StatusNotFound, "unknown_flow", "flow already consumed")
		return
	}
	cur.consumed = true
	delete(s.githubLinkFlows, body.FlowID)
	s.githubLinkMu.Unlock()

	link := store.GitHubLink{
		SecretID:             snap.secretID,
		AccountID:            accountID,
		Host:                 snap.host,
		Login:                snap.pending.login,
		GithubUserID:         snap.pending.githubUserID,
		AppClientID:          s.githubLinkAppClientID,
		RefreshToken:         snap.pending.tuple.RefreshToken,
		RefreshExpiresAtUnix: snap.pending.tuple.RefreshExpiresAtUnix,
		AccessToken:          snap.pending.tuple.AccessToken,
		AccessExpiresAtUnix:  snap.pending.tuple.AccessExpiresAtUnix,
		TokenType:            tokenTypeOrBearer(snap.pending.tuple.TokenType),
		UpdatedAt:            now.Unix(),
	}
	persisted, err := s.githubLinkStore.GitHubLinks().RedeemUpsert(r.Context(), link)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "link persist failed")
		return
	}

	// METADATA ONLY: no token material crosses this boundary (invariant a).
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"secret_id":      persisted.SecretID,
		"host":           persisted.Host,
		"login":          persisted.Login,
		"github_user_id": persisted.GithubUserID,
		"version":        persisted.Version,
		"updated_at":     persisted.UpdatedAt,
		"status":         "linked",
	})
}

func githubLinkStatus(m store.GitHubLinkMeta) string {
	switch {
	case m.Revoked:
		return "revoked"
	case m.RelinkRequired:
		return "relink_required"
	default:
		return "linked"
	}
}

func (s *Service) serveGitHubLinkList(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.githubLinkAccountFromReq(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	rows, err := s.githubLinkStore.GitHubLinks().List(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "list failed")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		out = append(out, map[string]any{
			"secret_id": m.SecretID, "host": m.Host, "login": m.Login,
			"github_user_id": m.GithubUserID, "version": m.Version,
			"updated_at": m.UpdatedAt, "status": githubLinkStatus(m),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"links": out})
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

// constantTimeEqual compares two strings in constant time (timing-safe).
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range len(a) {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
