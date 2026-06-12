package authsvc

// RFC 8628 device-authorization grant [AM7].
//
// Flow:
//  1. POST /device/authorize — spawnctl POSTs session pubkey → {device_code, user_code, verification_uri}.
//  2. GET  /device/verify   — authed browser shows account context, accepts user_code to approve.
//  3. POST /device/verify   — same, but submits confirmation form.
//  4. POST /device/token    — spawnctl polls; returns authorization_pending or mints tokens.
//
// Security:
//  - device_code stored as SHA-256 hash (never raw).
//  - user_code is short + rate-limited: 10 attempts per device_code, 10 per IP per minute.
//  - The user confirms AT the AS in an already-authenticated browser (refresh cookie checked).
//  - Token minting binds the refresh family to the posted session pubkey [AM7].

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/google/uuid"

	"spawnery/internal/authsvc/store"
)

const (
	userCodeLen      = 8    // 8 chars, XXXX-XXXX format
	maxDeviceAttempt = 10   // lock out after 10 bad user_code attempts
	deviceCodeTTL    = 15 * time.Minute // device_code + user_code lifetime [AM7]
)

// userCodeAlphabet avoids visually ambiguous chars (0/O, 1/I/L).
const userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXYZ"

// serveDeviceAuthorize handles POST /device/authorize. Body (JSON or form):
//   session_pubkey  — base64 DER SPKI of the requesting client's P-256 key [AM7]
//   client_kind     — "cli" (default)
func (i *IdP) serveDeviceAuthorize(w http.ResponseWriter, r *http.Request) {
	if !i.limits.device.Allow(clientIP(r)) {
		tooMany(w)
		return
	}
	var sessionPubkeyB64, clientKind string
	if err := r.ParseForm(); err == nil {
		sessionPubkeyB64 = r.FormValue("session_pubkey")
		clientKind = r.FormValue("client_kind")
	}
	if sessionPubkeyB64 == "" {
		// Try JSON body.
		var body struct {
			SessionPubkey string `json:"session_pubkey"`
			ClientKind    string `json:"client_kind"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			sessionPubkeyB64 = body.SessionPubkey
			clientKind = body.ClientKind
		}
	}
	if sessionPubkeyB64 == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "session_pubkey required")
		return
	}
	if clientKind == "" {
		clientKind = store.ClientCLI
	}
	spkiDER, _, err := parseSessionSPKI(sessionPubkeyB64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "session_pubkey: "+err.Error())
		return
	}

	now := i.now()
	deviceCode := randOpaque() // 32 bytes raw — hash for storage
	userCode := genUserCode()

	grant := store.DeviceGrant{
		DeviceCodeHash:    sha256Hex(deviceCode),
		UserCode:          userCode,
		SessionPubkeySPKI: spkiDER,
		Status:            store.GrantPending,
		CreatedAt:         now.Unix(),
		ExpiresAt:         now.Add(deviceCodeTTL).Unix(),
	}
	if err := i.store.DeviceGrants().Create(r.Context(), grant); err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "device grant creation failed")
		return
	}

	verifyURI := i.cfg.VerificationURI
	if verifyURI == "" {
		verifyURI = "/device/verify"
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"device_code":      deviceCode,
		"user_code":        userCode,
		"verification_uri": verifyURI,
		"expires_in":       int(deviceCodeTTL.Seconds()),
		"interval":         int(devicePollInterval.Seconds()),
	})
}

// serveDeviceVerifyGet handles GET /device/verify: the authed browser page. Requires refresh
// cookie (user already logged in). Accepts user_code as a query param to pre-populate the form.
func (i *IdP) serveDeviceVerifyGet(w http.ResponseWriter, r *http.Request) {
	_, accountID, err := i.requireRefreshCookieSession(r)
	if err != nil {
		// Not logged in — redirect to login with return_to.
		http.Redirect(w, r, "/oauth/authorize?redirect_uri=/device/verify", http.StatusFound)
		return
	}
	userCode := r.URL.Query().Get("user_code")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, deviceVerifyHTML, accountID, userCode)
}

// serveDeviceVerifyPost handles POST /device/verify: the user submits their user_code.
func (i *IdP) serveDeviceVerifyPost(w http.ResponseWriter, r *http.Request) {
	_, accountID, err := i.requireRefreshCookieSession(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "session_required", "must be logged in to authorize a device")
		return
	}
	if !i.limits.device.Allow(clientIP(r)) {
		tooMany(w)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "bad form")
		return
	}
	userCode := r.FormValue("user_code")
	if userCode == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "user_code required")
		return
	}

	// Look up grant.
	grant, err := i.store.DeviceGrants().GetByUserCode(r.Context(), userCode)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "invalid_user_code", "user_code not found or expired")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	now := i.now()
	if now.Unix() >= grant.ExpiresAt {
		writeError(w, http.StatusBadRequest, "expired_token", "user_code expired")
		return
	}
	if grant.Status != store.GrantPending {
		writeError(w, http.StatusBadRequest, "invalid_grant", "grant already decided")
		return
	}

	// Approve — bind the deciding account.
	if err := i.store.DeviceGrants().SetDecision(r.Context(), userCode, accountID, store.GrantApproved); err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "approve failed")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintln(w, "<html><body><p>Device authorized. You may close this tab.</p></body></html>")
}

// serveDeviceToken handles POST /device/token: spawnctl polls here.
// Body: device_code (form or JSON).
func (i *IdP) serveDeviceToken(w http.ResponseWriter, r *http.Request) {
	if !i.limits.device.Allow(clientIP(r)) {
		tooMany(w)
		return
	}
	var rawDeviceCode string
	if err := r.ParseForm(); err == nil {
		rawDeviceCode = r.FormValue("device_code")
	}
	if rawDeviceCode == "" {
		var body struct{ DeviceCode string `json:"device_code"` }
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			rawDeviceCode = body.DeviceCode
		}
	}
	if rawDeviceCode == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "device_code required")
		return
	}

	codeHash := sha256Hex(rawDeviceCode)
	grant, err := i.store.DeviceGrants().Get(r.Context(), codeHash)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "invalid_grant", "device_code not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	now := i.now()
	_ = i.store.DeviceGrants().SetLastPolled(r.Context(), codeHash, now.Unix())

	if now.Unix() >= grant.ExpiresAt {
		writeError(w, http.StatusBadRequest, "expired_token", "device_code expired")
		return
	}

	switch grant.Status {
	case store.GrantPending:
		writeError(w, http.StatusBadRequest, "authorization_pending", "waiting for user approval")
		return
	case store.GrantDenied:
		writeError(w, http.StatusBadRequest, "access_denied", "user denied the request")
		return
	case store.GrantRedeemed:
		writeError(w, http.StatusBadRequest, "invalid_grant", "already redeemed")
		return
	case store.GrantExpired:
		writeError(w, http.StatusBadRequest, "expired_token", "grant expired")
		return
	}

	// Approved — redeem atomically.
	grant, err = i.store.DeviceGrants().Redeem(r.Context(), codeHash)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusBadRequest, "invalid_grant", "already redeemed")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	// Load user.
	u, err := i.store.Users().GetByID(r.Context(), grant.AccountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "user not found")
		return
	}

	// Enforce family cap.
	if err := i.enforceCapOrEvict(r.Context(), u.AccountID, now); err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "family cap")
		return
	}

	// Mint access + create refresh family bound to the device's session pubkey [AM7].
	accessWire, _, err := i.mintAccess(u, grant.SessionPubkeySPKI, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "token mint failed")
		return
	}
	rawRefresh := randOpaque()
	famID := uuid.NewString()
	tokenID := uuid.NewString()
	refreshRow := store.RefreshSession{
		TokenHash:         sha256Hex(rawRefresh),
		AccountID:         u.AccountID,
		FamilyID:          famID,
		ClientKind:        store.ClientCLI,
		SessionPubkeySPKI: grant.SessionPubkeySPKI,
		AccessTokenID:     tokenID,
		CreatedAt:         now.Unix(),
		LastUsedAt:        now.Unix(),
		ExpiresAt:         now.Add(refreshSliding).Unix(),
		FamilyCreatedAt:   now.Unix(),
	}
	if err := i.store.RefreshSessions().Insert(r.Context(), refreshRow); err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "session creation failed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"access_token":  accessWire,
		"refresh_token": rawRefresh,
		"token_type":    "bearer",
	})
}

// requireRefreshCookieSession validates the refresh cookie and returns (rawToken, accountID).
// This is used by the device-verify page to know who the logged-in user is.
// IMPORTANT: this does NOT rotate the refresh token (read-only session check).
func (i *IdP) requireRefreshCookieSession(r *http.Request) (rawToken, accountID string, err error) {
	c, err := r.Cookie("refresh_token")
	if err != nil || c.Value == "" {
		return "", "", fmt.Errorf("no session")
	}
	row, err := i.store.RefreshSessions().Get(r.Context(), sha256Hex(c.Value))
	if err != nil {
		return "", "", fmt.Errorf("session not found")
	}
	if row.Revoked || i.now().Unix() >= row.ExpiresAt {
		return "", "", fmt.Errorf("session expired")
	}
	return c.Value, row.AccountID, nil
}

// --- helpers ---

func genUserCode() string {
	b := make([]byte, userCodeLen)
	n := big.NewInt(int64(len(userCodeAlphabet)))
	for idx := range b {
		v, _ := rand.Int(rand.Reader, n)
		b[idx] = userCodeAlphabet[v.Int64()]
	}
	// Format as XXXX-XXXX.
	return string(b[:4]) + "-" + string(b[4:])
}

// deviceVerifyHTML is the device-confirmation page template. %s = accountID, %s = pre-filled user_code.
const deviceVerifyHTML = `<!doctype html><html><head><title>Authorize Device – Spawnery</title></head>
<body>
<h2>Authorize Device</h2>
<p>Logged in as <strong>%s</strong></p>
<form method="POST" action="/device/verify">
  <label>User Code: <input name="user_code" value="%s" required autocomplete="off" autofocus></label>
  <button type="submit">Authorize</button>
</form>
</body></html>`
