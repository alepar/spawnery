package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"
)

type skillBundleRepo struct{ db bun.IDB }

// runInTx runs fn in a transaction. If r.db is already a bun.Tx, fn runs inline (mirrors
// profileRepo.runInTx / bunStore.WithTx's flat-composition pattern).
func (r *skillBundleRepo) runInTx(ctx context.Context, fn func(db bun.IDB) error) error {
	top, ok := r.db.(*bun.DB)
	if !ok {
		return fn(r.db) // already inside a tx — run inline
	}
	return top.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return fn(tx)
	})
}

// Create inserts a new bundle. Maps a unique-constraint violation on
// (creator_id, source_url, source_ref, source_subdir) to ErrConflict.
func (r *skillBundleRepo) Create(ctx context.Context, b SkillBundle) error {
	_, err := r.db.NewInsert().Model(&b).Exec(ctx)
	if err != nil && isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// Get returns the bundle for bundleID. ErrNotFound when absent.
func (r *skillBundleRepo) Get(ctx context.Context, bundleID string) (SkillBundle, error) {
	var b SkillBundle
	err := r.db.NewSelect().Model(&b).Where("bundle_id = ?", bundleID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return SkillBundle{}, ErrNotFound
	}
	return b, err
}

// GetByKey returns the bundle for (creatorID, url, ref, subdir) — the re-paste lookup.
// ErrNotFound when absent.
func (r *skillBundleRepo) GetByKey(ctx context.Context, creatorID, url, ref, subdir string) (SkillBundle, error) {
	var b SkillBundle
	err := r.db.NewSelect().Model(&b).
		Where("creator_id = ? AND source_url = ? AND source_ref = ? AND source_subdir = ?", creatorID, url, ref, subdir).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return SkillBundle{}, ErrNotFound
	}
	return b, err
}

// ListByCreator returns all bundles for the given creator, ordered by name ASC.
func (r *skillBundleRepo) ListByCreator(ctx context.Context, creatorID string) ([]SkillBundle, error) {
	var out []SkillBundle
	err := r.db.NewSelect().Model(&out).Where("creator_id = ?", creatorID).Order("name ASC").Scan(ctx)
	return out, err
}

// CreateVersion inserts a version and its members atomically. seq is assumed already set by the
// caller (allocation, e.g. MAX(seq)+1, is the caller's job inside its own tx). Maps a
// unique-constraint violation — on (bundle_id, seq), or on (version_id, source_subdir) for a
// duplicate member source_subdir — to ErrConflict.
func (r *skillBundleRepo) CreateVersion(ctx context.Context, v SkillBundleVersion, members []SkillBundleMember) error {
	return r.runInTx(ctx, func(db bun.IDB) error {
		if _, err := db.NewInsert().Model(&v).Exec(ctx); err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		for i := range members {
			members[i].VersionID = v.VersionID
			if _, err := db.NewInsert().Model(&members[i]).Exec(ctx); err != nil {
				if isUniqueViolation(err) {
					return ErrConflict
				}
				return err
			}
		}
		return nil
	})
}

// LatestVersion returns the version with the highest seq for bundleID. ErrNotFound when the
// bundle has no versions.
func (r *skillBundleRepo) LatestVersion(ctx context.Context, bundleID string) (SkillBundleVersion, error) {
	var v SkillBundleVersion
	err := r.db.NewSelect().Model(&v).
		Where("bundle_id = ?", bundleID).
		Order("seq DESC").
		Limit(1).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return SkillBundleVersion{}, ErrNotFound
	}
	return v, err
}

// GetVersion returns the version for versionID. ErrNotFound when absent.
func (r *skillBundleRepo) GetVersion(ctx context.Context, versionID string) (SkillBundleVersion, error) {
	var v SkillBundleVersion
	err := r.db.NewSelect().Model(&v).Where("version_id = ?", versionID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return SkillBundleVersion{}, ErrNotFound
	}
	return v, err
}

// ListVersions returns all versions of bundleID, ordered by seq ASC.
func (r *skillBundleRepo) ListVersions(ctx context.Context, bundleID string) ([]SkillBundleVersion, error) {
	var out []SkillBundleVersion
	err := r.db.NewSelect().Model(&out).Where("bundle_id = ?", bundleID).Order("seq ASC").Scan(ctx)
	return out, err
}

// Members returns the members of versionID, ordered by position ASC.
func (r *skillBundleRepo) Members(ctx context.Context, versionID string) ([]SkillBundleMember, error) {
	var out []SkillBundleMember
	err := r.db.NewSelect().Model(&out).Where("version_id = ?", versionID).Order("position ASC").Scan(ctx)
	return out, err
}

// MemberVersionIDs returns every version_id that references catalogID as a member — the
// kill-switch (sp-mwco.1.6) and delete reference-check (sp-mwco.3.3) query.
func (r *skillBundleRepo) MemberVersionIDs(ctx context.Context, catalogID string) ([]string, error) {
	var out []string
	err := r.db.NewSelect().Model((*SkillBundleMember)(nil)).
		Column("version_id").
		Where("catalog_id = ?", catalogID).
		Scan(ctx, &out)
	return out, err
}

// SetETag updates the bundle's conditional-refetch etag. ErrNotFound when absent.
func (r *skillBundleRepo) SetETag(ctx context.Context, bundleID, etag string, now int64) error {
	res, err := r.db.NewUpdate().Model((*SkillBundle)(nil)).
		Set("etag = ?", etag).
		Set("updated_at = ?", now).
		Where("bundle_id = ?", bundleID).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
