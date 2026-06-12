package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"
)

type deviceGrantRepo struct{ db bun.IDB }

func (r *deviceGrantRepo) Create(ctx context.Context, g DeviceGrant) error {
	_, err := r.db.NewInsert().Model(&g).Exec(ctx)
	return err
}

func (r *deviceGrantRepo) GetByUserCode(ctx context.Context, userCode string) (DeviceGrant, error) {
	var g DeviceGrant
	err := r.db.NewSelect().Model(&g).Where("user_code = ?", userCode).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return DeviceGrant{}, ErrNotFound
	}
	return g, err
}

func (r *deviceGrantRepo) Get(ctx context.Context, deviceCodeHash string) (DeviceGrant, error) {
	var g DeviceGrant
	err := r.db.NewSelect().Model(&g).Where("device_code_hash = ?", deviceCodeHash).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return DeviceGrant{}, ErrNotFound
	}
	return g, err
}

func (r *deviceGrantRepo) SetDecision(ctx context.Context, userCode, accountID, status string) error {
	res, err := r.db.NewUpdate().Model((*DeviceGrant)(nil)).
		Set("status = ?", status).
		Set("account_id = ?", accountID).
		Where("user_code = ? AND status = ?", userCode, GrantPending).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return nil
}

func (r *deviceGrantRepo) Redeem(ctx context.Context, deviceCodeHash string) (DeviceGrant, error) {
	var g DeviceGrant
	err := r.db.NewUpdate().Model(&g).
		Set("status = ?", GrantRedeemed).
		Where("device_code_hash = ? AND status = ?", deviceCodeHash, GrantApproved).
		Returning("*").
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return DeviceGrant{}, ErrConflict
	}
	return g, err
}

func (r *deviceGrantRepo) BumpAttempt(ctx context.Context, deviceCodeHash string) (int, error) {
	var g DeviceGrant
	err := r.db.NewUpdate().Model(&g).
		Set("attempt_count = attempt_count + 1").
		Where("device_code_hash = ?", deviceCodeHash).
		Returning("*").
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return g.AttemptCount, err
}

func (r *deviceGrantRepo) SetLastPolled(ctx context.Context, deviceCodeHash string, now int64) error {
	_, err := r.db.NewUpdate().Model((*DeviceGrant)(nil)).
		Set("last_polled_at = ?", now).
		Where("device_code_hash = ?", deviceCodeHash).
		Exec(ctx)
	return err
}

func (r *deviceGrantRepo) SetStatus(ctx context.Context, deviceCodeHash, status string) error {
	_, err := r.db.NewUpdate().Model((*DeviceGrant)(nil)).
		Set("status = ?", status).
		Where("device_code_hash = ?", deviceCodeHash).
		Exec(ctx)
	return err
}
