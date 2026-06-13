package store

import (
	"context"

	"github.com/uptrace/bun"
)

// MarkBootUnreachable marks every {starting, active, suspending} spawn unreachable — the crude
// CP-restart sweep (the CP lost all live routes on restart). 'suspending' is included because a CP
// crash mid-suspend leaves the spawn there with no live route; without sweeping it the spawn is
// stranded (never resolves). Live container rows are KEPT (user recreates). The grace-window +
// node-inventory + adopt protocol is a later (CP-wiring part 3) refinement.
func (r *spawnRepo) MarkBootUnreachable(ctx context.Context) (int, error) {
	res, err := r.db.NewUpdate().Model((*Spawn)(nil)).
		Set("status = ?", Unreachable).
		Set("status_seq = status_seq + 1").
		Where("status IN (?)", bun.In([]Status{Starting, Active, Suspending})).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
