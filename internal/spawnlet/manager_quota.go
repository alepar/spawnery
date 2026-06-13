package spawnlet

import (
	"context"
	"log"
	"time"
)

// deltaSizer is an optional extension of PodBackend that exposes per-spawn delta image sizes.
// The Manager type-asserts m.pod against this interface at CheckQuotas time.  When the
// assertion fails (the current Docker backend does not implement DeltaSize), quota enforcement
// is dormant — a single warning is logged per manager lifetime.
//
// Precision note: DeltaSize reports committed-image size (uncompressed layer bytes as reported by
// the engine's ImageInspect).  Snapshot-in-flight size (bytes written since the last capture) is
// NOT accounted for.  Thresholds should be set with a comfortable margin above the true limit.
type deltaSizer interface {
	// DeltaSize returns the total uncompressed size (in bytes) of the per-spawn delta image
	// currently committed for spawnID.  Returns 0,nil when the image does not exist yet
	// (pre-first-suspend spawn — quota not yet applicable).
	DeltaSize(ctx context.Context, spawnID string) (int64, error)
}

// CheckQuotas inspects the captured delta image size for every live spawn and applies the
// configured soft/hard thresholds:
//
//   - Hard threshold (DeltaQuotaHardMB): Stop the spawn. Note: Stop does NOT release the delta
//     image (only Delete does); the stopped spawn can still be resumed from its last delta.
//   - Soft threshold (DeltaQuotaSoftMB): Suspend the spawn (keeps the delta) and log a warning.
//
// If neither threshold is configured (both == 0) CheckQuotas is a no-op.
// If the backend does not implement deltaSizer, a dormant-quota warning is logged once and
// CheckQuotas returns without taking any action.
func (m *Manager) CheckQuotas(ctx context.Context) {
	if m.cfg.DeltaQuotaSoftMB == 0 && m.cfg.DeltaQuotaHardMB == 0 {
		return
	}
	ds, ok := m.pod.(deltaSizer)
	if !ok {
		if !m.quotaWarnedOnce {
			m.quotaWarnedOnce = true
			log.Printf("delta quota: backend does not expose DeltaSize; quota enforcement is dormant " +
				"(add DeltaSize to the PodBackend implementation to enable)")
		}
		return
	}

	for _, sp := range m.store.List() {
		sz, err := ds.DeltaSize(ctx, sp.ID)
		if err != nil {
			log.Printf("delta quota: DeltaSize for %s: %v (skipping)", sp.ID, err)
			continue
		}
		mb := sz >> 20 // bytes → MiB (truncating)

		if m.cfg.DeltaQuotaHardMB > 0 && mb >= m.cfg.DeltaQuotaHardMB {
			log.Printf("delta quota HARD for spawn=%s size=%dMiB threshold=%dMiB: stopping",
				sp.ID, mb, m.cfg.DeltaQuotaHardMB)
			if err := m.Stop(ctx, sp.ID); err != nil {
				log.Printf("delta quota: stop %s: %v", sp.ID, err)
			}
			continue
		}
		if m.cfg.DeltaQuotaSoftMB > 0 && mb >= m.cfg.DeltaQuotaSoftMB {
			log.Printf("delta quota SOFT for spawn=%s size=%dMiB threshold=%dMiB: suspending",
				sp.ID, mb, m.cfg.DeltaQuotaSoftMB)
			if _, err := m.Suspend(ctx, sp.ID); err != nil {
				log.Printf("delta quota: suspend %s: %v", sp.ID, err)
			}
		}
	}
}

// RunQuotaWatchdog runs CheckQuotas at the given interval until ctx is cancelled.
// cmd/spawnlet should start this as a goroutine when DeltaCapture is enabled.
// Env wiring of DELTA_QUOTA_SOFT_MB and DELTA_QUOTA_HARD_MB into ManagerConfig is
// a handoff (cmd/spawnlet is outside the allowed file set for this task).
func (m *Manager) RunQuotaWatchdog(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.CheckQuotas(ctx)
		}
	}
}
