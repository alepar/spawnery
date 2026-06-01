package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"

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

func (r *appRepo) Catalog(ctx context.Context, f CatalogFilter) ([]CatalogEntry, error) {
	var apps []App
	q := r.db.NewSelect().Model(&apps).
		Where("listed = ?", true).Where("visibility = ?", "public")
	if f.Query != "" {
		like := "%" + strings.ToLower(f.Query) + "%"
		q = q.Where("(LOWER(display_name) LIKE ? OR LOWER(summary) LIKE ? OR LOWER(tags) LIKE ?)", like, like, like)
	}
	if err := q.Order("display_name ASC").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]CatalogEntry, 0, len(apps))
	for _, a := range apps {
		var v AppVersion
		err := r.db.NewSelect().Model(&v).
			Where("app_id = ?", a.ID).Order("created_at DESC").Limit(1).Scan(ctx)
		e := CatalogEntry{App: a}
		if err == nil {
			e.LatestVersion, e.LatestTier = v.Version, v.Tier
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return tierRank(out[i].LatestTier) > tierRank(out[j].LatestTier)
	})
	return out, nil
}

func tierRank(t Tier) int {
	switch t {
	case TierReviewed:
		return 3
	case TierScanned:
		return 2
	case TierUnverified:
		return 1
	default:
		return 0
	}
}

func (r *appRepo) AppDetail(ctx context.Context, id string) (App, []AppVersion, error) {
	var a App
	err := r.db.NewSelect().Model(&a).Where("id = ? AND listed = ?", id, true).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, nil, ErrNotFound
	}
	if err != nil {
		return App{}, nil, err
	}
	var versions []AppVersion
	if err := r.db.NewSelect().Model(&versions).
		Where("app_id = ?", id).Order("created_at DESC").Scan(ctx); err != nil {
		return App{}, nil, err
	}
	return a, versions, nil
}
