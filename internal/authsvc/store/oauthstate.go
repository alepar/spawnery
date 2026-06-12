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

type authCodeRepo struct{ db bun.IDB }

func (r *authCodeRepo) Create(ctx context.Context, c AuthCode) error {
	_, err := r.db.NewInsert().Model(&c).Exec(ctx)
	return err
}

func (r *authCodeRepo) Consume(ctx context.Context, codeHash string) (AuthCode, error) {
	var c AuthCode
	err := r.db.NewUpdate().Model(&c).
		Set("used = 1").
		Where("code_hash = ? AND used = 0", codeHash).
		Returning("*").
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthCode{}, ErrNotFound
	}
	return c, err
}
