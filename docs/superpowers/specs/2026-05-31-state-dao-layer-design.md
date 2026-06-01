# Spawnery — Control-Plane State / DAO Layer (Design)

**Bead:** `sp-pc4`
**Status:** Draft **v5** (3rd roast — container-lifetime windows fixed; pending review)
**Date:** 2026-05-31
**Supersedes:** the preliminary schema in
[State/DAO Research Brief](2026-05-31-state-dao-layer-research-brief.md).
**Depends on:** [Spawn Lifecycle](2026-05-31-spawn-lifecycle-design.md) (status machine + the
consistency protocol §6) and the contracts predecessor **`sp-mqj`** (now incl. **episode generation
on every message + node inventory on Register/Heartbeat**).
**Hands off to:** `sp-gd9` (node-side per-mount suspend/resume + generation/inventory + idle), **E3**
(persistent backend), **E5** (manifest/catalog).

> **v5 changelog (3rd roast — the container split created zero/one/two-live-row windows):**
> `unreachable` **keeps** the live container (so Wait→adopt works; reconciliation gains an
> `unreachable`→adopt arm); start-new-episode txs **end-old-then-insert-new** (the partial unique
> never fires in correct operation — a violation is a loud backstop bug, not `ErrConflict`); CP
> fencing is "**look up live container; absent ⇒ drop; gen-mismatch ⇒ drop**" and the scheduler
> rendezvous is keyed by `(spawn_id, gen)`; `MarkDeleted` **ends the live container** in-tx; multi-CP
> is out of scope (the lock is single-process); episode-history retention named (E8); the per-slice
> "mounts stored-but-inert until sp-mqj" honesty + the e2e-needs-sp-mqj sequencing are stated.
>
> **v4 changelog:** the **running container is its own entity** (`spawn_containers`); `generation` +
> `node_id` moved off `spawns` onto it; spawn:container = **1-to-0..1** enforced by a **partial unique
> index** (DB-level single-live invariant; relax for future automerge 1-to-many); transitions update
> spawn + container atomically; reconciliation diffs node inventory against **live container rows**.
>
> **v3 changelog:** added `spawns.generation` + the `unreachable` status; transitions are
> **claim-in-DB-then-act under a per-spawn lock**; the CP runs **inventory reconciliation** on node
> (re)connect (adopt / stop-orphan / mark-unreachable) instead of blind flips; added the
> **`RecreateSpawn`** (user-driven recovery) path; the **node binds CP-sent mounts**; **boolean
> columns get an explicit per-dialect type**; the Postgres test honesty + the `secret-app`/manifest-id
> mismatch are resolved.

---

## 1. Goal & scope

Replace the CP's ephemeral in-memory maps with a durable, transactional **state layer**, and on it
implement the **entire CP-side spawn lifecycle + the DB↔container consistency protocol**
([lifecycle §6](2026-05-31-spawn-lifecycle-design.md)). The `spawns` table is the **CP index** and
the source of truth for **intent + ownership**; the node's Register/Heartbeat **inventory** is ground
truth for what containers actually run; the CP reconciles them.

**In scope (`sp-pc4`):** the `store` package; the CP-side **state machine** (status-guarded,
per-spawn-locked, claim-then-act); **inventory reconciliation** (adopt/stop/unreachable);
**generation fencing** at the CP (drop stale `SpawnStatus`; bump per episode); the lifecycle **RPCs**
(`CreateSpawn`, `ListSpawns`, `SuspendSpawn`, `ResumeSpawn`, **`RecreateSpawn`**, `DeleteSpawn`,
`Session`/WS ownership + auto-resume); the CP→node plumbing usage.

**Out of scope (→ `sp-gd9`/E1/E3):** node-side per-mount persist/restore + the **server-side backend
write-fence**, inventory *reporting*, the idle timer, the web UI, the persistent backend. **Honesty
note:** until those land, suspend/resume is a stub teardown on `Scratch` — the CP state machine +
reconciliation are real and testable, but **data does not survive a suspend**; lossless gates on E3.

**Stays in-memory:** `registry.Registry`, `router.Router` (live relay; a **projection of `active`
rows**), `scheduler`, the node's `spawnlet.Store`. **New in-memory:** a per-spawn-id lock table.

---

## 2. Stack

**Bun** over **modernc.org/sqlite** (driver name **`"sqlite"`**) + **PostgreSQL/pgx**; **goose**
migrations (two dialect trees); **`:memory:`** hermetic tests; repos over `bun.IDB` with
`Store.WithTx`. Opaque **TEXT** ids (spawns: **uuidv7** via `uuid.NewV7()`, google/uuid ≥ v1.6.0),
**INTEGER unix-seconds** timestamps. **Booleans:** Go `bool` fields mapped to **`INTEGER` (0/1) in
SQLite** and **`boolean` in Postgres** — explicit per-dialect DDL; the drift test asserts column
*type* per dialect (not just name), so `bool`↔`INTEGER`↔`boolean` can't silently diverge.

---

## 3. Schema

SQLite DDL shown; the Postgres tree differs in `text`/`bigint`/`boolean` types only.

```sql
CREATE TABLE owners ( id TEXT PRIMARY KEY, email TEXT, created_at INTEGER NOT NULL );
CREATE TABLE apps   ( id TEXT PRIMARY KEY, display_name TEXT, created_at INTEGER NOT NULL );

CREATE TABLE app_versions (
  app_id TEXT NOT NULL REFERENCES apps(id),
  version TEXT NOT NULL, ref TEXT NOT NULL,
  reviewed INTEGER NOT NULL DEFAULT 0,        -- pg: boolean
  created_at INTEGER NOT NULL,
  PRIMARY KEY (app_id, version)
);
CREATE INDEX idx_app_versions_reviewed ON app_versions(app_id, reviewed, created_at DESC);

-- declared mounts per version — extracted from the manifest ONCE at registration
CREATE TABLE app_version_mounts (
  app_id TEXT NOT NULL, version TEXT NOT NULL, name TEXT NOT NULL,
  required INTEGER NOT NULL DEFAULT 1,        -- pg: boolean
  PRIMARY KEY (app_id, version, name),
  FOREIGN KEY (app_id, version) REFERENCES app_versions(app_id, version)
);

CREATE TABLE spawns (
  id           TEXT PRIMARY KEY,              -- uuidv7
  owner_id     TEXT NOT NULL REFERENCES owners(id),
  app_id       TEXT NOT NULL REFERENCES apps(id),
  app_version  TEXT NOT NULL,                 -- pinned snapshot; (app_id,version) repo-validated at create
  app_ref      TEXT NOT NULL,                 -- denormalized; survives version delisting (NOT git-ref deletion)
  pinned       INTEGER NOT NULL DEFAULT 0,    -- pg: boolean
  model        TEXT NOT NULL,
  status       TEXT NOT NULL CHECK (status IN
               ('starting','active','suspending','suspended','unreachable','error','deleted')),
  recovered    INTEGER NOT NULL DEFAULT 0,    -- pg: boolean; set on recreate-from-unclean (NOT clean CP restart)
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL,
  suspended_at INTEGER,
  deleted_at   INTEGER
);
CREATE INDEX idx_spawns_owner  ON spawns(owner_id, last_used_at DESC);
CREATE INDEX idx_spawns_status ON spawns(status);

-- the RUNNING-CONTAINER (episode) entity, separate from the durable spawn.
-- spawn:container = 1-to-0..1 (at most one LIVE container per spawn), enforced by the partial
-- UNIQUE below. generation + node_id live HERE, not on the durable spawn. The node inventory
-- reconciles against the LIVE rows here. Future: data-backend automerge makes this 1-to-many
-- (relax/drop the partial unique). Ended rows are kept = the episode history (audit foundation).
CREATE TABLE spawn_containers (
  spawn_id   TEXT    NOT NULL REFERENCES spawns(id),
  generation INTEGER NOT NULL,                  -- episode number; the fencing token
  node_id    TEXT    NOT NULL,                  -- where this episode runs
  phase      TEXT    NOT NULL CHECK (phase IN ('starting','active','suspending','stopped','lost')),
  started_at INTEGER NOT NULL,
  ended_at   INTEGER,                           -- NULL while LIVE
  PRIMARY KEY (spawn_id, generation)
);
-- HARD INVARIANT (DB-enforced, not app-asserted): ≤1 live container per spawn. Partial unique
-- indexes work on both SQLite and Postgres. A second live insert fails → the 0..1 is guaranteed.
CREATE UNIQUE INDEX uniq_live_container ON spawn_containers(spawn_id) WHERE ended_at IS NULL;
CREATE INDEX idx_live_by_node ON spawn_containers(node_id) WHERE ended_at IS NULL; -- inventory diff

CREATE TABLE spawn_mounts (
  spawn_id       TEXT NOT NULL REFERENCES spawns(id),
  name           TEXT NOT NULL,               -- must ∈ app_version_mounts (repo-validated)
  backend_uri    TEXT NOT NULL,
  persist_marker TEXT,                         -- per-mount suspend state; written INCREMENTALLY per mount
  PRIMARY KEY (spawn_id, name)
);
```

**Notes:** the **running container is a first-class entity** (`spawn_containers`) — the durable
`spawns` row carries no `generation`/`node_id`; those identify an *episode* and live on the container
row. The CP's reconciliation diffs node inventory against the **live** container rows. The partial
unique index makes "≤1 live container per spawn" a **DB invariant**, so a concurrency bug can't
produce two live containers even if the app logic slips. **Live-container lookup** is the literal
`WHERE spawn_id=? AND ended_at IS NULL` (rides `uniq_live_container` on both engines; a test asserts
the plan uses it); `ListByOwner` is `spawns LEFT JOIN spawn_containers … ON sc.spawn_id=s.id AND
sc.ended_at IS NULL`. **Episode-history retention:** ended rows accumulate per suspend/resume/recreate
cycle — unbounded; a retention/age-out policy is **named here, deferred to E8 (audit)**, so list/diff
queries don't degrade with history. `app_ref` survives version **delisting** (catalog hide) but **not
git-ref deletion** — resume/recreate against a deleted creator ref fails the node-side clone → spawn
`error` surfaced as **"this app version was removed"** (not a silent provision failure); mitigated
only by post-MVP App-snapshot (system design §2). `persist_marker` is per-mount, written **as each
mount finishes**, so crash recovery distinguishes "none done" from "all done, signal lost" (lifecycle
§6.6).

---

## 4. Domain types & interfaces

Bun tags on domain types; repos over `bun.IDB`. **All transitions are status+generation-guarded**
(`UPDATE … WHERE id=? AND status IN(<from>) [AND generation=?]`, assert rowcount=1) and run under a
**per-spawn lock** held by the CP layer across `{claim → node command → await}`.

```go
type Status string // starting|active|suspending|suspended|unreachable|error|deleted
type Phase  string // starting|active|suspending|stopped|lost  (container episode)

type Spawn struct {                       // the durable entity (no generation/node_id)
    ID, OwnerID, AppID, AppVersion, AppRef, Model string
    Pinned, Recovered bool
    Status     Status
    CreatedAt, LastUsedAt int64
    SuspendedAt, DeletedAt *int64
}
type Container struct {                    // the running episode; spawn:container = 1-to-0..1
    SpawnID    string
    Generation int64
    NodeID     string
    Phase      Phase
    StartedAt  int64
    EndedAt    *int64                       // nil while LIVE
}
type Mount     struct { Name, BackendURI, PersistMarker string }
type MountDecl struct { AppID, Version, Name string; Required bool }
type RunningSpawn struct { SpawnID string; Generation int64; Phase Phase } // node inventory item

type SpawnRepo interface {
    Create(ctx, s Spawn, mounts []Mount) error  // tx: spawn(starting) + container(gen 1, starting); validates version + mounts
    Get(ctx, id string) (Spawn, error)           // NOT-FOUND on deleted for lifecycle ops
    LiveContainer(ctx, id string) (Container, bool, error)
    GetMounts(ctx, id string) ([]Mount, error)
    ListByOwner(ctx, ownerID string) ([]Spawn, error)

    // claim-then-act (tx, in THIS order): (1) UPDATE spawns->starting WHERE status IN(from) — rowcount=0
    // => ErrConflict; (2) END any live container (ended_at=now, phase=lost); (3) INSERT new live
    // container gen = COALESCE(MAX(generation),0)+1 (never reused; (spawn_id,generation) PK enforces).
    // A uniq_live_container violation here is a LOUD BUG (the backstop fired), NOT ErrConflict.
    ClaimStarting(ctx, id string, from []Status) (newGen int64, err error)
    SetActive(ctx, id string, gen int64) error      // tx: spawn->active + container.phase=active   (guard status='starting' & gen live)
    SetSuspending(ctx, id string, gen int64) error  // tx: spawn->suspending + container.phase=suspending
    SetMountMarker(ctx, id, mount, marker string) error // incremental per mount
    SetSuspended(ctx, id string, gen int64) error   // tx: spawn->suspended + container.ended_at=now, phase=stopped
    SetError(ctx, id string) error                  // tx: spawn->error + end live container(phase=lost) if any
    EndContainer(ctx, id string, gen int64, p Phase) error // end a specific episode (orphan stop / recreate supersede)
    MarkUnreachable(ctx, ids []string) (int, error) // spawn->unreachable; live container row KEPT (fate unknown — adopt arm needs it)
    MarkRecovered(ctx, id string) error
    Touch(ctx, id string, ts int64) error
    MarkDeleted(ctx, id string, ts int64) error     // tx: guard status IN('active','suspended','unreachable','error'); ALSO end any live container(phase=lost)

    LiveContainersByNode(ctx, nodeID string) ([]Container, error) // reconciliation diff target
    Adopt(ctx, id, nodeID string, gen int64) error  // confirm a still-running episode (no restart)
}
```
`OwnerRepo`/`AppRepo` as v2 (apps + versions + `DeclaredMounts` + `UpsertVersion(v, mounts)`).
Every transition is a **`WithTx`** that updates `spawns` **and** `spawn_containers` together (no
drift between the durable status and the episode phase). The **`uniq_live_container` index is the
hard guard** for single-live; the per-spawn lock (CP layer) additionally serializes the
`{claim → node command → await}` so two handlers can't interleave node commands for one spawn.

---

## 5. Transactions & per-spawn lock

`Store.WithTx` (Bun `RunInTx`) makes every transition atomic across **`spawns` + `spawn_containers`**
(status and episode phase never drift), and makes `Create` (spawn + container + mounts) and
`UpsertVersion` (version + decls) atomic. **Ordering rule (mandatory):** any tx that starts a new
episode (`ClaimStarting`, recreate) **ends the prior live container first, then inserts the new live
row** — otherwise the two rows momentarily collide on `uniq_live_container` and the tx aborts. So in
correct operation the uniq index **never fires**; if it does, that is a **loud backstop bug**, not a
user-facing conflict (conflict is the status-guard returning rowcount=0).

Orthogonally, the CP holds a **per-spawn-id lock** around every mutating op so the claim-guard and
the node command can't be split by a competing handler (lifecycle §6.3). **Multi-CP is out of scope
for `sp-pc4`:** decide-then-act ordering relies on a single CP process holding that lock. The DB's
`uniq_live_container` invariant guarantees ≤1 live container even across processes, but it does **not**
order *node commands* across CPs — multi-CP needs a DB-advisory-lock / per-spawn leader or a
node-side command sequencer (deferred to `sp-jf7`).

---

## 6. Package layout

```
internal/cp/store/{store.go, open.go}
internal/cp/store/bunstore/{bunstore,owners,apps,spawns,testing,schema_test}.go
internal/cp/store/migrations/{sqlite,pg}/0001_init.sql
internal/cp/lock/spawnlock.go        # per-spawn-id keyed mutex (CP layer)
```

---

## 7. CP-side lifecycle + consistency (integration)

The store replaces `apps.Resolver` and the router's `owner` authority; **both `Session` (gRPC) and
the WebSocket `HandleWS`** read ownership from the DB. `Router.Owner()` is removed; the live route is
a **projection keyed on `spawns.status = 'active'`** (rebuilt on adopt/resume, torn on
suspend/delete) — **not** on container-liveness: an `unreachable` spawn keeps a live container row but
has **no route** (relaying to a dead node would be wrong). **Scheduler split:** `mint()` +
`provision(id, gen, appRef, model, mounts) (nodeID, err)`; `CreateSpawn` mints, `Resume`/`Recreate`
reuse the id. **The node binds the CP-sent `mounts`**, validating names against its manifest at the
ref (mismatch → `error`).

> **Slice honesty (M1):** in the `sp-pc4`-only slice (before `sp-mqj` adds the `StartSpawn` mount
> field and `sp-gd9` makes the node consume it), `spawn_mounts.backend_uri` is **recorded but inert**
> — the node still binds its own manifest parse. The storage-picker is wired end-to-end only with
> `sp-mqj` + `sp-gd9`.

**Decide-then-act ordering** (every op, under the per-spawn lock):

| RPC / event | Behavior |
|---|---|
| **`CreateSpawn`** (req carries per-mount `{name, backend_uri}`) | `LatestReviewed`; validate mount names ⊆ `DeclaredMounts`; mint uuidv7; `WithTx{ Create(starting, gen 1, mounts) }`; `provision(id, 1, …)`; ACTIVE→`SetActive(id,node,1)`, err→`SetError`. |
| **`ResumeSpawn`** / auto-on-attach | `ClaimStarting(id, from=[suspended])` → newGen (ErrConflict if not suspended); `provision(id, newGen, …)`; `SetActive(id,node,newGen)`. |
| **`RecreateSpawn`** (user-acked) | `ClaimStarting(id, from=[unreachable,error])` → newGen; `provision` from last checkpoint; `SetActive`; `MarkRecovered`. Old generation is fenced (backend CAS) and `Stop`ped on its node's return (reconciliation). |
| **`SuspendSpawn`** | `SetSuspending(id, gen)`; send node `Suspend(id,gen)`; node persists each mount → `SetMountMarker` incrementally → suspend-complete → `SetSuspended(id, gen)` + tear route. |
| **`DeleteSpawn`** | `MarkDeleted` (guarded) **first**, then best-effort node `Stop` + route drop. Rejected while `suspending`. |
| **`Session`/WS attach** | DB ownership (reject deleted); `suspended`→auto-resume; `unreachable`→present Recreate/Wait (no auto-resume); takeover closes prior client; `Touch`. |
| **node Register/Heartbeat inventory** | **reconcile** (below). |
| **node stream close** | start a grace window; **do not flip status**. |
| **CP boot** | wait for inventories within a grace window, then reconcile; `suspending`→marker-probe. |

**Inventory reconciliation** (on every node (re)connect, diff the node's `RunningSpawn[]` vs
`LiveContainersByNode`):
- node item `(spawn_id, gen)` matches a **live container** row → **`Adopt`**: rebind route if the
  spawn is `active`; **if the spawn is `unreachable`, flip it back to `active`** (the Wait→adopt path)
  and rebind. No restart.
- node runs `(spawn_id, gen)` with **no matching live container** (suspended/deleted/error, or a
  superseded gen after recreate) → **`Stop(spawn_id, gen)`** (orphan).
- a **live container** the node does **not** report after grace → **`MarkUnreachable`** the spawn and
  **keep the live container row** (`phase` unchanged — fate unknown). The row is ended **only** when
  the user acks Recreate (the new episode supersedes it) or the node returns running a superseded gen.
  Keeping it live is what lets a returning node re-`Adopt` (don't end-on-unreachable — that would make
  the returning container look like an orphan to Stop).

**Generation fencing at the CP:** for every node→CP `SpawnStatus`, **look up the live container**
(`LiveContainer(id)`): **none → drop** (no live episode to confirm); **`report.gen ≠ live.gen` → drop**
(stale report from a superseded episode); else apply. A guard rowcount≠1 is a **superseded no-op**
(markers the dropped report carried are scheduled for GC). The scheduler's start/await **rendezvous is
keyed by `(spawn_id, generation)`** (not `spawn_id` alone), and `OnStatus` applies the same gen-drop
**before** signalling — so a stale gen-G ACTIVE can't satisfy a gen-G+1 (recreate) waiter.

**`spawn.yml` vs the DB cache:** the DB cache (`app_ref`, `app_version`, `model`, mounts) is
**authoritative-in-practice for routing/provisioning until E5** (the CP can't read `spawn.yml` yet);
auto-upgrade mutates the cache; `spawn.yml` reconciliation is E5.

**Seeding (id mismatch fixed):** the canonical app id is the **manifest `id`** (`spawnery/secret`),
**not** the resolver key (`secret-app`). `Open` seeds `apps(id='spawnery/secret')` + its version +
declared mounts, and `cmd/cp/main.go`'s resolver key is corrected to match (one-line change) so E5
registration (which reads the manifest `id`) won't orphan spawn FKs. **Seeds are passed in, not
ambient:** `Open(ctx, Config{…, SeedOwners []string, SeedApps []AppSeed})` receives the already-parsed
token→owner map + app config from `main` (it does not re-read the env — `parseTokens` lives in
`main`). Every token's owner is seeded → a row, so `auth` always resolves to a real owner. Owner rows
are **not GC'd** when a token is removed (orphaned spawns are inert, not auto-deleted — documented).

---

## 8. Migrations & config

- **goose** + `//go:embed` per dialect; `Open` = `goose.Up` → (await inventories) reconcile → seed.
- **Config:** `CP_DB_DRIVER`, `CP_DB_DSN`. SQLite carries pragmas
  (`?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)`); **Postgres carries none** (WAL/foreign_keys
  are SQLite-only). `Open` wires modernc `"sqlite"`+`sqlitedialect` or pgx+`pgdialect`.

---

## 9. Testing

- **Hermetic unit (`:memory:`):** every repo method; **guard rejections** (e.g. `SetActive` on a
  non-`starting` or wrong-gen row fails; `ClaimStarting` from a wrong status → ErrConflict); the
  deleted-filter; version-existence validation; `MarkDeleted` state set.
- **Consistency tests (the headline):** (a) **two concurrent Resumes** → exactly one `ClaimStarting`
  wins (status-guard rowcount), one `provision`; (b) **stale-gen `SpawnStatus` dropped** (incl. the
  no-live-container case → drop); (c) **adopt** on reconnect with matching gen (no new episode);
  (d) **orphan Stop** when node runs a no-live-container/superseded gen; (e) **unreachable → Recreate**
  bumps gen and the old gen is fenced; (f) **unreachable → Wait → node returns → adopt** flips back to
  `active` (the live row was kept); (g) Suspend-vs-Delete can't interleave; (h) the
  **`uniq_live_container` backstop** — a forced 2nd live insert raises a violation classified by
  **constraint name** (`isLiveContainerViolation`, portable across modernc `SQLITE_CONSTRAINT_UNIQUE`
  + pgx `23505`), surfaced as a loud bug, not `ErrConflict`. Run against the `:memory:` store with a
  fake node driver (the existing `runNode` split + `fakeSender` test seam supports this).
- **Schema-drift snapshot:** `goose.Up`, assert each table's columns **and types** match the Bun
  structs per dialect (catches `bool`↔`INTEGER`↔`boolean`).
- **CP handler tests:** real `:memory:` store; ownership rejection on **both** gRPC + WS;
  reconciliation; per-spawn-lock serialization.
- **Postgres schema-soundness test:** a build-tagged test that runs `goose.Up` on Postgres and
  asserts the dialect deltas that actually bite — **the status CHECK rejects a bad value, an upsert
  second-write updates, a `bool` field round-trips true/false, INTEGER-vs-bigint timestamps
  round-trip**. **CI honesty:** this needs a Postgres service in CI (a `.github/workflows` job or a
  `just` recipe with a container) — **there is none today**; this test is **deferred until that CI
  exists** and is marked/skipped-loud (build tag), not silently green.
- **Build-tagged e2e:** the suspend/resume/recreate **verbs don't exist until `sp-mqj`** defines the
  RPCs, so the `sp-pc4`-only e2e is **`create → list → delete` + reconciliation-via-fake-node** (adopt
  / orphan-stop / unreachable). The full `create→suspend→resume→recreate→delete` e2e lands **after
  `sp-mqj`**, and even then — under `Scratch` — asserts **CP-index + reconciliation bookkeeping**, not
  data survival (gated on E3 + `sp-gd9`).

---

## 10. Scope boundary & dependencies

- **`sp-mqj` (hard predecessor):** `cp.v1` RPCs incl. `RecreateSpawn`; `node.v1` `Suspend` message;
  **`generation` on `StartSpawn`/`StopSpawn`/`Suspend`/`SessionOpen`/`SessionClose` + `SpawnStatus`**;
  `StartSpawn` repeated mount field; `SUSPENDED` phase + suspend-complete signal w/ per-mount markers;
  **`RunningSpawn` inventory on `Register`/`Heartbeat`**.
- **`sp-gd9`:** node-side per-mount persist/restore + **server-side backend write-fence** by
  generation, inventory reporting, binds CP-sent mounts, idle timer, takeover fence, web UI (incl.
  the `unreachable`/Recreate control + scratch-reset/recovered notices).
- **E3:** persistent backend → lossless suspend/resume (the suspend path reuses E3 **incremental**
  push/bundle). **E5:** version registration populating `app_version_mounts` + `spawn.yml` reconcile.

---

## 11. Success criteria

1. `go test ./...` (hermetic) green: repos, guard rejections, deleted-filter, version validation,
   schema-drift (columns **and** types), the **live-container uniqueness invariant** (a 2nd live
   container insert fails), and the **consistency tests** (two-resume, stale-gen drop, adopt,
   orphan-stop, unreachable→recreate-fences-old) — on `:memory:`, no Docker/cgo.
2. The CP uses the store for create/ownership(gRPC+WS)/list/suspend/resume/recreate/delete +
   **inventory reconciliation**; the in-memory `apps`/owner maps are gone; cleanup uses Destroy.
3. A transient node stream-drop **adopts** on reconnect (no second container); a real failure →
   `unreachable` → user **Recreate** fences the old container.
4. CP restart reconciles via node inventory (adopt / `unreachable` / marker-probed `suspending`), not
   blind flips; `recovered` is set only on recreate-from-unclean, not a clean restart.
5. App versioning: create validates the version exists; delisting (not ref-deletion) doesn't break
   resume.
6. The Postgres schema-soundness test (when CI exists) round-trips CHECK/upsert/bool/timestamps.
