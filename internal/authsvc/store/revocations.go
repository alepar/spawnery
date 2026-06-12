package store

import (
	"context"

	"github.com/uptrace/bun"
)

type revocationRepo struct{ db bun.IDB }

func (r *revocationRepo) Append(ctx context.Context, ev RevocationEvent) (int64, error) {
	ev.Seq = 0 // assigned by AUTOINCREMENT
	if _, err := r.db.NewInsert().Model(&ev).Exec(ctx); err != nil {
		return 0, err
	}
	return ev.Seq, nil
}

func (r *revocationRepo) Since(ctx context.Context, seq int64) ([]RevocationEvent, error) {
	var evs []RevocationEvent
	err := r.db.NewSelect().Model(&evs).
		Where("seq > ?", seq).
		OrderExpr("seq ASC").
		Scan(ctx)
	return evs, err
}
