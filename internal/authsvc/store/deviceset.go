package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"
)

type deviceSetRepo struct{ db bun.IDB }

// Head returns the latest version and head hash for accountID.
func (r *deviceSetRepo) Head(ctx context.Context, accountID string) (headHash []byte, version uint64, found bool, err error) {
	var row DeviceSetEntry
	err = r.db.NewSelect().Model(&row).
		Where("account_id = ?", accountID).
		OrderExpr("version DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, false, nil
		}
		return nil, 0, false, err
	}
	return row.HeadHash, row.Version, true, nil
}

// Append CAS-appends one entry.  The CAS is performed inside a transaction so
// no two concurrent callers can both observe "no head" and both succeed.
func (r *deviceSetRepo) Append(ctx context.Context, accountID string, version uint64, prevHash, headHash, entryBytes []byte, now int64) error {
	// If we are already inside a tx (bun.Tx), r.db.RunInTx will start a nested
	// tx on SQLite which errors — but that path never happens because the HTTP
	// handler calls Append directly (not inside WithTx).  If we ever need to
	// call Append inside a WithTx, the caller wraps with its own tx and passes
	// a tx-backed repo; RunInTx on a bun.Tx will just call fn with the same tx.
	top, ok := r.db.(*bun.DB)
	if !ok {
		// Already inside a tx — run the CAS inline.
		return r.appendCAS(ctx, r.db, accountID, version, prevHash, headHash, entryBytes, now)
	}
	return top.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return r.appendCAS(ctx, tx, accountID, version, prevHash, headHash, entryBytes, now)
	})
}

func (r *deviceSetRepo) appendCAS(ctx context.Context, db bun.IDB, accountID string, version uint64, prevHash, headHash, entryBytes []byte, now int64) error {
	// Read current head inside the tx.
	var head DeviceSetEntry
	err := db.NewSelect().Model(&head).
		Where("account_id = ?", accountID).
		OrderExpr("version DESC").
		Limit(1).
		Scan(ctx)

	noHead := errors.Is(err, sql.ErrNoRows)
	if err != nil && !noHead {
		return err
	}

	if noHead {
		// Genesis: must be version 1 with no prevHash.
		if version != 1 || len(prevHash) != 0 {
			return ErrConflict
		}
	} else {
		// Subsequent entry: prevHash must match stored head and version must be consecutive.
		if version != head.Version+1 {
			return ErrConflict
		}
		if !bytes.Equal(prevHash, head.HeadHash) {
			return ErrConflict
		}
	}

	row := DeviceSetEntry{
		AccountID:  accountID,
		Version:    version,
		PrevHash:   prevHash,
		HeadHash:   headHash,
		EntryBytes: entryBytes,
		CreatedAt:  now,
	}
	_, err = db.NewInsert().Model(&row).Exec(ctx)
	return err
}

// FetchAll returns the StoredEntry bytes for all entries in ascending version order.
func (r *deviceSetRepo) FetchAll(ctx context.Context, accountID string) ([][]byte, error) {
	var rows []DeviceSetEntry
	err := r.db.NewSelect().Model(&rows).
		Where("account_id = ?", accountID).
		OrderExpr("version ASC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, len(rows))
	for i, row := range rows {
		out[i] = row.EntryBytes
	}
	return out, nil
}
