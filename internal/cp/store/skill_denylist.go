package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"
)

type skillDenylistRepo struct{ db bun.IDB }

// Deny upserts a denial: re-denying an already-denied sha updates reason/denied_by/created_at
// and returns no error (sp-mwco.3.2 §4.2 D6 — the kill switch must be idempotent under a retry).
func (r *skillDenylistRepo) Deny(ctx context.Context, d SkillObjectDenial) error {
	_, err := r.db.NewInsert().Model(&d).
		On("CONFLICT (sha256) DO UPDATE").
		Set("reason = EXCLUDED.reason").
		Set("denied_by = EXCLUDED.denied_by").
		Set("created_at = EXCLUDED.created_at").
		Exec(ctx)
	return err
}

// Allow removes a denial. ErrNotFound when the sha is not currently denied.
func (r *skillDenylistRepo) Allow(ctx context.Context, sha256 string) error {
	res, err := r.db.NewDelete().Model((*SkillObjectDenial)(nil)).Where("sha256 = ?", sha256).Exec(ctx)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// List returns every denial, ordered created_at DESC (newest first).
func (r *skillDenylistRepo) List(ctx context.Context) ([]SkillObjectDenial, error) {
	var out []SkillObjectDenial
	err := r.db.NewSelect().Model(&out).Order("created_at DESC").Scan(ctx)
	return out, err
}

// Denied looks up which of the given shas are currently denied, keyed by sha256. Denied(nil)
// or Denied([]) returns an empty map and issues no query.
func (r *skillDenylistRepo) Denied(ctx context.Context, shas []string) (map[string]SkillObjectDenial, error) {
	out := map[string]SkillObjectDenial{}
	if len(shas) == 0 {
		return out, nil
	}
	var rows []SkillObjectDenial
	if err := r.db.NewSelect().Model(&rows).Where("sha256 IN (?)", bun.In(shas)).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return nil, err
	}
	for _, row := range rows {
		out[row.SHA256] = row
	}
	return out, nil
}
