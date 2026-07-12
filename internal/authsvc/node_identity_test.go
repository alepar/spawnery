package authsvc

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDevNodeIdentityHeaderIsInert(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("X-Spawnery-"+"Dev-Node-Id", "spoofed")
	if id, ok := nodeIDFromContext(req.Context()); ok || id != "" {
		t.Fatalf("production-capable request header created node identity: ok=%v id=%q", ok, id)
	}
}
