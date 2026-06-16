package authsvc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/store"
)

// failingRevokeExchanger wraps a GitHubLinkExchanger but always returns an error from RevokeAppGrant.
// Used to assert the local fail-closed invariant: DB revoke must still run even when GitHub is unreachable.
type failingRevokeExchanger struct {
	GitHubLinkExchanger
}

func (f failingRevokeExchanger) RevokeAppGrant(_ context.Context, _ string) error {
	return errors.New("github unreachable")
}

// postRevoke sends a POST /github/link/revoke to the Service with optional account header.
func postRevoke(t *testing.T, s *Service, secretID, account string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"secret_id": {secretID}}
	r := httptest.NewRequest(http.MethodPost, "/github/link/revoke", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if account != "" {
		r.Header.Set("X-Test-Account", account)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

// linkAndRedeem drives the full authorize→callback→redeem flow to persist a link to the DB.
func linkAndRedeem(t *testing.T, s *Service, ex GitHubLinkExchanger, secretID, account string, now time.Time) {
	t.Helper()
	c := linkOnce(t, s, ex, secretID, account, now)
	if w := redeem(s, account, c); w.Code != http.StatusOK {
		t.Fatalf("redeem = %d: %s", w.Code, w.Body.String())
	}
}

func TestGitHubLinkRevokeKillsGrantAndFailsClosed(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	linkAndRedeem(t, s, ex, "sec-1", "acct-1", now)

	w := postRevoke(t, s, "sec-1", "acct-1")
	if w.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	// DB flag flipped: the link is gone for the (revoked=0) reader → fail-closed.
	if _, err := st.GitHubLinks().Get(context.Background(), "sec-1"); err != store.ErrNotFound {
		t.Fatalf("Get after revoke = %v, want ErrNotFound", err)
	}
}

func TestGitHubLinkRevokeRejectsNonOwner(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })
	linkAndRedeem(t, s, ex, "sec-1", "acct-1", now)

	w := postRevoke(t, s, "sec-1", "intruder")
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-owner revoke = %d, want 403", w.Code)
	}
	// Link survives a rejected revoke.
	if _, err := st.GitHubLinks().Get(context.Background(), "sec-1"); err != nil {
		t.Fatalf("link must survive non-owner revoke: %v", err)
	}
}

func TestGitHubLinkRevokeNotFound(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })

	w := postRevoke(t, s, "missing", "acct-1")
	if w.Code != http.StatusNotFound {
		t.Fatalf("revoke of missing link = %d, want 404", w.Code)
	}
}

func TestGitHubLinkRevokeUnauthenticated(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	s := newLinkAS(t, st, ex, func() time.Time { return now })
	linkAndRedeem(t, s, ex, "sec-1", "acct-1", now)

	w := postRevoke(t, s, "sec-1", "") // no X-Test-Account header
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated revoke = %d, want 401", w.Code)
	}
}

// TestGitHubLinkRevokeGitHubFailureStillLocalRevoke verifies the spec §16.5 invariant:
// the local DB revoke (fail-closed) is authoritative and must succeed even when the GitHub
// grant-delete call fails. The handler must:
//   (a) still flip the DB revoked flag (link.Get returns ErrNotFound after the call), and
//   (b) return 502 to signal the remote teardown failed (operator may retry later).
//
// This test is the critical regression guard for "local fail-closed must not hinge on
// GitHub reachability" — live access tokens lapse by TTL ≤8h so the local revoke is enough.
func TestGitHubLinkRevokeGitHubFailureStillLocalRevoke(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	realEx, _ := newLinkExchanger(t)
	// Wrap the real exchanger so that RevokeAppGrant always errors.
	failEx := failingRevokeExchanger{realEx}
	s := newLinkAS(t, st, failEx, func() time.Time { return now })

	// Build a live link using the failing exchanger (authorize/exchange/fetchuser succeed; only
	// RevokeAppGrant is broken).
	linkAndRedeem(t, s, failEx, "sec-fail", "acct-fail", now)

	// Verify the link exists before we try to revoke it.
	if _, err := st.GitHubLinks().Get(context.Background(), "sec-fail"); err != nil {
		t.Fatalf("precondition: link not found before revoke: %v", err)
	}

	w := postRevoke(t, s, "sec-fail", "acct-fail")

	// (b) HTTP response must be 502 — remote teardown failed.
	if w.Code != http.StatusBadGateway {
		t.Fatalf("revoke with GitHub failure status = %d, want 502; body=%s", w.Code, w.Body.String())
	}

	// (a) DB revoke must have run: the link is locally dead regardless of GitHub reachability.
	if _, err := st.GitHubLinks().Get(context.Background(), "sec-fail"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("link must be locally revoked after GitHub failure; Get = %v, want ErrNotFound", err)
	}
}

func TestRevokedLinkFailsClosedOnMint(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	ex, _ := newLinkExchanger(t)
	linkSvc := newLinkAS(t, st, ex, func() time.Time { return now })
	linkAndRedeem(t, linkSvc, ex, "sec-1", "acct-1", now)

	// Read the live link to build a matching mint reference.
	link, err := st.GitHubLinks().Get(context.Background(), "sec-1")
	if err != nil {
		t.Fatalf("get link: %v", err)
	}

	// Revoke through the owner trigger.
	if w := postRevoke(t, linkSvc, "sec-1", "acct-1"); w.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d", w.Code)
	}

	// A mint service over the SAME store must now fail closed.
	provider := &testGitHubMintProvider{} // refresh never reached; link is gone pre-refresh
	mintSvc := newMintAS(t,
		WithGitHubMinting(st, provider),
		WithNodeIdentityExtractor(func(context.Context) (string, bool) { return "node-1", true }),
		WithGitHubMintAuthorizer(GitHubMintAuthorizerFunc(func(context.Context, GitHubMintAuthorization) error { return nil })),
	)
	_, err = mintSvc.MintGitHubAccessToken(context.Background(), connect.NewRequest(&authv1.MintGitHubAccessTokenRequest{
		RequestId: "r1",
		SpawnId:   "sp-1",
		LinkRef: &authv1.GitHubLinkRef{
			SecretId:   "sec-1",
			Version:    link.Version,
			DeliveryId: link.DeliveryID,
		},
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("mint after revoke code = %v, want NotFound; err=%v", connect.CodeOf(err), err)
	}
}
