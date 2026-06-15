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

// encodeProfileEntry marshals Targets and MCPSecretRefs into their JSON columns.
// Empty Targets defaults to ["all"].
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
	return nil
}

// decodeProfileEntry unmarshals the JSON text columns back into slice fields.
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
	return nil
}

// Compile-time check that *profileRepo fully implements ProfileRepo.
var _ ProfileRepo = (*profileRepo)(nil)
