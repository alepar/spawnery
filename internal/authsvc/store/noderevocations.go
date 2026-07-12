package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
)

type nodeRevocationRepo struct{ db bun.IDB }

func (r *nodeRevocationRepo) Revoke(ctx context.Context, row NodeRevocation) (bool, error) {
	if row.NodeID == "" || row.IssuerSerial == "" || row.LeafSerial == "" || row.RevokedAt <= 0 {
		return false, errors.New("authsvc/store: invalid node certificate revocation")
	}
	result, err := r.db.NewInsert().Model(&row).On("CONFLICT (node_id) DO NOTHING").Exec(ctx)
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if inserted == 1 {
		return true, nil
	}
	existing, err := r.Get(ctx, row.NodeID)
	if err != nil {
		return false, err
	}
	if existing.IssuerSerial != row.IssuerSerial || existing.LeafSerial != row.LeafSerial {
		return false, fmt.Errorf("authsvc/store: node %s is already revoked with another certificate", row.NodeID)
	}
	_, err = r.db.NewUpdate().Model((*NodeRevocation)(nil)).
		Set("reason = ?", row.Reason).
		Where("node_id = ?", row.NodeID).
		Exec(ctx)
	return false, err
}

func (r *nodeRevocationRepo) Get(ctx context.Context, nodeID string) (NodeRevocation, error) {
	var row NodeRevocation
	err := r.db.NewSelect().Model(&row).Where("node_id = ?", nodeID).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return NodeRevocation{}, ErrNotFound
	}
	return row, err
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

func (r *nodeRevocationRepo) ListByIssuer(ctx context.Context, issuerSerial string) ([]NodeRevocation, error) {
	var rows []NodeRevocation
	err := r.db.NewSelect().Model(&rows).
		Where("issuer_serial = ?", issuerSerial).
		OrderExpr("leaf_serial ASC").
		Scan(ctx)
	return rows, err
}

func (r *nodeRevocationRepo) GetCRL(ctx context.Context, issuerSerial string) (NodeRevocationCRL, error) {
	var row NodeRevocationCRL
	err := r.db.NewSelect().Model(&row).Where("issuer_serial = ?", issuerSerial).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return NodeRevocationCRL{}, ErrNotFound
	}
	return row, err
}

func (r *nodeRevocationRepo) PutCRL(ctx context.Context, row NodeRevocationCRL) error {
	if row.IssuerSerial == "" || row.Number == "" || len(row.PEM) == 0 {
		return errors.New("authsvc/store: invalid node revocation CRL")
	}
	_, err := r.db.NewInsert().Model(&row).
		On("CONFLICT (issuer_serial) DO UPDATE").
		Set("number = EXCLUDED.number").
		Set("pem = EXCLUDED.pem").
		Exec(ctx)
	return err
}
