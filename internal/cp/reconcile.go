package cp

import (
	"context"
	"log"
	"time"
)

// StartReconciler launches the background reconcile loop; it runs until ctx is cancelled. Call once,
// after NewServer. The CP has no graceful-shutdown path today, so main passes a process-lifetime ctx.
func (s *Server) StartReconciler(ctx context.Context) {
	go s.reconcileLoop(ctx)
}

func (s *Server) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(s.reconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reconcileTick(ctx)
		}
	}
}

// reconcileTick scans for spawns whose effective model has not been applied to a running pod and
// drives each toward convergence (sequentially, under the per-spawn lock). It reuses pushModel — the
// same gen-fenced, request_id-correlated push the inline SetSpawnModel handler uses — which already:
//   - re-pushes to a live pod on a connected node and MarkModelApplied on the ok ack;
//   - MarkModelApplied immediately when there is no live pod (nothing to diverge);
//   - MarkModelApplyFailed (leaving applied=false) on no-connected-node / DB error / NAK / timeout.
//
// The reconciler adds only a bounded per-spawn give-up clock (in memory) on top of that.
func (s *Server) reconcileTick(ctx context.Context) {
	rows, err := s.st.Spawns().ListUnappliedModel(ctx)
	if err != nil {
		log.Printf("reconcile: list unapplied: %v", err)
		return
	}
	seen := make(map[string]bool, len(rows))
	for _, sp := range rows {
		seen[sp.ID] = true
		s.reconcileSpawn(ctx, sp.ID)
	}
	// Prune give-up state for spawns no longer unapplied (now applied, or deleted): bounds the map and
	// ensures a future SetSpawnModel starts with a clean clock.
	for id := range s.giveUp {
		if !seen[id] {
			delete(s.giveUp, id)
		}
	}
}

// reconcileSpawn reconciles one spawn under its per-spawn lock (serializing against a concurrent
// SetSpawnModel). It re-reads the spawn (the list row may be stale), enforces the bounded give-up
// window per (spawn, current model), and otherwise delegates to pushModel. Touches s.giveUp, which is
// only ever accessed from the single reconciler goroutine, so no lock is needed on the map itself.
func (s *Server) reconcileSpawn(ctx context.Context, spawnID string) {
	unlock := s.locks.Lock(spawnID)
	defer unlock()

	sp, err := s.st.Spawns().Get(ctx, spawnID)
	if err != nil || sp.ModelApplied {
		// Gone, or already converged (a concurrent SetSpawnModel/provision applied it): drop tracking.
		delete(s.giveUp, spawnID)
		return
	}

	att, ok := s.giveUp[spawnID]
	if !ok || att.model != sp.Model {
		// First time we see this spawn at this model (or the model just changed) — (re)start the clock.
		att = reconcileAttempt{model: sp.Model, first: s.now()}
		s.giveUp[spawnID] = att
	}
	if s.now().Sub(att.first) > s.reconcileGiveUp {
		// Bounded retry window exhausted: stop retrying. Leave model_applied=false; the last failure
		// reason written by pushModel remains in model_apply_detail for the UI's "pending" badge. A CP
		// restart (or any later recreate/resume) resets this and tries again.
		return
	}

	if s.pushModel(ctx, spawnID, sp.Model) {
		delete(s.giveUp, spawnID) // applied — stop tracking
	}
}
