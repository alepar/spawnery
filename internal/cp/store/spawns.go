package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
)

type spawnRepo struct{ db bun.IDB }

// Create inserts the spawn (status=starting) + its live container (gen 1) + mount rows. The caller
// MUST wrap this in Store.WithTx so the three writes are atomic. The caller's s.Status is forced to
// Starting and each mounts[i].SpawnID is set in-place (callers should not rely on mount values after
// an error). Validation is referential only: the pinned (app_id, app_version) must exist and every
// provided mount name must be declared on that version. Completeness of REQUIRED mounts is a
// CP-layer concern (enforced where the CreateSpawn request assembles declared×user-choice), not here.
func (r *spawnRepo) Create(ctx context.Context, s Spawn, mounts []Mount) error {
	// Count is load-bearing: a valid app version may declare ZERO mounts (e.g. a storage-less app
	// like zork), so version existence cannot be inferred from the declared-mounts query below.
	n, err := r.db.NewSelect().Model((*AppVersion)(nil)).
		Where("app_id = ? AND version = ?", s.AppID, s.AppVersion).Count(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("store: app version %s@%s does not exist", s.AppID, s.AppVersion)
	}
	var decls []MountDecl
	if err := r.db.NewSelect().Model(&decls).
		Where("app_id = ? AND version = ?", s.AppID, s.AppVersion).Scan(ctx); err != nil {
		return err
	}
	declared := map[string]bool{}
	for _, d := range decls {
		declared[d.Name] = true
	}
	for _, m := range mounts {
		if !declared[m.Name] {
			return fmt.Errorf("store: mount %q not declared on %s@%s", m.Name, s.AppID, s.AppVersion)
		}
	}

	s.Status = Starting
	if _, err := r.db.NewInsert().Model(&s).Exec(ctx); err != nil {
		return err
	}
	// node_id is NOT NULL; a fresh spawn starts with an empty node until SetActive binds one.
	c := Container{SpawnID: s.ID, Generation: 1, NodeID: "", Phase: PhaseStarting, StartedAt: s.CreatedAt}
	if _, err := r.db.NewInsert().Model(&c).Exec(ctx); err != nil {
		return err
	}
	for i := range mounts {
		mounts[i].SpawnID = s.ID
		if _, err := r.db.NewInsert().Model(&mounts[i]).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (r *spawnRepo) Get(ctx context.Context, id string) (Spawn, error) {
	var s Spawn
	err := r.db.NewSelect().Model(&s).
		Where("id = ?", id).Where("status <> ?", Deleted).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return Spawn{}, ErrNotFound
	}
	return s, err
}

func (r *spawnRepo) LiveContainer(ctx context.Context, id string) (Container, bool, error) {
	var c Container
	err := r.db.NewSelect().Model(&c).
		Where("spawn_id = ? AND ended_at IS NULL", id).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return Container{}, false, nil
	}
	if err != nil {
		return Container{}, false, err
	}
	return c, true, nil
}

func (r *spawnRepo) GetMounts(ctx context.Context, id string) ([]Mount, error) {
	var out []Mount
	err := r.db.NewSelect().Model(&out).Where("spawn_id = ?", id).Order("name ASC").Scan(ctx)
	return out, err
}

func (r *spawnRepo) ListByOwner(ctx context.Context, ownerID string) ([]Spawn, error) {
	var out []Spawn
	err := r.db.NewSelect().Model(&out).
		Where("owner_id = ?", ownerID).Where("status <> ?", Deleted).
		Order("last_used_at DESC").Scan(ctx)
	return out, err
}
