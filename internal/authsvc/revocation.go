package authsvc

// GET /revocations?since=<seq> — signed revocation feed for the CP (A2) to poll [AM10/(7)].
//
// Signing: each entry is a certified artifact envelope with type revocation-entry. A2 verifies
// its embedded leaf-first chain against the environment root before parsing the payload.
//
// Response: JSON array of SignedRevocationEntry. The CP verifies the sig field, then
// must advance its checkpoint past the highest seq it has processed to avoid re-delivering.
//
// Access control: if IdPConfig.CPSecret is non-empty (production), the CP MUST supply
// "Authorization: Bearer <CPSecret>" or the request is rejected 401. This is a
// server-to-server trust boundary; configure the secret via env/deploy config (see deploy/authsvc).

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"spawnery/internal/authsvc/store"
	"spawnery/internal/authsvc/token"
)

// SignedRevocationEntry is one entry in the /revocations feed that A2 consumes.
//
// SECURITY CONTRACT (A2 integrators must read this):
// Sig is the full certified revocation-entry envelope. The outer fields (Seq, AccountID,
// FamilyID, TokenIDs, RevokedAt) are convenience copies — they are NOT authenticated. A2 MUST
// call token.Verifier.Verify on Sig and read only the verified payload bytes (WM9 discipline).
// Never trust the outer fields before verification; they are vulnerable to tampering.
type SignedRevocationEntry struct {
	Seq       int64  `json:"seq"`
	AccountID string `json:"account_id"`
	FamilyID  string `json:"family_id"`
	TokenIDs  string `json:"token_ids"` // JSON array of access-token token_ids
	RevokedAt int64  `json:"revoked_at"`
	Sig       string `json:"sig"` // certified envelope; verify before trusting outer fields
}

// serveRevocations handles GET /revocations?since=<seq>.
// Returns all events with seq > since, each signed with the AS session key [AM10].
func (i *IdP) serveRevocations(w http.ResponseWriter, r *http.Request) {
	if i.cfg.CPSecret != "" {
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(i.cfg.CPSecret)) != 1 {
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

// signRevocationEntry signs a revocation event using the certified artifact discipline.
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
	wire, err := i.signers.sign(token.ArtifactTypeRevocation, bodyBytes)
	if err != nil {
		return SignedRevocationEntry{}, fmt.Errorf("revocation: sign: %w", err)
	}
	return SignedRevocationEntry{
		Seq: ev.Seq, AccountID: ev.AccountID, FamilyID: ev.FamilyID,
		TokenIDs: ev.TokenIDs, RevokedAt: ev.RevokedAt,
		Sig: wire, // full wire so verifier can verify directly
	}, nil
}
