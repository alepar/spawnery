package node

import (
	"context"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/spawnlet"
)

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
