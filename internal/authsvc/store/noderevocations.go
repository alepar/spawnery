package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"
)

type nodeRevocationRepo struct{ db bun.IDB }

func (r *nodeRevocationRepo) Revoke(ctx context.Context, nodeID, reason string, revokedAt int64) error {
	row := NodeRevocation{NodeID: nodeID, Reason: reason, RevokedAt: revokedAt}
	_, err := r.db.NewInsert().Model(&row).
		On("CONFLICT (node_id) DO UPDATE").
		Set("reason = EXCLUDED.reason").
		Set("revoked_at = EXCLUDED.revoked_at").
		Exec(ctx)
	return err
}

func (r *nodeRevocationRepo) IsRevoked(ctx context.Context, nodeID string) (bool, error) {
	var row NodeRevocation
	err := r.db.NewSelect().Model(&row).
		Where("node_id = ?", nodeID).
		Limit(1).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *nodeRevocationRepo) List(ctx context.Context) ([]NodeRevocation, error) {
	var rows []NodeRevocation
	err := r.db.NewSelect().Model(&rows).
		OrderExpr("node_id ASC").
		Scan(ctx)
	return rows, err
}
