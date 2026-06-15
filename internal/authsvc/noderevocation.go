package authsvc

import (
	"encoding/json"
	"net/http"
)

func (s *Service) serveNodeRevocations(w http.ResponseWriter, r *http.Request) {
	rows, err := s.nodeRevocations.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "node revocations unavailable")
		return
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.NodeID)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		RevokedNodeIDs []string `json:"revoked_node_ids"`
		GeneratedAt    int64    `json:"generated_at"`
	}{
		RevokedNodeIDs: ids,
		GeneratedAt:    s.now().Unix(),
	})
}
