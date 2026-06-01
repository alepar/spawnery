package store

import (
	"context"

	"github.com/uptrace/bun"
)

// MarkBootUnreachable marks every {starting, active} spawn unreachable — the crude CP-restart sweep
// (the CP lost all live routes on restart). Live container rows are KEPT (user recreates). The
// grace-window + node-inventory + adopt protocol is a later (CP-wiring part 3) refinement.
func (r *spawnRepo) MarkBootUnreachable(ctx context.Context) (int, error) {
	res, err := r.db.NewUpdate().Model((*Spawn)(nil)).
		Set("status = ?", Unreachable).
		Where("status IN (?)", bun.In([]Status{Starting, Active})).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
