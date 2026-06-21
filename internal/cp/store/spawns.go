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

// CountByStatus returns the number of non-deleted spawns per status, for metrics gauges.
// Deleted rows are excluded: they are soft-deleted and their count is unbounded.
func (r *spawnRepo) CountByStatus(ctx context.Context) (map[Status]int, error) {
	type row struct {
		Status Status `bun:"status"`
		Count  int    `bun:"count"`
	}
	var rows []row
	err := r.db.NewSelect().
		TableExpr("spawns").
		ColumnExpr("status, COUNT(*) AS count").
		Where("status <> ?", Deleted).
		GroupExpr("status").
		Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	out := make(map[Status]int, len(rows))
	for _, ro := range rows {
		out[ro.Status] = ro.Count
	}
	return out, nil
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

func (r *spawnRepo) LatestContainer(ctx context.Context, id string) (Container, bool, error) {
	var c Container
	err := r.db.NewSelect().Model(&c).
		Where("spawn_id = ?", id).
		Order("generation DESC").
		Limit(1).
		Scan(ctx)
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

// SetBaseImageDigest records the content-addressable base-image digest resolved by the node at
// create time (spec §4 / sp-ei4.1.10). Like Rename/SetModel, it refuses deleted spawns and
// returns ErrNotFound when no row is updated.
func (r *spawnRepo) SetBaseImageDigest(ctx context.Context, id, digest string) error {
	res, err := r.db.NewUpdate().Model((*Spawn)(nil)).
		Set("base_image_digest = ?", digest).
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
// Every guardStatus call bumps status_seq so that any concurrent CAS on the row observes the change.
func (r *spawnRepo) guardStatus(ctx context.Context, id string, from []Status, set func(*bun.UpdateQuery) *bun.UpdateQuery) error {
	q := r.db.NewUpdate().Model((*Spawn)(nil)).
		Set("status_seq = status_seq + 1").
		Where("id = ?", id).Where("status IN (?)", bun.In(from))
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
	// Accepts Starting (fresh create / recreate) and Resuming (resume path, sp-u53.7.5).
	if err := r.guardStatus(ctx, id, []Status{Starting, Resuming}, func(q *bun.UpdateQuery) *bun.UpdateQuery {
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

func (r *spawnRepo) SetForking(ctx context.Context, id string, gen int64, captureDeadlineTS int64) error {
	if err := r.guardContainerGen(ctx, id, gen); err != nil {
		return err
	}
	return r.guardStatus(ctx, id, []Status{Active}, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Set("status = ?", Forking).Set("fork_capture_deadline = ?", captureDeadlineTS)
	})
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

// RevertSuspended rolls a starting or resuming episode back to suspended (migration defined-failure
// path). It is gen-fenced on the (failed) target container and ends that row, so the spawn returns
// to exactly the suspended state it held before the migrate attempt — the prior suspend's per-mount
// markers remain the recoverable state, and the user can resume on the source.
// starting|resuming -> suspended.
func (r *spawnRepo) RevertSuspended(ctx context.Context, id string, gen int64) error {
	if err := r.guardContainerGen(ctx, id, gen); err != nil {
		return err
	}
	// Accepts Starting (old path) and Resuming (sp-u53.7.5: resume now passes through Resuming).
	if err := r.guardStatus(ctx, id, []Status{Starting, Resuming}, func(q *bun.UpdateQuery) *bun.UpdateQuery {
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
	// Includes Resuming (sp-u53.7.5: failed resume can bail from Resuming status).
	if err := r.guardStatus(ctx, id, []Status{Starting, Active, Suspending, Resuming, Unreachable}, func(q *bun.UpdateQuery) *bun.UpdateQuery {
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
		Set("status_seq = status_seq + 1").
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
	// status_seq bump closes the idle-reaper TOCTOU: a reaper that read (seq=S) then calls
	// Acquire(expectedSeq=S) will get ErrConflict if activity arrived between the two calls.
	_, err := r.db.NewUpdate().Model((*Spawn)(nil)).
		Set("last_used_at = ?", ts).
		Set("status_seq = status_seq + 1").
		Where("id = ?", id).Exec(ctx)
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

// Acquire atomically claims the spawn row. See SpawnRepo.Acquire for full semantics.
func (r *spawnRepo) Acquire(ctx context.Context, id, holder, leaseID string, nowTS, deadlineTS, expectedSeq int64) (int64, error) {
	res, err := r.db.NewUpdate().Model((*Spawn)(nil)).
		Set("claim_holder = ?", holder).
		Set("claim_lease_id = ?", leaseID).
		Set("claim_deadline = ?", deadlineTS).
		Set("status_seq = status_seq + 1").
		Where("id = ?", id).
		Where("status_seq = ?", expectedSeq).
		Where("(claim_holder IS NULL OR claim_deadline < ?)", nowTS).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, ErrConflict
	}
	return expectedSeq + 1, nil
}

func (r *spawnRepo) AcquireForkingRecovery(ctx context.Context, id, holder, leaseID string, nowTS, deadlineTS, expectedSeq int64) (int64, error) {
	res, err := r.db.NewUpdate().Model((*Spawn)(nil)).
		Set("claim_holder = ?", holder).
		Set("claim_lease_id = ?", leaseID).
		Set("claim_deadline = ?", deadlineTS).
		Set("status_seq = status_seq + 1").
		Where("id = ?", id).
		Where("status = ?", Forking).
		Where("status_seq = ?", expectedSeq).
		Where("(claim_holder IS NULL OR claim_deadline < ? OR (fork_capture_deadline IS NOT NULL AND fork_capture_deadline < ?))", nowTS, nowTS).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, ErrConflict
	}
	return expectedSeq + 1, nil
}

// Heartbeat extends the claim deadline without bumping status_seq. See SpawnRepo.Heartbeat.
func (r *spawnRepo) Heartbeat(ctx context.Context, id, leaseID string, newDeadlineTS int64) error {
	res, err := r.db.NewUpdate().Model((*Spawn)(nil)).
		Set("claim_deadline = ?", newDeadlineTS).
		Where("id = ?", id).
		Where("claim_lease_id = ?", leaseID).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrClaimLost
	}
	return nil
}

// Release clears claim columns and bumps status_seq. See SpawnRepo.Release.
func (r *spawnRepo) Release(ctx context.Context, id, leaseID string) error {
	res, err := r.db.NewUpdate().Model((*Spawn)(nil)).
		Set("claim_holder = NULL").
		Set("claim_lease_id = NULL").
		Set("claim_deadline = NULL").
		Set("status_seq = status_seq + 1").
		Where("id = ?", id).
		Where("claim_lease_id = ?", leaseID).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrClaimLost
	}
	return nil
}

// TransitionClaimed performs a generation+lease+seq-fenced status transition. See SpawnRepo.TransitionClaimed.
// The generation fence is a correlated subquery on spawn_containers: only the current live episode
// (ended_at IS NULL) is matched, so a pod recreate that started a new generation returns rowcount 0.
// The subquery references "id" without a table qualifier — SQLite's UPDATE does not allow "spawns.id"
// in a correlated subquery WHERE clause; the unqualified form resolves to the outer spawns row's id.
func (r *spawnRepo) TransitionClaimed(ctx context.Context, id, leaseID string, expectedSeq, expectedGen int64, to Status) (int64, error) {
	res, err := r.db.NewUpdate().Model((*Spawn)(nil)).
		Set("status = ?", to).
		Set("status_seq = status_seq + 1").
		Where("id = ?", id).
		Where("status_seq = ?", expectedSeq).
		Where("claim_lease_id = ?", leaseID).
		Where("? = (SELECT generation FROM spawn_containers WHERE spawn_id = id AND ended_at IS NULL)", expectedGen).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, ErrConflict
	}
	return expectedSeq + 1, nil
}

func (r *spawnRepo) TransitionForkingRecovered(ctx context.Context, id, leaseID string, expectedSeq, expectedGen int64) (int64, error) {
	res, err := r.db.NewUpdate().Model((*Spawn)(nil)).
		Set("status = ?", Active).
		Set("fork_capture_deadline = NULL").
		Set("claim_holder = NULL").
		Set("claim_lease_id = NULL").
		Set("claim_deadline = NULL").
		Set("status_seq = status_seq + 1").
		Where("id = ?", id).
		Where("status = ?", Forking).
		Where("status_seq = ?", expectedSeq).
		Where("claim_lease_id = ?", leaseID).
		Where("? = (SELECT generation FROM spawn_containers WHERE spawn_id = id AND ended_at IS NULL)", expectedGen).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, ErrConflict
	}
	return expectedSeq + 1, nil
}

func (r *spawnRepo) MarkForkingLost(ctx context.Context, id string, expectedSeq int64) (int64, error) {
	var newSeq int64
	err := r.withTx(ctx, func(tx *spawnRepo) error {
		res, err := tx.db.NewUpdate().Model((*Spawn)(nil)).
			Set("status = ?", Errored).
			Set("fork_capture_deadline = NULL").
			Set("claim_holder = NULL").
			Set("claim_lease_id = NULL").
			Set("claim_deadline = NULL").
			Set("status_seq = status_seq + 1").
			Where("id = ?", id).
			Where("status = ?", Forking).
			Where("status_seq = ?", expectedSeq).
			Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrConflict
		}
		ts, err := tx.lastUsedTS(ctx, id)
		if err != nil {
			return err
		}
		if err := tx.endLiveContainer(ctx, id, PhaseLost, ts); err != nil {
			return err
		}
		newSeq = expectedSeq + 1
		return nil
	})
	return newSeq, err
}

// ListStranded returns spawns in transient statuses whose claim is absent or expired (nowTS is a
// unix timestamp; store never reads the wall clock). See SpawnRepo.ListStranded.
func (r *spawnRepo) ListStranded(ctx context.Context, nowTS int64) ([]Spawn, error) {
	var out []Spawn
	err := r.db.NewSelect().Model(&out).
		Where("status IN (?)", bun.In(transientStatuses)).
		Where("(claim_holder IS NULL OR claim_deadline < ?)", nowTS).
		Order("id ASC").
		Scan(ctx)
	return out, err
}

func (r *spawnRepo) ListRecoverableForking(ctx context.Context, nowTS int64) ([]Spawn, error) {
	var out []Spawn
	err := r.db.NewSelect().Model(&out).
		Where("status = ?", Forking).
		Where("(claim_holder IS NULL OR claim_deadline < ? OR (fork_capture_deadline IS NOT NULL AND fork_capture_deadline < ?))", nowTS, nowTS).
		Order("id ASC").
		Scan(ctx)
	return out, err
}

// ReconcileSuspendedAfterError implements SpawnRepo.ReconcileSuspendedAfterError.
// It transitions Errored→Suspended without touching the container row (the container
// was already ended by SetError in the stall path). Caller is responsible for gen-fencing
// before calling (e.g. via LatestContainer comparison).
func (r *spawnRepo) ReconcileSuspendedAfterError(ctx context.Context, id string) error {
	return r.guardStatus(ctx, id, []Status{Errored}, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Set("status = ?", Suspended).Set("suspended_at = last_used_at")
	})
}

// ListLiveByProfileIDs returns non-deleted spawns whose profile_id is in the given set,
// ordered by id ASC for deterministic output. Returns nil, nil when profileIDs is empty
// (no query issued). Used by the kill-switch (sp-nrzf.3.9).
func (r *spawnRepo) ListLiveByProfileIDs(ctx context.Context, profileIDs []string) ([]Spawn, error) {
	if len(profileIDs) == 0 {
		return nil, nil
	}
	var out []Spawn
	err := r.db.NewSelect().Model(&out).
		Where("profile_id IN (?)", bun.In(profileIDs)).
		Where("status <> ?", Deleted).
		Order("id ASC").
		Scan(ctx)
	return out, err
}

func (r *spawnRepo) MarkDeletedClaimed(ctx context.Context, id, leaseID string, expectedSeq, expectedGen int64, ts int64) (int64, error) {
	var newSeq int64
	err := r.withTx(ctx, func(tx *spawnRepo) error {
		var err error
		newSeq, err = tx.markDeletedClaimed(ctx, id, leaseID, expectedSeq, expectedGen, ts)
		return err
	})
	return newSeq, err
}

func (r *spawnRepo) withTx(ctx context.Context, fn func(tx *spawnRepo) error) error {
	top, ok := r.db.(*bun.DB)
	if !ok {
		return fn(r)
	}
	return top.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return fn(&spawnRepo{db: tx})
	})
}

func (r *spawnRepo) markDeletedClaimed(ctx context.Context, id, leaseID string, expectedSeq, expectedGen int64, ts int64) (int64, error) {
	q := r.db.NewUpdate().Model((*Spawn)(nil)).
		Set("status = ?", Deleted).
		Set("deleted_at = ?", ts).
		Set("fork_capture_deadline = NULL").
		Set("claim_holder = NULL").
		Set("claim_lease_id = NULL").
		Set("claim_deadline = NULL").
		Set("status_seq = status_seq + 1").
		Where("id = ?", id).
		Where("status_seq = ?", expectedSeq).
		Where("claim_lease_id = ?", leaseID)
	if expectedGen == 0 {
		q = q.Where("NOT EXISTS (SELECT 1 FROM spawn_containers WHERE spawn_id = id AND ended_at IS NULL)")
	} else {
		q = q.Where("? = (SELECT generation FROM spawn_containers WHERE spawn_id = id AND ended_at IS NULL)", expectedGen)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, ErrConflict
	}
	if err := r.endLiveContainer(ctx, id, PhaseLost, ts); err != nil {
		return 0, err
	}
	return expectedSeq + 1, nil
}

// Compile-time check that *spawnRepo fully implements SpawnRepo.
var _ SpawnRepo = (*spawnRepo)(nil)
