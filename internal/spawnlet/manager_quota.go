package spawnlet

import (
	"context"
)

// deltaSizer is an optional extension of PodBackend that exposes per-spawn delta image sizes.
// The Manager type-asserts m.pod against this interface in DeltaSize. When the assertion fails
// (the current Docker backend does not implement DeltaSize), DeltaSize returns 0, nil —
// metrics reporting treats this as "unknown" rather than forcing the CP to act on stale data.
//
// Precision note: DeltaSize reports committed-image size (uncompressed layer bytes as reported by
// the engine's ImageInspect). Snapshot-in-flight size (bytes written since the last capture) is
// NOT accounted for. Thresholds should be set with a comfortable margin above the true limit.
type deltaSizer interface {
	// DeltaSize returns the total uncompressed size (in bytes) of the per-spawn delta image
	// currently committed for spawnID. Returns 0,nil when the image does not exist yet
	// (pre-first-suspend spawn — quota not yet applicable).
	DeltaSize(ctx context.Context, spawnID string) (int64, error)
}

// DeltaSize returns the captured delta-image size in bytes for spawnID, for CP-side quota
// evaluation. Returns 0, nil when:
//   - the pod backend does not implement deltaSizer (size unavailable — treated as unknown)
//   - the delta image does not exist yet (pre-first-suspend spawn)
//
// The CP evaluator gates quota enforcement on quotaSuspendMB > 0 and a non-zero reported size,
// so a 0 return is safe to emit on every heartbeat without triggering false quota trips.
func (m *Manager) DeltaSize(ctx context.Context, spawnID string) (int64, error) {
	ds, ok := m.pod.(deltaSizer)
	if !ok {
		return 0, nil
	}
	return ds.DeltaSize(ctx, spawnID)
}
