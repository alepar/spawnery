# Spawnery — Control-Plane State / DAO Layer (Design)

**Bead:** `sp-pc4`
**Status:** Draft **v3** (post 2nd adversarial roast — consistency protocol; pending review)
**Date:** 2026-05-31
**Supersedes:** the preliminary schema in
[State/DAO Research Brief](2026-05-31-state-dao-layer-research-brief.md).
**Depends on:** [Spawn Lifecycle](2026-05-31-spawn-lifecycle-design.md) (status machine + the
consistency protocol §6) and the contracts predecessor **`sp-mqj`** (now incl. **episode generation
on every message + node inventory on Register/Heartbeat**).
**Hands off to:** `sp-gd9` (node-side per-mount suspend/resume + generation/inventory + idle), **E3**
(persistent backend), **E5** (manifest/catalog).

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
  generation   INTEGER NOT NULL DEFAULT 0,    -- bumped per starting episode; fencing token
  node_id      TEXT,                          -- current episode's node; NULL when not active
  recovered    INTEGER NOT NULL DEFAULT 0,    -- pg: boolean; set on recreate-from-unclean (NOT clean CP restart)
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL,
  suspended_at INTEGER,
  deleted_at   INTEGER
);
CREATE INDEX idx_spawns_owner  ON spawns(owner_id, last_used_at DESC);
CREATE INDEX idx_spawns_status ON spawns(status);
CREATE INDEX idx_spawns_node   ON spawns(node_id);

CREATE TABLE spawn_mounts (
  spawn_id       TEXT NOT NULL REFERENCES spawns(id),
  name           TEXT NOT NULL,               -- must ∈ app_version_mounts (repo-validated)
  backend_uri    TEXT NOT NULL,
  persist_marker TEXT,                         -- per-mount suspend state; written INCREMENTALLY per mount
  PRIMARY KEY (spawn_id, name)
);
```

**Notes:** `app_ref` survives version **delisting** (catalog hide) but **not git-ref deletion** —
if the App's definition tag is deleted, resume's node-side clone fails regardless (system design §2;
mitigated only by post-MVP App-snapshot). `persist_marker` is per-mount and written **as each mount
finishes** so crash recovery can distinguish "none done" from "all done, signal lost" (lifecycle
§6.6).

---

## 4. Domain types & interfaces

Bun tags on domain types; repos over `bun.IDB`. **All transitions are status+generation-guarded**
(`UPDATE … WHERE id=? AND status IN(<from>) [AND generation=?]`, assert rowcount=1) and run under a
**per-spawn lock** held by the CP layer across `{claim → node command → await}`.

```go
type Status string // starting|active|suspending|suspended|unreachable|error|deleted

type Spawn struct {
    ID, OwnerID, AppID, AppVersion, AppRef, Model string
    Pinned, Recovered bool
    Status     Status
    Generation int64
    NodeID     string
    CreatedAt, LastUsedAt int64
    SuspendedAt, DeletedAt *int64
}
type Mount     struct { Name, BackendURI, PersistMarker string }
type MountDecl struct { AppID, Version, Name string; Required bool }
type RunningSpawn struct { ID string; Generation int64; Phase string } // from node inventory

type SpawnRepo interface {
    Create(ctx, s Spawn, mounts []Mount) error      // status=starting, generation=1; validates version + mount names
    Get(ctx, id string) (Spawn, error)              // NOT-FOUND on deleted for lifecycle ops
    GetMounts(ctx, id string) ([]Mount, error)
    ListByOwner(ctx, ownerID string) ([]Spawn, error)

    // claim-then-act: ClaimStarting bumps generation, returns the new gen (ErrConflict if rowcount≠1)
    ClaimStarting(ctx, id string, from []Status) (newGen int64, err error)
    SetActive(ctx, id, nodeID string, gen int64) error      // WHERE status='starting' AND generation=gen
    SetSuspending(ctx, id string, gen int64) error          // WHERE status='active'   AND generation=gen
    SetMountMarker(ctx, id, mount, marker string) error     // incremental, per mount
    SetSuspended(ctx, id string, gen int64) error           // WHERE status='suspending' AND generation=gen
    SetError(ctx, id string) error
    MarkUnreachable(ctx, ids []string) (int, error)         // node deemed failed
    MarkRecovered(ctx, id string) error
    Touch(ctx, id string, ts int64) error
    MarkDeleted(ctx, id string, ts int64) error             // WHERE status IN('active','suspended','unreachable','error')

    // reconciliation inputs
    ActiveByNode(ctx, nodeID string) ([]Spawn, error)       // CP diffs vs node inventory
    Adopt(ctx, id, nodeID string, gen int64) error          // confirm a still-running episode
}
```
`OwnerRepo`/`AppRepo` as v2 (apps + versions + `DeclaredMounts` + `UpsertVersion(v, mounts)`).
`Store.WithTx` composes repos. The **per-spawn lock** is a CP-layer keyed mutex (not the DB) — the DB
guard prevents illegal *state* transitions; the lock prevents two handlers interleaving their *node
commands* for one spawn.

---

## 5. Transactions & per-spawn lock

`Store.WithTx` (Bun `RunInTx`) makes `Create` (spawn + mounts) and `UpsertVersion` (version + decls)
atomic. Orthogonally, the CP holds a **per-spawn-id lock** around every mutating op so that the
claim-guard and the node command can't be split by a competing handler (lifecycle §6.3).

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
the WebSocket `HandleWS`** read ownership from the DB. `Router.Owner()` is removed; the route is a
**projection of `active` rows** (rebuilt on adopt/resume, torn on suspend/delete). **Scheduler split:**
`mint()` + `provision(id, gen, appRef, model, mounts) (nodeID, err)`; `CreateSpawn` mints,
`Resume`/`Recreate` reuse the id. **The node binds the CP-sent `mounts`**, validating names against
its manifest at the ref (mismatch → `error`).

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

**Inventory reconciliation** (on every node (re)connect, diff `RunningSpawn[]` vs `ActiveByNode`):
- gen matches a DB-`active` row → **`Adopt`** (rebind route, no restart).
- node runs a spawn the DB says `suspended`/`deleted`/`error`/older-gen → **`Stop(id, gen)`** (orphan).
- DB-`active`/`starting` not in the node's inventory (and unclaimed elsewhere) after grace →
  **`MarkUnreachable`** (user-driven recovery).

**Generation fencing at the CP:** every node→CP `SpawnStatus` carries a generation; the handler
**drops it if `gen ≠ row.generation`** (kills stale ACTIVE/SUSPENDED from a superseded episode). A
guard rowcount≠1 is treated as a **superseded no-op** (and any markers the dropped report carried are
scheduled for GC).

**`spawn.yml` vs the DB cache:** the DB cache (`app_ref`, `app_version`, `model`, mounts) is
**authoritative-in-practice for routing/provisioning until E5** (the CP can't read `spawn.yml` yet);
auto-upgrade mutates the cache; `spawn.yml` reconciliation is E5.

**Seeding (id mismatch fixed):** the canonical app id is the **manifest `id`** (`spawnery/secret`),
**not** the resolver key (`secret-app`). `Open` seeds `apps(id='spawnery/secret')` + its version +
declared mounts, and `cmd/cp/main.go`'s resolver key is corrected to match (one-line change) so E5
registration (which reads the manifest `id`) won't orphan spawn FKs. Owners are seeded **from the
`CP_DEV_TOKENS` map** (every token's owner → a row), so `auth` always resolves to a real owner.
Owner rows are **not GC'd** when a token is removed (orphaned spawns are inert, not auto-deleted —
documented).

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
  wins, one `provision`; (b) **stale-generation `SpawnStatus` dropped**; (c) **adopt** on reconnect
  with matching gen (no new episode); (d) **orphan Stop** when node runs a non-active/older-gen
  spawn; (e) **unreachable → Recreate** bumps gen and the old gen is fenced; (f) Suspend-vs-Delete
  can't interleave. These run against the `:memory:` store with a fake node driver.
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
- **Build-tagged e2e:** `create→list→suspend→resume→recreate→delete` through the stub agent;
  **honest scope:** under `Scratch` this asserts **CP-index + reconciliation bookkeeping**, not data
  survival (gated on E3 + `sp-gd9`).

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
   schema-drift (columns **and** types), the **consistency tests** (two-resume, stale-gen drop,
   adopt, orphan-stop, unreachable→recreate-fences-old) — on `:memory:`, no Docker/cgo.
2. The CP uses the store for create/ownership(gRPC+WS)/list/suspend/resume/recreate/delete +
   **inventory reconciliation**; the in-memory `apps`/owner maps are gone; cleanup uses Destroy.
3. A transient node stream-drop **adopts** on reconnect (no second container); a real failure →
   `unreachable` → user **Recreate** fences the old container.
4. CP restart reconciles via node inventory (adopt / `unreachable` / marker-probed `suspending`), not
   blind flips; `recovered` is set only on recreate-from-unclean, not a clean restart.
5. App versioning: create validates the version exists; delisting (not ref-deletion) doesn't break
   resume.
6. The Postgres schema-soundness test (when CI exists) round-trips CHECK/upsert/bool/timestamps.
