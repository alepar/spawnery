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
	s.ModelApplied = true // a fresh pod is started with spawns.model, so it is applied from birth
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

// Rename sets the spawn's display name. ErrNotFound if the spawn is missing or deleted
// (unlike Touch/MarkRecovered, rename is rejected for deleted spawns — the object is gone
// from the caller's view). No uniqueness is enforced — duplicate names are allowed (the
// spawn id is the real key).
func (r *spawnRepo) Rename(ctx context.Context, id, name string) error {
	res, err := r.db.NewUpdate().Model((*Spawn)(nil)).
		Set("name = ?", name).
		Where("id = ?", id).Where("status <> ?", Deleted).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// SetModel writes the new model and marks it unapplied (model_applied=false), clearing any prior
// failure detail — all in one UPDATE (atomic). The CP SetSpawnModel handler calls this. Like Rename,
// it refuses deleted spawns and returns ErrNotFound when no row is updated.
func (r *spawnRepo) SetModel(ctx context.Context, id, model string) error {
	res, err := r.db.NewUpdate().Model((*Spawn)(nil)).
		Set("model = ?", model).
		Set("model_applied = ?", false).
		Set("model_apply_detail = ?", "").
		Where("id = ?", id).Where("status <> ?", Deleted).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// MarkModelApplied marks the running pod's model as matching spawns.model and clears the failure
// detail. Idempotent (no rowcount guard — mirrors Touch/MarkRecovered); the reconciler calls it on a
// successful push and for the suspended/no-live-pod arm.
func (r *spawnRepo) MarkModelApplied(ctx context.Context, id string) error {
	_, err := r.db.NewUpdate().Model((*Spawn)(nil)).
		Set("model_applied = ?", true).
		Set("model_apply_detail = ?", "").
		Where("id = ?", id).Exec(ctx)
	return err
}

// MarkModelApplyFailed leaves model_applied=false and records the last failure reason. Idempotent;
// the reconciler calls it when it gives up after the bounded retry window.
func (r *spawnRepo) MarkModelApplyFailed(ctx context.Context, id, detail string) error {
	_, err := r.db.NewUpdate().Model((*Spawn)(nil)).
		Set("model_apply_detail = ?", detail).
		Where("id = ?", id).Exec(ctx)
	return err
}

// ListUnappliedModel returns non-deleted spawns whose effective model has not yet been applied to a
// running pod (model_applied=false) — the reconciler's scan input.
func (r *spawnRepo) ListUnappliedModel(ctx context.Context) ([]Spawn, error) {
	var out []Spawn
	err := r.db.NewSelect().Model(&out).
		Where("model_applied = ?", false).Where("status <> ?", Deleted).Scan(ctx)
	return out, err
}

// guardStatus runs a status-guarded UPDATE on spawns; rowcount=0 -> ErrConflict.
// The set closure must add ONLY .Set(...) clauses; the id + status WHERE is owned by guardStatus.
func (r *spawnRepo) guardStatus(ctx context.Context, id string, from []Status, set func(*bun.UpdateQuery) *bun.UpdateQuery) error {
	q := r.db.NewUpdate().Model((*Spawn)(nil)).Where("id = ?", id).Where("status IN (?)", bun.In(from))
	res, err := set(q).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	return nil
}

// endLiveContainer ends the spawn's current live container (if any). Idempotent.
func (r *spawnRepo) endLiveContainer(ctx context.Context, id string, p Phase, ts int64) error {
	_, err := r.db.NewUpdate().Model((*Container)(nil)).
		Set("ended_at = ?", ts).Set("phase = ?", p).
		Where("spawn_id = ? AND ended_at IS NULL", id).Exec(ctx)
	return err
}

func (r *spawnRepo) maxGen(ctx context.Context, id string) (int64, error) {
	var hi sql.NullInt64
	err := r.db.NewSelect().Model((*Container)(nil)).
		ColumnExpr("MAX(generation)").Where("spawn_id = ?", id).Scan(ctx, &hi)
	return hi.Int64, err
}

// lastUsedTS reads the spawn's last_used_at — the store's episode-bookkeeping clock (the store does
// not read the wall clock; the CP passes real timestamps via Touch at higher-level call sites).
func (r *spawnRepo) lastUsedTS(ctx context.Context, id string) (int64, error) {
	var ts int64
	err := r.db.NewSelect().Model((*Spawn)(nil)).ColumnExpr("last_used_at").Where("id = ?", id).Scan(ctx, &ts)
	return ts, err
}

// ClaimStarting (caller wraps in WithTx): (1) guard spawn->starting WHERE status IN(from);
// (2) end the old live container; (3) insert a NEW live container at gen=max+1.
func (r *spawnRepo) ClaimStarting(ctx context.Context, id string, from []Status) (int64, error) {
	if err := r.guardStatus(ctx, id, from, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Set("status = ?", Starting)
	}); err != nil {
		return 0, err
	}
	ts, err := r.lastUsedTS(ctx, id)
	if err != nil {
		return 0, err
	}
	if err := r.endLiveContainer(ctx, id, PhaseLost, ts); err != nil {
		return 0, err
	}
	hi, err := r.maxGen(ctx, id)
	if err != nil {
		return 0, err
	}
	newGen := hi + 1
	c := Container{SpawnID: id, Generation: newGen, NodeID: "", Phase: PhaseStarting, StartedAt: ts}
	if _, err := r.db.NewInsert().Model(&c).Exec(ctx); err != nil {
		return 0, err // a uniq_live_container violation here is a backstop bug, surfaced loudly
	}
	return newGen, nil
}

func (r *spawnRepo) SetActive(ctx context.Context, id, nodeID string, gen int64) error {
	if err := r.guardStatus(ctx, id, []Status{Starting}, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Set("status = ?", Active)
	}); err != nil {
		return err
	}
	res, err := r.db.NewUpdate().Model((*Container)(nil)).
		Set("phase = ?", PhaseActive).Set("node_id = ?", nodeID).
		Where("spawn_id = ? AND generation = ? AND ended_at IS NULL", id, gen).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	return nil
}

func (r *spawnRepo) SetSuspending(ctx context.Context, id string, gen int64) error {
	if err := r.guardContainerGen(ctx, id, gen); err != nil {
		return err
	}
	if err := r.guardStatus(ctx, id, []Status{Active}, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Set("status = ?", Suspending)
	}); err != nil {
		return err
	}
	return r.setContainerPhase(ctx, id, gen, PhaseSuspending)
}

func (r *spawnRepo) SetSuspended(ctx context.Context, id string, gen int64) error {
	if err := r.guardContainerGen(ctx, id, gen); err != nil {
		return err
	}
	if err := r.guardStatus(ctx, id, []Status{Suspending}, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Set("status = ?", Suspended).Set("suspended_at = last_used_at")
	}); err != nil {
		return err
	}
	ts, err := r.lastUsedTS(ctx, id)
	if err != nil {
		return err
	}
	return r.endLiveContainer(ctx, id, PhaseStopped, ts)
}

// RevertSuspended rolls a starting episode back to suspended (migration defined-failure path). It is
// gen-fenced on the (failed) target container and ends that row, so the spawn returns to exactly the
// suspended state it held before the migrate attempt — the prior suspend's per-mount markers remain
// the recoverable state, and the user can resume on the source. starting -> suspended.
func (r *spawnRepo) RevertSuspended(ctx context.Context, id string, gen int64) error {
	if err := r.guardContainerGen(ctx, id, gen); err != nil {
		return err
	}
	if err := r.guardStatus(ctx, id, []Status{Starting}, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Set("status = ?", Suspended).Set("suspended_at = last_used_at")
	}); err != nil {
		return err
	}
	ts, err := r.lastUsedTS(ctx, id)
	if err != nil {
		return err
	}
	return r.endLiveContainer(ctx, id, PhaseStopped, ts)
}

func (r *spawnRepo) SetError(ctx context.Context, id string) error {
	if err := r.guardStatus(ctx, id, []Status{Starting, Active, Suspending, Unreachable}, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Set("status = ?", Errored)
	}); err != nil {
		return err
	}
	ts, err := r.lastUsedTS(ctx, id)
	if err != nil {
		return err
	}
	return r.endLiveContainer(ctx, id, PhaseLost, ts)
}

func (r *spawnRepo) MarkUnreachable(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res, err := r.db.NewUpdate().Model((*Spawn)(nil)).
		Set("status = ?", Unreachable).
		Where("id IN (?)", bun.In(ids)).Where("status IN (?)", bun.In([]Status{Starting, Active})).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	// count = spawns newly transitioned to unreachable (already-unreachable/suspended/etc. ids are excluded), not total-now-unreachable.
	return int(n), nil // live container row is intentionally KEPT (adopt arm needs it)
}

// MarkReachable flips unreachable->active — the adopt arm's "node came back" path. Gen-fenced:
// the flip applies only while (id, gen) is still the live container (a recreate fences it out via
// ErrConflict). ONLY unreachable spawns flip; any other status -> ErrConflict, spawn untouched.
// The live container row is left as-is (Adopt owns the node_id rebind).
func (r *spawnRepo) MarkReachable(ctx context.Context, id string, gen int64) error {
	if err := r.guardContainerGen(ctx, id, gen); err != nil {
		return err
	}
	return r.guardStatus(ctx, id, []Status{Unreachable}, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Set("status = ?", Active)
	})
}

func (r *spawnRepo) MarkRecovered(ctx context.Context, id string) error {
	_, err := r.db.NewUpdate().Model((*Spawn)(nil)).Set("recovered = ?", true).Where("id = ?", id).Exec(ctx)
	return err
}

func (r *spawnRepo) Touch(ctx context.Context, id string, ts int64) error {
	_, err := r.db.NewUpdate().Model((*Spawn)(nil)).Set("last_used_at = ?", ts).Where("id = ?", id).Exec(ctx)
	return err
}

func (r *spawnRepo) MarkDeleted(ctx context.Context, id string, ts int64) error {
	if err := r.guardStatus(ctx, id, []Status{Starting, Active, Suspended, Unreachable, Errored}, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Set("status = ?", Deleted).Set("deleted_at = ?", ts)
	}); err != nil {
		return err
	}
	return r.endLiveContainer(ctx, id, PhaseLost, ts)
}

func (r *spawnRepo) EndContainer(ctx context.Context, id string, gen int64, p Phase) error {
	ts, err := r.lastUsedTS(ctx, id)
	if err != nil {
		return err
	}
	_, err = r.db.NewUpdate().Model((*Container)(nil)).
		Set("ended_at = ?", ts).Set("phase = ?", p).
		Where("spawn_id = ? AND generation = ? AND ended_at IS NULL", id, gen).Exec(ctx)
	return err
}

func (r *spawnRepo) SetMountMarker(ctx context.Context, id, mount, marker string) error {
	_, err := r.db.NewUpdate().Model((*Mount)(nil)).
		Set("persist_marker = ?", marker).
		Where("spawn_id = ? AND name = ?", id, mount).Exec(ctx)
	return err
}

// setContainerPhase updates the (id, gen) live container's phase; rowcount=0 -> ErrConflict (stale gen).
func (r *spawnRepo) setContainerPhase(ctx context.Context, id string, gen int64, p Phase) error {
	res, err := r.db.NewUpdate().Model((*Container)(nil)).
		Set("phase = ?", p).
		Where("spawn_id = ? AND generation = ? AND ended_at IS NULL", id, gen).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	return nil
}

// guardContainerGen verifies (id, gen) is the current live container; else ErrConflict.
func (r *spawnRepo) guardContainerGen(ctx context.Context, id string, gen int64) error {
	n, err := r.db.NewSelect().Model((*Container)(nil)).
		Where("spawn_id = ? AND generation = ? AND ended_at IS NULL", id, gen).Count(ctx)
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	return nil
}

func (r *spawnRepo) LiveContainersByNode(ctx context.Context, nodeID string) ([]Container, error) {
	var out []Container
	err := r.db.NewSelect().Model(&out).
		Where("node_id = ? AND ended_at IS NULL", nodeID).Scan(ctx)
	return out, err
}

// Adopt binds the current live container of a spawn to a node (rebind on reconnect; no restart).
func (r *spawnRepo) Adopt(ctx context.Context, id, nodeID string, gen int64) error {
	res, err := r.db.NewUpdate().Model((*Container)(nil)).
		Set("node_id = ?", nodeID).
		Where("spawn_id = ? AND generation = ? AND ended_at IS NULL", id, gen).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	return nil
}

// Compile-time check that *spawnRepo fully implements SpawnRepo.
var _ SpawnRepo = (*spawnRepo)(nil)
