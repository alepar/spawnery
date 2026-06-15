package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"
)

type secretRepo struct{ db bun.IDB }

func (r *secretRepo) Create(ctx context.Context, s Secret) error {
	s.Version = 1
	_, err := r.db.NewInsert().Model(&s).Exec(ctx)
	return err
}

func (r *secretRepo) Get(ctx context.Context, accountID, secretID string) (Secret, error) {
	var s Secret
	err := r.db.NewSelect().Model(&s).
		Where("account_id = ? AND secret_id = ?", accountID, secretID).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return Secret{}, ErrNotFound
	}
	if err != nil {
		return Secret{}, err
	}
	return s, nil
}

func (r *secretRepo) ListByOwner(ctx context.Context, accountID string, f SecretListFilter) ([]Secret, error) {
	var secrets []Secret
	q := r.db.NewSelect().Model(&secrets).
		Where("account_id = ?", accountID).
		Order("secret_id ASC")
	if f.DevicesetEpochBefore > 0 {
		q = q.Where("deviceset_epoch < ?", f.DevicesetEpochBefore)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return secrets, nil
}

func (r *secretRepo) Put(ctx context.Context, accountID, secretID string, expectedVersion uint64, next Secret) (uint64, error) {
	newVersion := expectedVersion + 1
	res, err := r.db.NewUpdate().Model((*Secret)(nil)).
		Set("type = ?", next.Type).
		Set("name = ?", next.Name).
		Set("provider = ?", next.Provider).
		Set("target_container = ?", next.TargetContainer).
		Set("env_var_name = ?", next.EnvVarName).
		Set("dest_path = ?", next.DestPath).
		Set("deviceset_epoch = ?", next.DevicesetEpoch).
		Set("envelope = ?", next.Envelope).
		Set("updated_at = ?", next.UpdatedAt).
		Set("version = ?", newVersion).
		Where("account_id = ? AND secret_id = ? AND version = ?", accountID, secretID, expectedVersion).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return newVersion, nil
	}
	exists, err := r.db.NewSelect().Model((*Secret)(nil)).
		Where("account_id = ? AND secret_id = ?", accountID, secretID).
		Exists(ctx)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, ErrNotFound
	}
	return 0, ErrConflict
}

func (r *secretRepo) Delete(ctx context.Context, accountID, secretID string) error {
	res, err := r.db.NewDelete().Model((*Secret)(nil)).
		Where("account_id = ? AND secret_id = ?", accountID, secretID).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
