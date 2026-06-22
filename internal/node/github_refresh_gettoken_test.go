package node

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
)

// freshFakeMintClient returns a *fakeMintClient pre-seeded with a success response.
func freshFakeMintClient(token string, expiryUnix int64) *fakeMintClient {
	return &fakeMintClient{
		resp: &authv1.MintGitHubAccessTokenResponse{
			AccessToken:         token,
			AccessExpiresAtUnix: expiryUnix,
		},
	}
}

// seedEntry registers a github link + an existing cached token for spawnID on the refresher.
func seedEntry(r *githubRefresher, spawnID, secretID string, token string, expiryUnix int64, now time.Time) {
	r.Note(githubRefreshEntry{
		SpawnID: spawnID, Generation: 1, SecretID: secretID,
		Version: 1, DeliveryID: "d-1",
	})
	r.mu.Lock()
	st := r.states[spawnID][secretID]
	st.token = token
	st.tokenExpiryUnix = expiryUnix
	r.mu.Unlock()
}

// TestGetTokenCacheHit verifies that GetToken returns the cached token without calling the mint
// client when the cached expiry has enough life left.
func TestGetTokenCacheHit(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	fake := freshFakeMintClient("fresh-token", base.Add(8*time.Hour).Unix())
	r := newGitHubRefresher(fake)
	r.now = func() time.Time { return base }

	// Seed with a token that expires in 1h (300s buffer → cache hit).
	seedEntry(r, "s1", "sec-1", "cached-token", base.Add(1*time.Hour).Unix(), base)

	tok, exp, err := r.GetToken(context.Background(), "s1", 300, false)
	if err != nil {
		t.Fatalf("GetToken cache-hit: unexpected error: %v", err)
	}
	if tok != "cached-token" {
		t.Fatalf("want cached-token, got %q", tok)
	}
	if exp != base.Add(1*time.Hour).Unix() {
		t.Fatalf("expiry mismatch: got %d", exp)
	}
	// Mint client must NOT have been called.
	if n := len(fake.calls()); n != 0 {
		t.Fatalf("mint client called %d times, want 0", n)
	}
}

// TestGetTokenStaleMints verifies that GetToken calls the mint client once when the cached
// token has insufficient life, then caches and returns the new token.
func TestGetTokenStaleMints(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	// Fresh mint returns a token expiring in 8h.
	newExpiry := base.Add(8 * time.Hour).Unix()
	fake := freshFakeMintClient("new-token", newExpiry)
	r := newGitHubRefresher(fake)
	r.now = func() time.Time { return base }

	// Cached token expires in 100s; minRemainingSeconds=300 → stale.
	seedEntry(r, "s1", "sec-1", "old-token", base.Add(100*time.Second).Unix(), base)

	tok, exp, err := r.GetToken(context.Background(), "s1", 300, false)
	if err != nil {
		t.Fatalf("stale mint: unexpected error: %v", err)
	}
	if tok != "new-token" {
		t.Fatalf("want new-token, got %q", tok)
	}
	if exp != newExpiry {
		t.Fatalf("expiry mismatch: got %d, want %d", exp, newExpiry)
	}
	// Exactly one mint call.
	if n := len(fake.calls()); n != 1 {
		t.Fatalf("want 1 mint call, got %d", n)
	}
	// Cached token updated.
	r.mu.Lock()
	st := r.states["s1"]["sec-1"]
	if st.token != "new-token" || st.tokenExpiryUnix != newExpiry {
		t.Fatalf("cache not updated: token=%q expiry=%d", st.token, st.tokenExpiryUnix)
	}
	r.mu.Unlock()
}

// TestGetTokenNoLink verifies that GetToken returns ErrGitHubNotLinked for an unknown spawn and
// does not call the mint client.
func TestGetTokenNoLink(t *testing.T) {
	fake := &fakeMintClient{}
	r := newGitHubRefresher(fake)

	_, _, err := r.GetToken(context.Background(), "no-such-spawn", 300, false)
	if !errors.Is(err, ErrGitHubNotLinked) {
		t.Fatalf("want ErrGitHubNotLinked, got %v", err)
	}
	if n := len(fake.calls()); n != 0 {
		t.Fatalf("mint client called %d times, want 0", n)
	}
}

// TestGetTokenRelinkRequired verifies that when the mint client returns connect FailedPrecondition,
// GetToken surfaces ErrGitHubRelinkRequired.
func TestGetTokenRelinkRequired(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	fake := &fakeMintClient{
		err: connect.NewError(connect.CodeFailedPrecondition, errors.New("link not found")),
	}
	r := newGitHubRefresher(fake)
	r.now = func() time.Time { return base }

	r.Note(githubRefreshEntry{SpawnID: "s1", Generation: 1, SecretID: "sec-1", Version: 1, DeliveryID: "d-1"})

	_, _, err := r.GetToken(context.Background(), "s1", 300, false)
	if !errors.Is(err, ErrGitHubRelinkRequired) {
		t.Fatalf("want ErrGitHubRelinkRequired, got %v", err)
	}
}

// TestGetTokenRateLimited verifies that when two consecutive stale-mint calls are made within
// minMintInterval with no usable cached token, the second returns ErrGitHubMintRateLimited and the
// mint client is called exactly once.
func TestGetTokenRateLimited(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	clock := base
	fake := &fakeMintClient{
		// First mint returns a token that is already expired (expiry = base - 1s).
		resp: &authv1.MintGitHubAccessTokenResponse{
			AccessToken:         "expiring-token",
			AccessExpiresAtUnix: base.Add(-1 * time.Second).Unix(),
		},
	}
	r := newGitHubRefresher(fake)
	r.now = func() time.Time { return clock }

	r.Note(githubRefreshEntry{SpawnID: "s1", Generation: 1, SecretID: "sec-1", Version: 1, DeliveryID: "d-1"})

	// First call: stale → mints (lastMintAt = base).
	_, _, _ = r.GetToken(context.Background(), "s1", 300, false)
	if n := len(fake.calls()); n != 1 {
		t.Fatalf("first call: want 1 mint, got %d", n)
	}

	// Advance clock by only 1s (still within minMintInterval=10s).
	clock = base.Add(1 * time.Second)

	// Second call: stale again (no usable life) AND rate-limited.
	_, _, err := r.GetToken(context.Background(), "s1", 300, false)
	if !errors.Is(err, ErrGitHubMintRateLimited) {
		t.Fatalf("want ErrGitHubMintRateLimited, got %v", err)
	}
	// Mint client NOT called a second time.
	if n := len(fake.calls()); n != 1 {
		t.Fatalf("second call: want still 1 mint total, got %d", n)
	}
}

// TestGetTokenFreshRequestIDs verifies that two separate mint calls (e.g. two different stale
// windows separated by more than minMintInterval) use DIFFERENT request ids so the AS cannot
// return a deduplicated expired-token response.
func TestGetTokenFreshRequestIDs(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	clock := base
	// Both mints return tokens that expire 5s in the future (stale on next call with 300s buffer).
	fake := &fakeMintClient{
		resp: &authv1.MintGitHubAccessTokenResponse{
			AccessToken:         "tok",
			AccessExpiresAtUnix: base.Add(5 * time.Second).Unix(),
		},
	}
	r := newGitHubRefresher(fake)
	r.now = func() time.Time { return clock }

	r.Note(githubRefreshEntry{SpawnID: "s1", Generation: 1, SecretID: "sec-1", Version: 1, DeliveryID: "d-1"})

	// First call: stale → mint.
	_, _, _ = r.GetToken(context.Background(), "s1", 300, false)

	// Advance clock past the rate-limit window.
	clock = base.Add(minMintInterval + 1*time.Second)

	// Second call: stale again → second mint.
	_, _, _ = r.GetToken(context.Background(), "s1", 300, false)

	calls := fake.calls()
	if len(calls) != 2 {
		t.Fatalf("want 2 mint calls, got %d", len(calls))
	}
	if calls[0].GetRequestId() == calls[1].GetRequestId() {
		t.Fatalf("both mints used the same request id %q; they must be distinct", calls[0].GetRequestId())
	}
}

// TestGetTokenForceBypassesCacheAndFloor verifies that force=true clears the cached token and
// the mint-rate floor so GetToken mints a fresh token even when the cache is warm and the floor
// has not yet elapsed. This is the node-side precondition for the sidecar 401-retry backstop
// (Phase 2): the straggler stale-token is evicted before the retry mint.
func TestGetTokenForceBypassesCacheAndFloor(t *testing.T) {
	base := time.Unix(1_900_000_000, 0)
	newExpiry := base.Add(8 * time.Hour).Unix()
	fake := freshFakeMintClient("forced-fresh-token", newExpiry)
	r := newGitHubRefresher(fake)
	r.now = func() time.Time { return base }

	// Seed with a token that is well within the cache window (8h remaining) and
	// set lastMintAt = now (inside the 10s floor) to simulate a recent mint attempt.
	seedEntry(r, "s1", "sec-1", "stale-dead-token", base.Add(8*time.Hour).Unix(), base)
	r.mu.Lock()
	r.states["s1"]["sec-1"].lastMintAt = base // floor: 0s elapsed, minMintInterval=10s
	r.mu.Unlock()

	// Confirm that a normal (force=false) call returns the cache-hit token (not a fresh mint).
	tokNormal, _, errNormal := r.GetToken(context.Background(), "s1", 300, false)
	if errNormal != nil {
		t.Fatalf("normal GetToken: unexpected error: %v", errNormal)
	}
	if tokNormal != "stale-dead-token" {
		t.Fatalf("normal GetToken: want stale-dead-token, got %q", tokNormal)
	}
	if n := len(fake.calls()); n != 0 {
		t.Fatalf("normal GetToken: mint client called %d times, want 0", n)
	}

	// Now call with force=true — must mint fresh even though cache is warm and floor is active.
	tok, exp, err := r.GetToken(context.Background(), "s1", 300, true)
	if err != nil {
		t.Fatalf("force GetToken: unexpected error: %v", err)
	}
	if tok != "forced-fresh-token" {
		t.Fatalf("force GetToken: want forced-fresh-token, got %q", tok)
	}
	if exp != newExpiry {
		t.Fatalf("force GetToken: expiry mismatch: got %d, want %d", exp, newExpiry)
	}
	// Exactly one mint call (for the force path).
	if n := len(fake.calls()); n != 1 {
		t.Fatalf("force GetToken: want 1 mint call, got %d", n)
	}
}
