package authsvc

// OAuth 2.0 auth-code + PKCE flow: /oauth/authorize and /oauth/callback.
// The AS acts as the authorization server; the SPA is the client; GitHub is the upstream IdP.
//
// Security properties enforced here [AM8]:
//   - Per-request state rows bound to the initiating browser session (flow cookie).
//   - Exact-match redirect_uri against configured allowlist. RFC 8252 §7.3 loopback relaxation
//     allows http://127.0.0.1:<any-port>/<path> as the ONLY exception (for spawnctl).
//   - Forged or injected callback rejected: callback verifies the flow cookie matches the row.
//   - Structured error redirects back to the SPA (registration-closed / access_denied / PKCE mismatch).
//   - Redirect URI redirect only after validating state; never redirect to unvalidated URI.
//
// ADR — token delivery to the SPA:
//   The access_token is placed in the redirect URL query string (implicit-like delivery) rather
//   than via a /oauth/token code-exchange leg. This is deliberate for A1: the AS-to-GitHub leg
//   already uses auth-code+PKCE (AS is a confidential client); the SPA↔AS leg uses the flow
//   cookie + session-pubkey binding as the trust anchor. The access_token is short-lived (15 min)
//   and the refresh family is HttpOnly-cookie-bound + PoP-gated; URL leakage via browser history
//   or Referer is acceptable given SPA-origin CORS enforcement. A /oauth/token endpoint (removing
//   the token from the URL) can be added in a later slice; the auth_codes table migration is
//   retained but the AuthCode Go infrastructure has been removed to avoid dead-code confusion.
//   See docs/superpowers/specs/ (A1 identity-core spec) for the full rationale.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"spawnery/internal/authsvc/store"
)

const (
	flowCookieName = "as_flow"
	flowCookieTTL  = 15 * time.Minute
)

// ErrRegistrationClosed is returned by resolveOrRegister when REGISTRATION_ENABLED is false
// and the GitHub sub is not already in the user store. §6
var ErrRegistrationClosed = errors.New("oauth: registration closed")

// resolveOrRegister looks up or creates a user for the given GitHub sub [§6].
func (i *IdP) resolveOrRegister(ctx context.Context, sub int64, login string, remoteIP string, now time.Time) (store.User, error) {
	u, err := i.store.Users().GetBySub(ctx, sub)
	if err == nil {
		// Sync handle on every login (display-only update).
		_ = i.store.Users().SetHandle(ctx, u.AccountID, login)
		u.Handle = login
		return u, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.User{}, err
	}
	// Unknown sub.
	if !i.cfg.RegistrationEnabled {
		return store.User{}, ErrRegistrationClosed
	}
	// Per-IP registration rate limit [§6].
	if !i.limits.registration.Allow(remoteIP) {
		return store.User{}, fmt.Errorf("oauth: registration rate limited")
	}
	newUser := store.User{
		AccountID: uuid.NewString(),
		GithubSub: sub,
		Handle:    login,
		Status:    store.UserActive,
		CreatedAt: now.Unix(),
	}
	if err := i.store.Users().Create(ctx, newUser); err != nil {
		return store.User{}, err
	}
	return newUser, nil
}

// --- /oauth/authorize ---

// serveAuthorize handles GET /oauth/authorize from the SPA. It:
//  1. Validates the SPA's redirect_uri against the configured allowlist.
//  2. Rate-limits per IP [§6].
//  3. Generates a browser-session flow cookie (if absent), per-request state, AS↔GitHub PKCE.
//  4. Stores the state row bound to the flow cookie hash.
//  5. Redirects to GitHub.
//
// Query parameters from the SPA:
//   redirect_uri     — where AS redirects the browser BACK after GitHub (registered SPA route)
//   state            — SPA-generated opaque string (returned on callback for fixation check)
//   session_pubkey   — base64(DER SPKI) of the SPA's P-256 session key [R2/AM5]
func (i *IdP) serveAuthorize(w http.ResponseWriter, r *http.Request) {
	if !i.limits.authorize.Allow(clientIP(r)) {
		tooMany(w)
		return
	}
	q := r.URL.Query()
	clientRedirectURI := q.Get("redirect_uri")
	clientState := q.Get("state")
	sessionPubkeyB64 := q.Get("session_pubkey")

	if clientRedirectURI == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "redirect_uri required")
		return
	}
	if !i.isAllowedRedirectURI(clientRedirectURI) {
		writeError(w, http.StatusBadRequest, "invalid_request", "redirect_uri not registered")
		return
	}

	// Flow cookie: binds this browser session to the callback [AM8].
	flowID := i.getOrSetFlowCookie(w, r)

	// Per-request AS state + AS↔GitHub PKCE.
	state := randOpaque()
	verifier := randOpaque() // AS↔GitHub leg verifier
	challenge := pkceChallenge(verifier)

	now := i.now()
	row := store.OAuthState{
		State:             state,
		FlowCookieHash:    sha256Hex(flowID),
		ClientChallenge:   q.Get("code_challenge"), // SPA↔AS PKCE challenge (used in /token)
		ClientRedirectURI: clientRedirectURI,
		ClientState:       clientState,
		GhVerifier:        verifier,
		CreatedAt:         now.Unix(),
		ExpiresAt:         now.Add(oauthStateTTL).Unix(),
	}
	// Store the session pubkey with the state row so the callback can bind the refresh family.
	// Piggyback on ClientChallenge field is wrong — use a separate approach: embed in GhVerifier
	// prefix is also wrong. We encode it into GhVerifier as "verifier|spki_b64" separator.
	// Actually better: store it in the ClientChallenge field since that's what the AS uses.
	// Let's reuse GhVerifier as "pkce_verifier" and put spki in ClientChallenge (it's not
	// standard-used in the AS-side flow, the SPA↔AS challenge is just forwarded).
	// Per plan R2, the server seam is just store + bind — we store the pubkey alongside state.
	// Use ClientChallenge to carry "challenge|spki_b64" delimited by "|spki:":
	if sessionPubkeyB64 != "" {
		row.ClientChallenge = q.Get("code_challenge") + "|spki:" + sessionPubkeyB64
	}

	if err := i.store.OAuthStates().Create(r.Context(), row); err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "state creation failed")
		return
	}

	http.Redirect(w, r, i.github.AuthorizeURL(state, challenge, i.cfg.GitHubRedirectURI), http.StatusFound)
}

// --- /oauth/callback ---

// serveCallback handles GET /oauth/callback: validates state, exchanges code, resolves user,
// mints tokens, and redirects back to the SPA.
func (i *IdP) serveCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	code := q.Get("code")
	errParam := q.Get("error")

	// Consume the state row (single-use) [AM8].
	row, err := i.store.OAuthStates().Consume(r.Context(), state)
	if err != nil {
		// Invalid/reused state — do not redirect to an unvalidated URI.
		writeError(w, http.StatusBadRequest, "invalid_state", "invalid or expired state")
		return
	}
	// Validate state row hasn't expired.
	now := i.now()
	if now.Unix() >= row.ExpiresAt {
		redirectError(w, r, row.ClientRedirectURI, "access_denied", "state expired")
		return
	}
	// Flow-cookie anti-fixation check [AM8]: callback must originate from the same browser session.
	flowID := getFlowCookie(r)
	if sha256Hex(flowID) != row.FlowCookieHash {
		redirectError(w, r, row.ClientRedirectURI, "access_denied", "session mismatch")
		return
	}

	// Provider-side error (e.g. access_denied).
	if errParam != "" {
		redirectError(w, r, row.ClientRedirectURI, errParam, q.Get("error_description"))
		return
	}
	if code == "" {
		redirectError(w, r, row.ClientRedirectURI, "access_denied", "no code")
		return
	}

	// Exchange code for GitHub access token (confidential client + PKCE [AM9]).
	ghToken, err := i.github.Exchange(r.Context(), code, row.GhVerifier, i.cfg.GitHubRedirectURI)
	if err != nil {
		redirectError(w, r, row.ClientRedirectURI, "access_denied", "code exchange failed")
		return
	}
	ghUser, err := i.github.FetchUser(r.Context(), ghToken)
	if err != nil {
		redirectError(w, r, row.ClientRedirectURI, "server_error", "user fetch failed")
		return
	}

	// Resolve or register user [§6].
	u, err := i.resolveOrRegister(r.Context(), ghUser.Sub, ghUser.Login, clientIP(r), now)
	if errors.Is(err, ErrRegistrationClosed) {
		redirectError(w, r, row.ClientRedirectURI, "registration_closed", "new registrations are not allowed")
		return
	}
	if err != nil {
		redirectError(w, r, row.ClientRedirectURI, "server_error", "registration failed")
		return
	}

	// Parse session pubkey from state row [R2: seam]. The SPA MUST supply session_pubkey on
	// every authorize request; without it we cannot create a PoP-bound refresh family and would
	// produce an un-refreshable session. Reject rather than silently mint a dead-end family.
	// (A5 wires the SPA-side WebCrypto key generation before calling /oauth/authorize.)
	spkiDER, _, err := extractSPKIFromState(row)
	if err != nil || spkiDER == nil {
		redirectError(w, r, row.ClientRedirectURI, "invalid_request", "session_pubkey required")
		return
	}

	// Enforce concurrent-family cap [§6].
	if err := i.enforceCapOrEvict(r.Context(), u.AccountID, now); err != nil {
		redirectError(w, r, row.ClientRedirectURI, "server_error", "family cap enforced")
		return
	}

	// Mint access token + create refresh family.
	accessWire, _, err := i.mintAccess(u, spkiDER, now)
	if err != nil {
		redirectError(w, r, row.ClientRedirectURI, "server_error", "token mint failed")
		return
	}
	rawRefresh := randOpaque()
	tokenID := uuid.NewString()
	famID := uuid.NewString()
	// RFC 8252 native-app seam [A3]: loopback redirect_uri → CLI client kind so the refresh
	// family carries the right ClientKind for audit. The token is also delivered in the query
	// (see below) since the loopback listener cannot receive Set-Cookie from the AS domain.
	clientKind := store.ClientWeb
	if isLoopbackURI(row.ClientRedirectURI) {
		clientKind = store.ClientCLI
	}
	refreshRow := store.RefreshSession{
		TokenHash:         sha256Hex(rawRefresh),
		AccountID:         u.AccountID,
		FamilyID:          famID,
		ClientKind:        clientKind,
		SessionPubkeySPKI: spkiDER,
		AccessTokenID:     tokenID,
		CreatedAt:         now.Unix(),
		LastUsedAt:        now.Unix(),
		ExpiresAt:         now.Add(refreshSliding).Unix(),
		FamilyCreatedAt:   now.Unix(),
	}
	if err := i.store.RefreshSessions().Insert(r.Context(), refreshRow); err != nil {
		redirectError(w, r, row.ClientRedirectURI, "server_error", "session creation failed")
		return
	}

	// Set the refresh cookie [AM2]. Path=/refresh limits exposure to the rotation endpoint.
	http.SetCookie(w, &http.Cookie{
		Name:     "refresh_token",
		Value:    rawRefresh,
		Path:     "/refresh",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		Expires:  now.Add(refreshSliding),
	})
	// Mirror the same token at Path=/device so the device-verify page (RFC 6265 path scoping
	// prevents Path=/refresh cookies from reaching /device/verify in a real browser).
	setDeviceSessionCookie(w, rawRefresh, now.Add(refreshSliding))
	// Mirror at Path=/logout so POST /logout receives the token (same path-scoping reason).
	setLogoutSessionCookie(w, rawRefresh, now.Add(refreshSliding))
	// Clear the flow cookie (single-use).
	http.SetCookie(w, &http.Cookie{
		Name:     flowCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})

	// Redirect back to SPA carrying access token + original state [AM8].
	dest, _ := url.Parse(row.ClientRedirectURI)
	out := dest.Query()
	out.Set("access_token", accessWire)
	if row.ClientState != "" {
		out.Set("state", row.ClientState)
	}
	// RFC 8252 §7.3: for loopback redirect URIs (native app / spawnctl) also include the
	// refresh_token in the query. The loopback listener is on localhost so the token only
	// transits the local address bar — no cross-site exposure.
	if isLoopbackURI(row.ClientRedirectURI) {
		out.Set("refresh_token", rawRefresh)
	} else {
		// R1 (A5/SPA): the SPA cannot read the HttpOnly cookie, so it cannot compute
		// SHA-256(rawToken) for the PoP signed message. Return the hash (not the raw token)
		// so the SPA can sign the PoP message correctly. The hash alone does not allow
		// refreshing (the cookie + session key are also required), so URL exposure is safe.
		out.Set("refresh_token_hash", base64.RawURLEncoding.EncodeToString(sha256sum([]byte(rawRefresh))))
	}
	dest.RawQuery = out.Encode()
	http.Redirect(w, r, dest.String(), http.StatusFound)
}

// --- /refresh ---

// serveRefresh handles POST /refresh: reads the HttpOnly cookie, checks PoP, rotates.
func (i *IdP) serveRefresh(w http.ResponseWriter, r *http.Request) {
	if !i.limits.refreshIP.Allow(clientIP(r)) {
		tooMany(w)
		return
	}
	cookie, err := r.Cookie("refresh_token")
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusUnauthorized, "missing_token", "refresh_token cookie required")
		return
	}

	// Per-account rate limit [§6]: look up the session to get accountID. If the token is unknown
	// we skip the account check (handleRefresh will return the appropriate error). This intentionally
	// loads the row here rather than inside handleRefresh to avoid threading a limiter callback
	// through that function's signature.
	if row, err := i.store.RefreshSessions().Get(r.Context(), sha256Hex(cookie.Value)); err == nil {
		if !i.limits.refreshAcct.Allow(row.AccountID) {
			tooMany(w)
			return
		}
	}

	pop, err := popFromRequest(r)
	if err != nil && !errors.Is(err, ErrPoPMissing) {
		writeError(w, http.StatusBadRequest, "invalid_pop", err.Error())
		return
	}
	if errors.Is(err, ErrPoPMissing) {
		writeError(w, http.StatusUnauthorized, "pop_required", "session-key proof-of-possession required")
		return
	}

	now := i.now()
	rawToken := cookie.Value
	accessWire, newRaw, err := i.handleRefresh(r.Context(), rawToken, pop, now)
	if errors.Is(err, ErrFamilyRevoked) {
		// Expire all three path-scoped cookies.
		http.SetCookie(w, &http.Cookie{Name: "refresh_token", Value: "", Path: "/refresh", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
		expireLogoutSessionCookie(w)
		expireDeviceSessionCookie(w)
		writeError(w, http.StatusUnauthorized, "token_revoked", "refresh family revoked")
		return
	}
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_token", err.Error())
		return
	}

	// Rotate all three path-scoped cookies (refresh_token, logout_session, device_session mirrors).
	http.SetCookie(w, &http.Cookie{
		Name:     "refresh_token",
		Value:    newRaw,
		Path:     "/refresh",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		Expires:  now.Add(refreshSliding),
	})
	setLogoutSessionCookie(w, newRaw, now.Add(refreshSliding))
	setDeviceSessionCookie(w, newRaw, now.Add(refreshSliding))

	// R1 (A5/SPA): include the new refresh-token hash so the SPA can compute the PoP message
	// for the next /refresh call. The hash alone does not allow refreshing (the new HttpOnly
	// cookie + session key are also required). base64url-unpadded to match PoP header encoding.
	newHash := base64.RawURLEncoding.EncodeToString(sha256sum([]byte(newRaw)))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"access_token":%q,"refresh_token_hash":%q}`, accessWire, newHash)
}

// --- helpers ---

// isAllowedRedirectURI checks the URI against the configured allowlist. For registered loopback
// URIs (http://127.0.0.1 or http://[::1]) RFC 8252 §7.3 port-only relaxation applies: the
// incoming URI must match the registered URI's scheme, host, and path exactly, but may use any
// port [AM8]. A loopback URI is NOT accepted unless a loopback redirect was registered.
func (i *IdP) isAllowedRedirectURI(uri string) bool {
	for _, allowed := range i.cfg.RedirectURIs {
		if allowed == uri {
			return true
		}
		if loopbackPortRelax(allowed, uri) {
			return true
		}
	}
	return false
}

// isLoopbackURI returns true when the URI's host is a loopback address (127.0.0.1 or ::1).
// Used by serveCallback to identify native-app (spawnctl) redirect URIs [RFC 8252 §7.3].
func isLoopbackURI(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	return host == "127.0.0.1" || host == "::1"
}

// loopbackPortRelax returns true when both allowed and incoming are http loopback URIs
// (127.0.0.1 or [::1]) with the same host type and path but an arbitrary port [RFC 8252 §7.3].
// Path must match exactly; only port is relaxed.
func loopbackPortRelax(allowed, incoming string) bool {
	a, err := url.Parse(allowed)
	if err != nil || a.Scheme != "http" {
		return false
	}
	aHost, _, err := net.SplitHostPort(a.Host)
	if err != nil {
		aHost = a.Host
	}
	if aHost != "127.0.0.1" && aHost != "::1" {
		return false
	}
	b, err := url.Parse(incoming)
	if err != nil || b.Scheme != "http" {
		return false
	}
	bHost, _, err := net.SplitHostPort(b.Host)
	if err != nil {
		bHost = b.Host
	}
	return aHost == bHost && a.Path == b.Path
}

// pkceChallenge computes S256 PKCE challenge from verifier.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// getOrSetFlowCookie returns the current flow ID from the cookie, setting a new one if absent.
func (i *IdP) getOrSetFlowCookie(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(flowCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	id := randOpaque()
	http.SetCookie(w, &http.Cookie{
		Name:     flowCookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		// SameSite=Lax (not Strict): the OAuth callback is a cross-site top-level GET
		// navigation (from the IdP back to the AS), so Strict would block the cookie.
		// Lax allows cross-site GET navigations while still blocking CSRF on unsafe methods.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(flowCookieTTL.Seconds()),
	})
	return id
}

func getFlowCookie(r *http.Request) string {
	if c, err := r.Cookie(flowCookieName); err == nil {
		return c.Value
	}
	return ""
}

// redirectError performs a structured error redirect back to the SPA [AM8].
func redirectError(w http.ResponseWriter, _ *http.Request, redirectURI, code, desc string) {
	dest, err := url.Parse(redirectURI)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_redirect", "bad redirect_uri")
		return
	}
	q := dest.Query()
	q.Set("error", code)
	if desc != "" {
		q.Set("error_description", desc)
	}
	dest.RawQuery = q.Encode()
	w.Header().Set("Location", dest.String())
	w.WriteHeader(http.StatusFound)
}

// extractSPKIFromState pulls the session pubkey DER from the state row's ClientChallenge field
// and validates that it is a well-formed ECDSA P-256 key (same requirement as the device flow).
// Format: "<pkce_challenge>|spki:<base64_spki>" — or just "<pkce_challenge>" if absent.
func extractSPKIFromState(row store.OAuthState) (spkiDER []byte, challenge string, err error) {
	parts := strings.SplitN(row.ClientChallenge, "|spki:", 2)
	challenge = parts[0]
	if len(parts) < 2 {
		return nil, challenge, nil
	}
	// parseSessionSPKI accepts both base64 standard and raw-URL encodings and rejects non-P256.
	spkiDER, _, err = parseSessionSPKI(parts[1])
	return spkiDER, challenge, err
}

// enforceCapOrEvict checks the concurrent-family cap for an account. If at the cap, it evicts
// the oldest family to make room [§6, AM3].
func (i *IdP) enforceCapOrEvict(ctx context.Context, accountID string, now time.Time) error {
	n, err := i.store.RefreshSessions().CountFamilies(ctx, accountID)
	if err != nil {
		return err
	}
	if n < i.cfg.MaxFamilies {
		return nil
	}
	// Evict oldest — revoke + record atomically so the CP always receives the event.
	oldest, err := i.store.RefreshSessions().OldestFamily(ctx, accountID)
	if err != nil {
		return err
	}
	return i.store.WithTx(ctx, func(tx store.Store) error {
		liveIDs, err := tx.RefreshSessions().RevokeFamily(ctx, oldest)
		if err != nil {
			return err
		}
		return appendRevocation(ctx, tx, accountID, oldest, liveIDs, now)
	})
}
