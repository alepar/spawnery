package node

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/safego"
)

// ErrGitHubNotLinked is returned by GetToken when the spawn has no GitHub link registered.
var ErrGitHubNotLinked = errors.New("spawn has no GitHub link (no github mount or link not yet delivered)")

// ErrGitHubRelinkRequired is returned by GetToken when the AS mint fails with
// FailedPrecondition or NotFound, indicating the link is broken or absent.
var ErrGitHubRelinkRequired = errors.New("GitHub link broken or revoked; re-link your GitHub account")

// ErrGitHubMintRateLimited is returned by GetToken when a fresh mint is needed but the
// per-spawn rate-limit floor (minMintInterval) has not elapsed since the last mint attempt.
var ErrGitHubMintRateLimited = errors.New("GitHub token mint rate-limited; try again shortly")

// minMintInterval is the minimum time between two mint attempts for the same spawn (GetToken path).
// Prevents the sidecar from hammering the AS when a near-expiry token is requested on every request.
const minMintInterval = 10 * time.Second

// getTokenMintCounter is a global monotonic counter used to generate fresh, unique request ids
// for GetToken mints (must never reuse a dedup id that could return an expired cached token).
var getTokenMintCounter atomic.Uint64

// GitHubMintClient is the subset of authv1connect.AuthServiceClient the refresher uses.
// authv1connect.NewAuthServiceClient(...) satisfies it; tests inject a fake.
type GitHubMintClient interface {
	MintGitHubAccessToken(context.Context, *connect.Request[authv1.MintGitHubAccessTokenRequest]) (*connect.Response[authv1.MintGitHubAccessTokenResponse], error)
}

// Default timing. The node estimates an ~8h access-token lifetime and triggers a refresh slightly
// INSIDE the AS rotate window (AS lead is 10m: it rotates only when expiry <= now+10m), so the AS
// actually rotates. When a delivery lacks a precise expiry (access_expires_at_unix = 0) it falls
// back to receipt-relative timing; a mint RESPONSE's access_expires_at_unix then refines refreshAt.
const (
	defaultAccessLifetime  = 8 * time.Hour
	nodeRefreshLead        = 8 * time.Minute // < AS 10m lead so the AS rotates on our call
	defaultRefreshInterval = defaultAccessLifetime - nodeRefreshLead
	refreshInFlightGrace   = 2 * time.Minute  // wait for the sealed fanout to arrive before re-minting
	refreshBackoffBase     = 30 * time.Second // exponential, capped
	refreshBackoffMax      = 5 * time.Minute
	refreshMintTimeout     = 30 * time.Second
	refreshTickInterval    = time.Minute
)

// githubRefreshEntry is the node-side record of a delivered GitHub link for one spawn. It carries the
// link reference (secret_id/version/delivery_id) the node presents to the AS — NOT any token.
type githubRefreshEntry struct {
	SpawnID             string
	Generation          uint64
	SecretID            string
	Version             uint64
	DeliveryID          string
	RepositoryID        string
	AccessExpiresAtUnix int64 // precise access-token expiry from delivery metadata (0 = unknown; use receipt-relative default).
}

type refreshState struct {
	entry       githubRefreshEntry
	refreshAt   time.Time     // earliest time to attempt a refresh
	nextAttempt time.Time     // backoff/grace floor for the next attempt
	backoffDur  time.Duration // last retry backoff step; 0 = first failure → use refreshBackoffBase
	inFlight    bool

	// GetToken token cache (per-spawn; only one github mount per spawn).
	token           string    // most recently minted access token (empty = not yet minted via GetToken)
	tokenExpiryUnix int64     // unix timestamp of the cached token's expiry (0 = unknown)
	lastMintAt      time.Time // time of the last mint attempt via GetToken (for rate-limiting)
}

type githubRefresher struct {
	client GitHubMintClient
	now    func() time.Time

	mu     sync.Mutex
	states map[string]map[string]*refreshState // spawnID -> secretID -> state
}

func newGitHubRefresher(client GitHubMintClient) *githubRefresher {
	return &githubRefresher{
		client: client,
		now:    time.Now,
		states: map[string]map[string]*refreshState{},
	}
}

// Note records (or refreshes) a delivered link for a spawn and schedules its next proactive
// refresh. When the entry carries a precise AccessExpiresAtUnix from the delivery metadata, that
// expiry drives the schedule (important on resume when the token may already be near expiry).
// Falls back to receipt-relative timing when the expiry is absent (0). Safe to call repeatedly
// (every delivery/fanout). nil-safe.
func (r *githubRefresher) Note(e githubRefreshEntry) {
	if r == nil || e.SpawnID == "" || e.SecretID == "" {
		return
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	bySecret := r.states[e.SpawnID]
	if bySecret == nil {
		bySecret = map[string]*refreshState{}
		r.states[e.SpawnID] = bySecret
	}
	st := bySecret[e.SecretID]
	if st == nil {
		st = &refreshState{}
		bySecret[e.SecretID] = st
	}
	st.entry = e
	if e.AccessExpiresAtUnix > 0 {
		// Precise schedule: refresh just inside the AS rotate window relative to the actual expiry.
		st.refreshAt = time.Unix(e.AccessExpiresAtUnix, 0).Add(-nodeRefreshLead)
	} else {
		// Fallback: receipt-relative default (8h lifetime assumption).
		st.refreshAt = now.Add(defaultRefreshInterval)
	}
	st.nextAttempt = time.Time{}
	st.inFlight = false
	st.backoffDur = 0
}

// Forget drops all link state for a spawn (stop/suspend). nil-safe.
func (r *githubRefresher) Forget(spawnID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.states, spawnID)
}

// due returns a snapshot of entries eligible for a mint attempt at `now` (refreshAt+nextAttempt
// elapsed, not in-flight). Pure read used by Tick and tests.
func (r *githubRefresher) due(now time.Time) []githubRefreshEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []githubRefreshEntry
	for _, bySecret := range r.states {
		for _, st := range bySecret {
			if st.inFlight {
				continue
			}
			if now.Before(st.refreshAt) {
				continue
			}
			if !st.nextAttempt.IsZero() && now.Before(st.nextAttempt) {
				continue
			}
			out = append(out, st.entry)
		}
	}
	return out
}

func githubRefreshRequestID(secretID string, version uint64) string {
	return fmt.Sprintf("node-refresh-%s-v%d", secretID, version)
}

// githubInitialMintRequestID is the idempotency key for the at-provision INITIAL mint of a mount's
// github link. It is stable across a retried StartSpawn for the same (spawn, generation, link) so the
// AS dedups, and distinct from the proactive-refresh request ids (githubRefreshRequestID).
func githubInitialMintRequestID(spawnID, secretID string, generation uint64) string {
	return fmt.Sprintf("node-initial-mint-%s-%s-g%d", spawnID, secretID, generation)
}

// MintInitialResult is the outcome of an at-provision INITIAL mint: the access token + its precise
// expiry, plus the linked GitHub identity (login + numeric user id) from the AS mint response
// (design §1.3), consumed by the §1.2 git-identity render (sp-m859.1).
type MintInitialResult struct {
	Token               string
	AccessExpiresAtUnix int64
	Login               string
	UserID              int64
}

// MintInitial performs the synchronous at-provision INITIAL mint for a github mount. It presents the
// node identity (carried by r.client) and an initial link-ref (secret_id only — the AS resolves the
// link's current version/delivery_id, T1). Unlike the proactive Tick path it does NOT mutate refresh
// scheduling state: the caller renders the returned token, then calls Note to begin proactive refresh.
// Returns MintInitialResult carrying the access token, its precise expiry (0 = unknown), and the
// linked GitHub identity (Login/UserID from the AS response). A FailedPrecondition/NotFound from the
// AS (broken/absent link) is surfaced as a clear "link your GitHub account first" error so the spawn
// fails with an actionable message (spec §'Create-time initial token delivery' step 5). nil-safe: a nil
// refresher or nil client means the dev lane lacks the mint channel — an explicit error, never a token.
func (r *githubRefresher) MintInitial(ctx context.Context, spawnID string, generation uint64, secretID, repositoryID string) (MintInitialResult, error) {
	if r == nil || r.client == nil {
		return MintInitialResult{}, fmt.Errorf("github mint client unavailable (node->AS mint channel not configured)")
	}
	cctx, cancel := context.WithTimeout(ctx, refreshMintTimeout)
	defer cancel()
	resp, err := r.client.MintGitHubAccessToken(cctx, connect.NewRequest(&authv1.MintGitHubAccessTokenRequest{
		RequestId:    githubInitialMintRequestID(spawnID, secretID, generation),
		SpawnId:      spawnID,
		Generation:   generation,
		RepositoryId: repositoryID, // audit/expected-target only; never a scope reducer (containment e).
		LinkRef: &authv1.GitHubLinkRef{
			SecretId: secretID, // INITIAL: version/delivery_id unset; AS resolves the current tuple (T1).
		},
	}))
	if err != nil {
		if code := connect.CodeOf(err); code == connect.CodeFailedPrecondition || code == connect.CodeNotFound {
			return MintInitialResult{}, fmt.Errorf("link your GitHub account first: %w", err)
		}
		return MintInitialResult{}, err
	}
	return MintInitialResult{
		Token:               resp.Msg.GetAccessToken(),
		AccessExpiresAtUnix: resp.Msg.GetAccessExpiresAtUnix(),
		Login:               resp.Msg.GetLogin(),
		UserID:              resp.Msg.GetUserId(),
	}, nil
}

// Tick attempts a mint for every due entry at `now`. Each attempt marks the entry in-flight and sets
// a grace floor so the node waits for the sealed fanout (which re-Notes the new version) instead of
// hammering the AS. On success the mint RESPONSE's access_expires_at_unix refines refreshAt; on
// failure an exponential backoff floor is set. nil-safe; a nil client makes Tick a no-op.
func (r *githubRefresher) Tick(ctx context.Context, now time.Time) {
	if r == nil || r.client == nil {
		return
	}
	for _, e := range r.due(now) {
		r.attempt(ctx, e, now)
	}
}

func (r *githubRefresher) attempt(ctx context.Context, e githubRefreshEntry, now time.Time) {
	// Reserve the slot (mark in-flight + grace floor) BEFORE the RPC so a concurrent Tick can't double-fire.
	if !r.beginAttempt(e, now) {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, refreshMintTimeout)
	defer cancel()
	resp, err := r.client.MintGitHubAccessToken(cctx, connect.NewRequest(&authv1.MintGitHubAccessTokenRequest{
		RequestId:    githubRefreshRequestID(e.SecretID, e.Version),
		SpawnId:      e.SpawnID,
		Generation:   e.Generation,
		RepositoryId: e.RepositoryID,
		LinkRef: &authv1.GitHubLinkRef{
			SecretId:   e.SecretID,
			Version:    e.Version,
			DeliveryId: e.DeliveryID,
		},
	}))
	if err != nil {
		r.failAttempt(e, now)
		return
	}
	r.succeedAttempt(e, now, resp.Msg.GetAccessExpiresAtUnix())
}

// beginAttempt marks the entry in-flight + sets the grace floor. Returns false if the entry vanished
// (spawn forgotten) or is already in-flight.
func (r *githubRefresher) beginAttempt(e githubRefreshEntry, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.lookupLocked(e.SpawnID, e.SecretID)
	if st == nil || st.inFlight {
		return false
	}
	st.inFlight = true
	st.nextAttempt = now.Add(refreshInFlightGrace)
	return true
}

func (r *githubRefresher) failAttempt(e githubRefreshEntry, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.lookupLocked(e.SpawnID, e.SecretID)
	if st == nil {
		return
	}
	st.inFlight = false
	d := nextRetryBackoff(st.backoffDur)
	st.backoffDur = d
	st.nextAttempt = now.Add(d)
}

func (r *githubRefresher) succeedAttempt(e githubRefreshEntry, now time.Time, accessExpiresAtUnix int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.lookupLocked(e.SpawnID, e.SecretID)
	if st == nil {
		return
	}
	st.inFlight = false
	st.backoffDur = 0
	// Keep the in-flight grace nextAttempt set by beginAttempt: the AS either rotated the token
	// (a sealed fanout is incoming, re-minting before Note arrives is redundant) or confirmed the
	// token is still valid (no fanout, but hammering again immediately wastes AS quota). The fanout
	// delivery (or the next proactive window) will call Note, resetting nextAttempt.
	if accessExpiresAtUnix > 0 {
		// Precise correction: schedule the next refresh just inside the AS rotate window.
		st.refreshAt = time.Unix(accessExpiresAtUnix, 0).Add(-nodeRefreshLead)
	}
	// else: keep the receipt-relative refreshAt set by Note (or the in-flight grace governs until the
	// sealed fanout re-Notes a new version).
}

func (r *githubRefresher) lookupLocked(spawnID, secretID string) *refreshState {
	bySecret := r.states[spawnID]
	if bySecret == nil {
		return nil
	}
	return bySecret[secretID]
}

// nextRetryBackoff doubles from refreshBackoffBase up to refreshBackoffMax using prev as the last
// backoff step (0 = first failure, returns base). Deterministic: the same prev always yields the
// same next step regardless of wall-clock timing.
func nextRetryBackoff(prev time.Duration) time.Duration {
	if prev < refreshBackoffBase {
		return refreshBackoffBase
	}
	next := prev * 2
	if next > refreshBackoffMax {
		return refreshBackoffMax
	}
	return next
}

// mintNow performs a single mint RPC for entry e using a FRESH, monotone request id so the AS
// never deduplicates it against a prior (possibly expired-token) response. Maps FailedPrecondition
// and NotFound codes to ErrGitHubRelinkRequired. Other mint errors are wrapped and returned.
// Callers must hold r.mu for the duration of token-cache updates, but NOT during the RPC itself.
func (r *githubRefresher) mintNow(ctx context.Context, e githubRefreshEntry) (token string, expiryUnix int64, err error) {
	if r.client == nil {
		return "", 0, fmt.Errorf("github mint client unavailable (node->AS mint channel not configured)")
	}
	// Fresh request id: combine the stable secretID/version with a global monotone counter so
	// every GetToken mint call has a unique id and the AS cannot return an expired cached response.
	seq := getTokenMintCounter.Add(1)
	requestID := fmt.Sprintf("node-gettoken-%s-v%d-s%d", e.SecretID, e.Version, seq)

	cctx, cancel := context.WithTimeout(ctx, refreshMintTimeout)
	defer cancel()
	resp, merr := r.client.MintGitHubAccessToken(cctx, connect.NewRequest(&authv1.MintGitHubAccessTokenRequest{
		RequestId:    requestID,
		SpawnId:      e.SpawnID,
		Generation:   e.Generation,
		RepositoryId: e.RepositoryID,
		LinkRef: &authv1.GitHubLinkRef{
			SecretId:   e.SecretID,
			Version:    e.Version,
			DeliveryId: e.DeliveryID,
		},
	}))
	if merr != nil {
		code := connect.CodeOf(merr)
		if code == connect.CodeFailedPrecondition || code == connect.CodeNotFound {
			return "", 0, ErrGitHubRelinkRequired
		}
		return "", 0, merr
	}
	return resp.Msg.GetAccessToken(), resp.Msg.GetAccessExpiresAtUnix(), nil
}

// GetToken returns a valid GitHub access token for the single github-mount linked to spawnID.
// It returns the cached token when it has at least minRemainingSeconds of life left; otherwise
// it mints a fresh one (subject to per-spawn rate-limiting). The returned expiryUnix is the
// token's expiry as a Unix timestamp (0 = unknown). Thread-safe. nil-safe (nil refresher or
// nil client → ErrGitHubNotLinked / wrapped error).
func (r *githubRefresher) GetToken(ctx context.Context, spawnID string, minRemainingSeconds int64) (string, int64, error) {
	if r == nil {
		return "", 0, ErrGitHubNotLinked
	}

	r.mu.Lock()
	bySecret := r.states[spawnID]
	if len(bySecret) == 0 {
		r.mu.Unlock()
		return "", 0, ErrGitHubNotLinked
	}
	// Pick the single states[spawnID] entry (one github mount per spawn, one linked account).
	var e githubRefreshEntry
	var st *refreshState
	for _, s := range bySecret {
		e = s.entry
		st = s
		break
	}

	now := r.now()

	// Cache-hit check: cached token has enough remaining life.
	if st.token != "" {
		remaining := st.tokenExpiryUnix - now.Unix()
		if st.tokenExpiryUnix > 0 && remaining >= minRemainingSeconds {
			tok, exp := st.token, st.tokenExpiryUnix
			r.mu.Unlock()
			return tok, exp, nil
		}
	}

	// Need a fresh mint. Apply rate-limit gate.
	if !st.lastMintAt.IsZero() && now.Sub(st.lastMintAt) < minMintInterval {
		// Rate-limited: return cached token if it has ANY remaining life, else error.
		if st.token != "" && (st.tokenExpiryUnix == 0 || now.Unix() < st.tokenExpiryUnix) {
			tok, exp := st.token, st.tokenExpiryUnix
			r.mu.Unlock()
			return tok, exp, ErrGitHubMintRateLimited
		}
		r.mu.Unlock()
		return "", 0, ErrGitHubMintRateLimited
	}

	// Record the mint attempt timestamp BEFORE releasing the lock (so concurrent callers see it).
	st.lastMintAt = now
	r.mu.Unlock()

	// Mint outside the lock (the RPC may block).
	tok, exp, err := r.mintNow(ctx, e)
	if err != nil {
		return "", 0, err
	}

	// Store the new token.
	r.mu.Lock()
	// Re-lookup: the spawn might have been forgotten while we were minting.
	if bySecret2 := r.states[spawnID]; bySecret2 != nil {
		if st2 := bySecret2[e.SecretID]; st2 != nil {
			st2.token = tok
			st2.tokenExpiryUnix = exp
		}
	}
	r.mu.Unlock()

	return tok, exp, nil
}

// run drives Tick on a fixed cadence until ctx is cancelled. Started once from node.Run; survives CP
// reconnects (the refresher is process-lived, like secretReplay). nil-safe; nil client → idle.
func (r *githubRefresher) run(ctx context.Context) {
	if r == nil || r.client == nil {
		return
	}
	t := time.NewTicker(refreshTickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			safego.Run("node.github-refresh-tick", func() { r.Tick(ctx, r.now()) })
		}
	}
}
