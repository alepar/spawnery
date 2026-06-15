package cp

import (
	"context"
	"fmt"
	"log"
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

type forkSourceRestoredWaiters struct {
	mu sync.Mutex
	m  map[string]chan *nodev1.ForkSourceRestored
}

type forkTransferExportedWaiters struct {
	mu sync.Mutex
	m  map[string]chan *nodev1.ForkTransferExported
}

type forkTransferImportedWaiters struct {
	mu sync.Mutex
	m  map[string]chan *nodev1.ForkTransferImported
}

func newForkWaiters() *forkWaiters {
	return &forkWaiters{m: map[string]chan *nodev1.ForkSameNodeComplete{}}
}

func newForkSourceRestoredWaiters() *forkSourceRestoredWaiters {
	return &forkSourceRestoredWaiters{m: map[string]chan *nodev1.ForkSourceRestored{}}
}

func newForkTransferExportedWaiters() *forkTransferExportedWaiters {
	return &forkTransferExportedWaiters{m: map[string]chan *nodev1.ForkTransferExported{}}
}

func newForkTransferImportedWaiters() *forkTransferImportedWaiters {
	return &forkTransferImportedWaiters{m: map[string]chan *nodev1.ForkTransferImported{}}
}

type forkTurnBoundaryWaiters struct {
	mu sync.Mutex
	m  map[string]chan *nodev1.ForkTurnBoundaryComplete
}

func newForkTurnBoundaryWaiters() *forkTurnBoundaryWaiters {
	return &forkTurnBoundaryWaiters{m: map[string]chan *nodev1.ForkTurnBoundaryComplete{}}
}

func (w *forkTurnBoundaryWaiters) register(transferSetID string) chan *nodev1.ForkTurnBoundaryComplete {
	ch := make(chan *nodev1.ForkTurnBoundaryComplete, 1)
	w.mu.Lock()
	w.m[transferSetID] = ch
	w.mu.Unlock()
	return ch
}

func (w *forkTurnBoundaryWaiters) unregister(transferSetID string) {
	w.mu.Lock()
	delete(w.m, transferSetID)
	w.mu.Unlock()
}

func (w *forkTurnBoundaryWaiters) deliver(msg *nodev1.ForkTurnBoundaryComplete) bool {
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

func (s *Server) deliverForkTurnBoundaryComplete(msg *nodev1.ForkTurnBoundaryComplete) bool {
	if s.forkTurnBoundaries == nil {
		return false
	}
	return s.forkTurnBoundaries.deliver(msg)
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

func (w *forkSourceRestoredWaiters) register(transferSetID string) chan *nodev1.ForkSourceRestored {
	ch := make(chan *nodev1.ForkSourceRestored, 1)
	w.mu.Lock()
	w.m[transferSetID] = ch
	w.mu.Unlock()
	return ch
}

func (w *forkSourceRestoredWaiters) unregister(transferSetID string) {
	w.mu.Lock()
	delete(w.m, transferSetID)
	w.mu.Unlock()
}

func (w *forkSourceRestoredWaiters) deliver(msg *nodev1.ForkSourceRestored) bool {
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

func (s *Server) deliverForkSourceRestored(msg *nodev1.ForkSourceRestored) bool {
	if s.forkSourceRestored == nil {
		return false
	}
	return s.forkSourceRestored.deliver(msg)
}

func (w *forkTransferExportedWaiters) register(transferSetID string) chan *nodev1.ForkTransferExported {
	ch := make(chan *nodev1.ForkTransferExported, 1)
	w.mu.Lock()
	w.m[transferSetID] = ch
	w.mu.Unlock()
	return ch
}

func (w *forkTransferExportedWaiters) unregister(transferSetID string) {
	w.mu.Lock()
	delete(w.m, transferSetID)
	w.mu.Unlock()
}

func (w *forkTransferExportedWaiters) deliver(msg *nodev1.ForkTransferExported) bool {
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

func (s *Server) deliverForkTransferExported(msg *nodev1.ForkTransferExported) bool {
	if s.forkTransferExports == nil {
		return false
	}
	return s.forkTransferExports.deliver(msg)
}

func (w *forkTransferImportedWaiters) register(transferSetID string) chan *nodev1.ForkTransferImported {
	ch := make(chan *nodev1.ForkTransferImported, 1)
	w.mu.Lock()
	w.m[transferSetID] = ch
	w.mu.Unlock()
	return ch
}

func (w *forkTransferImportedWaiters) unregister(transferSetID string) {
	w.mu.Lock()
	delete(w.m, transferSetID)
	w.mu.Unlock()
}

func (w *forkTransferImportedWaiters) deliver(msg *nodev1.ForkTransferImported) bool {
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

func (s *Server) deliverForkTransferImported(msg *nodev1.ForkTransferImported) bool {
	if s.forkTransferImports == nil {
		return false
	}
	return s.forkTransferImports.deliver(msg)
}

type sameNodeForkMaterializer struct {
	s       *Server
	timeout time.Duration
}

type forkTurnBoundaryWaiter interface {
	WaitForForkTurnBoundary(context.Context, forkMaterializeRequest) error
}

type forkTurnBoundaryReleaser interface {
	ReleaseForkTurnBoundary(context.Context, forkMaterializeRequest) error
}

type forkSourceRestoredMaterializer interface {
	MaterializeForkWithSourceRestored(context.Context, forkMaterializeRequest, func() error) (forkMaterializeResult, error)
}

func (s *Server) materializeForkWithSourceRestored(ctx context.Context, req forkMaterializeRequest, onSourceRestored func() error) (forkMaterializeResult, error) {
	m := s.forkMaterializerOrDefault()
	if sr, ok := m.(forkSourceRestoredMaterializer); ok {
		return sr.MaterializeForkWithSourceRestored(ctx, req, onSourceRestored)
	}
	result, err := m.MaterializeFork(ctx, req)
	if err != nil {
		return forkMaterializeResult{}, err
	}
	if onSourceRestored != nil {
		if err := onSourceRestored(); err != nil {
			return forkMaterializeResult{}, err
		}
	}
	return result, nil
}

func (s *Server) waitForkTurnBoundary(ctx context.Context, req forkMaterializeRequest) error {
	waiter, ok := s.forkMaterializerOrDefault().(forkTurnBoundaryWaiter)
	if !ok {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("fork materializer cannot gate source turn boundary"))
	}
	return waiter.WaitForForkTurnBoundary(ctx, req)
}

func (s *Server) releaseForkTurnBoundary(ctx context.Context, req forkMaterializeRequest) {
	releaser, ok := s.forkMaterializerOrDefault().(forkTurnBoundaryReleaser)
	if !ok {
		return
	}
	if err := releaser.ReleaseForkTurnBoundary(ctx, req); err != nil {
		log.Printf("release fork turn-boundary %s: %v", req.TransferSetID, err)
	}
}

func newSameNodeForkMaterializer(s *Server, timeout time.Duration) forkMaterializer {
	if timeout == 0 {
		timeout = defaultForkMaterializeTimeout
	}
	return &sameNodeForkMaterializer{s: s, timeout: timeout}
}

func (m *sameNodeForkMaterializer) WaitForForkTurnBoundary(ctx context.Context, req forkMaterializeRequest) (err error) {
	n, ok := m.s.reg.Get(req.SourceNodeID)
	if !ok || n.Sender == nil {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("source node %q is not connected", req.SourceNodeID))
	}
	if m.s.forkTurnBoundaries == nil {
		m.s.forkTurnBoundaries = newForkTurnBoundaryWaiters()
	}
	ch := m.s.forkTurnBoundaries.register(req.TransferSetID)
	defer m.s.forkTurnBoundaries.unregister(req.TransferSetID)

	sent := false
	defer func() {
		if err != nil && sent {
			_ = m.ReleaseForkTurnBoundary(context.WithoutCancel(ctx), req)
		}
	}()
	if err := n.Sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTurnBoundary{ForkTurnBoundary: &nodev1.ForkTurnBoundary{
		SourceSpawnId:    req.SourceSpawn.ID,
		SourceGeneration: req.SourceGeneration,
		TransferSetId:    req.TransferSetID,
	}}}); err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("send fork turn-boundary command to node %q: %w", req.SourceNodeID, err))
	}
	sent = true

	timer := time.NewTimer(m.timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("timed out waiting for fork turn boundary %s", req.TransferSetID))
	case msg := <-ch:
		if msg.GetError() != "" {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("fork turn-boundary gate failed: %s", msg.GetError()))
		}
		if msg.GetSourceSpawnId() != req.SourceSpawn.ID || msg.GetTransferSetId() != req.TransferSetID {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("fork turn-boundary completion ids do not match request"))
		}
		return nil
	}
}

func (m *sameNodeForkMaterializer) ReleaseForkTurnBoundary(ctx context.Context, req forkMaterializeRequest) error {
	n, ok := m.s.reg.Get(req.SourceNodeID)
	if !ok || n.Sender == nil {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("source node %q is not connected", req.SourceNodeID))
	}
	if err := n.Sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_ReleaseForkTurnBoundary{ReleaseForkTurnBoundary: &nodev1.ReleaseForkTurnBoundary{
		SourceSpawnId:    req.SourceSpawn.ID,
		SourceGeneration: req.SourceGeneration,
		TransferSetId:    req.TransferSetID,
	}}}); err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("send fork turn-boundary release to node %q: %w", req.SourceNodeID, err))
	}
	return nil
}

func (m *sameNodeForkMaterializer) MaterializeFork(ctx context.Context, req forkMaterializeRequest) (forkMaterializeResult, error) {
	return m.MaterializeForkWithSourceRestored(ctx, req, nil)
}

func (m *sameNodeForkMaterializer) cancelForkSameNode(req forkMaterializeRequest) {
	n, ok := m.s.reg.Get(req.SourceNodeID)
	if !ok || n.Sender == nil {
		return
	}
	if err := n.Sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_CancelForkSameNode{CancelForkSameNode: &nodev1.CancelForkSameNode{
		SourceSpawnId: req.SourceSpawn.ID,
		ForkSpawnId:   req.ForkSpawn.ID,
		TransferSetId: req.TransferSetID,
	}}}); err != nil {
		log.Printf("cancel same-node fork %s on node %s: %v", req.TransferSetID, req.SourceNodeID, err)
	}
}

func (m *sameNodeForkMaterializer) MaterializeForkWithSourceRestored(ctx context.Context, req forkMaterializeRequest, onSourceRestored func() error) (forkMaterializeResult, error) {
	if req.SourceNodeID != req.TargetNodeID {
		return m.materializeCrossNodeForkWithSourceRestored(ctx, req, onSourceRestored)
	}
	n, ok := m.s.reg.Get(req.SourceNodeID)
	if !ok || n.Sender == nil {
		return forkMaterializeResult{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("source node %q is not connected", req.SourceNodeID))
	}
	if m.s.forks == nil {
		m.s.forks = newForkWaiters()
	}
	if m.s.forkSourceRestored == nil {
		m.s.forkSourceRestored = newForkSourceRestoredWaiters()
	}
	ch := m.s.forks.register(req.TransferSetID)
	defer m.s.forks.unregister(req.TransferSetID)
	restoredCh := m.s.forkSourceRestored.register(req.TransferSetID)
	defer m.s.forkSourceRestored.unregister(req.TransferSetID)

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
	sourceRestored := false
	handleSourceRestored := func(msg *nodev1.ForkSourceRestored) error {
		if msg.GetError() != "" {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("node fork source restore failed: %s", msg.GetError()))
		}
		if msg.GetSourceSpawnId() != req.SourceSpawn.ID || msg.GetSourceGeneration() != req.SourceGeneration || msg.GetTransferSetId() != req.TransferSetID {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("fork source-restored ids do not match request"))
		}
		if !sourceRestored && onSourceRestored != nil {
			if err := onSourceRestored(); err != nil {
				return err
			}
		}
		sourceRestored = true
		return nil
	}
	select {
	case <-ctx.Done():
		m.cancelForkSameNode(req)
		return forkMaterializeResult{}, ctx.Err()
	default:
	}
	for {
		select {
		case <-ctx.Done():
			m.cancelForkSameNode(req)
			return forkMaterializeResult{}, ctx.Err()
		case <-timer.C:
			m.cancelForkSameNode(req)
			return forkMaterializeResult{}, connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("timed out waiting for same-node fork %s", req.TransferSetID))
		case msg := <-restoredCh:
			if err := handleSourceRestored(msg); err != nil {
				return forkMaterializeResult{}, err
			}
		case msg := <-ch:
			if !sourceRestored {
				select {
				case restored := <-restoredCh:
					if err := handleSourceRestored(restored); err != nil {
						return forkMaterializeResult{}, err
					}
				default:
					return forkMaterializeResult{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("same-node fork completed before source-restored acknowledgement"))
				}
			}
			return forkResultFromComplete(msg, req)
		}
	}
}

func (m *sameNodeForkMaterializer) materializeCrossNodeForkWithSourceRestored(ctx context.Context, req forkMaterializeRequest, onSourceRestored func() error) (forkMaterializeResult, error) {
	sourceNode, ok := m.s.reg.Get(req.SourceNodeID)
	if !ok || sourceNode.Sender == nil {
		return forkMaterializeResult{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("source node %q is not connected", req.SourceNodeID))
	}
	targetNode, ok := m.s.reg.Get(req.TargetNodeID)
	if !ok || targetNode.Sender == nil {
		return forkMaterializeResult{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("target node %q is not connected", req.TargetNodeID))
	}
	keyMaterial, err := m.s.forkTargetKeyMaterial(req.TargetNodeID)
	if err != nil {
		return forkMaterializeResult{}, err
	}
	if m.s.forkSourceRestored == nil {
		m.s.forkSourceRestored = newForkSourceRestoredWaiters()
	}
	if m.s.forkTransferExports == nil {
		m.s.forkTransferExports = newForkTransferExportedWaiters()
	}
	if m.s.forkTransferImports == nil {
		m.s.forkTransferImports = newForkTransferImportedWaiters()
	}
	restoredCh := m.s.forkSourceRestored.register(req.TransferSetID)
	defer m.s.forkSourceRestored.unregister(req.TransferSetID)
	exportedCh := m.s.forkTransferExports.register(req.TransferSetID)
	defer m.s.forkTransferExports.unregister(req.TransferSetID)
	importedCh := m.s.forkTransferImports.register(req.TransferSetID)
	defer m.s.forkTransferImports.unregister(req.TransferSetID)

	cancel := func() { m.cancelForkTransfer(req) }
	if err := sourceNode.Sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTransferExport{ForkTransferExport: &nodev1.ForkTransferExport{
		SourceSpawnId:       req.SourceSpawn.ID,
		ForkSpawnId:         req.ForkSpawn.ID,
		SourceGeneration:    req.SourceGeneration,
		TargetGeneration:    req.TargetGeneration,
		TransferSetId:       req.TransferSetID,
		TargetNodeId:        req.TargetNodeID,
		TargetNodeClass:     targetNode.Class,
		TargetNodeOwner:     targetNode.Owner,
		TargetSignedSubkey:  keyMaterial.signedSubKey,
		TargetNodeCertChain: keyMaterial.certChain,
	}}}); err != nil {
		return forkMaterializeResult{}, connect.NewError(connect.CodeUnavailable, fmt.Errorf("send fork transfer export to node %q: %w", req.SourceNodeID, err))
	}

	timer := time.NewTimer(m.timeout)
	defer timer.Stop()
	sourceRestored := false
	for {
		select {
		case <-ctx.Done():
			cancel()
			return forkMaterializeResult{}, ctx.Err()
		case <-timer.C:
			cancel()
			return forkMaterializeResult{}, connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("timed out waiting for cross-node fork %s", req.TransferSetID))
		case msg := <-restoredCh:
			if msg.GetError() != "" {
				cancel()
				return forkMaterializeResult{}, connect.NewError(connect.CodeInternal, fmt.Errorf("node fork source restore failed: %s", msg.GetError()))
			}
			if msg.GetSourceSpawnId() != req.SourceSpawn.ID || msg.GetSourceGeneration() != req.SourceGeneration || msg.GetTransferSetId() != req.TransferSetID {
				cancel()
				return forkMaterializeResult{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("fork source-restored ids do not match request"))
			}
			if !sourceRestored && onSourceRestored != nil {
				if err := onSourceRestored(); err != nil {
					cancel()
					return forkMaterializeResult{}, err
				}
			}
			sourceRestored = true
		case msg := <-exportedCh:
			if msg.GetError() != "" {
				cancel()
				return forkMaterializeResult{}, connect.NewError(connect.CodeInternal, fmt.Errorf("node fork export failed: %s", msg.GetError()))
			}
			if msg.GetSourceSpawnId() != req.SourceSpawn.ID || msg.GetForkSpawnId() != req.ForkSpawn.ID || msg.GetTransferSetId() != req.TransferSetID {
				cancel()
				return forkMaterializeResult{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("fork transfer export ids do not match request"))
			}
			if len(msg.GetSealedTransferKey()) == 0 || len(msg.GetPayload()) == 0 {
				cancel()
				return forkMaterializeResult{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("fork transfer export is missing ciphertext"))
			}
			if err := targetNode.Sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkTransferImport{ForkTransferImport: &nodev1.ForkTransferImport{
				SourceSpawnId:     req.SourceSpawn.ID,
				ForkSpawnId:       req.ForkSpawn.ID,
				TargetGeneration:  req.TargetGeneration,
				TransferSetId:     req.TransferSetID,
				SealedTransferKey: append([]byte(nil), msg.GetSealedTransferKey()...),
				Payload:           append([]byte(nil), msg.GetPayload()...),
			}}}); err != nil {
				cancel()
				return forkMaterializeResult{}, connect.NewError(connect.CodeUnavailable, fmt.Errorf("send fork transfer import to node %q: %w", req.TargetNodeID, err))
			}
		case msg := <-importedCh:
			if !sourceRestored {
				cancel()
				return forkMaterializeResult{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("cross-node fork completed before source-restored acknowledgement"))
			}
			return forkResultFromImported(msg, req)
		}
	}
}

func (m *sameNodeForkMaterializer) cancelForkTransfer(req forkMaterializeRequest) {
	cancel := &nodev1.CPMessage{Msg: &nodev1.CPMessage_CancelForkTransfer{CancelForkTransfer: &nodev1.CancelForkTransfer{
		SourceSpawnId: req.SourceSpawn.ID,
		ForkSpawnId:   req.ForkSpawn.ID,
		TransferSetId: req.TransferSetID,
	}}}
	if n, ok := m.s.reg.Get(req.SourceNodeID); ok && n.Sender != nil {
		if err := n.Sender.Send(cancel); err != nil {
			log.Printf("cancel fork transfer %s on source node %s: %v", req.TransferSetID, req.SourceNodeID, err)
		}
	}
	if req.TargetNodeID == req.SourceNodeID {
		return
	}
	if n, ok := m.s.reg.Get(req.TargetNodeID); ok && n.Sender != nil {
		if err := n.Sender.Send(cancel); err != nil {
			log.Printf("cancel fork transfer %s on target node %s: %v", req.TransferSetID, req.TargetNodeID, err)
		}
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

func forkResultFromImported(msg *nodev1.ForkTransferImported, req forkMaterializeRequest) (forkMaterializeResult, error) {
	if msg.GetError() != "" {
		return forkMaterializeResult{}, connect.NewError(connect.CodeInternal, fmt.Errorf("node fork import failed: %s", msg.GetError()))
	}
	if msg.GetSourceSpawnId() != req.SourceSpawn.ID || msg.GetForkSpawnId() != req.ForkSpawn.ID {
		return forkMaterializeResult{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("fork import ids do not match request"))
	}
	mountPins := mountPinsFromForkComplete(msg.GetMounts())
	rootfsPins, err := rootfsPinsFromArtifacts(msg.GetRootfsArtifacts(), req.TargetGeneration, req.SourceSpawn.BaseImageDigest)
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
	return rootfsPinsFromArtifacts(msg.GetRootfsArtifacts(), targetGeneration, baseImageDigest)
}

func rootfsPinsFromArtifacts(artifacts []*nodev1.RootfsArtifact, targetGeneration uint64, baseImageDigest string) ([]store.RootfsArtifactPin, error) {
	if len(artifacts) == 0 {
		return nil, nil
	}
	pins := make([]store.RootfsArtifactPin, 0, len(artifacts))
	for _, art := range artifacts {
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
	pins = sortedRootfsArtifactPins(pins)
	if err := validateRootfsArtifactPinChain(pins); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return pins, nil
}
