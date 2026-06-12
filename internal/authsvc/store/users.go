package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/uptrace/bun"
)

type userRepo struct{ db bun.IDB }

func (r *userRepo) GetBySub(ctx context.Context, githubSub int64) (User, error) {
	var u User
	err := r.db.NewSelect().Model(&u).Where("github_sub = ?", githubSub).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

func (r *userRepo) GetByID(ctx context.Context, accountID string) (User, error) {
	var u User
	err := r.db.NewSelect().Model(&u).Where("account_id = ?", accountID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

func (r *userRepo) Create(ctx context.Context, u User) error {
	_, err := r.db.NewInsert().Model(&u).Exec(ctx)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrConflict
	}
	return err
}

func (r *userRepo) SetStatus(ctx context.Context, accountID, status string) error {
	return r.updateOne(ctx, accountID, "status", status)
}

func (r *userRepo) SetHandle(ctx context.Context, accountID, handle string) error {
	return r.updateOne(ctx, accountID, "handle", handle)
}

func (r *userRepo) updateOne(ctx context.Context, accountID, col, val string) error {
	res, err := r.db.NewUpdate().Model((*User)(nil)).
		Set(col+" = ?", val).
		Where("account_id = ?", accountID).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
