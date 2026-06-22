package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/uptrace/bun"
)

type customizationCatalogRepo struct{ db bun.IDB }

// Create inserts a new catalog entry. Returns an error on duplicate catalog_id.
func (r *customizationCatalogRepo) Create(ctx context.Context, e CustomizationCatalogEntry) error {
	_, err := r.db.NewInsert().Model(&e).Exec(ctx)
	return err
}

// Get returns the entry for catalogID. ErrNotFound when absent.
func (r *customizationCatalogRepo) Get(ctx context.Context, catalogID string) (CustomizationCatalogEntry, error) {
	var e CustomizationCatalogEntry
	err := r.db.NewSelect().Model(&e).Where("catalog_id = ?", catalogID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return CustomizationCatalogEntry{}, ErrNotFound
	}
	return e, err
}

// List returns only listed=true entries, ordered by name ASC.
func (r *customizationCatalogRepo) List(ctx context.Context) ([]CustomizationCatalogEntry, error) {
	var out []CustomizationCatalogEntry
	err := r.db.NewSelect().Model(&out).Where("listed = ?", true).Order("name ASC").Scan(ctx)
	return out, err
}

// ListByCreator returns all entries for the given creator (including unlisted), ordered by name ASC.
func (r *customizationCatalogRepo) ListByCreator(ctx context.Context, creatorID string) ([]CustomizationCatalogEntry, error) {
	var out []CustomizationCatalogEntry
	err := r.db.NewSelect().Model(&out).Where("creator_id = ?", creatorID).Order("name ASC").Scan(ctx)
	return out, err
}

// Update replaces name, description, and content for an entry. ErrNotFound when absent.
func (r *customizationCatalogRepo) Update(ctx context.Context, catalogID string, name, description string, content []byte, now int64) error {
	res, err := r.db.NewUpdate().Model((*CustomizationCatalogEntry)(nil)).
		Set("name = ?", name).
		Set("description = ?", description).
		Set("content = ?", content).
		Set("updated_at = ?", now).
		Where("catalog_id = ?", catalogID).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetListed sets the listing visibility of an entry. ErrNotFound when absent.
func (r *customizationCatalogRepo) SetListed(ctx context.Context, catalogID string, listed bool) error {
	res, err := r.db.NewUpdate().Model((*CustomizationCatalogEntry)(nil)).
		Set("listed = ?", listed).
		Where("catalog_id = ?", catalogID).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes an entry. ErrNotFound when absent.
func (r *customizationCatalogRepo) Delete(ctx context.Context, catalogID string) error {
	res, err := r.db.NewDelete().Model((*CustomizationCatalogEntry)(nil)).
		Where("catalog_id = ?", catalogID).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetByCreatorSHA returns the entry for (creatorID, sha256hex). ErrNotFound when absent.
func (r *customizationCatalogRepo) GetByCreatorSHA(ctx context.Context, creatorID, sha256hex string) (CustomizationCatalogEntry, error) {
	var e CustomizationCatalogEntry
	err := r.db.NewSelect().Model(&e).
		Where("creator_id = ? AND sha256 = ?", creatorID, sha256hex).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return CustomizationCatalogEntry{}, ErrNotFound
	}
	return e, err
}

// CreateSkill inserts a URL-ingested skill entry. Maps a unique-constraint violation on
// (creator_id, sha256) to ErrConflict.
func (r *customizationCatalogRepo) CreateSkill(ctx context.Context, e CustomizationCatalogEntry) error {
	_, err := r.db.NewInsert().Model(&e).Exec(ctx)
	if err != nil && isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// isUniqueViolation reports whether err is a unique-constraint violation from the SQLite or
// Postgres driver. There is no cross-driver sentinel; we inspect the error message.
// SQLite: "UNIQUE constraint failed: ..." (modernc/mattn drivers)
// Postgres: SQLSTATE 23505 "duplicate key value violates unique constraint"
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || // SQLite
		strings.Contains(msg, "duplicate key value violates unique constraint") || // Postgres
		strings.Contains(msg, "23505") // Postgres SQLSTATE in some driver wrappers
}
