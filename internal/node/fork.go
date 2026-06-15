package node

import (
	"context"
	"fmt"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/spawnlet"
)

const forkTurnBoundaryPoll = 10 * time.Millisecond

type forkIngressBarrier struct {
	sourceGeneration uint64
	transferSetID    string
}

func (b forkIngressBarrier) matches(other forkIngressBarrier) bool {
	return b.sourceGeneration == other.sourceGeneration && b.transferSetID == other.transferSetID
}

func (a *attacher) forkSameNode(ctx context.Context, m *nodev1.ForkSameNode) {
	defer a.releaseForkBarrier(m.GetSourceSpawnId(), func(b forkIngressBarrier) bool {
		return b.matches(forkIngressBarrier{sourceGeneration: m.GetSourceGeneration(), transferSetID: m.GetTransferSetId()})
	})
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
	acquired := false
	if err := a.acquireForkBarrier(ctx, m.GetSourceSpawnId(), m.GetSourceGeneration(), m.GetTransferSetId()); err != nil {
		logErr("forkTurnBoundary "+m.GetSourceSpawnId(), err)
		reply.Error = err.Error()
	} else {
		acquired = true
	}
	if err := a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_ForkTurnBoundaryComplete{ForkTurnBoundaryComplete: reply}}); err != nil && acquired {
		a.releaseForkBarrier(m.GetSourceSpawnId(), func(b forkIngressBarrier) bool {
			return b.matches(forkIngressBarrier{sourceGeneration: m.GetSourceGeneration(), transferSetID: m.GetTransferSetId()})
		})
	}
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
	a.releaseForkBarrier(m.GetSpawnId(), func(b forkIngressBarrier) bool {
		return b.sourceGeneration == m.GetGeneration()
	})
	_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_UnpauseIfPausedComplete{UnpauseIfPausedComplete: reply}})
}

func (a *attacher) acquireForkBarrier(ctx context.Context, spawnID string, sourceGeneration uint64, transferSetID string) error {
	if transferSetID == "" {
		return fmt.Errorf("fork turn-boundary gate unavailable: missing transfer set")
	}
	barrier := forkIngressBarrier{sourceGeneration: sourceGeneration, transferSetID: transferSetID}
	poll := forkTurnBoundaryPoll
	if poll <= 0 {
		poll = 10 * time.Millisecond
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		err, acquired := a.tryAcquireForkBarrier(spawnID, barrier)
		if err != nil {
			return err
		}
		if acquired {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (a *attacher) tryAcquireForkBarrier(spawnID string, barrier forkIngressBarrier) (error, bool) {
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
	if hasRelay {
		a.mu.Unlock()
		return fmt.Errorf("fork turn-boundary gate unavailable for non-ACP source"), false
	}
	if len(pumps) == 0 {
		a.mu.Unlock()
		return fmt.Errorf("fork turn-boundary gate unavailable: no observable ACP pump"), false
	}
	var acquired []*Pump
	for _, p := range pumps {
		if !p.tryAcquireForkBarrier(barrier) {
			for _, held := range acquired {
				held.releaseForkBarrier(func(b forkIngressBarrier) bool { return b.matches(barrier) })
			}
			a.mu.Unlock()
			return nil, false
		}
		acquired = append(acquired, p)
	}
	a.ensureForkBarriersLocked()
	a.forkBarriers[spawnID] = barrier
	a.mu.Unlock()
	return nil, true
}

func (a *attacher) ensureForkBarriersLocked() {
	if a.forkBarriers == nil {
		a.forkBarriers = map[string]forkIngressBarrier{}
	}
}

func (a *attacher) applyForkBarrierLocked(spawnID string, p *Pump) {
	if a.forkBarriers == nil || p == nil {
		return
	}
	if b, ok := a.forkBarriers[spawnID]; ok {
		p.setForkBarrier(b)
	}
}

func (a *attacher) releaseForkBarrier(spawnID string, match func(forkIngressBarrier) bool) {
	a.mu.Lock()
	var releasePumps []*Pump
	if a.forkBarriers != nil {
		if b, ok := a.forkBarriers[spawnID]; ok && match(b) {
			delete(a.forkBarriers, spawnID)
			for key, p := range a.pumps {
				if key.spawnID == spawnID {
					releasePumps = append(releasePumps, p)
				}
			}
		}
	}
	a.mu.Unlock()
	for _, p := range releasePumps {
		p.releaseForkBarrier(match)
	}
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
