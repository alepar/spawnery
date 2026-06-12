package authsvc

// /logout — revoke family + expire cookie + emit revocation event [AM10].
// "Sign out everywhere" revokes ALL non-revoked families for the account.

import (
	"context"
	"net/http"
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

	if r.URL.Query().Get("everywhere") == "1" {
		i.logoutEverywhere(r.Context(), row.AccountID, now)
	} else {
		liveIDs, rErr := i.store.RefreshSessions().RevokeFamily(r.Context(), row.FamilyID)
		if rErr == nil {
			_ = appendRevocation(r.Context(), i.store, row.AccountID, row.FamilyID, liveIDs, now)
		}
	}

	expireRefreshCookie(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// logoutEverywhere revokes all non-revoked families for the account and emits per-family events.
func (i *IdP) logoutEverywhere(ctx context.Context, accountID string, now interface{ Unix() int64 }) {
	// We iterate families via CountFamilies/OldestFamily loop to avoid a bulk-select.
	// In practice accounts have ≤20 families (cap), so this is fine.
	for {
		oldest, err := i.store.RefreshSessions().OldestFamily(ctx, accountID)
		if err != nil {
			return
		}
		liveIDs, err := i.store.RefreshSessions().RevokeFamily(ctx, oldest)
		if err != nil {
			return
		}
		_ = appendRevocation(ctx, i.store, accountID, oldest, liveIDs, i.now())
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
