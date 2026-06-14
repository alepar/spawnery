package cp

import (
	"context"
	"log"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/store"
)

// evaluateSpawnMetrics inspects the per-spawn metrics reported in rs (from the node heartbeat/
// register) and drives a lossless SUSPEND when the spawn is over-quota or idle. This is the
// CP-side implementation of §6 (node-local detectors → CP-side reporters): the node is now a
// pure reporter; the CP decides and acts.
//
// Pre-conditions (checked here, all must hold before any driver is started):
//   - The evaluator policy is enabled (s.evaluatorEnabled=true); caller guards this.
//   - The spawn is Active in the store (non-transient status, not claimed).
//   - rs.DeltaSizeBytes or rs.LastActivityUnixMs carries a signal (over-quota or idle).
//   - No driver is already in flight for this spawn (de-dup via evaluatorInFlight).
//
// Safety invariant (§ Key decisions, §6): a partitioned node sends no heartbeat → the CP
// receives no reports → evaluateSpawnMetrics is never called → spawns are preserved.
// This is structural: the function only runs inside adoptOrStop, which only runs when a node
// actively reports the spawn on a heartbeat. No heartbeat, no evaluation, no transition.
func (s *Server) evaluateSpawnMetrics(ctx context.Context, spawnID string, rs *nodev1.RunningSpawn) {
	if rs == nil {
		return
	}

	// --- Quick pre-checks (no lock needed on in-flight map yet) ---

	// Read the spawn row to confirm it is Active and unclaimed.
	sp, err := s.st.Spawns().Get(ctx, spawnID)
	if err != nil {
		return // transient store error; next heartbeat retries
	}
	if sp.Status != store.Active {
		return // transient status (Suspending/Resuming) or dormant — skip
	}
	now := s.now().UnixNano()
	if sp.ClaimHolder != nil && sp.ClaimDeadline != nil && *sp.ClaimDeadline > now {
		return // actively claimed by a driver — skip
	}

	// --- Determine trigger ---

	var reason string

	// Quota check: over-quota when DeltaSizeBytes/MiB >= quotaSuspendMB (and quota is enabled).
	if s.quotaSuspendMB > 0 && rs.DeltaSizeBytes > 0 {
		mb := rs.DeltaSizeBytes >> 20 // bytes → MiB (truncating)
		if mb >= s.quotaSuspendMB {
			reason = "quota"
		}
	}

	// Idle check: last_activity_unix_ms > 0 means the node has activity data; 0 means no pump/relay
	// (pre-launch or unknown) — skip to avoid false-idle trips.
	if reason == "" && rs.LastActivityUnixMs > 0 {
		lastAt := time.UnixMilli(rs.LastActivityUnixMs)
		elapsed := s.now().Sub(lastAt)
		attached := s.rt.Attached(spawnID)
		budget := s.idleDetachedTimeout
		if attached {
			budget = s.idleAttachedTimeout
		}
		if elapsed >= budget {
			reason = "idle"
		}
	}

	if reason == "" {
		return // no trigger
	}

	// --- De-dup: one driver per spawn ---
	s.evaluatorMu.Lock()
	if _, inflight := s.evaluatorInFlight[spawnID]; inflight {
		s.evaluatorMu.Unlock()
		return // driver already running; next heartbeat will trigger again if still applicable
	}
	s.evaluatorInFlight[spawnID] = struct{}{}
	s.evaluatorMu.Unlock()

	// --- Launch async driver ---
	// context.WithoutCancel: the suspend must complete even if the heartbeat ctx is cancelled
	// (connection closed). suspendLocked uses a detached store ctx internally for post-round-trip
	// writes, so the cancel is already handled there for the success path; we mirror that here for
	// the withClaim frame.
	driveCtx := context.WithoutCancel(ctx)
	ownerID := sp.OwnerID
	log.Printf("evaluator: spawn=%s trigger=%s delta_mb=%d last_activity_ms=%d -> suspending",
		spawnID, reason, rs.DeltaSizeBytes>>20, rs.LastActivityUnixMs)
	go func() {
		defer func() {
			s.evaluatorMu.Lock()
			delete(s.evaluatorInFlight, spawnID)
			s.evaluatorMu.Unlock()
		}()
		if err := s.withClaim(driveCtx, spawnID, func(cctx context.Context, leaseID string) error {
			_, serr := s.suspendLocked(cctx, ownerID, spawnID, false, leaseID)
			return serr
		}); err != nil {
			log.Printf("evaluator: spawn=%s suspend failed: %v", spawnID, err)
		}
	}()
}
