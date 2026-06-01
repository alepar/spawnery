package store

import "context"

// Temporary stubs so *spawnRepo satisfies SpawnRepo while Task 7 implements the real bodies.
// Each panics if called; current tests never call them.

func (r *spawnRepo) LiveContainersByNode(ctx context.Context, nodeID string) ([]Container, error) {
	panic("store: LiveContainersByNode not implemented (Task 7)")
}
func (r *spawnRepo) Adopt(ctx context.Context, id, nodeID string, gen int64) error {
	panic("store: Adopt not implemented (Task 7)")
}
