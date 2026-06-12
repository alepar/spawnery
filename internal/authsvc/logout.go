package authsvc

// /logout — revoke family + expire cookie + emit revocation event [AM10].
// "Sign out everywhere" revokes ALL non-revoked families for the account.

import (
	"context"
	"net/http"

	"spawnery/internal/authsvc/store"
)

// serveLogout handles POST /logout. Accepts ?everywhere=1 to revoke all families.
func (i *IdP) serveLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("refresh_token")
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusUnauthorized, "missing_token", "refresh_token cookie required")
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

// expireRefreshCookie sets the refresh_token cookie to expired.
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
}
