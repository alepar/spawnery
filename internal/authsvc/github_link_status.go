package authsvc

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"

	"spawnery/internal/authsvc/store"
)

// linkStatusRequest is the JSON body for POST /internal/github/link-status.
type linkStatusRequest struct {
	AccountID string `json:"account_id"`
}

// linkStatusResponse is the JSON response for POST /internal/github/link-status.
// Status is one of "active", "relink_required", or "none".
type linkStatusResponse struct {
	Status string `json:"status"`
}

// serveGitHubLinkStatus handles POST /internal/github/link-status.
// It is a CP→AS server-to-server endpoint; not CORS/SPA-exposed.
//
// Auth: X-Spawnery-AS-Secret header must equal s.cpRPCSecret (constant-time compare).
// Request: {"account_id": "<accountID>"}
// Response: {"status": "active"|"relink_required"|"none"}
//
// The link is looked up by secretID = "gh:" + account_id. The handler never returns token
// material — only the link state.
func (s *Service) serveGitHubLinkStatus(w http.ResponseWriter, r *http.Request) {
	// Constant-time secret validation (always compare full lengths to resist timing).
	got := r.Header.Get("X-Spawnery-AS-Secret")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.cpRPCSecret)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req linkStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AccountID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if s.githubMintStore == nil {
		http.Error(w, "link store not configured", http.StatusInternalServerError)
		return
	}

	secretID := "gh:" + req.AccountID
	link, err := s.githubMintStore.GitHubLinks().Get(r.Context(), secretID)

	var status string
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = "none"
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	case link.RelinkRequired:
		status = "relink_required"
	default:
		status = "active"
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(linkStatusResponse{Status: status})
}
