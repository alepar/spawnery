package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
)

type profileRepo struct{ db bun.IDB }

// runInTx runs fn in a transaction. If r.db is already a bun.Tx, fn runs inline
// (mirrors the bunStore.WithTx flat-composition pattern).
func (r *profileRepo) runInTx(ctx context.Context, fn func(db bun.IDB) error) error {
	top, ok := r.db.(*bun.DB)
	if !ok {
		return fn(r.db) // already inside a tx — run inline
	}
	return top.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return fn(tx)
	})
}

// Create inserts a new Profile row (version must be 1 for a fresh profile).
func (r *profileRepo) Create(ctx context.Context, p Profile) error {
	if p.Version == 0 {
		p.Version = 1
	}
	_, err := r.db.NewInsert().Model(&p).Exec(ctx)
	return err
}

// Get loads a profile and all its entries + secret refs. Returns ErrNotFound when absent.
func (r *profileRepo) Get(ctx context.Context, profileID string) (Profile, []ProfileEntry, []ProfileSecret, error) {
	var p Profile
	err := r.db.NewSelect().Model(&p).Where("profile_id = ?", profileID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, nil, nil, ErrNotFound
	}
	if err != nil {
		return Profile{}, nil, nil, err
	}

	var entries []ProfileEntry
	if err := r.db.NewSelect().Model(&entries).
		Where("profile_id = ?", profileID).
		Order("entry_id ASC").
		Scan(ctx); err != nil {
		return Profile{}, nil, nil, err
	}
	for i := range entries {
		if err := decodeProfileEntry(&entries[i]); err != nil {
			return Profile{}, nil, nil, err
		}
	}

	var secrets []ProfileSecret
	if err := r.db.NewSelect().Model(&secrets).
		Where("profile_id = ?", profileID).
		Order("secret_id ASC").
		Scan(ctx); err != nil {
		return Profile{}, nil, nil, err
	}

	return p, entries, secrets, nil
}

// ListByOwner returns all profiles owned by the given owner.
func (r *profileRepo) ListByOwner(ctx context.Context, ownerID string) ([]Profile, error) {
	var profiles []Profile
	if err := r.db.NewSelect().Model(&profiles).
		Where("owner_id = ?", ownerID).
		Scan(ctx); err != nil {
		return nil, err
	}
	return profiles, nil
}

// Rename CAS-renames a profile. Bumps version and updated_at.
// Returns ErrNotFound when the profile is absent, ErrConflict when expectedVersion is stale.
func (r *profileRepo) Rename(ctx context.Context, profileID string, expectedVersion uint64, name string, now int64) (uint64, error) {
	return r.casUpdate(ctx, profileID, expectedVersion, now, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Set("name = ?", name)
	})
}

// Delete removes the profile and its children (entries and secret refs) atomically.
func (r *profileRepo) Delete(ctx context.Context, profileID string) error {
	return r.runInTx(ctx, func(db bun.IDB) error {
		if _, err := db.NewDelete().Model((*ProfileSecret)(nil)).Where("profile_id = ?", profileID).Exec(ctx); err != nil {
			return err
		}
		if _, err := db.NewDelete().Model((*ProfileEntry)(nil)).Where("profile_id = ?", profileID).Exec(ctx); err != nil {
			return err
		}
		res, err := db.NewDelete().Model((*Profile)(nil)).Where("profile_id = ?", profileID).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// AddEntry CAS-inserts an entry into the profile atomically.
func (r *profileRepo) AddEntry(ctx context.Context, profileID string, expectedVersion uint64, e ProfileEntry, now int64) (uint64, error) {
	e.ProfileID = profileID
	if err := encodeProfileEntry(&e); err != nil {
		return 0, err
	}
	var newVersion uint64
	err := r.runInTx(ctx, func(db bun.IDB) error {
		ver, uerr := casUpdateDB(ctx, db, profileID, expectedVersion, now, func(q *bun.UpdateQuery) *bun.UpdateQuery {
			return q
		})
		if uerr != nil {
			return uerr
		}
		newVersion = ver
		if _, ierr := db.NewInsert().Model(&e).Exec(ctx); ierr != nil {
			return ierr
		}
		return nil
	})
	return newVersion, err
}

// UpdateEntryPin CAS-repins a bundle_ref entry onto a new version + overrides atomically
// (sp-mwco.1.8 §4.9 — RepinProfileBundle's store call). Returns ErrNotFound when the profile
// version matches but the entry row is absent, ErrConflict when expectedVersion is stale.
func (r *profileRepo) UpdateEntryPin(ctx context.Context, profileID string, expectedVersion uint64, entryID, versionID, overridesJSON string, now int64) (uint64, error) {
	var newVersion uint64
	err := r.runInTx(ctx, func(db bun.IDB) error {
		ver, uerr := casUpdateDB(ctx, db, profileID, expectedVersion, now, func(q *bun.UpdateQuery) *bun.UpdateQuery {
			return q
		})
		if uerr != nil {
			return uerr
		}
		newVersion = ver
		res, uerr := db.NewUpdate().Model((*ProfileEntry)(nil)).
			Set("version_id = ?", versionID).
			Set("bundle_overrides = ?", overridesJSON).
			Where("profile_id = ? AND entry_id = ?", profileID, entryID).
			Exec(ctx)
		if uerr != nil {
			return uerr
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
	return newVersion, err
}

// RemoveEntry CAS-removes an entry from the profile atomically.
func (r *profileRepo) RemoveEntry(ctx context.Context, profileID string, expectedVersion uint64, entryID string, now int64) (uint64, error) {
	var newVersion uint64
	err := r.runInTx(ctx, func(db bun.IDB) error {
		ver, uerr := casUpdateDB(ctx, db, profileID, expectedVersion, now, func(q *bun.UpdateQuery) *bun.UpdateQuery {
			return q
		})
		if uerr != nil {
			return uerr
		}
		newVersion = ver
		if _, derr := db.NewDelete().Model((*ProfileEntry)(nil)).
			Where("profile_id = ? AND entry_id = ?", profileID, entryID).
			Exec(ctx); derr != nil {
			return derr
		}
		return nil
	})
	return newVersion, err
}

// AddSecretRef CAS-adds a secret reference to the profile atomically.
func (r *profileRepo) AddSecretRef(ctx context.Context, profileID string, expectedVersion uint64, secretID string, now int64) (uint64, error) {
	var newVersion uint64
	err := r.runInTx(ctx, func(db bun.IDB) error {
		ver, uerr := casUpdateDB(ctx, db, profileID, expectedVersion, now, func(q *bun.UpdateQuery) *bun.UpdateQuery {
			return q
		})
		if uerr != nil {
			return uerr
		}
		newVersion = ver
		ps := ProfileSecret{ProfileID: profileID, SecretID: secretID}
		if _, ierr := db.NewInsert().Model(&ps).Exec(ctx); ierr != nil {
			return ierr
		}
		return nil
	})
	return newVersion, err
}

// RemoveSecretRef CAS-removes a secret reference from the profile atomically.
func (r *profileRepo) RemoveSecretRef(ctx context.Context, profileID string, expectedVersion uint64, secretID string, now int64) (uint64, error) {
	var newVersion uint64
	err := r.runInTx(ctx, func(db bun.IDB) error {
		ver, uerr := casUpdateDB(ctx, db, profileID, expectedVersion, now, func(q *bun.UpdateQuery) *bun.UpdateQuery {
			return q
		})
		if uerr != nil {
			return uerr
		}
		newVersion = ver
		if _, derr := db.NewDelete().Model((*ProfileSecret)(nil)).
			Where("profile_id = ? AND secret_id = ?", profileID, secretID).
			Exec(ctx); derr != nil {
			return derr
		}
		return nil
	})
	return newVersion, err
}

// ListProfileIDsByCatalogRef returns the distinct profile_ids of profiles that contain at least
// one catalog_ref entry pointing to the given catalogID. Empty slice (not error) when none match.
func (r *profileRepo) ListProfileIDsByCatalogRef(ctx context.Context, catalogID string) ([]string, error) {
	var ids []string
	err := r.db.NewSelect().
		TableExpr("profile_entries").
		ColumnExpr("DISTINCT profile_id").
		Where("source_kind = ? AND catalog_id = ?", string(ProfileSourceCatalog), catalogID).
		Scan(ctx, &ids)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// ListProfileIDsByBundleVersions returns the distinct profile_ids of profiles that contain at
// least one bundle_ref entry pinned to one of the given versionIDs. Empty slice (not error) when
// versionIDs is empty or none match — mirrors ListProfileIDsByCatalogRef's never-nil contract.
func (r *profileRepo) ListProfileIDsByBundleVersions(ctx context.Context, versionIDs []string) ([]string, error) {
	if len(versionIDs) == 0 {
		return []string{}, nil
	}
	var ids []string
	err := r.db.NewSelect().
		TableExpr("profile_entries").
		ColumnExpr("DISTINCT profile_id").
		Where("source_kind = ? AND version_id IN (?)", string(ProfileSourceBundle), bun.In(versionIDs)).
		Scan(ctx, &ids)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// CountRefsByCatalogRef returns the number of distinct profiles, and the number of distinct
// owners of those profiles, that contain a catalog_ref entry pointing to catalogID. Zero refs
// returns (0, 0, nil) — never an error.
func (r *profileRepo) CountRefsByCatalogRef(ctx context.Context, catalogID string) (int, int, error) {
	var row struct {
		Profiles int `bun:"profiles"`
		Owners   int `bun:"owners"`
	}
	err := r.db.NewSelect().
		TableExpr("profile_entries AS pe").
		Join("JOIN profiles AS pf ON pf.profile_id = pe.profile_id").
		ColumnExpr("COUNT(DISTINCT pe.profile_id) AS profiles").
		ColumnExpr("COUNT(DISTINCT pf.owner_id) AS owners").
		Where("pe.source_kind = ? AND pe.catalog_id = ?", string(ProfileSourceCatalog), catalogID).
		Scan(ctx, &row)
	if err != nil {
		return 0, 0, err
	}
	return row.Profiles, row.Owners, nil
}

// ListProfileIDsByBundleRef returns the distinct profile_ids of profiles that contain at least
// one bundle_ref entry pinned to the given bundleID. Empty slice (not error) when none match.
func (r *profileRepo) ListProfileIDsByBundleRef(ctx context.Context, bundleID string) ([]string, error) {
	var ids []string
	err := r.db.NewSelect().
		TableExpr("profile_entries").
		ColumnExpr("DISTINCT profile_id").
		Where("source_kind = ? AND bundle_id = ?", string(ProfileSourceBundle), bundleID).
		Scan(ctx, &ids)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// ListProfileIDsByBundleVersionRef returns the distinct profile_ids of profiles that contain at
// least one bundle_ref entry pinned to the given versionID. Empty slice (not error) when none
// match.
func (r *profileRepo) ListProfileIDsByBundleVersionRef(ctx context.Context, versionID string) ([]string, error) {
	var ids []string
	err := r.db.NewSelect().
		TableExpr("profile_entries").
		ColumnExpr("DISTINCT profile_id").
		Where("source_kind = ? AND version_id = ?", string(ProfileSourceBundle), versionID).
		Scan(ctx, &ids)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// CountBundleRefs returns the number of distinct profiles, and the number of distinct owners of
// those profiles, that contain a bundle_ref entry pinned to bundleID. Zero refs returns
// (0, 0, nil) — never an error. Mirrors CountRefsByCatalogRef's shape.
func (r *profileRepo) CountBundleRefs(ctx context.Context, bundleID string) (int, int, error) {
	var row struct {
		Profiles int `bun:"profiles"`
		Owners   int `bun:"owners"`
	}
	err := r.db.NewSelect().
		TableExpr("profile_entries AS pe").
		Join("JOIN profiles AS pf ON pf.profile_id = pe.profile_id").
		ColumnExpr("COUNT(DISTINCT pe.profile_id) AS profiles").
		ColumnExpr("COUNT(DISTINCT pf.owner_id) AS owners").
		Where("pe.source_kind = ? AND pe.bundle_id = ?", string(ProfileSourceBundle), bundleID).
		Scan(ctx, &row)
	if err != nil {
		return 0, 0, err
	}
	return row.Profiles, row.Owners, nil
}

// CountBundleVersionRefs returns the number of distinct profiles, and the number of distinct
// owners of those profiles, that contain a bundle_ref entry pinned to versionID. Zero refs
// returns (0, 0, nil) — never an error.
func (r *profileRepo) CountBundleVersionRefs(ctx context.Context, versionID string) (int, int, error) {
	var row struct {
		Profiles int `bun:"profiles"`
		Owners   int `bun:"owners"`
	}
	err := r.db.NewSelect().
		TableExpr("profile_entries AS pe").
		Join("JOIN profiles AS pf ON pf.profile_id = pe.profile_id").
		ColumnExpr("COUNT(DISTINCT pe.profile_id) AS profiles").
		ColumnExpr("COUNT(DISTINCT pf.owner_id) AS owners").
		Where("pe.source_kind = ? AND pe.version_id = ?", string(ProfileSourceBundle), versionID).
		Scan(ctx, &row)
	if err != nil {
		return 0, 0, err
	}
	return row.Profiles, row.Owners, nil
}

// casUpdate is a convenience wrapper around casUpdateDB using r.db.
func (r *profileRepo) casUpdate(ctx context.Context, profileID string, expectedVersion uint64, now int64, extra func(*bun.UpdateQuery) *bun.UpdateQuery) (uint64, error) {
	return casUpdateDB(ctx, r.db, profileID, expectedVersion, now, extra)
}

// casUpdateDB is the shared CAS guard: UPDATE profiles SET version=version+1, updated_at=now [, extras]
// WHERE profile_id=? AND version=?. Distinguishes missing vs stale via a SELECT after rowcount==0.
func casUpdateDB(ctx context.Context, db bun.IDB, profileID string, expectedVersion uint64, now int64, extra func(*bun.UpdateQuery) *bun.UpdateQuery) (uint64, error) {
	q := db.NewUpdate().Model((*Profile)(nil)).
		Set("version = version + 1").
		Set("updated_at = ?", now).
		Where("profile_id = ? AND version = ?", profileID, expectedVersion)
	q = extra(q)
	res, err := q.Exec(ctx)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return expectedVersion + 1, nil
	}
	// rowcount 0: distinguish missing from stale.
	exists, err := db.NewSelect().Model((*Profile)(nil)).
		Where("profile_id = ?", profileID).Exists(ctx)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, ErrNotFound
	}
	return 0, ErrConflict
}

// --- JSON helpers -----------------------------------------------------------

// bundleOverrides is the JSON shape of ProfileEntry.BundleOverridesJSON (sp-mwco.1.8 §4.4):
// {"exclude":["skills/foo"],"rename":{"skills/bar":"bar-2"}}, keyed by member source_subdir.
type bundleOverrides struct {
	Exclude []string          `json:"exclude,omitempty"`
	Rename  map[string]string `json:"rename,omitempty"`
}

// EncodeBundleOverrides marshals exclude/rename overrides into the bundle_overrides column shape.
// Returns "" when both are empty — the non-bundle-entry / no-overrides default. Exported so the cp
// package can build the same JSON shape for ProfileRepo.UpdateEntryPin without duplicating it.
func EncodeBundleOverrides(exclude []string, rename map[string]string) (string, error) {
	if len(exclude) == 0 && len(rename) == 0 {
		return "", nil
	}
	b, err := json.Marshal(bundleOverrides{Exclude: exclude, Rename: rename})
	if err != nil {
		return "", fmt.Errorf("store: encode bundle overrides: %w", err)
	}
	return string(b), nil
}

// encodeProfileEntry marshals Targets, MCPSecretRefs, and (for bundle_ref entries)
// ExcludeSubdirs/RenameSubdirs into their JSON columns. Empty Targets defaults to ["all"].
func encodeProfileEntry(e *ProfileEntry) error {
	targets := e.Targets
	if len(targets) == 0 {
		targets = []string{"all"}
	}
	tb, err := json.Marshal(targets)
	if err != nil {
		return fmt.Errorf("store: encode entry targets: %w", err)
	}
	e.TargetsJSON = string(tb)

	refs := e.MCPSecretRefs
	if refs == nil {
		refs = []string{}
	}
	rb, err := json.Marshal(refs)
	if err != nil {
		return fmt.Errorf("store: encode entry mcp_secret_refs: %w", err)
	}
	e.SecretRefsJSON = string(rb)

	if e.SourceKind == ProfileSourceBundle {
		ov, err := EncodeBundleOverrides(e.ExcludeSubdirs, e.RenameSubdirs)
		if err != nil {
			return err
		}
		e.BundleOverridesJSON = ov
	} else {
		e.BundleOverridesJSON = ""
	}
	return nil
}

// decodeProfileEntry unmarshals the JSON text columns back into slice/map fields.
func decodeProfileEntry(e *ProfileEntry) error {
	if e.TargetsJSON != "" {
		if err := json.Unmarshal([]byte(e.TargetsJSON), &e.Targets); err != nil {
			return fmt.Errorf("store: decode entry targets: %w", err)
		}
	}
	if e.SecretRefsJSON != "" {
		if err := json.Unmarshal([]byte(e.SecretRefsJSON), &e.MCPSecretRefs); err != nil {
			return fmt.Errorf("store: decode entry mcp_secret_refs: %w", err)
		}
	}
	if e.BundleOverridesJSON != "" {
		var ov bundleOverrides
		if err := json.Unmarshal([]byte(e.BundleOverridesJSON), &ov); err != nil {
			return fmt.Errorf("store: decode entry bundle_overrides: %w", err)
		}
		e.ExcludeSubdirs = ov.Exclude
		e.RenameSubdirs = ov.Rename
	}
	return nil
}

// Compile-time check that *profileRepo fully implements ProfileRepo.
var _ ProfileRepo = (*profileRepo)(nil)
