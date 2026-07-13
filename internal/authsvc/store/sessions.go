package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"
)

type refreshSessionRepo struct{ db bun.IDB }

const (
	maxLiveAccessGenerationsPerFamily        = 384
	maxLiveAccessGenerationsPerAccountSecond = 384
	maxAccessTokenIDBytes                    = 64
)

func (r *refreshSessionRepo) Get(ctx context.Context, tokenHash string) (RefreshSession, error) {
	var s RefreshSession
	err := r.db.NewSelect().Model(&s).Where("token_hash = ?", tokenHash).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return RefreshSession{}, ErrNotFound
	}
	return s, err
}

func (r *refreshSessionRepo) Insert(ctx context.Context, s RefreshSession) error {
	if top, ok := r.db.(*bun.DB); ok {
		return top.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			return (&refreshSessionRepo{db: tx}).insert(ctx, s)
		})
	}
	return r.insert(ctx, s)
}

func (r *refreshSessionRepo) insert(ctx context.Context, s RefreshSession) error {
	if s.AccessExpiresAt <= 0 || s.CPAccessTokenID == "" || s.NodeAccessTokenID == "" ||
		len(s.CPAccessTokenID) > maxAccessTokenIDBytes || len(s.NodeAccessTokenID) > maxAccessTokenIDBytes {
		return errors.New("authsvc/store: invalid paired access token")
	}
	var liveGenerations int
	if err := r.db.NewSelect().Model((*RefreshSession)(nil)).ColumnExpr("COUNT(*)").
		Where("family_id = ? AND revoked = 0 AND access_expires_at > ?", s.FamilyID, s.CreatedAt).
		Scan(ctx, &liveGenerations); err != nil {
		return err
	}
	if liveGenerations >= maxLiveAccessGenerationsPerFamily {
		return errors.New("authsvc/store: refresh family has too many live access generations")
	}
	var accountSecondGenerations int
	if err := r.db.NewSelect().Model((*RefreshSession)(nil)).ColumnExpr("COUNT(*)").
		Where("account_id = ? AND created_at = ? AND revoked = 0 AND access_expires_at > ?", s.AccountID, s.CreatedAt, s.CreatedAt).
		Scan(ctx, &accountSecondGenerations); err != nil {
		return err
	}
	if accountSecondGenerations >= maxLiveAccessGenerationsPerAccountSecond {
		return errors.New("authsvc/store: account has too many access generations in one second")
	}
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

func (r *refreshSessionRepo) RevokeFamily(ctx context.Context, familyID string, now int64) ([]RevokedToken, error) {
	return r.revoke(ctx, "family_id", familyID, now, false)
}

func (r *refreshSessionRepo) RevokeAccount(ctx context.Context, accountID string, now int64) ([]RevokedToken, error) {
	return r.revoke(ctx, "account_id", accountID, now, true)
}

func (r *refreshSessionRepo) revoke(ctx context.Context, column, value string, now int64, cutoffEvent bool) ([]RevokedToken, error) {
	var live []RefreshSession
	query := r.db.NewSelect().Model(&live).
		Column("cp_access_token_id", "node_access_token_id", "access_expires_at").
		Where(column+" = ? AND revoked = 0 AND access_expires_at > ?", value, now).
		OrderExpr("created_at ASC, token_hash ASC")
	if cutoffEvent {
		query = query.Where("created_at >= ?", now)
	}
	if err := query.Scan(ctx); err != nil {
		return nil, err
	}
	if _, err := r.db.NewUpdate().Model((*RefreshSession)(nil)).
		Set("revoked = 1").
		Set("successor_cache = NULL").
		Where(column+" = ?", value).
		Exec(ctx); err != nil {
		return nil, err
	}
	tokens := make([]RevokedToken, 0, len(live)*2)
	for _, s := range live {
		tokens = append(tokens,
			RevokedToken{TokenID: s.CPAccessTokenID, RetainUntil: s.AccessExpiresAt},
			RevokedToken{TokenID: s.NodeAccessTokenID, RetainUntil: s.AccessExpiresAt},
		)
	}
	return tokens, nil
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
