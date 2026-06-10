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
	"github.com/google/uuid"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

// defaultSetModelPushTimeout bounds the inline best-effort SetModel push: a single attempt waiting
// for the node's SetModelResult ack. Persistent failure is the reconciler's job (sp-bp9w.7), not this
// handler's — so we wait only long enough for one healthy node->sidecar round trip.
const defaultSetModelPushTimeout = 3 * time.Second

// modelWaiters correlates an inline SetSpawnModel push with the async SetModelResult the node sends
// back on its Attach stream. Keyed by a per-push request_id (NOT spawn_id): keying by spawn_id alone
// is unsafe, because a late ack from a timed-out push could be misrouted to a SUBSEQUENT push's
// waiter for the same spawn, masking a real failure permanently. The node echoes the request_id from
// the SetModel it handled into SetModelResult, so each ack matches exactly the push that is waiting.
type modelWaiters struct {
	mu sync.Mutex
	m  map[string]chan *nodev1.SetModelResult
}

func newModelWaiters() *modelWaiters {
	return &modelWaiters{m: map[string]chan *nodev1.SetModelResult{}}
}

// register installs a buffered (cap 1) waiter for requestID and returns its channel. Call BEFORE
// sending SetModel so a fast ack is never missed.
func (w *modelWaiters) register(requestID string) chan *nodev1.SetModelResult {
	ch := make(chan *nodev1.SetModelResult, 1)
	w.mu.Lock()
	w.m[requestID] = ch
	w.mu.Unlock()
	return ch
}

func (w *modelWaiters) unregister(requestID string) {
	w.mu.Lock()
	delete(w.m, requestID)
	w.mu.Unlock()
}

// deliver routes an inbound SetModelResult to its waiter (if any), matched by request_id. Non-blocking:
// a late/duplicate/stale ack whose request_id has no live waiter (or a full buffer) is dropped rather
// than blocking the node receive loop or being misattributed to another push.
func (w *modelWaiters) deliver(r *nodev1.SetModelResult) {
	w.mu.Lock()
	ch, ok := w.m[r.GetRequestId()]
	w.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- r:
	default:
	}
}

// SetSpawnModel changes a spawn's inference model (owner-guarded). It persists the new model as the
// durable source of truth (model_applied=false in one store txn) and then makes a single, bounded,
// best-effort inline push to the hosting node so a healthy live pod switches immediately. It never
// blocks on retries — persistent failure is the reconciler's job (sp-bp9w.7). Returns the active model
// and whether it was applied to the running pod.
func (s *Server) SetSpawnModel(ctx context.Context, req *connect.Request[cpv1.SetSpawnModelRequest]) (*connect.Response[cpv1.SetSpawnModelResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	model := strings.TrimSpace(req.Msg.Model)
	if model == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("model must not be empty"))
	}
	spawnID := req.Msg.SpawnId

	unlock := s.locks.Lock(spawnID)
	defer unlock()

	sp, err := s.st.Spawns().Get(ctx, spawnID)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if sp.OwnerID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}

	// Durable source of truth: write model + model_applied=false in one atomic store update.
	if err := s.st.Spawns().SetModel(ctx, spawnID, model); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	applied := s.pushModel(ctx, spawnID, model)
	return connect.NewResponse(&cpv1.SetSpawnModelResponse{Model: model, Applied: applied}), nil
}

// pushModel does the single bounded best-effort inline SetModel push for spawnID, updating the
// model_applied flag accordingly, and reports whether the model is now applied. Caller holds the
// per-spawn lock. Never retries.
func (s *Server) pushModel(ctx context.Context, spawnID, model string) bool {
	// Post-decision store writes use a detached ctx so the model_applied/detail audit survives a
	// client disconnect; the push + await below still use the cancellable ctx + timeout.
	storeCtx := context.WithoutCancel(ctx)

	c, hasLive, err := s.st.Spawns().LiveContainer(ctx, spawnID)
	if err != nil {
		// A real DB error is NOT "no live pod": don't mark applied (that would falsely report success
		// and the reconciler — which scans model_applied=false — would never fix it). Record the
		// failure and report applied=false so the reconciler retries.
		if merr := s.st.Spawns().MarkModelApplyFailed(storeCtx, spawnID, fmt.Sprintf("check live pod: %v", err)); merr != nil {
			log.Printf("SetSpawnModel %s: MarkModelApplyFailed (live check error): %v", spawnID, merr)
		}
		return false
	}
	if !hasLive {
		// No running pod (suspended/stopped/terminal) — nothing to diverge. Resume/recreate bakes the
		// DB model into the fresh pod, so treat as already applied.
		if merr := s.st.Spawns().MarkModelApplied(storeCtx, spawnID); merr != nil {
			log.Printf("SetSpawnModel %s: MarkModelApplied (no live pod): %v", spawnID, merr)
		}
		return true
	}
	node, ok := s.reg.Get(c.NodeID)
	if !ok {
		// Pod is running but its node is not currently connected (unreachable). Can't push now; leave
		// model_applied=false so the reconciler re-pushes on reconnect.
		if merr := s.st.Spawns().MarkModelApplyFailed(storeCtx, spawnID, "no connected node hosting spawn"); merr != nil {
			log.Printf("SetSpawnModel %s: MarkModelApplyFailed (no node): %v", spawnID, merr)
		}
		return false
	}

	// Correlate this specific push with its ack by a unique request_id (NOT spawn_id): a late ack from
	// a prior timed-out push for the same spawn carries the OLD request_id and is dropped, so it can't
	// corrupt this push's result.
	requestID := uuid.Must(uuid.NewV7()).String()
	ch := s.models.register(requestID)
	defer s.models.unregister(requestID)

	if err := node.Sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_SetModel{SetModel: &nodev1.SetModel{
		SpawnId: spawnID, Generation: uint64(c.Generation), Model: model, RequestId: requestID,
	}}}); err != nil {
		if merr := s.st.Spawns().MarkModelApplyFailed(storeCtx, spawnID, fmt.Sprintf("push send failed: %v", err)); merr != nil {
			log.Printf("SetSpawnModel %s: MarkModelApplyFailed (send): %v", spawnID, merr)
		}
		return false
	}

	wait, cancel := context.WithTimeout(ctx, s.setModelTimeout)
	defer cancel()
	select {
	case res := <-ch:
		if res.GetOk() {
			if merr := s.st.Spawns().MarkModelApplied(storeCtx, spawnID); merr != nil {
				log.Printf("SetSpawnModel %s: MarkModelApplied (ack ok): %v", spawnID, merr)
				return false
			}
			return true
		}
		detail := res.GetDetail()
		if detail == "" {
			detail = "node reported failure"
		}
		if merr := s.st.Spawns().MarkModelApplyFailed(storeCtx, spawnID, detail); merr != nil {
			log.Printf("SetSpawnModel %s: MarkModelApplyFailed (ack not-ok): %v", spawnID, merr)
		}
		return false
	case <-wait.Done():
		if merr := s.st.Spawns().MarkModelApplyFailed(storeCtx, spawnID, "timeout waiting for node ack"); merr != nil {
			log.Printf("SetSpawnModel %s: MarkModelApplyFailed (timeout): %v", spawnID, merr)
		}
		return false
	}
}
