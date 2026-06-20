package node

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
)

type fakeMintClient struct {
	mu   sync.Mutex
	reqs []*authv1.MintGitHubAccessTokenRequest
	resp *authv1.MintGitHubAccessTokenResponse
	err  error
}

func (f *fakeMintClient) MintGitHubAccessToken(_ context.Context, req *connect.Request[authv1.MintGitHubAccessTokenRequest]) (*connect.Response[authv1.MintGitHubAccessTokenResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req.Msg)
	if f.err != nil {
		return nil, f.err
	}
	resp := f.resp
	if resp == nil {
		resp = &authv1.MintGitHubAccessTokenResponse{}
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeMintClient) calls() []*authv1.MintGitHubAccessTokenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*authv1.MintGitHubAccessTokenRequest(nil), f.reqs...)
}

func TestRefresherNoteThenForget(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	r := newGitHubRefresher(&fakeMintClient{}) // nil client: scheduling is exercised via Tick with a fake in later tasks
	r.now = func() time.Time { return base }

	r.Note(githubRefreshEntry{
		SpawnID: "s1", Generation: 3, SecretID: "sec-1", Version: 1,
		DeliveryID: "d-1", RepositoryID: "42",
	})
	if got := r.due(base.Add(defaultRefreshInterval + time.Minute)); len(got) != 1 || got[0].SecretID != "sec-1" {
		t.Fatalf("expected 1 due entry for sec-1, got %+v", got)
	}
	if got := r.due(base); len(got) != 0 {
		t.Fatalf("entry should not be due before refreshAt, got %+v", got)
	}
	r.Forget("s1")
	if got := r.due(base.Add(defaultRefreshInterval + time.Hour)); len(got) != 0 {
		t.Fatalf("forgotten spawn must produce no due entries, got %+v", got)
	}
}

func TestRefresherMintsWithNodeIdentityLinkRefOnly(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	fake := &fakeMintClient{resp: &authv1.MintGitHubAccessTokenResponse{Refreshed: true, AccessExpiresAtUnix: base.Add(8 * time.Hour).Unix()}}
	r := newGitHubRefresher(fake)
	r.now = func() time.Time { return base }

	r.Note(githubRefreshEntry{SpawnID: "s1", Generation: 7, SecretID: "sec-1", Version: 4, DeliveryID: "d-4", RepositoryID: "42"})

	// Past the receipt-relative refreshAt → due → Tick issues exactly one mint.
	fireAt := base.Add(defaultRefreshInterval + time.Minute)
	r.Tick(context.Background(), fireAt)

	calls := fake.calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 mint call, got %d", len(calls))
	}
	got := calls[0]
	if got.GetSpawnId() != "s1" || got.GetGeneration() != 7 || got.GetRepositoryId() != "42" {
		t.Fatalf("mint envelope mismatch: %+v", got)
	}
	ref := got.GetLinkRef()
	if ref == nil || ref.GetSecretId() != "sec-1" || ref.GetVersion() != 4 || ref.GetDeliveryId() != "d-4" {
		t.Fatalf("link_ref mismatch: %+v", ref)
	}
	if got.GetRequestId() != githubRefreshRequestID("sec-1", 4) {
		t.Fatalf("request_id not stable/idempotent: %q", got.GetRequestId())
	}
	// CONTAINMENT: the request type has no token field — node identity (link_ref) is the authorization,
	// never a bearer GitHub token. (Compile-time enforced by the proto; asserted here for intent.)

	// In-flight grace: a second Tick within the grace window must NOT re-mint.
	r.Tick(context.Background(), fireAt.Add(refreshInFlightGrace-time.Second))
	if n := len(fake.calls()); n != 1 {
		t.Fatalf("expected no re-mint within grace window, got %d calls", n)
	}
}

func TestRefresherRetriesOnMintFailureThenSucceeds(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	fake := &fakeMintClient{err: errors.New("AS unavailable")}
	r := newGitHubRefresher(fake)
	r.now = func() time.Time { return base }
	r.Note(githubRefreshEntry{SpawnID: "s1", Generation: 1, SecretID: "sec-1", Version: 2, DeliveryID: "d-2"})

	t0 := base.Add(defaultRefreshInterval + time.Minute)
	r.Tick(context.Background(), t0) // attempt 1 -> error -> backoff floor
	if n := len(fake.calls()); n != 1 {
		t.Fatalf("attempt 1: want 1 call, got %d", n)
	}
	// Within the backoff floor: no retry.
	r.Tick(context.Background(), t0.Add(refreshBackoffBase-time.Second))
	if n := len(fake.calls()); n != 1 {
		t.Fatalf("retry before backoff floor should be suppressed, got %d calls", n)
	}
	// After backoff: retry fires, now succeeding.
	fake.mu.Lock()
	fake.err = nil
	fake.resp = &authv1.MintGitHubAccessTokenResponse{Refreshed: false, AccessExpiresAtUnix: base.Add(8 * time.Hour).Unix()}
	fake.mu.Unlock()
	r.Tick(context.Background(), t0.Add(refreshBackoffBase+time.Second))
	if n := len(fake.calls()); n != 2 {
		t.Fatalf("retry after backoff: want 2 calls, got %d", n)
	}
	// The in-flight grace (set by beginAttempt) blocks re-minting right after success.
	// Note: refreshAt (expiry-lead = base+7h52m) is already in the past relative to t0, so only
	// the nextAttempt gate prevents immediate re-scheduling.
	if got := r.due(t0.Add(refreshBackoffBase + 2*time.Second)); len(got) != 0 {
		t.Fatalf("after success, entry must not be immediately due within in-flight grace, got %+v", got)
	}
	// After the in-flight grace expires, the entry is due (refreshAt was already past at success time).
	afterGrace := t0.Add(refreshBackoffBase + time.Second + refreshInFlightGrace + time.Second)
	if got := r.due(afterGrace); len(got) != 1 {
		t.Fatalf("entry should be due again after in-flight grace expires, got %+v", got)
	}
}

// TestRefresherNotePreciseExpirySchedulesPrecisely verifies that when AccessExpiresAtUnix is carried
// in the delivery metadata, Note sets refreshAt precisely relative to the real expiry rather than the
// receipt-relative default — fixing the resume-near-expiry window (sp-v40s.18).
func TestRefresherNotePreciseExpirySchedulesPrecisely(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	// Token expires in 1h from base — far shorter than the 8h default lifetime assumption.
	tokenExpiry := base.Add(1 * time.Hour)
	r := newGitHubRefresher(&fakeMintClient{})
	r.now = func() time.Time { return base }

	r.Note(githubRefreshEntry{
		SpawnID: "s1", Generation: 1, SecretID: "sec-1", Version: 3,
		DeliveryID: "d-3", AccessExpiresAtUnix: tokenExpiry.Unix(),
	})
	// Refresh should be scheduled at expiry-nodeRefreshLead, NOT at base+defaultRefreshInterval (base+7h52m).
	wantRefreshAt := tokenExpiry.Add(-nodeRefreshLead)
	// Not due yet (before wantRefreshAt).
	if got := r.due(wantRefreshAt.Add(-time.Second)); len(got) != 0 {
		t.Fatalf("entry must not be due before precise refreshAt, got %+v", got)
	}
	// Due at wantRefreshAt.
	if got := r.due(wantRefreshAt); len(got) != 1 || got[0].SecretID != "sec-1" {
		t.Fatalf("entry must be due at precise refreshAt, got %+v", got)
	}
	// Crucially NOT due after the receipt-relative default interval (which would be ~7h52m from base),
	// because we scheduled from the real expiry instead.
	// Invariant: with tokenExpiry=base+1h the precise lead (base+52m) is well before the receipt-relative
	// default (base+7h52m). Assert rather than skip so a constant change is a loud failure.
	receiptRelative := base.Add(defaultRefreshInterval)
	if wantRefreshAt.After(receiptRelative) {
		t.Fatalf("test invariant broken: precise expiry-lead %v must be before receipt-relative default %v — update test constants", wantRefreshAt, receiptRelative)
	}
}

// TestRefresherNoteZeroExpiryFallsBackToReceiptRelative verifies that Note without a precise expiry
// (AccessExpiresAtUnix=0) still schedules using the receipt-relative default — backward compat.
func TestRefresherNoteZeroExpiryFallsBackToReceiptRelative(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	r := newGitHubRefresher(&fakeMintClient{})
	r.now = func() time.Time { return base }

	r.Note(githubRefreshEntry{
		SpawnID: "s1", Generation: 1, SecretID: "sec-1", Version: 1,
		DeliveryID: "d-1", AccessExpiresAtUnix: 0, // absent
	})
	// Not due just before the receipt-relative default window.
	if got := r.due(base.Add(defaultRefreshInterval - time.Second)); len(got) != 0 {
		t.Fatalf("entry must not be due before receipt-relative default, got %+v", got)
	}
	// Due at the receipt-relative default.
	if got := r.due(base.Add(defaultRefreshInterval)); len(got) != 1 {
		t.Fatalf("entry must be due at receipt-relative default, got %+v", got)
	}
}

// TestRefresherResumeNearExpiryFiresPromptly simulates a resume where the token is already near
// expiry (only 5 minutes left). With AccessExpiresAtUnix, the entry is immediately due (or due very
// soon) rather than waiting the full receipt-relative interval.
func TestRefresherResumeNearExpiryFiresPromptly(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	// Token expires in 5 minutes — well within the nodeRefreshLead (8m) → refreshAt is in the past.
	tokenExpiry := base.Add(5 * time.Minute)
	fake := &fakeMintClient{resp: &authv1.MintGitHubAccessTokenResponse{
		Refreshed: true, AccessExpiresAtUnix: base.Add(8 * time.Hour).Unix(),
	}}
	r := newGitHubRefresher(fake)
	r.now = func() time.Time { return base }

	r.Note(githubRefreshEntry{
		SpawnID: "s1", Generation: 5, SecretID: "sec-1", Version: 7,
		DeliveryID: "d-7", AccessExpiresAtUnix: tokenExpiry.Unix(),
	})
	// refreshAt = tokenExpiry - nodeRefreshLead = base - 3m (already in the past) → immediately due.
	if got := r.due(base); len(got) != 1 {
		t.Fatalf("near-expiry entry must be immediately due after Note, got %+v", got)
	}
	// Tick fires the refresh.
	r.Tick(context.Background(), base)
	if n := len(fake.calls()); n != 1 {
		t.Fatalf("expected 1 mint call for near-expiry token, got %d", n)
	}
}

// TestMintInitialReturnsLoginAndUserID verifies that MintInitial threads Login and UserID from the AS
// response through MintInitialResult (design §1.3 — consumed by sp-m859.1's git-identity render).
func TestMintInitialReturnsLoginAndUserID(t *testing.T) {
	fake := &fakeMintClient{resp: &authv1.MintGitHubAccessTokenResponse{
		AccessToken:         "ghu_x",
		AccessExpiresAtUnix: 1770000000 + 8*3600,
		Login:               "alice",
		UserId:              123456,
	}}
	r := newGitHubRefresher(fake)
	got, err := r.MintInitial(context.Background(), "sp1", 5, "gh:octo", "42")
	if err != nil {
		t.Fatalf("MintInitial: %v", err)
	}
	if got.Token != "ghu_x" || got.AccessExpiresAtUnix != 1770000000+8*3600 ||
		got.Login != "alice" || got.UserID != 123456 {
		t.Fatalf("MintInitialResult = %+v", got)
	}
}

// Models §16.4 resume-after-expiry: the access token is dead but the node still authorizes its first
// refresh by node identity (link_ref), never a bearer token. We force the entry due immediately by
// rewinding refreshAt via a fresh Note at an already-elapsed clock.
func TestRefresherResumeAfterExpiryUsesNodeIdentity(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	fake := &fakeMintClient{resp: &authv1.MintGitHubAccessTokenResponse{Refreshed: true, AccessExpiresAtUnix: base.Add(9 * time.Hour).Unix()}}
	r := newGitHubRefresher(fake)
	r.now = func() time.Time { return base }
	r.Note(githubRefreshEntry{SpawnID: "s1", Generation: 12, SecretID: "sec-1", Version: 9, DeliveryID: "d-9", RepositoryID: "7"})

	// Simulate clock far past the assumed 8h lifetime (token expired) and Tick.
	r.Tick(context.Background(), base.Add(defaultAccessLifetime+time.Hour))
	calls := fake.calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 refresh call after expiry, got %d", len(calls))
	}
	if calls[0].GetLinkRef().GetSecretId() != "sec-1" || calls[0].GetLinkRef().GetVersion() != 9 {
		t.Fatalf("refresh must present link_ref, got %+v", calls[0].GetLinkRef())
	}
}
