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
	result, err := r.db.NewInsert().Model(&row).On("CONFLICT DO NOTHING").Exec(ctx)
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
	existing, err := r.getCertificate(ctx, row.IssuerSerial, row.LeafSerial)
	if err != nil {
		return false, err
	}
	if existing.NodeID != row.NodeID {
		return false, fmt.Errorf("authsvc/store: certificate is already revoked for node %s", existing.NodeID)
	}
	_, err = r.db.NewUpdate().Model((*NodeRevocation)(nil)).
		Set("reason = ?", row.Reason).
		Where("issuer_serial = ? AND leaf_serial = ?", row.IssuerSerial, row.LeafSerial).
		Exec(ctx)
	return false, err
}

func (r *nodeRevocationRepo) getCertificate(ctx context.Context, issuerSerial, leafSerial string) (NodeRevocation, error) {
	var row NodeRevocation
	err := r.db.NewSelect().Model(&row).
		Where("issuer_serial = ? AND leaf_serial = ?", issuerSerial, leafSerial).
		Limit(1).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return NodeRevocation{}, ErrNotFound
	}
	return row, err
}

func (r *nodeRevocationRepo) ListByIssuer(ctx context.Context, issuerSerial string) ([]NodeRevocation, error) {
	var rows []NodeRevocation
	err := r.db.NewSelect().Model(&rows).
		Where("issuer_serial = ?", issuerSerial).
		OrderExpr("leaf_serial ASC").
		Scan(ctx)
	return rows, err
}

func (r *nodeRevocationRepo) ListLegacy(ctx context.Context) ([]NodeRevocation, error) {
	var rows []NodeRevocation
	err := r.db.NewSelect().Model(&rows).
		Where("issuer_serial = '' OR leaf_serial = ''").
		OrderExpr("node_id ASC, id ASC").
		Scan(ctx)
	return rows, err
}

func (r *nodeRevocationRepo) ReconcileLegacy(ctx context.Context, nodeID, issuerSerial, leafSerial string) error {
	if nodeID == "" || issuerSerial == "" || leafSerial == "" {
		return errors.New("authsvc/store: invalid legacy node revocation reconciliation")
	}
	result, err := r.db.NewUpdate().Model((*NodeRevocation)(nil)).
		Set("issuer_serial = ?", issuerSerial).
		Set("leaf_serial = ?", leafSerial).
		Where("node_id = ? AND issuer_serial = '' AND leaf_serial = ''", nodeID).
		Exec(ctx)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return fmt.Errorf("authsvc/store: legacy node revocation %s matched %d rows", nodeID, updated)
	}
	return nil
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
