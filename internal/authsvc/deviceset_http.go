package authsvc

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"spawnery/internal/authsvc/store"
	"spawnery/internal/secrets/seal"
)

// deviceSetHandler bundles the device-set registry HTTP handlers.
type deviceSetHandler struct {
	st             store.DeviceSetRepo
	spaOrigin      string
	accountFromReq AccountFromRequest
}

// corsBearerSimple wraps device-set endpoints with a simple-request CORS policy
// that allows Bearer-based auth from the SPA origin.  Unlike the credentialed
// IdP cookie endpoints, these use Authorization: Bearer (not cookies) so the
// CORS model is "simple + origin check" — no Allow-Credentials.
func (h *deviceSetHandler) corsBearerSimple(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if h.spaOrigin == "" || origin != h.spaOrigin {
				writeError(w, http.StatusForbidden, "origin_forbidden", "origin not allowed")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// serveAppend handles POST /devices/append.
//
// Request body (JSON):
//
//	{ "entry": "<base64-encoded json.Marshal(seal.StoredEntry)>" }
//
// Response 200 (JSON):
//
//	{ "version": <uint64>, "head": "<base64-encoded head hash>" }
//
// Response 409 (JSON) on CAS conflict:
//
//	{ "error": "conflict", "head": "<base64 current head hash>", "version": <uint64> }
func (h *deviceSetHandler) serveAppend(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.accountFromReq(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "valid Bearer token required")
		return
	}

	var req struct {
		Entry string `json:"entry"` // base64-encoded json.Marshal(seal.StoredEntry)
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "cannot decode request body")
		return
	}
	entryBytes, err := base64.StdEncoding.DecodeString(req.Entry)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_entry", "entry must be base64-encoded StoredEntry JSON")
		return
	}

	var entry seal.StoredEntry
	if err := json.Unmarshal(entryBytes, &entry); err != nil {
		writeError(w, http.StatusBadRequest, "bad_entry", "entry is not a valid StoredEntry")
		return
	}

	version, prevHash, err := entry.VersionAndPrevHash()
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_entry", "cannot read version/prevHash from entry")
		return
	}
	headHash, err := entry.Hash()
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_entry", "cannot compute entry hash (unsigned?)")
		return
	}

	now := time.Now().UnixNano()
	if err := h.st.Append(r.Context(), accountID, version, prevHash, headHash, entryBytes, now); err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Return current head so the client can rebase.
			curHead, curVer, _, _ := h.st.Head(r.Context(), accountID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":   "conflict",
				"head":    base64.StdEncoding.EncodeToString(curHead),
				"version": curVer,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "append failed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version": version,
		"head":    base64.StdEncoding.EncodeToString(headHash),
	})
}

// serveList handles GET /devices.
//
// Response 200 (JSON):
//
//	{ "entries": ["<base64>", ...], "head": "<base64 head hash>", "version": <uint64> }
//
// When the account has no entries, entries is [], head is "", version is 0.
func (h *deviceSetHandler) serveList(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.accountFromReq(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "valid Bearer token required")
		return
	}

	entries, err := h.st.FetchAll(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "fetch failed")
		return
	}

	encoded := make([]string, len(entries))
	for i, e := range entries {
		encoded[i] = base64.StdEncoding.EncodeToString(e)
	}

	headHash, version, _, _ := h.st.Head(r.Context(), accountID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"entries": encoded,
		"head":    base64.StdEncoding.EncodeToString(headHash),
		"version": version,
	})
}
