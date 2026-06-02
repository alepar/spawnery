package cp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
	"spawnery/internal/cp/telemetry"
)

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
	owner, _ := auth.OwnerFromContext(ctx)
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

// SuspendSpawn tears down the running container but keeps the spawn row in 'suspended' status
// (resumable). Non-lossless: in-container working state and agent memory are NOT preserved (lossless
// suspend is gated on E3). active -> suspending -> (node teardown) -> suspended.
func (s *Server) SuspendSpawn(ctx context.Context, req *connect.Request[cpv1.SuspendSpawnRequest]) (*connect.Response[cpv1.SuspendSpawnResponse], error) {
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
	if sp.Status != store.Active {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("spawn is not active"))
	}
	c, hasLive, err := s.st.Spawns().LiveContainer(ctx, req.Msg.SpawnId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !hasLive {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("no live container"))
	}
	gen := c.Generation
	if err := s.st.Spawns().SetSuspending(ctx, req.Msg.SpawnId, gen); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Tear down the container on the node + drop the route, then finalize suspended.
	s.rt.StopOnNode(req.Msg.SpawnId)
	s.rt.Drop(req.Msg.SpawnId)
	if err := s.st.Spawns().SetSuspended(ctx, req.Msg.SpawnId, gen); err != nil {
		// The container was already torn down above; we couldn't record 'suspended'. Compensate to a
		// terminal 'error' state — MarkBootUnreachable doesn't sweep 'suspending', so the spawn would
		// otherwise be stranded. Mirrors CreateSpawn's SetError-on-failure path.
		if serr := s.st.Spawns().SetError(ctx, req.Msg.SpawnId); serr != nil {
			log.Printf("SuspendSpawn %s: SetError after SetSuspended failure also failed: %v", req.Msg.SpawnId, serr)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	_ = s.tel.Emit(telemetry.Event{Kind: "session_end", Owner: owner, SpawnID: req.Msg.SpawnId, Timestamp: time.Now().UTC()})
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
	nodeID, err := s.sched.Provision(ctx, req.Msg.SpawnId, sp.AppRef, sp.Model, placement)
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
	return connect.NewResponse(&cpv1.ResumeSpawnResponse{}), nil
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
			Name: sp.Name,
		}
	}
	return connect.NewResponse(&cpv1.ListSpawnsResponse{Spawns: out}), nil
}
