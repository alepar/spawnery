package authsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"spawnery/internal/authsvc/store"
	"spawnery/internal/pki"
)

const testLinkStatusSecret = "test-cp-rpc-secret"

// newLinkStatusService builds a minimal Service for /internal/github/link-status tests.
// It wires a real store (NewTestStore) and the cpRPCSecret; no GitHub provider is needed
// because the handler only reads link metadata, never touches token exchange.
func newLinkStatusService(t *testing.T, st store.Store) *Service {
	t.Helper()
	root, err := pki.NewRootCA("R")
	if err != nil {
		t.Fatalf("root CA: %v", err)
	}
	inter, err := root.NewIntermediate(pki.ClassSelfHosted)
	if err != nil {
		t.Fatalf("intermediate: %v", err)
	}
	return New(root.Cert, inter,
		WithGitHubMinting(st, nil), // nil provider — handler reads only, never mints
		WithCPRPCSecret(testLinkStatusSecret),
	)
}

// seedActiveLink inserts a non-revoked, non-relink-required link for accountID into st.
func seedActiveLink(t *testing.T, st store.Store, accountID string) {
	t.Helper()
	secretID := "gh:" + accountID
	now := time.Now().Unix()
	if err := st.GitHubLinks().Upsert(context.Background(), store.GitHubLink{
		SecretID:     secretID,
		AccountID:    accountID,
		Host:         "github.com",
		Login:        "user-" + accountID,
		GithubUserID: "1234",
		AppClientID:  "app-client",
		RefreshToken: "refresh-tok",
		AccessToken:  "access-tok",
		TokenType:    "bearer",
		Version:      1,
		DeliveryID:   "d-1",
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("seed active link: %v", err)
	}
}

func postLinkStatus(s *Service, secret, accountID string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(linkStatusRequest{AccountID: accountID})
	req := httptest.NewRequest(http.MethodPost, "/internal/github/link-status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-Spawnery-AS-Secret", secret)
	}
	rec := httptest.NewRecorder()
	s.serveGitHubLinkStatus(rec, req)
	return rec
}

func decodeLinkStatusResp(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp linkStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v (body=%q)", err, rec.Body.String())
	}
	return resp.Status
}

// TestGitHubLinkStatusActive: seeded active link → 200 {"status":"active"}.
func TestGitHubLinkStatusActive(t *testing.T) {
	st := store.NewTestStore(t)
	seedActiveLink(t, st, "alice")
	s := newLinkStatusService(t, st)

	rec := postLinkStatus(s, testLinkStatusSecret, "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeLinkStatusResp(t, rec); got != "active" {
		t.Fatalf("want status=active, got %q", got)
	}
}

// TestGitHubLinkStatusRelinkRequired: seeded link with relink_required=1 → 200 {"status":"relink_required"}.
func TestGitHubLinkStatusRelinkRequired(t *testing.T) {
	st := store.NewTestStore(t)
	seedActiveLink(t, st, "bob")
	if err := st.GitHubLinks().MarkRelinkRequired(context.Background(), "gh:bob", time.Now().Unix()); err != nil {
		t.Fatalf("mark relink: %v", err)
	}
	s := newLinkStatusService(t, st)

	rec := postLinkStatus(s, testLinkStatusSecret, "bob")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeLinkStatusResp(t, rec); got != "relink_required" {
		t.Fatalf("want status=relink_required, got %q", got)
	}
}

// TestGitHubLinkStatusNone: no link exists → 200 {"status":"none"}.
func TestGitHubLinkStatusNone(t *testing.T) {
	st := store.NewTestStore(t)
	s := newLinkStatusService(t, st)

	rec := postLinkStatus(s, testLinkStatusSecret, "charlie")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeLinkStatusResp(t, rec); got != "none" {
		t.Fatalf("want status=none, got %q", got)
	}
}

// TestGitHubLinkStatusBadSecret: wrong secret → 401.
func TestGitHubLinkStatusBadSecret(t *testing.T) {
	st := store.NewTestStore(t)
	s := newLinkStatusService(t, st)

	rec := postLinkStatus(s, "wrong-secret", "alice")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

// TestGitHubLinkStatusMissingSecret: no secret header → 401.
func TestGitHubLinkStatusMissingSecret(t *testing.T) {
	st := store.NewTestStore(t)
	s := newLinkStatusService(t, st)

	rec := postLinkStatus(s, "", "alice")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

// TestGitHubLinkStatusRouteRegistered: the /internal/github/link-status route is registered
// in the mux when cpRPCSecret is configured, and absent when it is not.
func TestGitHubLinkStatusRouteRegistered(t *testing.T) {
	st := store.NewTestStore(t)
	seedActiveLink(t, st, "alice")

	// With secret: route present.
	sWith := newLinkStatusService(t, st)
	body, _ := json.Marshal(linkStatusRequest{AccountID: "alice"})
	req := httptest.NewRequest(http.MethodPost, "/internal/github/link-status", bytes.NewReader(body))
	req.Header.Set("X-Spawnery-AS-Secret", testLinkStatusSecret)
	rec := httptest.NewRecorder()
	sWith.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with secret: want 200, got %d", rec.Code)
	}

	// Without secret: route absent (404).
	root, _ := pki.NewRootCA("R")
	inter, _ := root.NewIntermediate(pki.ClassSelfHosted)
	sWithout := New(root.Cert, inter, WithGitHubMinting(st, nil))
	rec2 := httptest.NewRecorder()
	sWithout.Handler().ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/internal/github/link-status", bytes.NewReader(body)))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("without secret: want 404, got %d", rec2.Code)
	}
}
