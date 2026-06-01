package store

import "context"

// Temporary stubs so *spawnRepo satisfies SpawnRepo while Tasks 6-7 implement the real bodies.
// Each panics if called; Task 5's tests never call them.

func (r *spawnRepo) ClaimStarting(ctx context.Context, id string, from []Status) (int64, error) {
	panic("store: ClaimStarting not implemented (Task 6)")
}
func (r *spawnRepo) SetActive(ctx context.Context, id string, gen int64) error {
	panic("store: SetActive not implemented (Task 6)")
}
func (r *spawnRepo) SetSuspending(ctx context.Context, id string, gen int64) error {
	panic("store: SetSuspending not implemented (Task 6)")
}
func (r *spawnRepo) SetMountMarker(ctx context.Context, id, mount, marker string) error {
	panic("store: SetMountMarker not implemented (Task 6)")
}
func (r *spawnRepo) SetSuspended(ctx context.Context, id string, gen int64) error {
	panic("store: SetSuspended not implemented (Task 6)")
}
func (r *spawnRepo) SetError(ctx context.Context, id string) error {
	panic("store: SetError not implemented (Task 6)")
}
func (r *spawnRepo) EndContainer(ctx context.Context, id string, gen int64, p Phase) error {
	panic("store: EndContainer not implemented (Task 6)")
}
func (r *spawnRepo) MarkUnreachable(ctx context.Context, ids []string) (int, error) {
	panic("store: MarkUnreachable not implemented (Task 6)")
}
func (r *spawnRepo) MarkRecovered(ctx context.Context, id string) error {
	panic("store: MarkRecovered not implemented (Task 6)")
}
func (r *spawnRepo) Touch(ctx context.Context, id string, ts int64) error {
	panic("store: Touch not implemented (Task 6)")
}
func (r *spawnRepo) MarkDeleted(ctx context.Context, id string, ts int64) error {
	panic("store: MarkDeleted not implemented (Task 6)")
}
func (r *spawnRepo) LiveContainersByNode(ctx context.Context, nodeID string) ([]Container, error) {
	panic("store: LiveContainersByNode not implemented (Task 7)")
}
func (r *spawnRepo) Adopt(ctx context.Context, id, nodeID string, gen int64) error {
	panic("store: Adopt not implemented (Task 7)")
}
