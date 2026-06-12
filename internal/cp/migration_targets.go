package cp

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
)

// ListMigrationTargets returns the owner-eligible nodes for a spawn migration: the owner's own
// self-hosted nodes + all cloud nodes. TargetEligible (single-node pre-suspend gate) is checked
// separately by MigrateSpawn, so the picker is advisory [WL8].
//
// journal_size_bytes is always 0 in this implementation — per-mount journal size stats surface
// via a separate Kopia bead (sp-e642).
func (s *Server) ListMigrationTargets(ctx context.Context, req *connect.Request[cpv1.ListMigrationTargetsRequest]) (*connect.Response[cpv1.ListMigrationTargetsResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	sp, err := s.st.Spawns().Get(ctx, req.Msg.SpawnId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if sp.OwnerID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}

	// Get the spawn's current hosting node so we can mark is_current.
	var currentNodeID string
	if c, ok, err := s.st.Spawns().LiveContainer(ctx, req.Msg.SpawnId); err == nil && ok {
		currentNodeID = c.NodeID
	}

	infos := s.reg.EligibleTargets(sp.OwnerID)
	targets := make([]*cpv1.MigrationTarget, 0, len(infos))
	for _, info := range infos {
		targets = append(targets, &cpv1.MigrationTarget{
			NodeId:    info.ID,
			Class:     info.Class,
			Yours:     info.Class == "self-hosted",
			Online:    info.Online,
			IsCurrent: info.ID == currentNodeID,
			// journal_size_bytes: TODO(sp-e642): wire Kopia journal stats when available.
			JournalSizeBytes: 0,
		})
	}
	return connect.NewResponse(&cpv1.ListMigrationTargetsResponse{Targets: targets}), nil
}
