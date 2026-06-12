package authsvc

import (
	"net/http"
	"strings"
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
