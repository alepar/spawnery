package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"
)

type appRepo struct{ db bun.IDB }

func (r *appRepo) Upsert(ctx context.Context, a App) error {
	_, err := r.db.NewInsert().Model(&a).
		On("CONFLICT (id) DO UPDATE").
		Set("display_name = EXCLUDED.display_name").
		Set("summary = EXCLUDED.summary").
		Set("tags = EXCLUDED.tags").
		Set("visibility = EXCLUDED.visibility").
		Set("listed = EXCLUDED.listed").
		Exec(ctx)
	return err
}

func (r *appRepo) Get(ctx context.Context, id string) (App, error) {
	var a App
	err := r.db.NewSelect().Model(&a).Where("id = ?", id).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	return a, err
}

func (r *appRepo) List(ctx context.Context) ([]App, error) {
	var out []App
	err := r.db.NewSelect().Model(&out).Order("id ASC").Scan(ctx)
	return out, err
}

func (r *appRepo) UpsertVersion(ctx context.Context, v AppVersion, mounts []MountDecl) error {
	if _, err := r.db.NewInsert().Model(&v).
		On("CONFLICT (app_id, version) DO UPDATE").
		Set("ref = EXCLUDED.ref").Set("tier = EXCLUDED.tier").
		Exec(ctx); err != nil {
		return err
	}
	for i := range mounts {
		if _, err := r.db.NewInsert().Model(&mounts[i]).
			On("CONFLICT (app_id, version, name) DO UPDATE").
			Set("required = EXCLUDED.required").
			Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (r *appRepo) GetVersion(ctx context.Context, appID, version string) (AppVersion, error) {
	var v AppVersion
	err := r.db.NewSelect().Model(&v).Where("app_id = ? AND version = ?", appID, version).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return AppVersion{}, ErrNotFound
	}
	return v, err
}

func (r *appRepo) LatestReviewed(ctx context.Context, appID string) (AppVersion, error) {
	var v AppVersion
	err := r.db.NewSelect().Model(&v).
		Where("app_id = ? AND tier = ?", appID, TierReviewed).
		Order("created_at DESC").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return AppVersion{}, ErrNotFound
	}
	return v, err
}

func (r *appRepo) DeclaredMounts(ctx context.Context, appID, version string) ([]MountDecl, error) {
	var out []MountDecl
	err := r.db.NewSelect().Model(&out).
		Where("app_id = ? AND version = ?", appID, version).
		Order("name ASC").Scan(ctx)
	return out, err
}
