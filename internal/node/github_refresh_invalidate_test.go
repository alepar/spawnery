package node

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	nodev1 "spawnery/gen/node/v1"
)

// versionGatingMintClient is a fake GitHubMintClient that accepts only mints whose LinkRef
// carries the expected (version, delivery_id) pair, and returns PermissionDenied for any other
// version/delivery_id. Used to prove that Invalidate correctly advances the node's version-gate
// pointer: if it were to swap version and delivery_id the gate would reject the subsequent mint.
type versionGatingMintClient struct {
	mu            sync.Mutex
	reqs          []*authv1.MintGitHubAccessTokenRequest
	expectVer     uint64
	expectDelID   string
	successToken  string
	successExpiry int64
}

func (f *versionGatingMintClient) MintGitHubAccessToken(_ context.Context, req *connect.Request[authv1.MintGitHubAccessTokenRequest]) (*connect.Response[authv1.MintGitHubAccessTokenResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req.Msg)
	ref := req.Msg.GetLinkRef()
	if ref.GetVersion() != f.expectVer || ref.GetDeliveryId() != f.expectDelID {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf(
			"version-gate mismatch: presented v%d/%q, expected v%d/%q",
			ref.GetVersion(), ref.GetDeliveryId(), f.expectVer, f.expectDelID,
		))
	}
	return connect.NewResponse(&authv1.MintGitHubAccessTokenResponse{
		AccessToken:         f.successToken,
		AccessExpiresAtUnix: f.successExpiry,
	}), nil
}

func (f *versionGatingMintClient) calls() []*authv1.MintGitHubAccessTokenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*authv1.MintGitHubAccessTokenRequest(nil), f.reqs...)
}

// TestInvalidateClearsTokenAndNextGetTokenPresentsNewVersion is the regression guard for
// spec requirement §6 (sp-v40s.22.1): after a rotation signal the node must clear the stale
// cached token, advance both version and delivery_id to the new values, and present exactly
// those new values in its next mint LinkRef. An arg-order swap in Invalidate would produce a
// mint with wrong (version, delivery_id), which the version-gating fake rejects.
func TestInvalidateClearsTokenAndNextGetTokenPresentsNewVersion(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	bigExpiry := base.Add(8 * time.Hour).Unix()
	rotatedExpiry := base.Add(16 * time.Hour).Unix()

	// Version-gating fake: only accepts the NEW (v2, "d-2") tuple produced by Invalidate.
	// A mint presenting the old (v1, "d-1") gets PermissionDenied — catching arg-order swaps.
	gate := &versionGatingMintClient{
		expectVer:     2,
		expectDelID:   "d-2",
		successToken:  "rotated-token",
		successExpiry: rotatedExpiry,
	}
	r := newGitHubRefresher(gate)
	r.now = func() time.Time { return base }

	// Step 1: Seed a refresh entry at (v1, "d-1") with a live cached token.
	// seedEntry calls Note then directly writes the token — no mint RPC involved.
	seedEntry(r, "sp1", "sec-1", "initial-token", bigExpiry, base)

	// Sanity: GetToken must return the cached token without calling the mint client.
	tok, _, err := r.GetToken(context.Background(), "sp1", 300, false)
	if err != nil {
		t.Fatalf("GetToken (cache hit): unexpected error: %v", err)
	}
	if tok != "initial-token" {
		t.Fatalf("GetToken (cache hit): want initial-token, got %q", tok)
	}
	if n := len(gate.calls()); n != 0 {
		t.Fatalf("cache hit must not call mint client; got %d calls", n)
	}

	// Verify raw state before Invalidate (sanity guard).
	r.mu.Lock()
	st := r.states["sp1"]["sec-1"]
	if st.entry.Version != 1 || st.entry.DeliveryID != "d-1" || st.token != "initial-token" {
		t.Fatalf("pre-Invalidate state mismatch: version=%d deliveryID=%q token=%q",
			st.entry.Version, st.entry.DeliveryID, st.token)
	}
	r.mu.Unlock()

	// Step 2: Rotation signal arrives — advance the pointer to (v2, "d-2").
	r.Invalidate("sp1", "sec-1", 2, "d-2", base.Add(4*time.Hour).Unix())

	// Step 3a: Assert the state was correctly updated.
	r.mu.Lock()
	st = r.states["sp1"]["sec-1"]
	if st.entry.Version != 2 {
		t.Fatalf("Invalidate: Version not advanced: got %d, want 2", st.entry.Version)
	}
	// This is the load-bearing equality: delivery_id must round-trip from Invalidate to the entry.
	if st.entry.DeliveryID != "d-2" {
		t.Fatalf("Invalidate: DeliveryID not advanced: got %q, want d-2 (delivery_id round-trip failure)", st.entry.DeliveryID)
	}
	if st.token != "" {
		t.Fatalf("Invalidate: stale cached token not cleared: got %q", st.token)
	}
	if st.tokenExpiryUnix != 0 {
		t.Fatalf("Invalidate: token expiry not cleared: got %d", st.tokenExpiryUnix)
	}
	if !st.lastMintAt.IsZero() {
		t.Fatalf("Invalidate: rate-limit floor not cleared: lastMintAt=%v", st.lastMintAt)
	}
	r.mu.Unlock()

	// Step 3b: GetToken must re-mint and present the NEW (v2, "d-2") in the LinkRef.
	// The version-gating fake returns PermissionDenied for any other tuple, so an arg-order
	// swap in Invalidate would surface here as ErrGitHubRelinkRequired rather than a token.
	tok, exp, err := r.GetToken(context.Background(), "sp1", 300, false)
	if err != nil {
		t.Fatalf("GetToken after Invalidate: %v — version-gate may have rejected a wrong (version, delivery_id) tuple", err)
	}
	if tok != "rotated-token" {
		t.Fatalf("GetToken after Invalidate: want rotated-token, got %q", tok)
	}
	if exp != rotatedExpiry {
		t.Fatalf("GetToken after Invalidate: expiry mismatch: got %d, want %d", exp, rotatedExpiry)
	}

	// Exactly one mint call (the cache was empty; no re-mint).
	calls := gate.calls()
	if len(calls) != 1 {
		t.Fatalf("want exactly 1 mint call after Invalidate, got %d", len(calls))
	}
	ref := calls[0].GetLinkRef()
	// These two assertions are the regression guard: the LinkRef fields must match what
	// Invalidate wrote to the entry (delivery_id especially must not be truncated/swapped).
	if ref.GetVersion() != 2 {
		t.Fatalf("mint LinkRef.Version = %d, want 2", ref.GetVersion())
	}
	if ref.GetDeliveryId() != "d-2" {
		t.Fatalf("mint LinkRef.DeliveryId = %q, want d-2 (delivery_id round-trip failure)", ref.GetDeliveryId())
	}
}

// TestHandleGithubTokenRotatedCallsInvalidate verifies the dispatch path in attach.go: a
// CPMessage_GithubTokenRotated message with gen=0 (no staleGen fence) correctly calls
// Invalidate on the githubRefresher with the exact args from the signal proto.
func TestHandleGithubTokenRotatedCallsInvalidate(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	bigExpiry := base.Add(8 * time.Hour).Unix()

	fake := freshFakeMintClient("rotated-token", base.Add(16*time.Hour).Unix())
	r := newGitHubRefresher(fake)
	r.now = func() time.Time { return base }
	seedEntry(r, "sp1", "sec-1", "initial-token", bigExpiry, base)

	// Build an attacher with the refresher wired in, using the existing test helper.
	mgr := newGooseManager(t, &scriptedPodBackend{})
	a := newAttacher(mgr, &fakeCPStream{})
	a.githubRefresh = r

	// Dispatch the signal. gen=0 bypasses the staleGen fence unconditionally.
	a.handle(context.Background(), &nodev1.CPMessage{
		Msg: &nodev1.CPMessage_GithubTokenRotated{
			GithubTokenRotated: &nodev1.GitHubTokenRotatedSignal{
				SpawnId:             "sp1",
				Generation:          0, // gen=0: never stale-fenced (staleGen returns false immediately)
				SecretId:            "sec-1",
				Version:             7,
				DeliveryId:          "d-7",
				AccessExpiresAtUnix: base.Add(4 * time.Hour).Unix(),
			},
		},
	})

	// Assert that Invalidate was called: token cleared and pointer advanced.
	r.mu.Lock()
	st := r.states["sp1"]["sec-1"]
	if st.entry.Version != 7 {
		t.Fatalf("dispatch: entry.Version = %d, want 7", st.entry.Version)
	}
	if st.entry.DeliveryID != "d-7" {
		t.Fatalf("dispatch: entry.DeliveryID = %q, want d-7", st.entry.DeliveryID)
	}
	if st.token != "" {
		t.Fatalf("dispatch: token not cleared after Invalidate; got %q", st.token)
	}
	r.mu.Unlock()
}

// TestHandleGithubTokenRotatedStaleGenDropped verifies that the staleGen fence in handle()
// drops a CPMessage_GithubTokenRotated whose generation is older than the live spawn's
// generation, leaving the refresher state untouched.
func TestHandleGithubTokenRotatedStaleGenDropped(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	bigExpiry := base.Add(8 * time.Hour).Unix()
	ctx := context.Background()

	fake := freshFakeMintClient("rotated-token", base.Add(16*time.Hour).Unix())
	r := newGitHubRefresher(fake)
	r.now = func() time.Time { return base }
	seedEntry(r, "sp1", "sec-1", "initial-token", bigExpiry, base)

	// Register the spawn at generation=5 so the manager's SpawnGeneration("sp1") returns 5.
	// Use mgr.Create (no agent started) — sufficient for the staleGen fence lookup.
	mgr := newGooseManager(t, &scriptedPodBackend{})
	if _, err := mgr.Create(ctx, "sp1", writeNodeApp(t), "m", "sp1", "app", 5); err != nil {
		t.Fatalf("Create spawn: %v", err)
	}

	a := newAttacher(mgr, &fakeCPStream{})
	a.githubRefresh = r

	// Stale signal: gen=4 < live=5 → must be dropped; Invalidate must NOT be called.
	a.handle(ctx, &nodev1.CPMessage{
		Msg: &nodev1.CPMessage_GithubTokenRotated{
			GithubTokenRotated: &nodev1.GitHubTokenRotatedSignal{
				SpawnId:    "sp1",
				Generation: 4, // stale: gen 4 < live 5
				SecretId:   "sec-1",
				Version:    7,
				DeliveryId: "d-7",
			},
		},
	})

	// State must be unchanged: token still present, version still 1.
	r.mu.Lock()
	st := r.states["sp1"]["sec-1"]
	if st.token != "initial-token" {
		t.Fatalf("stale gen: token should be unchanged; got %q", st.token)
	}
	if st.entry.Version != 1 {
		t.Fatalf("stale gen: Version should be unchanged; got %d", st.entry.Version)
	}
	r.mu.Unlock()

	// Matching signal: gen=5 == live=5 → NOT stale; Invalidate MUST be called.
	a.handle(ctx, &nodev1.CPMessage{
		Msg: &nodev1.CPMessage_GithubTokenRotated{
			GithubTokenRotated: &nodev1.GitHubTokenRotatedSignal{
				SpawnId:    "sp1",
				Generation: 5, // current generation: passes the fence
				SecretId:   "sec-1",
				Version:    7,
				DeliveryId: "d-7",
			},
		},
	})

	r.mu.Lock()
	st = r.states["sp1"]["sec-1"]
	if st.entry.Version != 7 {
		t.Fatalf("matching gen: Version not advanced; got %d, want 7", st.entry.Version)
	}
	if st.token != "" {
		t.Fatalf("matching gen: token not cleared; got %q", st.token)
	}
	r.mu.Unlock()
}
