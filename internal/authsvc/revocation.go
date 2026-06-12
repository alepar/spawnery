package authsvc

// GET /revocations?since=<seq> — signed revocation feed for the CP (A2) to poll [AM10/(7)].
//
// Signing: each entry is signed with the AS session Ed25519 key under domain prefix
// "spawnery/revocation/v1" (token.RevocationDomainPrefix). A2 verifies against the same
// key set it uses for session tokens (same role, distinct domain). See token package.
//
// Response: JSON array of SignedRevocationEntry. A2 pins the AS public key (same key set
// it receives for session tokens) and verifies the sig field over the entry bytes. The CP
// must advance its checkpoint past the highest seq it has processed to avoid re-delivering.
//
// R7 note: reusing the AS session-signing key for the revocation feed means the CP pins one
// key set and consumes both artifacts. This is deliberate; confirm with A2 owner if a
// dedicated feed key is required (the domain prefix already separates the two namespaces).
//
// Access control: if IdPConfig.CPSecret is non-empty (production), the CP MUST supply
// "Authorization: Bearer <CPSecret>" or the request is rejected 401. This is a
// server-to-server trust boundary; configure the secret via env/deploy config (see deploy/authsvc).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"spawnery/internal/authsvc/store"
	"spawnery/internal/authsvc/token"
)

// SignedRevocationEntry is one entry in the /revocations feed that A2 consumes.
type SignedRevocationEntry struct {
	Seq       int64  `json:"seq"`
	AccountID string `json:"account_id"`
	FamilyID  string `json:"family_id"`
	TokenIDs  string `json:"token_ids"` // JSON array of access-token token_ids
	RevokedAt int64  `json:"revoked_at"`
	Sig       string `json:"sig"` // base64url(ed25519(RevocationDomainPrefix || entry_bytes))
}

// serveRevocations handles GET /revocations?since=<seq>.
// Returns all events with seq > since, each signed with the AS session key [AM10].
func (i *IdP) serveRevocations(w http.ResponseWriter, r *http.Request) {
	if i.cfg.CPSecret != "" {
		if r.Header.Get("Authorization") != "Bearer "+i.cfg.CPSecret {
			writeError(w, http.StatusUnauthorized, "unauthorized", "CP bearer secret required")
			return
		}
	}
	sinceStr := r.URL.Query().Get("since")
	var since int64
	if sinceStr != "" {
		var err error
		since, err = strconv.ParseInt(sinceStr, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "since must be an integer")
			return
		}
	}

	evs, err := i.store.Revocations().Since(r.Context(), since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	entries := make([]SignedRevocationEntry, 0, len(evs))
	for _, ev := range evs {
		e, err := i.signRevocationEntry(ev)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "server_error", "signing failed")
			return
		}
		entries = append(entries, e)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(entries)
}

// signRevocationEntry signs a revocation event using the token.SignArtifact discipline.
// The entry bytes are the canonical JSON of {seq,account_id,family_id,token_ids,revoked_at}.
// The sig is over (RevocationDomainPrefix || entry_bytes) — same raw-bytes discipline as tokens.
func (i *IdP) signRevocationEntry(ev store.RevocationEvent) (SignedRevocationEntry, error) {
	type entryBody struct {
		Seq       int64  `json:"seq"`
		AccountID string `json:"account_id"`
		FamilyID  string `json:"family_id"`
		TokenIDs  string `json:"token_ids"`
		RevokedAt int64  `json:"revoked_at"`
	}
	body := entryBody{
		Seq: ev.Seq, AccountID: ev.AccountID, FamilyID: ev.FamilyID,
		TokenIDs: ev.TokenIDs, RevokedAt: ev.RevokedAt,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return SignedRevocationEntry{}, fmt.Errorf("revocation: marshal: %w", err)
	}
	wire := token.SignArtifact(token.RevocationDomainPrefix, bodyBytes, i.cfg.SigningKey)
	// wire = base64url(bodyBytes) "." base64url(sig) — extract just the sig part.
	return SignedRevocationEntry{
		Seq: ev.Seq, AccountID: ev.AccountID, FamilyID: ev.FamilyID,
		TokenIDs: ev.TokenIDs, RevokedAt: ev.RevokedAt,
		Sig: wire, // full wire so verifier can verify directly
	}, nil
}
