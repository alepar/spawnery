package authsvc

import (
	"net/http"
	"strings"
	"time"

	"spawnery/internal/authsvc/token"
)

// AccountFromRequest extracts the authenticated account ID from an HTTP request.
// Returns ("", false) if the request is not authenticated or the account cannot
// be resolved.  Implementations are injected at construction time so tests can
// substitute a deterministic stand-in without real JWT parsing (injectable seam).
type AccountFromRequest func(r *http.Request) (accountID string, ok bool)

// FixedAccountFromRequest always returns the given accountID.  Useful in tests.
func FixedAccountFromRequest(accountID string) AccountFromRequest {
	return func(_ *http.Request) (string, bool) { return accountID, true }
}

// SessionBearerAccount extracts the account from a Bearer AS session token, verified against the
// AS's own published key set (own + next). Used to authenticate the owner-driven GitHub link
// endpoints (one link per account; secret_id is account-derived). A missing/expired/forged token
// or a blank account returns ("", false).
//
// Audience checking is intentionally omitted — any validly AS-signed live session identifies the
// account; the link redeem adds the channel-completer secret (cookie/rc) on top per the
// owner-link design. If now is nil, time.Now is used.
func SessionBearerAccount(ks token.KeySet, now func() time.Time) AccountFromRequest {
	if now == nil {
		now = time.Now
	}
	return BearerTokenAccount(func(tok string) (string, bool) {
		body, err := token.Verify(tok, ks, now())
		if err != nil || body.GetAccountId() == "" {
			return "", false
		}
		return body.GetAccountId(), true
	})
}

// BearerTokenAccount extracts the account ID from the Authorization header via a
// caller-supplied lookup function (e.g. JWT parsing).  The lookup receives the
// raw token string (after stripping "Bearer ") and returns the account ID.
// Returns ("", false) if the header is missing, malformed, or the lookup returns
// a blank ID.
func BearerTokenAccount(lookup func(token string) (accountID string, ok bool)) AccountFromRequest {
	return func(r *http.Request) (string, bool) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			return "", false
		}
		tok, found := strings.CutPrefix(auth, "Bearer ")
		if !found || tok == "" {
			return "", false
		}
		id, ok := lookup(tok)
		if !ok || id == "" {
			return "", false
		}
		return id, true
	}
}
