package cp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	authv1 "spawnery/gen/auth/v1"
	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/store"
	"spawnery/internal/cp/telemetry"
	"spawnery/internal/intent"
)

// defaultSuspendTimeout is a generous absolute backstop for suspendLocked — it fires only if
// the per-transition stall window (defaultSuspendStallWindow) fails to catch a wedge first.
// Most suspend failures are caught earlier by the stall window; the backstop defends against a
// completely frozen CP driver that never resets the stall timer.
const defaultSuspendTimeout = 10 * time.Minute

// defaultSuspendStallWindow is the operative timeout for suspend: if no progress event arrives
// within this window, the suspend is considered stalled (wedged) and is moved to 'error'. Unlike
// the old 30s total deadline, the stall window resets on each phase-level progress event from the
// node, so a slow-but-progressing snapshot does not spuriously fail (sp-u53.7.2).
const defaultSuspendStallWindow = 30 * time.Second

// defaultResumeTimeout is the generous absolute backstop for resumeLocked (the node's startSpawn).
const defaultResumeTimeout = 10 * time.Minute

// defaultResumeStallWindow is the stall window for resume: if no progress event from the node
// arrives within this window, the resume is considered stalled and moved to 'error' (or reverted
// to 'suspended' on migration). Symmetric to the suspend stall window (sp-u53.7.2).
const defaultResumeStallWindow = 30 * time.Second

// SuspendProgressHint is the CP-side progress signal for the stall detector (sp-u53.7.2). It
// carries the same fields as the proto SuspendProgress message but as a Go type, decoupling the
// stall detector from the wire protocol. When proto/node/v1/node.proto gains a SuspendProgress
// message, the server's Receive loop will convert it via a thin adapter and call progress().
// Fields Phase and Detail are informational (logged only). Markers carry partial per-mount
// markers received so far — they are accumulated in the waiter and persisted on stall so a
// wedge does not strand partial results.
type SuspendProgressHint struct {
	SpawnID    string
	Generation uint64
	Phase      string
	Detail     string
	Markers    map[string]string
}

// suspendWaiters correlates a SuspendSpawn with the async SuspendComplete the hosting node sends
// back on its Attach stream, carrying the per-mount persist markers. Keyed by spawn_id: the DB
// claim serialises suspends, so at most one waiter per spawn is ever live. The waiter records the
// episode generation it expects; deliver drops a SuspendComplete whose generation differs (a
// stale-episode reply from a superseded pod), mirroring the node-side generation fence.
type suspendWaiters struct {
	mu sync.Mutex
	m  map[string]*suspendWaiter
}

type suspendWaiter struct {
	gen        uint64
	ch         chan *nodev1.SuspendComplete
	progressCh chan struct{} // buffered cap 1; non-blocking coalescing progress signal for the stall loop
	markersMu  sync.Mutex
	// partialMarkers accumulates per-mount markers from SuspendProgress events. On stall these are
	// persisted to the store so a wedge does not strand markers that already arrived.
	partialMarkers map[string]string
	// lastPhase and lastDetail track the most recent progress event for UI display (transition_phase
	// and transition_detail in SpawnSummary — surfaced by ListSpawns when status=Suspending).
	lastPhase  string
	lastDetail string
}

func newSuspendWaiters() *suspendWaiters {
	return &suspendWaiters{m: map[string]*suspendWaiter{}}
}

// register installs a buffered (cap 1) waiter for (spawnID, gen) and returns the waiter. Call
// BEFORE sending Suspend so a fast SuspendComplete is never missed.
func (w *suspendWaiters) register(spawnID string, gen uint64) *suspendWaiter {
	wt := &suspendWaiter{
		gen:            gen,
		ch:             make(chan *nodev1.SuspendComplete, 1),
		progressCh:     make(chan struct{}, 1),
		partialMarkers: map[string]string{},
	}
	w.mu.Lock()
	w.m[spawnID] = wt
	w.mu.Unlock()
	return wt
}

func (w *suspendWaiters) unregister(spawnID string) {
	w.mu.Lock()
	delete(w.m, spawnID)
	w.mu.Unlock()
}

// deliver routes an inbound SuspendComplete to its waiter (if any), matched by spawn_id AND
// generation. Returns true when delivered to a live, gen-matched waiter; false when there is no
// live waiter or the generation is stale (caller may then reconcile the late reply). Non-blocking:
// a reply with no live waiter, or one whose generation != the awaiting episode's (a stale-episode
// reply from a superseded pod), is NOT sent to the channel.
func (w *suspendWaiters) deliver(sc *nodev1.SuspendComplete) bool {
	w.mu.Lock()
	wt, ok := w.m[sc.GetSpawnId()]
	w.mu.Unlock()
	if !ok || wt.gen != sc.GetGeneration() {
		return false // no live waiter or stale-generation reply
	}
	select {
	case wt.ch <- sc:
		return true
	default:
		return false // channel full (duplicate delivery); treat as no live waiter
	}
}

// progress routes a SuspendProgress hint to its waiter (if any), matched by spawn_id AND
// generation. Stale-generation progress is dropped (returns false, no stall-timer reset). On match
// it accumulates partial markers, tracks the last phase/detail for UI display, and signals
// progressCh (non-blocking coalescing send) so the stall loop resets its timer.
// Safe to call from any goroutine.
func (w *suspendWaiters) progress(h SuspendProgressHint) bool {
	w.mu.Lock()
	wt, ok := w.m[h.SpawnID]
	w.mu.Unlock()
	if !ok || wt.gen != h.Generation {
		return false // no live waiter or stale-generation progress: drop (no stall-timer reset)
	}
	// Accumulate partial markers and track last phase/detail under the waiter's own lock.
	wt.markersMu.Lock()
	for k, v := range h.Markers {
		wt.partialMarkers[k] = v
	}
	if h.Phase != "" {
		wt.lastPhase = h.Phase
		wt.lastDetail = h.Detail
	}
	wt.markersMu.Unlock()
	// Non-blocking coalescing send: if progressCh is already full, this progress event is already
	// queued and the stall timer will be reset for this window — no need to block or drop-and-retry.
	select {
	case wt.progressCh <- struct{}{}:
	default:
	}
	return true
}

// lastProgress returns the most recent phase/detail for a live suspend waiter (used by ListSpawns
// to populate SpawnSummary.TransitionPhase/TransitionDetail). Returns empty strings if no waiter.
func (w *suspendWaiters) lastProgress(spawnID string) (phase, detail string) {
	w.mu.Lock()
	wt, ok := w.m[spawnID]
	w.mu.Unlock()
	if !ok {
		return "", ""
	}
	wt.markersMu.Lock()
	phase, detail = wt.lastPhase, wt.lastDetail
	wt.markersMu.Unlock()
	return phase, detail
}

// ResumeProgressHint is the CP-side progress signal for the resume stall detector (sp-u53.7.2).
// Symmetric to SuspendProgressHint; carries the same wire fields but as a Go type. When
// proto/node/v1/node.proto's ResumeProgress message arrives, the server's Receive loop converts
// it and calls resumeWaiters.progress() so the stall timer can reset.
type ResumeProgressHint struct {
	SpawnID    string
	Generation uint64
	Phase      string
	Detail     string
}

// resumeWaiters correlates a ResumeSpawn with the async node progress and ACTIVE status the
// hosting node sends back on its Attach stream. Keyed by spawn_id. The waiter tracks the
// episode generation it expects; stale-generation signals are dropped.
// Symmetric to suspendWaiters but without markers accumulation (no per-mount state for resume).
type resumeWaiters struct {
	mu sync.Mutex
	m  map[string]*resumeWaiter
}

type resumeWaiter struct {
	gen        uint64
	progressCh chan struct{} // buffered cap 1; non-blocking coalescing progress signal for the stall loop
	lastMu     sync.Mutex
	lastPhase  string // most recent phase emitted by the node; for UI display (SpawnSummary.TransitionPhase)
	lastDetail string
}

func newResumeWaiters() *resumeWaiters {
	return &resumeWaiters{m: map[string]*resumeWaiter{}}
}

func (w *resumeWaiters) register(spawnID string, gen uint64) *resumeWaiter {
	wt := &resumeWaiter{
		gen:        gen,
		progressCh: make(chan struct{}, 1),
	}
	w.mu.Lock()
	w.m[spawnID] = wt
	w.mu.Unlock()
	return wt
}

func (w *resumeWaiters) unregister(spawnID string) {
	w.mu.Lock()
	delete(w.m, spawnID)
	w.mu.Unlock()
}

// progress routes a ResumeProgressHint to its waiter (if any), matched by spawn_id AND generation.
// Stale-generation signals are dropped (no stall-timer reset). On match it updates lastPhase/lastDetail
// for UI display and signals progressCh so the stall loop resets its timer.
func (w *resumeWaiters) progress(h ResumeProgressHint) bool {
	w.mu.Lock()
	wt, ok := w.m[h.SpawnID]
	w.mu.Unlock()
	if !ok || wt.gen != h.Generation {
		return false
	}
	if h.Phase != "" {
		wt.lastMu.Lock()
		wt.lastPhase = h.Phase
		wt.lastDetail = h.Detail
		wt.lastMu.Unlock()
	}
	select {
	case wt.progressCh <- struct{}{}:
	default:
	}
	return true
}

// lastProgress returns the most recent phase/detail for a live resume waiter (used by ListSpawns
// to populate SpawnSummary.TransitionPhase/TransitionDetail). Returns empty strings if no waiter.
func (w *resumeWaiters) lastProgress(spawnID string) (phase, detail string) {
	w.mu.Lock()
	wt, ok := w.m[spawnID]
	w.mu.Unlock()
	if !ok {
		return "", ""
	}
	wt.lastMu.Lock()
	phase, detail = wt.lastPhase, wt.lastDetail
	wt.lastMu.Unlock()
	return phase, detail
}

// maxSpawnNameRunes caps a spawn display name (rune count). Shared by RenameSpawn (and any future
// name validation).
const maxSpawnNameRunes = 80

// toSummaryStatus maps the store's durable status to the cp.v1 wire enum.
func toSummaryStatus(s store.Status) cpv1.SpawnStatus {
	switch s {
	case store.Starting:
		return cpv1.SpawnStatus_SPAWN_STATUS_STARTING
	case store.Active:
		return cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE
	case store.Suspending:
		return cpv1.SpawnStatus_SPAWN_STATUS_SUSPENDING
	case store.Suspended:
		return cpv1.SpawnStatus_SPAWN_STATUS_SUSPENDED
	case store.Resuming:
		return cpv1.SpawnStatus_SPAWN_STATUS_RESUMING
	case store.Unreachable:
		return cpv1.SpawnStatus_SPAWN_STATUS_UNREACHABLE
	case store.Errored:
		return cpv1.SpawnStatus_SPAWN_STATUS_ERROR
	case store.Deleted:
		return cpv1.SpawnStatus_SPAWN_STATUS_DELETED
	default:
		return cpv1.SpawnStatus_SPAWN_STATUS_UNSPECIFIED
	}
}

// DeleteSpawn tears down any running container and soft-deletes the spawn. It reuses the same
// teardown path as StopSpawn (today they're identical; in Part 3b StopSpawn becomes suspend while
// DeleteSpawn stays a destroy). destroy_data is accepted but INERT for scratch backends — there is
// no persistent data to destroy; real backend-destroy lands with E3.
func (s *Server) DeleteSpawn(ctx context.Context, req *connect.Request[cpv1.DeleteSpawnRequest]) (*connect.Response[cpv1.DeleteSpawnResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	if err := s.stop(ctx, owner, req.Msg.SpawnId); err != nil {
		return nil, err
	}
	_ = req.Msg.DestroyData // inert until E3 persistent backends; see doc comment.
	return connect.NewResponse(&cpv1.DeleteSpawnResponse{}), nil
}

// RenameSpawn sets a spawn's display name (owner-guarded). Duplicate names are allowed — the
// spawn id is the real key; the name is a display label.
func (s *Server) RenameSpawn(ctx context.Context, req *connect.Request[cpv1.RenameSpawnRequest]) (*connect.Response[cpv1.RenameSpawnResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	name := strings.TrimSpace(req.Msg.Name)
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name must not be empty"))
	}
	if len([]rune(name)) > maxSpawnNameRunes {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name too long (max %d)", maxSpawnNameRunes))
	}
	unlock := s.locks.Lock(req.Msg.SpawnId)
	defer unlock()
	sp, err := s.st.Spawns().Get(ctx, req.Msg.SpawnId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if sp.OwnerID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}
	if err := s.st.Spawns().Rename(ctx, req.Msg.SpawnId, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&cpv1.RenameSpawnResponse{}), nil
}

// SuspendSpawn persists the spawn's mounts on the hosting node and tears the container down, keeping
// the spawn row 'suspended' (resumable) with the per-mount persist markers recorded.
//
// Claim-fenced flow (sp-u53.7.5): Acquire DB claim → Active→Suspending (CAS, lease+gen fenced, BEFORE
// the round-trip) → ask node to persist+tear down → await SuspendComplete:
//   - success: record markers + drop route + Suspending→Suspended.
//   - gate-abort (SuspendComplete.error non-empty): revert Suspending→Active, return FailedPrecondition
//     (spawn still running, spec §6).
//   - timeout: SetError + drop route (design §5: "persist failure → error").
//   - claim-lost (heartbeat expired mid-round-trip): bail, commit no further transition.
func (s *Server) SuspendSpawn(ctx context.Context, req *connect.Request[cpv1.SuspendSpawnRequest]) (*connect.Response[cpv1.SuspendSpawnResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	id := req.Msg.SpawnId
	// Quick owner check before acquiring the claim (avoids unnecessary DB claim for foreign spawns).
	if sp, err := s.st.Spawns().Get(ctx, id); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	} else if sp.OwnerID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}
	if err := s.withClaim(ctx, id, func(cctx context.Context, leaseID string) error {
		_, serr := s.suspendLocked(cctx, owner, id, false, leaseID)
		return serr
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(&cpv1.SuspendSpawnResponse{}), nil
}

// suspendLocked is the claim-fenced suspend core. The CALLER must hold the DB claim for id (via
// withClaim). It owns the Active→Suspending→Suspended/gate-abort sequence and returns a connect error
// on any failure, leaving the spawn in a DEFINED state:
//   - success: 'suspended' (markers recorded, route dropped)
//   - gate-abort: spawn left ACTIVE (revert Suspending→Active), FailedPrecondition (spec §6)
//   - timeout/await-failure: 'error' (sp-a7fs contract)
//   - claim-lost: bail immediately, no further transition committed
//
// Active→Suspending is written BEFORE the node round-trip so sweepers read Suspending and skip.
func (s *Server) suspendLocked(ctx context.Context, owner, id string, captureRootfsArtifact bool, leaseID string) (*nodev1.SuspendComplete, error) {
	sp, err := s.st.Spawns().Get(ctx, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if sp.OwnerID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}
	if sp.Status != store.Active {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("spawn is not active"))
	}
	c, hasLive, err := s.st.Spawns().LiveContainer(ctx, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !hasLive {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("no live container"))
	}
	gen := c.Generation

	// Write Active→Suspending BEFORE the round-trip (lease+seq+gen fenced via TransitionClaimed).
	// Sweepers (reconcileInventory) read the transient status and skip — no in-memory exemption set.
	seq := sp.StatusSeq
	if _, err := s.st.Spawns().TransitionClaimed(ctx, id, leaseID, seq, gen, store.Suspending); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("Active→Suspending: %w", err))
	}

	// Register the stall-detecting waiter BEFORE asking the node to suspend, so a fast
	// SuspendComplete or SuspendProgress is never missed. The node persists each mount, tears the
	// pod down, emits phase-level progress events (sp-u53.7.2), and replies with per-mount markers.
	wt := s.suspends.register(id, uint64(gen))
	defer s.suspends.unregister(id)
	s.rt.SuspendOnNode(id, uint64(gen), captureRootfsArtifact)

	// Post-round-trip store writes use a detached ctx so the outcome survives a client disconnect.
	storeCtx := context.WithoutCancel(ctx)

	// Generous absolute backstop: fires only if the stall detector (below) somehow fails to catch
	// a wedge — e.g. the CP goroutine itself is stuck. The operative failure signal is stall.C.
	wait, waitCancel := context.WithTimeout(ctx, s.suspendTimeout)
	defer waitCancel()

	// Per-transition stall window: resets on each progress event. If no progress arrives within
	// suspendStallWindow the suspend is considered wedged → SetError (design §5 "persist failure → error").
	stall := time.NewTimer(s.suspendStallWindow)
	defer stall.Stop()

	// persistAccumulatedMarkers writes partial markers (from SuspendProgress events) to the store.
	// Best-effort: a failure is logged and teardown continues so a wedge does not strand them.
	persistAccumulatedMarkers := func(label string) {
		wt.markersMu.Lock()
		acc := make(map[string]string, len(wt.partialMarkers))
		for k, v := range wt.partialMarkers {
			acc[k] = v
		}
		wt.markersMu.Unlock()
		for name, marker := range acc {
			if merr := s.st.Spawns().SetMountMarker(storeCtx, id, name, marker); merr != nil {
				log.Printf("suspendLocked %s: %s SetMountMarker(%s): %v", id, label, name, merr)
			}
		}
	}

	for {
		select {
		case sc := <-wt.ch:
			return s.handleSuspendReply(storeCtx, owner, id, gen, leaseID, sc)

		case <-wt.progressCh:
			// Progress event: node is still making forward progress — reset the stall timer.
			// Opportunistically persist any partial markers that have accumulated.
			if !stall.Stop() {
				select {
				case <-stall.C:
				default:
				}
			}
			stall.Reset(s.suspendStallWindow)
			persistAccumulatedMarkers("progress")

		case <-stall.C:
			// STALL: no progress for suspendStallWindow — node is wedged. Close the sp-iuo1 drop
			// window FIRST: unregister the waiter so any SuspendComplete arriving from here on gets
			// deliver()==false and is routed to reconcileLateSuspend, then drain anything already
			// buffered (a reply that raced the timer) and finalise it as the real outcome rather
			// than erroring. unregister is idempotent with the deferred one above.
			s.suspends.unregister(id)
			select {
			case sc := <-wt.ch:
				return s.handleSuspendReply(storeCtx, owner, id, gen, leaseID, sc)
			default:
			}
			// Genuinely wedged: persist partial markers (best-effort), then move to terminal 'error'.
			// reconcileLateSuspend will flip Errored→Suspended if the node eventually finishes (sp-iuo1).
			persistAccumulatedMarkers("stall")
			s.rt.Drop(id)
			if serr := s.st.Spawns().SetError(storeCtx, id); serr != nil {
				log.Printf("suspendLocked %s: SetError after stall also failed: %v", id, serr)
			}
			return nil, connect.NewError(connect.CodeDeadlineExceeded,
				fmt.Errorf("suspend stalled (no progress for %s)", s.suspendStallWindow))

		case <-wait.Done():
			if ctx.Err() != nil {
				// claimCtx was cancelled (ErrClaimLost from heartbeat, or client disconnect).
				// Bail: commit no further transition; the recovery sweep will handle this.
				return nil, connect.NewError(connect.CodeAborted, fmt.Errorf("claim lost or request cancelled during suspend"))
			}
			// Absolute backstop expired. Design §5 "persist failure → error". Close the drop window
			// first (same as the stall path): unregister, drain a raced reply, else error.
			s.suspends.unregister(id)
			select {
			case sc := <-wt.ch:
				return s.handleSuspendReply(storeCtx, owner, id, gen, leaseID, sc)
			default:
			}
			s.rt.Drop(id)
			if serr := s.st.Spawns().SetError(storeCtx, id); serr != nil {
				log.Printf("suspendLocked %s: SetError after suspend timeout also failed: %v", id, serr)
			}
			return nil, connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("timed out awaiting node suspend"))
		}
	}
}

// handleSuspendReply finalises a suspend from a node SuspendComplete. A gate-abort (error set)
// reverts Suspending→Active (lease-fenced, retrying a benign seq bump) and returns
// FailedPrecondition — the spawn is still running. A clean reply records the per-mount markers,
// drops the route, and finalises Suspended. Shared by the main wait AND the stall/backstop drains
// so a reply that races the stall/backstop timer is finalised here rather than dropped (sp-iuo1).
func (s *Server) handleSuspendReply(storeCtx context.Context, owner, id string, gen int64, leaseID string, sc *nodev1.SuspendComplete) (*nodev1.SuspendComplete, error) {
	// Fail-closed gate (spec §6): node aborted the suspend — nothing reaped/torn down, spawn still
	// running. Revert Suspending→Active so the row is consistent and the caller can retry.
	if detail := sc.GetError(); detail != "" {
		const maxRevertRetries = 3
		var revertErr error
		for i := 0; i < maxRevertRetries; i++ {
			freshSP, gerr := s.st.Spawns().Get(storeCtx, id)
			if gerr != nil {
				revertErr = gerr
				break
			}
			_, revertErr = s.st.Spawns().TransitionClaimed(storeCtx, id, leaseID,
				freshSP.StatusSeq, gen, store.Active)
			if revertErr == nil || !errors.Is(revertErr, store.ErrConflict) {
				break
			}
			// Benign seq bump (e.g. Touch): re-read and retry.
		}
		if revertErr != nil {
			log.Printf("suspendLocked %s: revert Suspending→Active failed: %v", id, revertErr)
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("suspend failed: %s — spawn left running", detail))
	}
	// Success path: record markers, drop route, finalise suspended.
	for _, mk := range sc.GetMarkers() {
		if merr := s.st.Spawns().SetMountMarker(storeCtx, id, mk.GetName(), mk.GetMarker()); merr != nil {
			log.Printf("SuspendSpawn %s: SetMountMarker(%s): %v", id, mk.GetName(), merr)
		}
	}
	s.rt.Drop(id)
	if err := s.st.Spawns().SetSuspended(storeCtx, id, gen); err != nil {
		// Pod torn down but couldn't record 'suspended'. Compensate to terminal 'error'.
		if serr := s.st.Spawns().SetError(storeCtx, id); serr != nil {
			log.Printf("suspendLocked %s: SetError after SetSuspended failure also failed: %v", id, serr)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	_ = s.tel.Emit(telemetry.Event{Kind: "session_end", Owner: owner, SpawnID: id, Timestamp: time.Now().UTC()})
	return sc, nil
}

// reconcileLateSuspend handles a SuspendComplete that arrived with no live waiter — the CP's
// stall window fired and left the spawn in Errored, but the node genuinely finished suspending
// later (sp-iuo1). If the spawn is still Errored and the completed generation matches its latest
// container, this flips Errored→Suspended and records the markers so the spawn is resumable.
//
// This function is called from a background goroutine (server.go) so it must not block the node
// receive loop. It uses a best-effort withClaim: if the spawn is busy (another driver holds it
// — e.g. RecreateSpawn mid-flight), it simply logs and returns.
func (s *Server) reconcileLateSuspend(ctx context.Context, sc *nodev1.SuspendComplete) {
	if sc.GetError() != "" {
		return // gate failure: node refused the suspend, spawn is still running — nothing to reconcile
	}
	id := sc.GetSpawnId()
	gen := int64(sc.GetGeneration())
	claimErr := s.withClaim(ctx, id, func(cctx context.Context, leaseID string) error {
		sp, err := s.st.Spawns().Get(cctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil // spawn deleted/unknown: no-op
		}
		if err != nil {
			return err
		}
		if sp.Status != store.Errored {
			return nil // already recovered, recreated, or active — do not interfere
		}
		// Gen fence: verify the latest container (ended or live) matches this late reply's gen.
		// Prevents misattributing a very-late reply from an old episode when the spawn has been
		// recreated and stalled again at a higher generation.
		if c, hasAny, _ := s.st.Spawns().LatestContainer(cctx, id); hasAny && c.Generation != gen {
			return nil // gen mismatch: this reply belongs to a superseded episode; skip
		}
		// Record any markers the late SuspendComplete carries (they may complement partial markers
		// already persisted by the stall path).
		for _, mk := range sc.GetMarkers() {
			if merr := s.st.Spawns().SetMountMarker(cctx, id, mk.GetName(), mk.GetMarker()); merr != nil {
				log.Printf("reconcileLateSuspend %s: SetMountMarker(%s): %v", id, mk.GetName(), merr)
			}
		}
		// Flip Errored→Suspended (best-effort). The container was already ended by SetError in the
		// stall path — ReconcileSuspendedAfterError does not attempt to end it again.
		if rerr := s.st.Spawns().ReconcileSuspendedAfterError(cctx, id); rerr != nil {
			log.Printf("reconcileLateSuspend %s: ReconcileSuspendedAfterError: %v", id, rerr)
		}
		return nil
	})
	if claimErr != nil {
		// Spawn busy (another driver holds the claim) or not found: log and give up.
		// The spawn is already in a defined state (Errored or moved on by the other driver).
		log.Printf("reconcileLateSuspend %s: withClaim: %v", id, claimErr)
	}
}

// SetSuspendStallWindow overrides the stall window for the current server. Used in tests to make
// the stall detector fire quickly. In production this is always defaultSuspendStallWindow.
func (s *Server) SetSuspendStallWindow(d time.Duration) { s.suspendStallWindow = d }

// SetResumeStallWindow overrides the resume stall window. Used in tests to make the stall detector
// fire quickly. In production this is always defaultResumeStallWindow.
func (s *Server) SetResumeStallWindow(d time.Duration) { s.resumeStallWindow = d }

// ResumeSpawn provisions a FRESH container for a suspended spawn (non-lossless — a brand-new
// container, no prior in-container state). suspended -> starting -> resuming -> active. Reuses
// the same scheduler.Provision + SetActive path as CreateSpawn, with the same orphan-window
// compensation on failure.
func (s *Server) ResumeSpawn(ctx context.Context, req *connect.Request[cpv1.ResumeSpawnRequest]) (*connect.Response[cpv1.ResumeSpawnResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	id := req.Msg.SpawnId
	if sp, err := s.st.Spawns().Get(ctx, id); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	} else if sp.OwnerID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}
	// A plain resume re-places anywhere the policy allows (no override) and, on failure, lands in
	// 'error' (sp-a7fs contract). MigrateSpawn reuses resumeLocked with an override + revert-on-fail.
	if err := s.withClaim(ctx, id, func(cctx context.Context, leaseID string) error {
		_, err := s.resumeLocked(cctx, owner, id, placementOverride{}, false, intent.OpResumeSpawn, nil, leaseID)
		return err
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(&cpv1.ResumeSpawnResponse{}), nil
}

// placementOverride forces a resume's target (sp-u53.5.3): NodeID pins one node, Class restricts to a
// node class (e.g. "cloud"). A zero value = no override (the plain ResumeSpawn placement).
type placementOverride struct {
	NodeID string
	Class  string
}

type rootfsRestorePins struct {
	SourceGeneration uint64
	Pins             []store.RootfsArtifactPin
}

func schedulerRootfsRestore(rootfs *rootfsRestorePins) *scheduler.RootfsRestore {
	if rootfs == nil || len(rootfs.Pins) == 0 {
		return nil
	}
	out := make([]*nodev1.RootfsArtifact, 0, len(rootfs.Pins))
	for _, pin := range rootfs.Pins {
		out = append(out, &nodev1.RootfsArtifact{
			ArtifactId:       pin.ArtifactID,
			Generation:       pin.Generation,
			Sequence:         int32(pin.Sequence),
			BaseImageDigest:  pin.BaseImageDigest,
			Format:           pin.Format,
			ContentDigest:    pin.ContentDigest,
			UncompressedSize: pin.UncompressedSize,
		})
	}
	return &scheduler.RootfsRestore{SourceGeneration: rootfs.SourceGeneration, Artifacts: out}
}

func rootfsPinsFromSuspend(sc *nodev1.SuspendComplete, sourceGeneration uint64, baseImageDigest string) ([]store.RootfsArtifactPin, error) {
	if sc == nil || len(sc.GetRootfsArtifacts()) == 0 {
		return nil, nil
	}
	pins := make([]store.RootfsArtifactPin, 0, len(sc.GetRootfsArtifacts()))
	for _, art := range sc.GetRootfsArtifacts() {
		if art.GetArtifactId() == "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("rootfs artifact restore pin is missing artifact id"))
		}
		if art.GetGeneration() != sourceGeneration {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("rootfs artifact %s generation %d does not match source generation %d",
				art.GetArtifactId(), art.GetGeneration(), sourceGeneration))
		}
		if baseImageDigest != "" && art.GetBaseImageDigest() != "" && art.GetBaseImageDigest() != baseImageDigest {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("rootfs artifact %s base digest %s does not match pinned base digest %s",
				art.GetArtifactId(), art.GetBaseImageDigest(), baseImageDigest))
		}
		pins = append(pins, store.RootfsArtifactPin{
			ArtifactID:       art.GetArtifactId(),
			ArtifactType:     "rootfs_delta",
			Generation:       art.GetGeneration(),
			Sequence:         int(art.GetSequence()),
			BaseImageDigest:  art.GetBaseImageDigest(),
			Format:           art.GetFormat(),
			ContentDigest:    art.GetContentDigest(),
			UncompressedSize: art.GetUncompressedSize(),
		})
	}
	return pins, nil
}

// resumeLocked is the shared resume core. The CALLER must hold the DB claim for id (via withClaim).
// It claims 'starting' (bumping generation), transitions to 'resuming', provisions onto a placement
// — optionally forced by ov — and finalises 'active'. On provision/activation failure it leaves a
// DEFINED state: revertOnFail=true (migration) rolls back to 'suspended'; false (plain resume) goes
// to 'error'. Returns the node the spawn resumed on.
// op identifies the lifecycle operation for the A4 PendingIntent domain tag [AC1].
func (s *Server) resumeLocked(ctx context.Context, owner, id string, ov placementOverride, revertOnFail bool, op intent.Op, rootfs *rootfsRestorePins, leaseID string) (string, error) {
	sp, err := s.st.Spawns().Get(ctx, id)
	if err != nil {
		return "", connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if sp.OwnerID != owner {
		return "", connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}
	if sp.Status != store.Suspended {
		return "", connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("spawn is not suspended"))
	}
	// Placement is re-evaluated against the version's CURRENT tier at resume time.
	ver, err := s.st.Apps().GetVersion(ctx, sp.AppID, sp.AppVersion)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, err)
	}
	placement, err := s.placementFor(ctx, owner, sp.AppID, ver)
	if err != nil {
		return "", err
	}
	placement.Image = sp.Image
	placement.TargetNodeID = ov.NodeID
	placement.RequireClass = ov.Class

	var gen int64
	if err := s.st.WithTx(ctx, func(tx store.Store) error {
		g, e := tx.Spawns().ClaimStarting(ctx, id, []store.Status{store.Suspended})
		gen = g
		return e
	}); err != nil {
		return "", connect.NewError(connect.CodeInternal, err)
	}

	// Transition Starting→Resuming under the held claim (lease+seq+gen fenced). Sweepers now read
	// Resuming and skip this spawn during the provision round-trip.
	freshSP, err := s.st.Spawns().Get(ctx, id)
	if err != nil {
		s.failResume(ctx, id, gen, revertOnFail, "Get-for-Resuming")
		return "", connect.NewError(connect.CodeInternal, err)
	}
	if _, err := s.st.Spawns().TransitionClaimed(ctx, id, leaseID, freshSP.StatusSeq, gen, store.Resuming); err != nil {
		s.failResume(ctx, id, gen, revertOnFail, "Starting→Resuming")
		return "", connect.NewError(connect.CodeInternal, fmt.Errorf("Starting→Resuming: %w", err))
	}

	// A4 two-phase sign-after-resolve [AC1]: pick node → register pending intent → await client.
	// Skipped when intentEnabled=false.
	var env *authv1.AuthEnvelope
	var secrets []*nodev1.SealedSecret
	mounts, mountsErr := s.st.Spawns().GetMounts(ctx, id)
	if mountsErr != nil {
		s.failResume(ctx, id, gen, revertOnFail, "GetMounts")
		return "", connect.NewError(connect.CodeInternal, mountsErr)
	}
	var arts []store.Artifact
	var requiredSecretIDs []string
	if s.intentEnabled {
		var aerr error
		arts, aerr = s.st.Spawns().GetArtifacts(ctx, id)
		if aerr != nil {
			s.failResume(ctx, id, gen, revertOnFail, "GetArtifacts")
			return "", connect.NewError(connect.CodeInternal, aerr)
		}
		requiredSecretIDs = startupSecretIDsFromArtifacts(arts)
		targetNodeID, pickErr := s.sched.PickNodeID(placement)
		if pickErr != nil {
			s.failResume(ctx, id, gen, revertOnFail, "PickNodeID")
			return "", pickErr
		}
		pi := buildPendingIntent(op, id, uint64(gen), targetNodeID, sp.Image, sp.AppRef, sp.Model, "", mounts, requiredSecretIDs)
		ch := s.pendingIntents.register(id, owner, pi)
		defer s.pendingIntents.cleanup(id)
		submission, awaitErr := s.pendingIntents.await(ctx, ch)
		err = awaitErr
		if err != nil {
			s.failResume(ctx, id, gen, revertOnFail, "await SignedIntent")
			return "", connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("await SignedIntent: %w", err))
		}
		if err := validateSubmittedStartupSecrets(requiredSecretIDs, submission.Secrets); err != nil {
			s.failResume(ctx, id, gen, revertOnFail, "validate startup secrets")
			return "", connect.NewError(connect.CodeFailedPrecondition, err)
		}
		env = submission.Env
		secrets = submission.Secrets
		placement.TargetNodeID = targetNodeID
	}

	// Register the resume stall-detecting waiter BEFORE launching the provision goroutine so a fast
	// ResumeProgress is never missed. The stall window resets on each phase event from the node
	// (sp-u53.7.2). The waiter is keyed by the NEW generation (gen) from ClaimStarting above.
	rwt := s.resumes.register(id, uint64(gen))
	defer s.resumes.unregister(id)

	// Run Provision in a goroutine so the stall select loop can race it.
	type provisionResult struct {
		nodeID string
		err    error
	}
	provCh := make(chan provisionResult, 1)
	provCtx, provCancel := context.WithCancel(ctx)
	defer provCancel()
	go func() {
		provisionArts := arts
		if provisionArts == nil {
			provisionArts, _ = s.st.Spawns().GetArtifacts(provCtx, id)
		}
		n, e := s.sched.Provision(provCtx, id, sp.AppRef, sp.Model, sp.Name, sp.AppID, sp.RunnableID, sp.Mode, uint64(gen), placement, env, storeToNodeMounts(mounts), sp.BaseImageDigest, schedulerRootfsRestore(rootfs), storeToNodeArtifacts(provisionArts), secrets)
		provCh <- provisionResult{n, e}
	}()

	// Generous absolute backstop: fires only if the stall detector fails to catch a wedge.
	wait, waitCancel := context.WithTimeout(ctx, s.resumeTimeout)
	defer waitCancel()

	// Per-transition stall window: resets on each ResumeProgress event from the node.
	stall := time.NewTimer(s.resumeStallWindow)
	defer stall.Stop()

	storeCtx := context.WithoutCancel(ctx)
	for {
		select {
		case res := <-provCh:
			if res.err != nil {
				s.failResume(storeCtx, id, gen, revertOnFail, "provision")
				return "", res.err
			}
			nodeID := res.nodeID
			// SetActive now accepts Resuming (extended from-set in store/spawns.go, sp-u53.7.5).
			if err := s.st.Spawns().SetActive(storeCtx, id, nodeID, gen); err != nil {
				s.rt.StopOnNode(id)
				s.rt.Drop(id)
				s.failResume(storeCtx, id, gen, revertOnFail, "SetActive")
				return "", connect.NewError(connect.CodeInternal, err)
			}
			// Fresh container started with spawns.model -> mark applied.
			if merr := s.st.Spawns().MarkModelApplied(storeCtx, id); merr != nil {
				log.Printf("resumeLocked %s: MarkModelApplied after resume: %v", id, merr)
			}
			return nodeID, nil

		case <-rwt.progressCh:
			// Progress from the node: reset the stall timer.
			if !stall.Stop() {
				select {
				case <-stall.C:
				default:
				}
			}
			stall.Reset(s.resumeStallWindow)

		case <-stall.C:
			// STALL: no progress for resumeStallWindow — node is wedged. Cancel provision and
			// move the spawn to a defined state (error or reverted to suspended on migration).
			provCancel()
			s.rt.Drop(id)
			s.failResume(storeCtx, id, gen, revertOnFail, "resume stall")
			return "", connect.NewError(connect.CodeDeadlineExceeded,
				fmt.Errorf("resume stalled (no progress for %s)", s.resumeStallWindow))

		case <-wait.Done():
			if ctx.Err() != nil {
				// claimCtx was cancelled (ErrClaimLost from heartbeat, or client disconnect).
				provCancel()
				return "", connect.NewError(connect.CodeAborted, fmt.Errorf("claim lost or request cancelled during resume"))
			}
			// Absolute backstop expired.
			provCancel()
			s.rt.Drop(id)
			s.failResume(storeCtx, id, gen, revertOnFail, "resume timeout")
			return "", connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("timed out awaiting node resume"))
		}
	}
}

// failResume puts a failed resume into a DEFINED state. For a migration (revert=true) it rolls the
// starting/resuming episode BACK to 'suspended' (RevertSuspended accepts Starting and Resuming,
// sp-u53.7.5) and cleans the target artifacts. For a plain resume (revert=false) it goes to terminal
// 'error'. Uses a detached store ctx so cleanup survives a client disconnect.
func (s *Server) failResume(ctx context.Context, id string, gen int64, revert bool, stage string) {
	storeCtx := context.WithoutCancel(ctx)
	if !revert {
		if serr := s.st.Spawns().SetError(storeCtx, id); serr != nil {
			log.Printf("resumeLocked %s: SetError after %s failure also failed: %v", id, stage, serr)
		}
		return
	}
	s.rt.StopOnNode(id)
	s.rt.Drop(id)
	if rerr := s.st.Spawns().RevertSuspended(storeCtx, id, gen); rerr != nil {
		// Couldn't roll back to suspended — fall back to 'error' (still a defined, recreate-able state).
		log.Printf("MigrateSpawn %s: RevertSuspended after %s failure failed: %v; falling back to error", id, stage, rerr)
		if serr := s.st.Spawns().SetError(storeCtx, id); serr != nil {
			log.Printf("MigrateSpawn %s: SetError fallback also failed: %v", id, serr)
		}
	}
}

// MigrateSpawn does data-only local<->cloud migration (sp-u53.5.3, design §5): under a SINGLE DB
// claim it SUSPENDS the spawn on its source node (reusing suspendLocked) then RESUMES it with a
// placement override onto the target. One claim covers both halves so the entire suspend→resume
// sequence is mutually exclusive with other drivers. On any resume failure the spawn lands in a
// DEFINED state (back to 'suspended', target artifacts cleaned), never a half-migrated hang.
func (s *Server) MigrateSpawn(ctx context.Context, req *connect.Request[cpv1.MigrateSpawnRequest]) (*connect.Response[cpv1.MigrateSpawnResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	id := req.Msg.SpawnId
	targetNode := strings.TrimSpace(req.Msg.TargetNodeId)
	targetClass := strings.TrimSpace(req.Msg.TargetClass)
	if targetNode == "" && targetClass == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("a migration target (node id or class) is required"))
	}
	if targetNode != "" && targetClass != "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("specify a target node id OR a class, not both"))
	}

	sp, err := s.st.Spawns().Get(ctx, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if sp.OwnerID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}
	// Pre-validate a specific-node target's tenancy BEFORE claiming, so a foreign self-hosted target
	// is rejected with the spawn left untouched.
	if targetNode != "" {
		exists, eligible := s.reg.TargetEligible(targetNode, owner)
		if !exists {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("target node %q is not connected", targetNode))
		}
		if !eligible {
			return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("target node %q belongs to another owner", targetNode))
		}
	}
	// Durability-class guard (sp-8dkp §2): checked BEFORE claiming so a rejected move leaves the
	// spawn untouched.
	liveNode := s.liveNodeForSpawn(ctx, id)
	if err := s.guardCrossNodeDurability(ctx, id, liveNode, targetNode, req.Msg.UpgradeToOwnerSealed); err != nil {
		return nil, err
	}
	if targetNode == "" && targetClass != "" {
		ver, err := s.st.Apps().GetVersion(ctx, sp.AppID, sp.AppVersion)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		placement, err := s.placementFor(ctx, owner, sp.AppID, ver)
		if err != nil {
			return nil, err
		}
		placement.Image = sp.Image
		placement.RequireClass = targetClass
		pickedNode, err := s.sched.PickNodeID(placement)
		if err != nil {
			return nil, err
		}
		targetNode = pickedNode
		targetClass = ""
	}
	if sp.Status != store.Active && sp.Status != store.Suspended {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("spawn must be active or suspended to migrate"))
	}

	sourceGeneration, sourceNodeID := uint64(0), liveNode
	if c, hasLive, cerr := s.st.Spawns().LiveContainer(ctx, id); cerr != nil {
		return nil, connect.NewError(connect.CodeInternal, cerr)
	} else if hasLive {
		sourceGeneration = uint64(c.Generation)
		sourceNodeID = c.NodeID
	} else if c, hasAny, cerr := s.st.Spawns().LatestContainer(ctx, id); cerr != nil {
		return nil, connect.NewError(connect.CodeInternal, cerr)
	} else if hasAny {
		sourceGeneration = uint64(c.Generation)
		sourceNodeID = c.NodeID
	}
	targetGeneration := sourceGeneration + 1
	transferSetID := uuid.NewString()
	now := s.now().UnixNano()
	if err := s.st.TransferSets().Create(ctx, store.TransferSet{
		ID:                transferSetID,
		SpawnID:           id,
		SourceGeneration:  sourceGeneration,
		TargetGeneration:  targetGeneration,
		SourceNodeID:      sourceNodeID,
		TargetNodeID:      targetNode,
		BaseImageDigest:   sp.BaseImageDigest,
		TransferKeyStatus: store.TransferKeyTargetReady,
		Status:            store.TransferSetPending,
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create transfer set: %w", err))
	}
	if err := s.st.TransferSets().SetStatus(ctx, transferSetID, store.TransferSetCapturing, s.now().UnixNano()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("mark transfer set capturing: %w", err))
	}

	// Hold ONE claim across the entire suspend→transfer-set→resume body (spec §5).
	var (
		sourceComplete *nodev1.SuspendComplete
		rootfsPins     []store.RootfsArtifactPin
		nodeID         string
		migrateErr     error
	)
	claimErr := s.withClaim(ctx, id, func(cctx context.Context, leaseID string) error {
		// Suspend the source if still active (markers persist). An already-suspended spawn skips
		// straight to the placement-overridden resume.
		switch sp.Status {
		case store.Active:
			sc, err := s.suspendLocked(cctx, owner, id, true, leaseID)
			if err != nil {
				_ = s.st.TransferSets().SetStatus(context.WithoutCancel(ctx), transferSetID, store.TransferSetFailed, s.now().UnixNano())
				migrateErr = err
				return err
			}
			sourceComplete = sc
		case store.Suspended:
			// already down — resume elsewhere.
		}

		if sourceComplete != nil {
			var err error
			rootfsPins, err = rootfsPinsFromSuspend(sourceComplete, sourceGeneration, sp.BaseImageDigest)
			if err != nil {
				_ = s.st.TransferSets().SetStatus(context.WithoutCancel(ctx), transferSetID, store.TransferSetFailed, s.now().UnixNano())
				migrateErr = err
				return err
			}
		}
		mountPins := map[string]string{}
		if sourceComplete != nil {
			for _, mk := range sourceComplete.GetMarkers() {
				mountPins[mk.GetName()] = mk.GetMarker()
			}
		} else {
			mounts, err := s.st.Spawns().GetMounts(cctx, id)
			if err != nil {
				_ = s.st.TransferSets().SetStatus(context.WithoutCancel(ctx), transferSetID, store.TransferSetFailed, s.now().UnixNano())
				migrateErr = connect.NewError(connect.CodeInternal, err)
				return migrateErr
			}
			for _, mk := range mounts {
				if mk.PersistMarker != "" {
					mountPins[mk.Name] = mk.PersistMarker
				}
			}
		}
		if err := s.st.TransferSets().SetPins(cctx, transferSetID, sourceGeneration, mountPins, rootfsPins, s.now().UnixNano()); err != nil {
			_ = s.st.TransferSets().SetStatus(context.WithoutCancel(ctx), transferSetID, store.TransferSetFailed, s.now().UnixNano())
			migrateErr = connect.NewError(connect.CodeInternal, fmt.Errorf("record transfer set pins: %w", err))
			return migrateErr
		}
		if err := s.st.TransferSets().SetStatus(cctx, transferSetID, store.TransferSetRestoring, s.now().UnixNano()); err != nil {
			_ = s.st.TransferSets().SetStatus(context.WithoutCancel(ctx), transferSetID, store.TransferSetFailed, s.now().UnixNano())
			migrateErr = connect.NewError(connect.CodeInternal, fmt.Errorf("mark transfer set restoring: %w", err))
			return migrateErr
		}
		var err error
		nodeID, err = s.resumeLocked(cctx, owner, id, placementOverride{NodeID: targetNode, Class: targetClass}, true, intent.OpMigrateSpawn,
			&rootfsRestorePins{SourceGeneration: sourceGeneration, Pins: rootfsPins}, leaseID)
		if err != nil {
			_ = s.st.TransferSets().SetStatus(context.WithoutCancel(ctx), transferSetID, store.TransferSetFailed, s.now().UnixNano())
			migrateErr = err
			return err
		}
		return nil
	})
	if claimErr != nil {
		if migrateErr != nil {
			return nil, migrateErr // already a connect error from inside the claim
		}
		return nil, claimErr
	}

	if err := s.st.TransferSets().SetTargetNode(ctx, transferSetID, nodeID, s.now().UnixNano()); err != nil {
		_ = s.st.TransferSets().SetStatus(context.WithoutCancel(ctx), transferSetID, store.TransferSetFailed, s.now().UnixNano())
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update transfer set target node: %w", err))
	}
	if err := s.st.TransferSets().SetStatus(ctx, transferSetID, store.TransferSetActive, s.now().UnixNano()); err != nil {
		_ = s.st.TransferSets().SetStatus(context.WithoutCancel(ctx), transferSetID, store.TransferSetFailed, s.now().UnixNano())
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("mark transfer set active: %w", err))
	}
	// Mark delivery-pending when the owner-sealed upgrade path is active.
	if req.Msg.UpgradeToOwnerSealed {
		s.deliveryPending.mark(id)
	}
	return connect.NewResponse(&cpv1.MigrateSpawnResponse{NodeId: nodeID, TransferSetId: transferSetID}), nil
}

// RecreateSpawn provisions a FRESH container for a spawn that lost its node (unreachable) or errored
// — user-driven recovery. It best-effort fences+stops any returned old container, then re-provisions
// at a NEW generation (unreachable|error) -> starting -> active. Non-lossless (a brand-new container),
// like ResumeSpawn; real data persistence is gated on E3.
func (s *Server) RecreateSpawn(ctx context.Context, req *connect.Request[cpv1.RecreateSpawnRequest]) (*connect.Response[cpv1.RecreateSpawnResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	unlock := s.locks.Lock(req.Msg.SpawnId)
	defer unlock()
	sp, err := s.st.Spawns().Get(ctx, req.Msg.SpawnId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if sp.OwnerID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}
	if sp.Status != store.Unreachable && sp.Status != store.Errored {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("spawn is not unreachable or errored"))
	}
	ver, err := s.st.Apps().GetVersion(ctx, sp.AppID, sp.AppVersion)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	placement, err := s.placementFor(ctx, owner, sp.AppID, ver)
	if err != nil {
		return nil, err
	}
	// Fence the old container: if its node has returned, tell it to stop + drop the route.
	s.rt.StopOnNode(req.Msg.SpawnId)
	s.rt.Drop(req.Msg.SpawnId)
	var gen int64
	if err := s.st.WithTx(ctx, func(tx store.Store) error {
		g, e := tx.Spawns().ClaimStarting(ctx, req.Msg.SpawnId, []store.Status{store.Unreachable, store.Errored})
		gen = g
		return e
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	placement.Image = sp.Image

	// A4 two-phase sign-after-resolve [AC1]: pick node → register pending intent → await client.
	var env *authv1.AuthEnvelope
	var secrets []*nodev1.SealedSecret
	mounts, mountsErr := s.st.Spawns().GetMounts(ctx, req.Msg.SpawnId)
	if mountsErr != nil {
		if serr := s.st.Spawns().SetError(ctx, req.Msg.SpawnId); serr != nil {
			log.Printf("RecreateSpawn %s: SetError after GetMounts failure also failed: %v", req.Msg.SpawnId, serr)
		}
		return nil, connect.NewError(connect.CodeInternal, mountsErr)
	}
	var arts []store.Artifact
	var requiredSecretIDs []string
	if s.intentEnabled {
		var aerr error
		arts, aerr = s.st.Spawns().GetArtifacts(ctx, req.Msg.SpawnId)
		if aerr != nil {
			if serr := s.st.Spawns().SetError(ctx, req.Msg.SpawnId); serr != nil {
				log.Printf("RecreateSpawn %s: SetError after GetArtifacts failure also failed: %v", req.Msg.SpawnId, serr)
			}
			return nil, connect.NewError(connect.CodeInternal, aerr)
		}
		requiredSecretIDs = startupSecretIDsFromArtifacts(arts)
		targetNodeID, pickErr := s.sched.PickNodeID(placement)
		if pickErr != nil {
			if serr := s.st.Spawns().SetError(ctx, req.Msg.SpawnId); serr != nil {
				log.Printf("RecreateSpawn %s: SetError after PickNodeID failure also failed: %v", req.Msg.SpawnId, serr)
			}
			return nil, pickErr
		}
		pi := buildPendingIntent(intent.OpRecreateSpawn, req.Msg.SpawnId, uint64(gen), targetNodeID, sp.Image, sp.AppRef, sp.Model, "", mounts, requiredSecretIDs)
		ch := s.pendingIntents.register(req.Msg.SpawnId, owner, pi)
		defer s.pendingIntents.cleanup(req.Msg.SpawnId)
		submission, awaitErr := s.pendingIntents.await(ctx, ch)
		if awaitErr != nil {
			if serr := s.st.Spawns().SetError(ctx, req.Msg.SpawnId); serr != nil {
				log.Printf("RecreateSpawn %s: SetError after await failure also failed: %v", req.Msg.SpawnId, serr)
			}
			return nil, connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("await SignedIntent: %w", awaitErr))
		}
		if err := validateSubmittedStartupSecrets(requiredSecretIDs, submission.Secrets); err != nil {
			if serr := s.st.Spawns().SetError(ctx, req.Msg.SpawnId); serr != nil {
				log.Printf("RecreateSpawn %s: SetError after startup secret validation failure also failed: %v", req.Msg.SpawnId, serr)
			}
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		env = submission.Env
		secrets = submission.Secrets
		placement.TargetNodeID = targetNodeID
	}

	if arts == nil {
		arts, _ = s.st.Spawns().GetArtifacts(ctx, req.Msg.SpawnId)
	}
	nodeID, err := s.sched.Provision(ctx, req.Msg.SpawnId, sp.AppRef, sp.Model, sp.Name, sp.AppID, sp.RunnableID, sp.Mode, uint64(gen), placement, env, storeToNodeMounts(mounts), sp.BaseImageDigest, nil, storeToNodeArtifacts(arts), secrets)
	if err != nil {
		if serr := s.st.Spawns().SetError(ctx, req.Msg.SpawnId); serr != nil {
			log.Printf("RecreateSpawn %s: SetError after provision failure also failed: %v", req.Msg.SpawnId, serr)
		}
		return nil, err
	}
	if err := s.st.Spawns().SetActive(ctx, req.Msg.SpawnId, nodeID, gen); err != nil {
		s.rt.StopOnNode(req.Msg.SpawnId)
		s.rt.Drop(req.Msg.SpawnId)
		if serr := s.st.Spawns().SetError(ctx, req.Msg.SpawnId); serr != nil {
			log.Printf("RecreateSpawn %s: SetError after SetActive failure also failed: %v", req.Msg.SpawnId, serr)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Fresh container started with spawns.model -> mark applied.
	if merr := s.st.Spawns().MarkModelApplied(context.WithoutCancel(ctx), req.Msg.SpawnId); merr != nil {
		log.Printf("RecreateSpawn %s: MarkModelApplied after recreate: %v", req.Msg.SpawnId, merr)
	}
	if rerr := s.st.Spawns().MarkRecovered(context.WithoutCancel(ctx), req.Msg.SpawnId); rerr != nil {
		log.Printf("RecreateSpawn %s: MarkRecovered after recreate: %v", req.Msg.SpawnId, rerr)
	}
	return connect.NewResponse(&cpv1.RecreateSpawnResponse{}), nil
}

// ListSpawns returns the authenticated owner's non-deleted spawns (the durable ledger).
func (s *Server) ListSpawns(ctx context.Context, _ *connect.Request[cpv1.ListSpawnsRequest]) (*connect.Response[cpv1.ListSpawnsResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	spawns, err := s.st.Spawns().ListByOwner(ctx, owner)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*cpv1.SpawnSummary, len(spawns))
	for i, sp := range spawns {
		var gen uint64
		if c, ok, cerr := s.st.Spawns().LiveContainer(ctx, sp.ID); ok && cerr == nil {
			gen = uint64(c.Generation)
		}
		// Populate live transition phase/detail for Suspending/Resuming spawns (sp-u53.7.2).
		// These come from the in-flight waiters and are not persisted — empty on CP restart.
		var transPhase, transDetail string
		switch sp.Status {
		case store.Suspending:
			transPhase, transDetail = s.suspends.lastProgress(sp.ID)
		case store.Resuming:
			transPhase, transDetail = s.resumes.lastProgress(sp.ID)
		}
		out[i] = &cpv1.SpawnSummary{
			SpawnId: sp.ID, AppId: sp.AppID, AppVersion: sp.AppVersion, Model: sp.Model,
			Status: toSummaryStatus(sp.Status), CreatedAt: sp.CreatedAt, LastUsedAt: sp.LastUsedAt,
			Name: sp.Name, Mode: sp.Mode, ModelApplied: sp.ModelApplied,
			Generation: gen, JournalKeyDeliveryPending: s.deliveryPending.isPending(sp.ID),
			TransitionPhase: transPhase, TransitionDetail: transDetail,
		}
	}
	return connect.NewResponse(&cpv1.ListSpawnsResponse{Spawns: out}), nil
}
