package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"
)

type ownerRepo struct{ db bun.IDB }

func (r *ownerRepo) Upsert(ctx context.Context, o Owner) error {
	_, err := r.db.NewInsert().Model(&o).
		On("CONFLICT (id) DO UPDATE").
		Set("email = EXCLUDED.email").
		Exec(ctx)
	return err
}

func (r *ownerRepo) Get(ctx context.Context, id string) (Owner, error) {
	var o Owner
	err := r.db.NewSelect().Model(&o).Where("id = ?", id).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return Owner{}, ErrNotFound
	}
	return o, err
}
