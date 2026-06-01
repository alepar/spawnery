package cp

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

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
		}
	}
	return connect.NewResponse(&cpv1.ListSpawnsResponse{Spawns: out}), nil
}
