package authsvc

// /logout — revoke family + expire cookie + emit revocation event [AM10].
// "Sign out everywhere" revokes ALL non-revoked families for the account.
//
// Cookie path scoping: the refresh_token cookie lives at Path=/refresh (RFC 6265 prevents it
// from reaching /logout in a real browser). /logout reads the logout_session mirror cookie
// (Path=/logout), which carries the same raw token value. Both are set together by serveCallback
// and serveRefresh. The device_session cookie uses the same pattern for Path=/device.

import (
	"context"
	"net/http"
	"time"

	"spawnery/internal/authsvc/store"
)

const logoutSessionCookieName = "logout_session"

// setLogoutSessionCookie sets the browser-session cookie scoped to Path=/logout so that
// POST /logout receives it. The refresh_token cookie lives at Path=/refresh and RFC 6265
// path scoping prevents it from reaching /logout in a real browser — this parallel cookie
// carries the same raw token value for the logout auth check only.
func setLogoutSessionCookie(w http.ResponseWriter, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     logoutSessionCookieName,
		Value:    value,
		Path:     "/logout",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		Expires:  expires,
	})
}

// expireLogoutSessionCookie clears the logout_session cookie.
func expireLogoutSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     logoutSessionCookieName,
		Value:    "",
		Path:     "/logout",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

// serveLogout handles POST /logout. Accepts ?everywhere=1 to revoke all families.
func (i *IdP) serveLogout(w http.ResponseWriter, r *http.Request) {
	// Read the logout_session mirror cookie (Path=/logout). The refresh_token cookie lives at
	// Path=/refresh and a browser will not send it to /logout (RFC 6265 path scoping).
	cookie, err := r.Cookie(logoutSessionCookieName)
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusUnauthorized, "missing_token", "logout_session cookie required")
		return
	}
	now := i.now()
	row, err := i.store.RefreshSessions().Get(r.Context(), sha256Hex(cookie.Value))
	if err != nil {
		// Cookie doesn't match any session — still expire it.
		expireRefreshCookie(w)
		writeError(w, http.StatusUnauthorized, "invalid_token", "session not found")
		return
	}

	var logoutErr error
	if r.URL.Query().Get("everywhere") == "1" {
		i.logoutEverywhere(r.Context(), row.AccountID, now)
	} else {
		logoutErr = i.store.WithTx(r.Context(), func(tx store.Store) error {
			liveIDs, err := tx.RefreshSessions().RevokeFamily(r.Context(), row.FamilyID)
			if err != nil {
				return err
			}
			return appendRevocation(r.Context(), tx, row.AccountID, row.FamilyID, liveIDs, now)
		})
	}

	expireRefreshCookie(w) // always expire the cookie regardless of revocation outcome
	if logoutErr != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "logout revocation failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// logoutEverywhereMaxFamilies is the safety cap for the logout-everywhere loop. It is larger
// than cfg.MaxFamilies (default 20) so normal accounts always complete, while guarding against
// an infinite loop if the OldestFamily invariant (returns only non-revoked rows) ever regresses.
const logoutEverywhereMaxFamilies = 200

// logoutEverywhere revokes all non-revoked families for the account and emits per-family events.
func (i *IdP) logoutEverywhere(ctx context.Context, accountID string, now interface{ Unix() int64 }) {
	// Iterate via OldestFamily; each successful RevokeFamily removes the family from the
	// non-revoked set, so the loop terminates when OldestFamily returns ErrNotFound.
	// The cap prevents an infinite loop if that invariant ever regresses.
	for range logoutEverywhereMaxFamilies {
		oldest, err := i.store.RefreshSessions().OldestFamily(ctx, accountID)
		if err != nil {
			return
		}
		if err := i.store.WithTx(ctx, func(tx store.Store) error {
			liveIDs, err := tx.RefreshSessions().RevokeFamily(ctx, oldest)
			if err != nil {
				return err
			}
			return appendRevocation(ctx, tx, accountID, oldest, liveIDs, i.now())
		}); err != nil {
			return
		}
	}
}

// expireRefreshCookie expires the refresh_token, logout_session, and device_session cookies.
func expireRefreshCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "refresh_token",
		Value:    "",
		Path:     "/refresh",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	expireLogoutSessionCookie(w)
	expireDeviceSessionCookie(w)
}
