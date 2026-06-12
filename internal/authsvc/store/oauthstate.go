package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"
)

type oauthStateRepo struct{ db bun.IDB }

func (r *oauthStateRepo) Create(ctx context.Context, s OAuthState) error {
	_, err := r.db.NewInsert().Model(&s).Exec(ctx)
	return err
}

func (r *oauthStateRepo) Consume(ctx context.Context, state string) (OAuthState, error) {
	var s OAuthState
	err := r.db.NewUpdate().Model(&s).
		Set("used = 1").
		Where("state = ? AND used = 0", state).
		Returning("*").
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthState{}, ErrNotFound
	}
	return s, err
}

