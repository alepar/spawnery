package authsvc

// GET /revocations?since=<seq>&limit=<n> serves bounded signed pages to authenticated CP/node
// consumers. Only seq and the certified artifact are exposed outside the signature; all binding
// data is deterministic protobuf inside the verified artifact payload.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/authsvc/token"
)

const maxRevocationPageEntries = 256

type SignedRevocationEntry struct {
	Seq int64  `json:"seq"`
	Sig string `json:"sig"`
}

type RevocationPage struct {
	Entries []SignedRevocationEntry `json:"entries"`
	HasMore bool                    `json:"has_more"`
}

func (i *IdP) serveRevocations(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	since, ok := parseRevocationPageInt(query["since"], 0, 0, int64(^uint64(0)>>1))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "since must be a non-negative integer")
		return
	}
	limit, ok := parseRevocationPageInt(query["limit"], maxRevocationPageEntries, 1, maxRevocationPageEntries)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "limit must be an integer from 1 through 256")
		return
	}

	events, hasMore, err := i.store.Revocations().PageAfter(r.Context(), since, int(limit), i.now().Unix())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "revocation page unavailable")
		return
	}
	entries := make([]SignedRevocationEntry, 0, len(events))
	for _, event := range events {
		entry, err := i.signRevocationEntry(event)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "server_error", "signing failed")
			return
		}
		entries = append(entries, entry)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(RevocationPage{Entries: entries, HasMore: hasMore})
}

func parseRevocationPageInt(values []string, defaultValue, minimum, maximum int64) (int64, bool) {
	if len(values) == 0 {
		return defaultValue, true
	}
	if len(values) != 1 || values[0] == "" {
		return 0, false
	}
	value, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || value < minimum || value > maximum {
		return 0, false
	}
	return value, true
}

func (i *IdP) signRevocationEntry(event store.RevocationEvent) (SignedRevocationEntry, error) {
	revokedTokens := make([]*authv1.RevokedToken, 0, len(event.RevokedTokens))
	for _, revoked := range event.RevokedTokens {
		revokedTokens = append(revokedTokens, &authv1.RevokedToken{
			TokenId: revoked.TokenID, RetainUntil: revoked.RetainUntil,
		})
	}
	body := &authv1.RevocationEntry{
		Seq: event.Seq, AccountId: event.AccountID, FamilyId: event.FamilyID,
		RevokedAt: event.RevokedAt, RevokedTokens: revokedTokens,
		RevokeTokensIssuedBefore: event.RevokeTokensIssuedBefore,
	}
	payload, err := (proto.MarshalOptions{Deterministic: true}).Marshal(body)
	if err != nil {
		return SignedRevocationEntry{}, fmt.Errorf("revocation: marshal: %w", err)
	}
	wire, err := i.signers.sign(token.ArtifactTypeRevocation, payload)
	if err != nil {
		return SignedRevocationEntry{}, fmt.Errorf("revocation: sign: %w", err)
	}
	return SignedRevocationEntry{Seq: event.Seq, Sig: wire}, nil
}
