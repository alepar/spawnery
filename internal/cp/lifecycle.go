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

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
	"spawnery/internal/cp/telemetry"
)

// defaultSuspendTimeout bounds how long SuspendSpawn waits for the hosting node's SuspendComplete
// after asking it to persist+tear down. A real journal final snapshot can take a while, so this is
// generous; on expiry the spawn is moved to 'error' (design §5: "persist failure → error").
const defaultSuspendTimeout = 30 * time.Second

// suspendWaiters correlates a SuspendSpawn with the async SuspendComplete the hosting node sends back
// on its Attach stream, carrying the per-mount persist markers. Keyed by spawn_id: the per-spawn lock
// SuspendSpawn holds serializes suspends, so at most one waiter per spawn is ever live. The waiter
// records the episode generation it expects; deliver drops a SuspendComplete whose generation differs
// (a stale-episode reply from a superseded pod), mirroring the node-side generation fence.
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
// the spawn row 'suspended' (resumable) with the per-mount persist markers recorded. Marker-protocol
// flow (sp-a7fs): decide-in-DB (SetSuspending, generation-fenced) → ask the node to persist+tear down
// (SuspendOnNode) → await SuspendComplete with a bounded timeout → record the markers + finalize
// 'suspended'. On timeout/await-failure the spawn is moved to 'error' (design §5: "persist failure →
// error"). active -> suspending -> (node persist + teardown) -> suspended.
func (s *Server) SuspendSpawn(ctx context.Context, req *connect.Request[cpv1.SuspendSpawnRequest]) (*connect.Response[cpv1.SuspendSpawnResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	id := req.Msg.SpawnId
	unlock := s.locks.Lock(id)
	defer unlock()
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
	// Decide-in-DB FIRST (generation-fenced): a recreate/stop racing in concurrently fences this out.
	if err := s.st.Spawns().SetSuspending(ctx, id, gen); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Register the marker waiter BEFORE asking the node to suspend, so a fast SuspendComplete is never
	// missed. The node persists each mount, tears the pod down, and replies with the per-mount markers.
	ch := s.suspends.register(id, uint64(gen))
	defer s.suspends.unregister(id)
	s.rt.SuspendOnNode(id, uint64(gen))

	wait, cancel := context.WithTimeout(ctx, s.suspendTimeout)
	defer cancel()
	// Post-decision store writes use a detached ctx so the suspend outcome survives a client disconnect.
	storeCtx := context.WithoutCancel(ctx)
	select {
	case sc := <-ch:
		// Record the per-mount persist markers (design §5: markers recorded incrementally) before
		// finalizing. A marker write failure is logged, not fatal — losing a marker degrades a later
		// resume to the seeded scratch dir, which is strictly better than failing the whole suspend.
		for _, mk := range sc.GetMarkers() {
			if merr := s.st.Spawns().SetMountMarker(storeCtx, id, mk.GetName(), mk.GetMarker()); merr != nil {
				log.Printf("SuspendSpawn %s: SetMountMarker(%s): %v", id, mk.GetName(), merr)
			}
		}
		s.rt.Drop(id)
		if err := s.st.Spawns().SetSuspended(storeCtx, id, gen); err != nil {
			// The pod is already torn down but we couldn't record 'suspended'. Compensate to a terminal
			// 'error' (immediately recreate-able), mirroring CreateSpawn's SetError-on-failure path.
			if serr := s.st.Spawns().SetError(storeCtx, id); serr != nil {
				log.Printf("SuspendSpawn %s: SetError after SetSuspended failure also failed: %v", id, serr)
			}
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	case <-wait.Done():
		// Persist did not complete in time (slow/wedged/unreachable node). Per design §5 ("persist
		// failure → error"), move the spawn to a terminal 'error' rather than leaving it stuck in
		// 'suspending'. Drop the route + best-effort fence: SetError ends the live container row, so a
		// later heartbeat's inventory orphan arm tells the node to destroy any pod that did tear down.
		s.rt.Drop(id)
		if serr := s.st.Spawns().SetError(storeCtx, id); serr != nil {
			log.Printf("SuspendSpawn %s: SetError after suspend await timeout also failed: %v", id, serr)
		}
		return nil, connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("timed out awaiting node suspend"))
	}
	_ = s.tel.Emit(telemetry.Event{Kind: "session_end", Owner: owner, SpawnID: id, Timestamp: time.Now().UTC()})
	return connect.NewResponse(&cpv1.SuspendSpawnResponse{}), nil
}

// ResumeSpawn provisions a FRESH container for a suspended spawn (non-lossless — a brand-new
// container, no prior in-container state). suspended -> starting (new generation) -> active. Reuses
// the same scheduler.Provision + SetActive path as CreateSpawn, with the same orphan-window
// compensation on failure.
func (s *Server) ResumeSpawn(ctx context.Context, req *connect.Request[cpv1.ResumeSpawnRequest]) (*connect.Response[cpv1.ResumeSpawnResponse], error) {
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
	if sp.Status != store.Suspended {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("spawn is not suspended"))
	}
	// Placement is re-evaluated against the version's CURRENT tier at resume time (a fresh container
	// is being allocated). If the version was reviewed at create time but has since been downgraded to
	// unverified, placementFor will block a non-author resume — intentional: routing policy must hold
	// for every new container, not only the first.
	ver, err := s.st.Apps().GetVersion(ctx, sp.AppID, sp.AppVersion)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	placement, err := s.placementFor(ctx, owner, sp.AppID, ver)
	if err != nil {
		return nil, err
	}
	var gen int64
	if err := s.st.WithTx(ctx, func(tx store.Store) error {
		g, e := tx.Spawns().ClaimStarting(ctx, req.Msg.SpawnId, []store.Status{store.Suspended})
		gen = g
		return e
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	placement.Image = sp.Image
	nodeID, err := s.sched.Provision(ctx, req.Msg.SpawnId, sp.AppRef, sp.Model, sp.Name, sp.AppID, sp.RunnableID, sp.Mode, uint64(gen), placement)
	if err != nil {
		if serr := s.st.Spawns().SetError(ctx, req.Msg.SpawnId); serr != nil {
			log.Printf("ResumeSpawn %s: SetError after provision failure also failed: %v", req.Msg.SpawnId, serr)
		}
		return nil, err
	}
	if err := s.st.Spawns().SetActive(ctx, req.Msg.SpawnId, nodeID, gen); err != nil {
		s.rt.StopOnNode(req.Msg.SpawnId)
		s.rt.Drop(req.Msg.SpawnId)
		if serr := s.st.Spawns().SetError(ctx, req.Msg.SpawnId); serr != nil {
			log.Printf("ResumeSpawn %s: SetError after SetActive failure also failed: %v", req.Msg.SpawnId, serr)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Fresh container started with spawns.model -> mark applied (resolves any pending/given-up switch).
	if merr := s.st.Spawns().MarkModelApplied(context.WithoutCancel(ctx), req.Msg.SpawnId); merr != nil {
		log.Printf("ResumeSpawn %s: MarkModelApplied after resume: %v", req.Msg.SpawnId, merr)
	}
	return connect.NewResponse(&cpv1.ResumeSpawnResponse{}), nil
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
	// Fence the old container: if its node has returned, tell it to stop + drop the route. The new
	// generation (below) is the durable fence; this is the best-effort eager teardown.
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
	nodeID, err := s.sched.Provision(ctx, req.Msg.SpawnId, sp.AppRef, sp.Model, sp.Name, sp.AppID, sp.RunnableID, sp.Mode, uint64(gen), placement)
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
	// Fresh container started with spawns.model -> mark applied (resolves any pending/given-up switch).
	if merr := s.st.Spawns().MarkModelApplied(context.WithoutCancel(ctx), req.Msg.SpawnId); merr != nil {
		log.Printf("RecreateSpawn %s: MarkModelApplied after recreate: %v", req.Msg.SpawnId, merr)
	}
	// Record that this spawn went through a user-driven recovery. Best-effort bookkeeping — the
	// recreate itself already succeeded, so log-don't-fail (mirrors MarkModelApplied above).
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
		out[i] = &cpv1.SpawnSummary{
			SpawnId: sp.ID, AppId: sp.AppID, AppVersion: sp.AppVersion, Model: sp.Model,
			Status: toSummaryStatus(sp.Status), CreatedAt: sp.CreatedAt, LastUsedAt: sp.LastUsedAt,
			Name: sp.Name, Mode: sp.Mode, ModelApplied: sp.ModelApplied,
		}
	}
	return connect.NewResponse(&cpv1.ListSpawnsResponse{Spawns: out}), nil
}
