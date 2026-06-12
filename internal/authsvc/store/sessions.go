package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"
)

type refreshSessionRepo struct{ db bun.IDB }

func (r *refreshSessionRepo) Get(ctx context.Context, tokenHash string) (RefreshSession, error) {
	var s RefreshSession
	err := r.db.NewSelect().Model(&s).Where("token_hash = ?", tokenHash).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return RefreshSession{}, ErrNotFound
	}
	return s, err
}

func (r *refreshSessionRepo) Insert(ctx context.Context, s RefreshSession) error {
	_, err := r.db.NewInsert().Model(&s).Exec(ctx)
	return err
}

func (r *refreshSessionRepo) Supersede(ctx context.Context, predecessorHash string, successor RefreshSession, successorCache string, now int64) error {
	// Clear stale caches first: after this, ONLY the row superseded right now can grace-replay
	// (older generations lose their cached pair — and the ≤45s plaintext successor token with it).
	if _, err := r.db.NewUpdate().Model((*RefreshSession)(nil)).
		Set("successor_cache = NULL").
		Where("family_id = ? AND token_hash != ?", successor.FamilyID, predecessorHash).
		Exec(ctx); err != nil {
		return err
	}
	res, err := r.db.NewUpdate().Model((*RefreshSession)(nil)).
		Set("superseded_by = ?", successor.TokenHash).
		Set("superseded_at = ?", now).
		Set("successor_cache = ?", successorCache).
		Set("last_used_at = ?", now).
		Where("token_hash = ? AND revoked = 0 AND superseded_by IS NULL", predecessorHash).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return r.Insert(ctx, successor)
}

func (r *refreshSessionRepo) RevokeFamily(ctx context.Context, familyID string) ([]string, error) {
	var live []RefreshSession
	if err := r.db.NewSelect().Model(&live).
		Column("access_token_id").
		Where("family_id = ? AND revoked = 0", familyID).
		Scan(ctx); err != nil {
		return nil, err
	}
	if _, err := r.db.NewUpdate().Model((*RefreshSession)(nil)).
		Set("revoked = 1").
		Set("successor_cache = NULL").
		Where("family_id = ?", familyID).
		Exec(ctx); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(live))
	for _, s := range live {
		ids = append(ids, s.AccessTokenID)
	}
	return ids, nil
}

func (r *refreshSessionRepo) CountFamilies(ctx context.Context, accountID string) (int, error) {
	var n int
	err := r.db.NewSelect().Model((*RefreshSession)(nil)).
		ColumnExpr("COUNT(DISTINCT family_id)").
		Where("account_id = ? AND revoked = 0", accountID).
		Scan(ctx, &n)
	return n, err
}

func (r *refreshSessionRepo) OldestFamily(ctx context.Context, accountID string) (string, error) {
	var familyID string
	err := r.db.NewSelect().Model((*RefreshSession)(nil)).
		Column("family_id").
		Where("account_id = ? AND revoked = 0", accountID).
		OrderExpr("family_created_at ASC").
		Limit(1).
		Scan(ctx, &familyID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return familyID, err
}

func (r *refreshSessionRepo) DeleteExpired(ctx context.Context, now int64) (int, error) {
	res, err := r.db.NewDelete().Model((*RefreshSession)(nil)).
		Where("expires_at < ?", now).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
