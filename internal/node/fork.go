package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/spawnlet"
)

const forkTurnBoundaryPoll = 10 * time.Millisecond

type forkIngressBarrier struct {
	sourceGeneration uint64
	transferSetID    string
}

type forkBarrierWait struct {
	spawnID string
	barrier forkIngressBarrier
	cancel  context.CancelFunc
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
		SourceRestored: func() error {
			return a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_ForkSourceRestored{ForkSourceRestored: &nodev1.ForkSourceRestored{
				SourceSpawnId:    m.GetSourceSpawnId(),
				SourceGeneration: m.GetSourceGeneration(),
				TransferSetId:    m.GetTransferSetId(),
			}}})
		},
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

func (a *attacher) startForkTurnBoundary(ctx context.Context, m *nodev1.ForkTurnBoundary) {
	barrier := forkIngressBarrier{sourceGeneration: m.GetSourceGeneration(), transferSetID: m.GetTransferSetId()}
	waitCtx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	if a.forkWaits == nil {
		a.forkWaits = map[string]forkBarrierWait{}
	}
	if prev, ok := a.forkWaits[m.GetTransferSetId()]; ok {
		prev.cancel()
	}
	a.forkWaits[m.GetTransferSetId()] = forkBarrierWait{
		spawnID: m.GetSourceSpawnId(),
		barrier: barrier,
		cancel:  cancel,
	}
	a.mu.Unlock()

	go func() {
		defer a.forgetForkBarrierWait(m.GetTransferSetId(), m.GetSourceSpawnId(), barrier)
		a.forkTurnBoundary(waitCtx, m)
	}()
}

func (a *attacher) releaseForkTurnBoundary(m *nodev1.ReleaseForkTurnBoundary) {
	if m == nil {
		return
	}
	barrier := forkIngressBarrier{sourceGeneration: m.GetSourceGeneration(), transferSetID: m.GetTransferSetId()}
	var cancel context.CancelFunc
	a.mu.Lock()
	if wait, ok := a.forkWaits[m.GetTransferSetId()]; ok && wait.spawnID == m.GetSourceSpawnId() && wait.barrier.matches(barrier) {
		cancel = wait.cancel
		delete(a.forkWaits, m.GetTransferSetId())
	}
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	a.releaseForkBarrier(m.GetSourceSpawnId(), func(b forkIngressBarrier) bool {
		return b.matches(barrier)
	})
}

func (a *attacher) forgetForkBarrierWait(transferSetID, spawnID string, barrier forkIngressBarrier) {
	a.mu.Lock()
	if wait, ok := a.forkWaits[transferSetID]; ok && wait.spawnID == spawnID && wait.barrier.matches(barrier) {
		delete(a.forkWaits, transferSetID)
	}
	a.mu.Unlock()
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

func (a *attacher) failedForkCleanup(ctx context.Context, m *nodev1.FailedForkCleanup) {
	reply := &nodev1.FailedForkCleanupComplete{
		RequestId:   m.GetRequestId(),
		ForkSpawnId: m.GetForkSpawnId(),
		Op:          m.GetOp(),
	}
	var err error
	switch m.GetOp() {
	case nodev1.FailedForkCleanupOp_FAILED_FORK_CLEANUP_OP_REVOKE_GENERATION:
		err = a.mgr.RevokeForkGeneration(ctx, m.GetForkSpawnId(), m.GetGeneration())
	case nodev1.FailedForkCleanupOp_FAILED_FORK_CLEANUP_OP_EMPTY_BUCKET:
		err = a.mgr.EmptyForkBucket(ctx, m.GetForkSpawnId(), m.GetBucket())
	case nodev1.FailedForkCleanupOp_FAILED_FORK_CLEANUP_OP_DROP_BUCKET:
		err = a.mgr.DropForkBucket(ctx, m.GetForkSpawnId(), m.GetBucket())
	default:
		err = fmt.Errorf("unknown failed fork cleanup op %s", m.GetOp())
	}
	if err != nil {
		logErr("failedForkCleanup "+m.GetForkSpawnId(), err)
		reply.Error = err.Error()
	}
	_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_FailedForkCleanupComplete{FailedForkCleanupComplete: reply}})
}

func (a *attacher) acquireForkBarrier(ctx context.Context, spawnID string, sourceGeneration uint64, transferSetID string) error {
	if transferSetID == "" {
		return fmt.Errorf("fork turn-boundary gate unavailable: missing transfer set")
	}
	barrier := forkIngressBarrier{sourceGeneration: sourceGeneration, transferSetID: transferSetID}
	matchesBarrier := func(b forkIngressBarrier) bool { return b.matches(barrier) }
	poll := forkTurnBoundaryPoll
	if poll <= 0 {
		poll = 10 * time.Millisecond
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			a.releaseForkBarrier(spawnID, matchesBarrier)
			return ctx.Err()
		default:
		}
		err, acquired := a.tryAcquireForkBarrier(ctx, spawnID, barrier)
		if err != nil {
			return err
		}
		if acquired {
			return nil
		}
		select {
		case <-ctx.Done():
			a.releaseForkBarrier(spawnID, matchesBarrier)
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (a *attacher) tryAcquireForkBarrier(ctx context.Context, spawnID string, barrier forkIngressBarrier) (error, bool) {
	a.mu.Lock()
	var pumps []*Pump
	for key, p := range a.pumps {
		if key.spawnID == spawnID {
			pumps = append(pumps, p)
		}
	}
	var relays []*tmuxRelay
	for key, r := range a.tmuxRelays {
		if key.spawnID == spawnID {
			relays = append(relays, r)
		}
	}
	if reg := a.sessions[spawnID]; reg != nil && reg.hasStarting() {
		a.mu.Unlock()
		return nil, false
	}
	if len(pumps) == 0 && len(relays) == 0 {
		a.mu.Unlock()
		return fmt.Errorf("fork turn-boundary gate unavailable: no observable ACP pump or tmux relay"), false
	}
	a.mu.Unlock()
	var acquired []*Pump
	for _, p := range pumps {
		if !p.tryAcquireForkBarrier(barrier) {
			for _, held := range acquired {
				held.releaseForkBarrier(func(b forkIngressBarrier) bool { return b.matches(barrier) })
			}
			return nil, false
		}
		acquired = append(acquired, p)
	}
	var acquiredRelays []*tmuxRelay
	for _, r := range relays {
		if !r.tryAcquireForkBarrier(barrier) {
			for _, held := range acquired {
				held.releaseForkBarrier(func(b forkIngressBarrier) bool { return b.matches(barrier) })
			}
			for _, held := range acquiredRelays {
				held.releaseForkBarrier(func(b forkIngressBarrier) bool { return b.matches(barrier) })
			}
			return nil, false
		}
		acquiredRelays = append(acquiredRelays, r)
	}
	a.mu.Lock()
	a.ensureForkBarriersLocked()
	a.forkBarriers[spawnID] = barrier
	a.mu.Unlock()
	if len(relays) > 0 {
		busy, err := a.sourceInferenceBusy(ctx, spawnID)
		if err != nil {
			a.releaseForkBarrier(spawnID, func(b forkIngressBarrier) bool { return b.matches(barrier) })
			return err, false
		}
		if busy {
			return nil, false
		}
	}
	return nil, true
}

func (a *attacher) sourceInferenceBusy(ctx context.Context, spawnID string) (bool, error) {
	sp, ok := a.mgr.Store().Get(spawnID)
	if !ok {
		return false, fmt.Errorf("fork turn-boundary gate unavailable: unknown spawn")
	}
	if sp.ControlURL == "" || sp.ControlToken == "" {
		return false, fmt.Errorf("fork turn-boundary gate unavailable: no sidecar control endpoint")
	}
	statusURL := strings.TrimSuffix(sp.ControlURL, "/model")
	if statusURL == sp.ControlURL {
		statusURL = strings.TrimRight(sp.ControlURL, "/")
	}
	statusURL += "/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+sp.ControlToken)
	doer := a.ctrlHTTP
	if doer == nil {
		doer = &http.Client{Timeout: controlPostTimeout}
	}
	resp, err := doer.Do(req)
	if err != nil {
		return false, fmt.Errorf("sidecar control status: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("sidecar control status returned %d", resp.StatusCode)
	}
	var body struct {
		Busy           bool  `json:"busy"`
		ActiveRequests int64 `json:"active_requests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, fmt.Errorf("decode sidecar control status: %w", err)
	}
	return body.Busy || body.ActiveRequests > 0, nil
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

func (a *attacher) forkBarrierActiveOrPendingLocked(spawnID string) bool {
	if a.forkBarriers != nil {
		if _, ok := a.forkBarriers[spawnID]; ok {
			return true
		}
	}
	for _, wait := range a.forkWaits {
		if wait.spawnID == spawnID {
			return true
		}
	}
	return false
}

func (a *attacher) releaseForkBarrier(spawnID string, match func(forkIngressBarrier) bool) {
	a.mu.Lock()
	var releasePumps []*Pump
	var releaseRelays []*tmuxRelay
	if a.forkBarriers != nil {
		if b, ok := a.forkBarriers[spawnID]; ok && match(b) {
			delete(a.forkBarriers, spawnID)
			for key, p := range a.pumps {
				if key.spawnID == spawnID {
					releasePumps = append(releasePumps, p)
				}
			}
			for key, r := range a.tmuxRelays {
				if key.spawnID == spawnID {
					releaseRelays = append(releaseRelays, r)
				}
			}
		}
	}
	a.mu.Unlock()
	for _, p := range releasePumps {
		p.releaseForkBarrier(match)
	}
	for _, r := range releaseRelays {
		r.releaseForkBarrier(match)
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
