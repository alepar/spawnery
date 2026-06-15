package node

import (
	"context"
	"fmt"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/spawnlet"
)

const forkTurnBoundaryPoll = 10 * time.Millisecond

func (a *attacher) forkSameNode(ctx context.Context, m *nodev1.ForkSameNode) {
	res, err := a.mgr.ForkSameNode(ctx, spawnlet.ForkSameNodeRequest{
		SourceSpawnID:    m.GetSourceSpawnId(),
		ForkSpawnID:      m.GetForkSpawnId(),
		TransferSetID:    m.GetTransferSetId(),
		SourceGeneration: m.GetSourceGeneration(),
		TargetGeneration: m.GetTargetGeneration(),
	})
	reply := &nodev1.ForkSameNodeComplete{
		SourceSpawnId: m.GetSourceSpawnId(),
		ForkSpawnId:   m.GetForkSpawnId(),
		TransferSetId: m.GetTransferSetId(),
		NodeId:        res.NodeID,
	}
	if err != nil {
		logErr("forkSameNode "+m.GetForkSpawnId(), err)
		reply.Error = err.Error()
	} else {
		reply.Mounts = mountPinsToProto(res.MountPins)
		reply.RootfsArtifacts = rootfsArtifactsToProto(res.RootfsArtifacts)
	}
	_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_ForkSameNodeComplete{ForkSameNodeComplete: reply}})
}

func (a *attacher) forkTurnBoundary(ctx context.Context, m *nodev1.ForkTurnBoundary) {
	reply := &nodev1.ForkTurnBoundaryComplete{
		SourceSpawnId:    m.GetSourceSpawnId(),
		SourceGeneration: m.GetSourceGeneration(),
		TransferSetId:    m.GetTransferSetId(),
	}
	if err := a.waitSpawnIdle(ctx, m.GetSourceSpawnId()); err != nil {
		logErr("forkTurnBoundary "+m.GetSourceSpawnId(), err)
		reply.Error = err.Error()
	}
	_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_ForkTurnBoundaryComplete{ForkTurnBoundaryComplete: reply}})
}

func (a *attacher) unpauseIfPaused(ctx context.Context, m *nodev1.UnpauseIfPaused) {
	reply := &nodev1.UnpauseIfPausedComplete{
		SpawnId:    m.GetSpawnId(),
		Generation: m.GetGeneration(),
	}
	if err := a.mgr.UnpauseIfPaused(ctx, m.GetSpawnId(), int64(m.GetGeneration())); err != nil {
		logErr("unpauseIfPaused "+m.GetSpawnId(), err)
		reply.Error = err.Error()
	}
	_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_UnpauseIfPausedComplete{UnpauseIfPausedComplete: reply}})
}

func (a *attacher) waitSpawnIdle(ctx context.Context, spawnID string) error {
	a.mu.Lock()
	var pumps []*Pump
	for key, p := range a.pumps {
		if key.spawnID == spawnID {
			pumps = append(pumps, p)
		}
	}
	hasRelay := false
	for key := range a.tmuxRelays {
		if key.spawnID == spawnID {
			hasRelay = true
			break
		}
	}
	a.mu.Unlock()
	if len(pumps) == 0 {
		if hasRelay {
			return fmt.Errorf("fork turn-boundary gate unavailable for non-ACP source")
		}
		return fmt.Errorf("fork turn-boundary gate unavailable: no observable ACP pump")
	}
	for _, p := range pumps {
		if err := p.waitIdle(ctx, forkTurnBoundaryPoll); err != nil {
			return err
		}
	}
	return nil
}

func mountPinsToProto(in map[string]string) []*nodev1.MountMarker {
	if len(in) == 0 {
		return nil
	}
	out := make([]*nodev1.MountMarker, 0, len(in))
	for name, marker := range in {
		out = append(out, &nodev1.MountMarker{Name: name, Marker: marker})
	}
	return out
}
