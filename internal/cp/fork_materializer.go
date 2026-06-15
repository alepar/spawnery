package cp

import (
	"context"
	"fmt"
	"sync"
	"time"

	"connectrpc.com/connect"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/store"
)

const defaultForkMaterializeTimeout = defaultSuspendTimeout

type forkWaiters struct {
	mu sync.Mutex
	m  map[string]chan *nodev1.ForkSameNodeComplete
}

func newForkWaiters() *forkWaiters {
	return &forkWaiters{m: map[string]chan *nodev1.ForkSameNodeComplete{}}
}

func (w *forkWaiters) register(transferSetID string) chan *nodev1.ForkSameNodeComplete {
	ch := make(chan *nodev1.ForkSameNodeComplete, 1)
	w.mu.Lock()
	w.m[transferSetID] = ch
	w.mu.Unlock()
	return ch
}

func (w *forkWaiters) unregister(transferSetID string) {
	w.mu.Lock()
	delete(w.m, transferSetID)
	w.mu.Unlock()
}

func (w *forkWaiters) deliver(msg *nodev1.ForkSameNodeComplete) bool {
	w.mu.Lock()
	ch, ok := w.m[msg.GetTransferSetId()]
	w.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- msg:
		return true
	default:
		return false
	}
}

func (s *Server) deliverForkSameNodeComplete(msg *nodev1.ForkSameNodeComplete) bool {
	if s.forks == nil {
		return false
	}
	return s.forks.deliver(msg)
}

type sameNodeForkMaterializer struct {
	s       *Server
	timeout time.Duration
}

func newSameNodeForkMaterializer(s *Server, timeout time.Duration) forkMaterializer {
	if timeout == 0 {
		timeout = defaultForkMaterializeTimeout
	}
	return &sameNodeForkMaterializer{s: s, timeout: timeout}
}

func (m *sameNodeForkMaterializer) MaterializeFork(ctx context.Context, req forkMaterializeRequest) (forkMaterializeResult, error) {
	if req.SourceNodeID != req.TargetNodeID {
		return forkMaterializeResult{}, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("cross-node fork materialization is not implemented in this slice"))
	}
	n, ok := m.s.reg.Get(req.SourceNodeID)
	if !ok || n.Sender == nil {
		return forkMaterializeResult{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("source node %q is not connected", req.SourceNodeID))
	}
	if m.s.forks == nil {
		m.s.forks = newForkWaiters()
	}
	ch := m.s.forks.register(req.TransferSetID)
	defer m.s.forks.unregister(req.TransferSetID)

	if err := n.Sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkSameNode{ForkSameNode: &nodev1.ForkSameNode{
		SourceSpawnId:    req.SourceSpawn.ID,
		ForkSpawnId:      req.ForkSpawn.ID,
		SourceGeneration: req.SourceGeneration,
		TargetGeneration: req.TargetGeneration,
		TransferSetId:    req.TransferSetID,
	}}}); err != nil {
		return forkMaterializeResult{}, connect.NewError(connect.CodeUnavailable, fmt.Errorf("send fork command to node %q: %w", req.SourceNodeID, err))
	}

	timer := time.NewTimer(m.timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return forkMaterializeResult{}, ctx.Err()
	case <-timer.C:
		return forkMaterializeResult{}, connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("timed out waiting for same-node fork %s", req.TransferSetID))
	case msg := <-ch:
		return forkResultFromComplete(msg, req)
	}
}

func forkResultFromComplete(msg *nodev1.ForkSameNodeComplete, req forkMaterializeRequest) (forkMaterializeResult, error) {
	if msg.GetError() != "" {
		return forkMaterializeResult{}, connect.NewError(connect.CodeInternal, fmt.Errorf("node fork failed: %s", msg.GetError()))
	}
	if msg.GetSourceSpawnId() != req.SourceSpawn.ID || msg.GetForkSpawnId() != req.ForkSpawn.ID {
		return forkMaterializeResult{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("fork completion ids do not match request"))
	}
	mountPins := mountPinsFromForkComplete(msg.GetMounts())
	rootfsPins, err := rootfsPinsFromForkComplete(msg, req.TargetGeneration, req.SourceSpawn.BaseImageDigest)
	if err != nil {
		return forkMaterializeResult{}, err
	}
	nodeID := msg.GetNodeId()
	if nodeID == "" {
		nodeID = req.TargetNodeID
	}
	return forkMaterializeResult{NodeID: nodeID, MountPins: mountPins, RootfsPins: rootfsPins}, nil
}

func mountPinsFromForkComplete(markers []*nodev1.MountMarker) map[string]string {
	if len(markers) == 0 {
		return nil
	}
	out := make(map[string]string, len(markers))
	for _, marker := range markers {
		if marker.GetName() != "" && marker.GetMarker() != "" {
			out[marker.GetName()] = marker.GetMarker()
		}
	}
	return out
}

func rootfsPinsFromForkComplete(msg *nodev1.ForkSameNodeComplete, targetGeneration uint64, baseImageDigest string) ([]store.RootfsArtifactPin, error) {
	if msg == nil || len(msg.GetRootfsArtifacts()) == 0 {
		return nil, nil
	}
	pins := make([]store.RootfsArtifactPin, 0, len(msg.GetRootfsArtifacts()))
	for _, art := range msg.GetRootfsArtifacts() {
		if art.GetArtifactId() == "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("fork rootfs artifact restore pin is missing artifact id"))
		}
		if art.GetGeneration() != targetGeneration {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("fork rootfs artifact %s generation %d does not match target generation %d",
				art.GetArtifactId(), art.GetGeneration(), targetGeneration))
		}
		if baseImageDigest != "" && art.GetBaseImageDigest() != "" && art.GetBaseImageDigest() != baseImageDigest {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("fork rootfs artifact %s base digest %s does not match pinned base digest %s",
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
