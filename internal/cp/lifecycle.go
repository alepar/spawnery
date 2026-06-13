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

// defaultSuspendTimeout bounds how long SuspendSpawn waits for the hosting node's SuspendComplete
// after asking it to persist+tear down. A real journal final snapshot can take a while, so this is
// generous; on expiry the spawn is moved to 'error' (design §5: "persist failure → error").
const defaultSuspendTimeout = 30 * time.Second

// suspendWaiters correlates a SuspendSpawn with the async SuspendComplete the hosting node sends back
// on its Attach stream, carrying the per-mount persist markers. Keyed by spawn_id: the DB claim
// serialises suspends, so at most one waiter per spawn is ever live. The waiter records the episode
// generation it expects; deliver drops a SuspendComplete whose generation differs (a stale-episode
// reply from a superseded pod), mirroring the node-side generation fence.
type suspendWaiters struct {
	mu sync.Mutex
	m  map[string]suspendWaiter
}

type suspendWaiter struct {
	gen uint64
	ch  chan *nodev1.SuspendComplete
}

func newSuspendWaiters() *suspendWaiters {
	return &suspendWaiters{m: map[string]suspendWaiter{}}
}

// register installs a buffered (cap 1) waiter for (spawnID, gen) and returns its channel. Call BEFORE
// sending Suspend so a fast SuspendComplete is never missed.
func (w *suspendWaiters) register(spawnID string, gen uint64) chan *nodev1.SuspendComplete {
	ch := make(chan *nodev1.SuspendComplete, 1)
	w.mu.Lock()
	w.m[spawnID] = suspendWaiter{gen: gen, ch: ch}
	w.mu.Unlock()
	return ch
}

func (w *suspendWaiters) unregister(spawnID string) {
	w.mu.Lock()
	delete(w.m, spawnID)
	w.mu.Unlock()
}

// deliver routes an inbound SuspendComplete to its waiter (if any), matched by spawn_id AND
// generation. Non-blocking: a reply with no live waiter, or one whose generation != the awaiting
// episode's (a stale-episode reply from a superseded pod), is dropped rather than blocking the node
// receive loop or being misattributed to a later suspend.
func (w *suspendWaiters) deliver(sc *nodev1.SuspendComplete) {
	w.mu.Lock()
	wt, ok := w.m[sc.GetSpawnId()]
	w.mu.Unlock()
	if !ok || wt.gen != sc.GetGeneration() {
		return
	}
	select {
	case wt.ch <- sc:
	default:
	}
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
	// Sweepers now read Suspending and skip — the inFlight exemption is no longer needed.
	seq := sp.StatusSeq
	if _, err := s.st.Spawns().TransitionClaimed(ctx, id, leaseID, seq, gen, store.Suspending); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("Active→Suspending: %w", err))
	}

	// Register the marker waiter BEFORE asking the node to suspend, so a fast SuspendComplete is
	// never missed. The node persists each mount, tears the pod down, and replies with per-mount
	// markers.
	ch := s.suspends.register(id, uint64(gen))
	defer s.suspends.unregister(id)
	s.rt.SuspendOnNode(id, uint64(gen), captureRootfsArtifact)

	wait, waitCancel := context.WithTimeout(ctx, s.suspendTimeout)
	defer waitCancel()
	// Post-round-trip store writes use a detached ctx so the outcome survives a client disconnect.
	storeCtx := context.WithoutCancel(ctx)

	select {
	case sc := <-ch:
		// Fail-closed gate (spec §6): node aborted the suspend — nothing reaped/torn down, spawn
		// still running. Revert Suspending→Active so the row is consistent and the caller can retry.
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

	case <-wait.Done():
		if ctx.Err() != nil {
			// claimCtx was cancelled (ErrClaimLost from heartbeat, or client disconnect).
			// Bail: commit no further transition; the recovery sweep will handle this.
			return nil, connect.NewError(connect.CodeAborted, fmt.Errorf("claim lost or request cancelled during suspend"))
		}
		// Pure suspendTimeout: persist did not complete in time. Design §5 "persist failure → error".
		s.rt.Drop(id)
		if serr := s.st.Spawns().SetError(storeCtx, id); serr != nil {
			log.Printf("suspendLocked %s: SetError after suspend await timeout also failed: %v", id, serr)
		}
		return nil, connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("timed out awaiting node suspend"))
	}
}

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
	if s.intentEnabled {
		targetNodeID, pickErr := s.sched.PickNodeID(placement)
		if pickErr != nil {
			s.failResume(ctx, id, gen, revertOnFail, "PickNodeID")
			return "", pickErr
		}
		mounts, _ := s.st.Spawns().GetMounts(ctx, id)
		pi := buildPendingIntent(op, id, uint64(gen), targetNodeID, sp.Image, sp.AppRef, sp.Model, "", mounts)
		ch := s.pendingIntents.register(id, owner, pi)
		defer s.pendingIntents.cleanup(id)
		env, err = s.pendingIntents.await(ctx, ch)
		if err != nil {
			s.failResume(ctx, id, gen, revertOnFail, "await SignedIntent")
			return "", connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("await SignedIntent: %w", err))
		}
		placement.TargetNodeID = targetNodeID
	}

	nodeID, err := s.sched.Provision(ctx, id, sp.AppRef, sp.Model, sp.Name, sp.AppID, sp.RunnableID, sp.Mode, uint64(gen), placement, env, sp.BaseImageDigest, schedulerRootfsRestore(rootfs))
	if err != nil {
		s.failResume(ctx, id, gen, revertOnFail, "provision")
		return "", err
	}
	// SetActive now accepts Resuming (extended from-set in store/spawns.go, sp-u53.7.5).
	if err := s.st.Spawns().SetActive(ctx, id, nodeID, gen); err != nil {
		s.rt.StopOnNode(id)
		s.rt.Drop(id)
		s.failResume(ctx, id, gen, revertOnFail, "SetActive")
		return "", connect.NewError(connect.CodeInternal, err)
	}
	// Fresh container started with spawns.model -> mark applied.
	if merr := s.st.Spawns().MarkModelApplied(context.WithoutCancel(ctx), id); merr != nil {
		log.Printf("resumeLocked %s: MarkModelApplied after resume: %v", id, merr)
	}
	return nodeID, nil
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
	if s.intentEnabled {
		targetNodeID, pickErr := s.sched.PickNodeID(placement)
		if pickErr != nil {
			if serr := s.st.Spawns().SetError(ctx, req.Msg.SpawnId); serr != nil {
				log.Printf("RecreateSpawn %s: SetError after PickNodeID failure also failed: %v", req.Msg.SpawnId, serr)
			}
			return nil, pickErr
		}
		mounts, _ := s.st.Spawns().GetMounts(ctx, req.Msg.SpawnId)
		pi := buildPendingIntent(intent.OpRecreateSpawn, req.Msg.SpawnId, uint64(gen), targetNodeID, sp.Image, sp.AppRef, sp.Model, "", mounts)
		ch := s.pendingIntents.register(req.Msg.SpawnId, owner, pi)
		defer s.pendingIntents.cleanup(req.Msg.SpawnId)
		var awaitErr error
		env, awaitErr = s.pendingIntents.await(ctx, ch)
		if awaitErr != nil {
			if serr := s.st.Spawns().SetError(ctx, req.Msg.SpawnId); serr != nil {
				log.Printf("RecreateSpawn %s: SetError after await failure also failed: %v", req.Msg.SpawnId, serr)
			}
			return nil, connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("await SignedIntent: %w", awaitErr))
		}
		placement.TargetNodeID = targetNodeID
	}

	nodeID, err := s.sched.Provision(ctx, req.Msg.SpawnId, sp.AppRef, sp.Model, sp.Name, sp.AppID, sp.RunnableID, sp.Mode, uint64(gen), placement, env, sp.BaseImageDigest, nil)
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
		out[i] = &cpv1.SpawnSummary{
			SpawnId: sp.ID, AppId: sp.AppID, AppVersion: sp.AppVersion, Model: sp.Model,
			Status: toSummaryStatus(sp.Status), CreatedAt: sp.CreatedAt, LastUsedAt: sp.LastUsedAt,
			Name: sp.Name, Mode: sp.Mode, ModelApplied: sp.ModelApplied,
			Generation: gen, JournalKeyDeliveryPending: s.deliveryPending.isPending(sp.ID),
		}
	}
	return connect.NewResponse(&cpv1.ListSpawnsResponse{Spawns: out}), nil
}
