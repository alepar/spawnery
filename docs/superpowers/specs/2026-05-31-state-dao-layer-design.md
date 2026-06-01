# Spawnery — Control-Plane State / DAO Layer (Design)

**Bead:** `sp-pc4`
**Status:** Draft **v2** (post adversarial roast; pending user review)
**Date:** 2026-05-31
**Supersedes:** the preliminary schema in
[State/DAO Research Brief](2026-05-31-state-dao-layer-research-brief.md).
**Depends on:** [Spawn Lifecycle](2026-05-31-spawn-lifecycle-design.md) (status machine + per-mount
markers), [System Design](2026-05-26-spawnery-system-design.md) §2/§8, and **a hard contracts
predecessor** `sp-mqj` (§10).
**Hands off to:** `sp-gd9` (node-side per-mount suspend/resume + idle timer + web UI), **E3**
(persistent backend; lossless suspend/resume gates on it), **E5** (manifest/catalog).

> **v2 changelog (what the roast changed):** mounts are **declared on the app version** at
> registration (CP never parses a manifest at spawn time) and **chosen in the CreateSpawn request**;
> `scheduler.Create` is **split** so a pre-minted/existing id can be (re)provisioned; the
> **WebSocket** entry point is in scope alongside gRPC; **all state transitions are status-guarded**;
> `Get` **filters deleted** for lifecycle ops; **seed owners from the token map**; the **DB cache is
> declared authoritative-until-E5**; Postgres ships as a **schema-soundness test**, not a CI stack;
> a **schema-drift snapshot test** guards Bun-tags-vs-goose-DDL; and the e2e claims are **reworded
> honestly** (bookkeeping under scratch; lossless gates on E3).

---

## 1. Goal & scope

Replace the CP's ephemeral in-memory maps with a durable, transactional **state layer**, and on it
implement the **entire CP-side spawn lifecycle**. Today the CP has *no durable record of a spawn*;
ownership/routing live only in `router.Router`'s in-memory `route`. This layer makes the `spawns`
table the **CP index** (system design §2) and the source of truth for ownership and lifecycle.

**In scope (`sp-pc4`):** the `store` package (schema, migrations, interfaces, Bun impl, hermetic
tests); the full **CP-side state machine** (status-guarded transitions, boot + node-evict
reconciliation, idempotent seeding); the lifecycle **RPCs** (`CreateSpawn` rewired, `ListSpawns`,
`SuspendSpawn`, `ResumeSpawn`, `DeleteSpawn`, `Session`/WS ownership + auto-resume); the
**CP→node plumbing** (the `Suspend` message, `StartSpawn` mounts, the `SUSPENDED` phase handling).

**Out of scope (→ `sp-gd9` / E1 / E3):** node-side **per-mount** persist/restore (writes the
per-mount markers), the idle timer, the web UI, and a persistent storage backend. **Honesty note:**
until those land, the node's suspend/resume is a **stub teardown** on `storage.Scratch` — the CP
state machine is real and testable, but **data does not survive a suspend**. Lossless suspend/resume
gates on E3 + `sp-gd9` (lifecycle §8).

**Stays in-memory:** `registry.Registry`, `router.Router` (live relay), `scheduler` signals, the
node's `spawnlet.Store`.

---

## 2. Stack (from the research brief)

**Bun** over **modernc.org/sqlite** (pure-Go, cgo-free, driver name **`"sqlite"`** — *not* mattn's
`"sqlite3"`) and **PostgreSQL/pgx** (server); **goose** migrations (`//go:embed`, two dialect
trees); **SQLite `:memory:`** hermetic tests; repos over **`bun.IDB`** with `Store.WithTx`. Opaque
**TEXT** ids (spawns: **uuidv7** via `uuid.NewV7()`, available in google/uuid ≥ v1.6.0 — bump from
the current `uuid.NewString()`/v4), **INTEGER unix-seconds** timestamps, status **TEXT + CHECK**.

---

## 3. Schema

SQLite DDL shown; Postgres is the same shape (`text`/`bigint`); only the goose trees differ.

```sql
CREATE TABLE owners (
  id TEXT PRIMARY KEY, email TEXT, created_at INTEGER NOT NULL
);

CREATE TABLE apps (
  id TEXT PRIMARY KEY, display_name TEXT, created_at INTEGER NOT NULL
);

-- immutable, content-addressed versions (git tags); "ref" lives here
CREATE TABLE app_versions (
  app_id TEXT NOT NULL REFERENCES apps(id),
  version TEXT NOT NULL, ref TEXT NOT NULL,
  reviewed INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (app_id, version)
);
CREATE INDEX idx_app_versions_reviewed ON app_versions(app_id, reviewed, created_at DESC);

-- declared mounts per version — extracted from the manifest ONCE at version registration,
-- so the CP never parses a manifest at spawn time (roast D1)
CREATE TABLE app_version_mounts (
  app_id TEXT NOT NULL, version TEXT NOT NULL,
  name TEXT NOT NULL,                  -- declared mount name (path under /app)
  required INTEGER NOT NULL DEFAULT 1, -- whether the user must bind it
  PRIMARY KEY (app_id, version, name),
  FOREIGN KEY (app_id, version) REFERENCES app_versions(app_id, version)
);

-- the CP index: durable lifecycle record + thin resume-critical config pointer
CREATE TABLE spawns (
  id           TEXT PRIMARY KEY,        -- uuidv7, stable across the lifecycle
  owner_id     TEXT NOT NULL REFERENCES owners(id),
  app_id       TEXT NOT NULL REFERENCES apps(id),   -- FK to apps only (see note)
  app_version  TEXT NOT NULL,           -- pinned snapshot; validated to exist at create (repo-level)
  app_ref      TEXT NOT NULL,           -- denormalized content ref; survives version delisting
  pinned       INTEGER NOT NULL DEFAULT 0,
  model        TEXT NOT NULL,
  status       TEXT NOT NULL
               CHECK (status IN ('starting','active','suspending','suspended','error','deleted')),
  node_id      TEXT,                     -- current active episode; NULL when suspended
  recovered    INTEGER NOT NULL DEFAULT 0, -- set when reconciled from an unclean shutdown (lifecycle §6)
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL,
  suspended_at INTEGER,
  deleted_at   INTEGER
);
CREATE INDEX idx_spawns_owner  ON spawns(owner_id, last_used_at DESC);
CREATE INDEX idx_spawns_status ON spawns(status);   -- reconciliation scans
CREATE INDEX idx_spawns_node   ON spawns(node_id);

-- per-spawn mount choices (the user's backend pick per declared mount) + per-mount persist marker
CREATE TABLE spawn_mounts (
  spawn_id       TEXT NOT NULL REFERENCES spawns(id),
  name           TEXT NOT NULL,         -- must match an app_version_mounts.name (repo-validated)
  backend_uri    TEXT NOT NULL,         -- managed:<repo> | github:owner/repo | gdrive:<id> | scratch
  persist_marker TEXT,                  -- per-mount suspend state (WIP branch / bundle marker); sp-gd9 writes
  PRIMARY KEY (spawn_id, name)
);
```

**Design notes**
- `spawns.app_id` FKs `apps` only; `app_version`/`app_ref` are a **denormalized pinned snapshot** so a
  spawn survives version delisting (system design §8). The FK can't enforce version existence, so the
  **repo validates `(app_id, app_version) ∈ app_versions` at create** (and a test asserts it).
- **Per-mount persist markers** live on `spawn_mounts.persist_marker` — there is **no single
  `suspend_ref`** (the roast killed the single-repo assumption).
- **Soft-delete** (`status='deleted'` + `deleted_at`); **every lifecycle `Get`/lookup filters
  deleted** (§4) — not just `ListByOwner`.
- `app_version_mounts` is populated at **version registration** (E5; seeded for the demo). The full
  manifest stays authoritative in `spawneryapp.yml`; only mount *names* are cached.

---

## 4. Domain types & interfaces (`internal/cp/store/store.go`)

Bun tags on the domain types. Repos run on `bun.IDB`. **Transitions are status-guarded** — each
`Set*` issues `UPDATE … WHERE id=? AND status IN(<valid-from>)` and returns an error if rowcount≠1,
so illegal transitions can't silently succeed (roast D16).

```go
type Status string // starting|active|suspending|suspended|error|deleted

type Owner       struct { ID, Email string; CreatedAt int64 }
type App         struct { ID, DisplayName string; CreatedAt int64 }
type AppVersion  struct { AppID, Version, Ref string; Reviewed bool; CreatedAt int64 }
type MountDecl   struct { AppID, Version, Name string; Required bool }
type Mount       struct { Name, BackendURI string; PersistMarker string }
type Spawn struct {
    ID, OwnerID, AppID, AppVersion, AppRef, Model string
    Pinned, Recovered bool
    Status     Status
    NodeID     string
    CreatedAt, LastUsedAt int64
    SuspendedAt, DeletedAt *int64
}

type OwnerRepo interface {
    Get(ctx, id string) (Owner, error)
    Upsert(ctx, o Owner) error
}
type AppRepo interface {
    Get(ctx, id string) (App, error)
    List(ctx) ([]App, error)
    Upsert(ctx, a App) error
    UpsertVersion(ctx, v AppVersion, mounts []MountDecl) error // version + declared mounts together
    GetVersion(ctx, appID, version string) (AppVersion, error)
    LatestReviewed(ctx, appID string) (AppVersion, error)
    DeclaredMounts(ctx, appID, version string) ([]MountDecl, error) // for the CreateSpawn surface
}
type SpawnRepo interface {
    Create(ctx, s Spawn, mounts []Mount) error // status=starting; validates version + mount names
    Get(ctx, id string) (Spawn, error)         // NOT-FOUND on deleted for lifecycle ops
    GetMounts(ctx, id string) ([]Mount, error)
    ListByOwner(ctx, ownerID string) ([]Spawn, error) // excludes deleted

    SetActive(ctx, id, nodeID string) error    // WHERE status='starting'
    SetSuspending(ctx, id string) error        // WHERE status='active'
    SetSuspended(ctx, id string, markers map[string]string) error // WHERE status='suspending'
    SetError(ctx, id string) error
    Touch(ctx, id string, ts int64) error
    MarkDeleted(ctx, id string, ts int64) error // WHERE status != 'deleted'

    ReconcileOrphans(ctx) (int, error)          // {starting,active}->suspended(recovered); {suspending}->error
    ReconcileNode(ctx, nodeID string) (int, error) // that node's {starting,active}->suspended
}
type Store interface {
    Owners() OwnerRepo; Apps() AppRepo; Spawns() SpawnRepo
    WithTx(ctx, fn func(Store) error) error
    Close() error
}
```

Repos never open their own tx; `Store.WithTx` composes them (so `Create`'s spawn-row + N
mount-rows insert atomically when the caller wraps in `WithTx`).

---

## 5. Transaction boundary

```go
func (s *Store) WithTx(ctx, fn func(store.Store) error) error {
    return s.db.RunInTx(ctx, nil, func(ctx, tx bun.Tx) error { return fn(&Store{db: tx}) })
}
```
Real uses now: `CreateSpawn` (spawn + mounts), `UpsertVersion` (version + declared mounts). Future:
spawn write + audit event.

---

## 6. Package layout

```
internal/cp/store/
  store.go            # domain types + interfaces (§4)
  open.go             # Open(ctx,Config)->Store: driver+dialect, goose.Up, ReconcileOrphans, seed
  bunstore/{bunstore,owners,apps,spawns}.go   # impl over bun.IDB
  bunstore/testing.go # NewTestStore(t) -> :memory: + goose.Up
  bunstore/schema_test.go  # drift guard: goose.Up then assert table columns == struct fields
  migrations/sqlite/0001_init.sql
  migrations/pg/0001_init.sql
```

---

## 7. CP-side lifecycle (integration)

The store replaces `apps.Resolver` and the router's `owner` authority. `Server` gains `st
store.Store`, drops `*apps.Resolver`; `router.Bind` drops `owner`; `Router.Owner()` is removed —
**and both client entry points are migrated: gRPC `Session` (`server.go`) AND the WebSocket
`HandleWS` (`ws.go`)** (roast D5). New CP→node plumbing: a `Suspend` message, `StartSpawn` carrying
the chosen mounts, and a `SUSPENDED` phase the node can report.

**Scheduler refactor (roast D3/D4):** split `scheduler.Create` (which today mints its own id) into
`mint()` + `provision(id, appRef, model, mounts) (nodeID, error)`. `CreateSpawn` mints the id;
`ResumeSpawn` reuses the existing id — both call `provision`.

**Ordering (roast D6):** `CreateSpawn` commits the `starting` row **before** `provision`; on the
ACTIVE signal the server calls `SetActive(id,nodeID)` (status-guarded `WHERE status='starting'`);
telemetry reads owner from the committed row, not the race-prone `route`. If `provision`
times out/errs → `SetError`.

| RPC / event | Behavior |
|---|---|
| **`CreateSpawn`** (request now carries per-mount `{name, backend_uri}`) | `LatestReviewed(appID)`; validate chosen mount names ⊆ `DeclaredMounts`; mint **uuidv7**; `WithTx{ Spawns().Create(starting, mounts) }`; `provision(id, app_ref, model, mounts)`; ACTIVE→`SetActive`, err→`SetError`. |
| **`ListSpawns`** | `Spawns().ListByOwner(owner)`. |
| **`SuspendSpawn`** | `Get` (reject deleted); `SetSuspending`; send node `Suspend`; on `SUSPENDED` report → `SetSuspended(id, markers)` + `router.Drop`. |
| **`ResumeSpawn`** | `Get` (require `suspended`); `GetMounts`; `provision(id, app_ref, model, mounts)`; `SetActive`. |
| **`DeleteSpawn`** (Destroy) | `Get` (reject deleted/`suspending`); if active, node `Stop` + `router.Drop`; `MarkDeleted`. Data backend preserved. |
| **`Session` / WS attach** | ownership via `Get` (reject deleted); if `suspended`, **auto-resume** then attach; **takeover** closes the prior client; `Touch(id, now)`. |
| **node evict** (`DropNode`) | `ReconcileNode(nodeID)` → those spawns `suspended`. |
| **CP boot** (`Open`) | `ReconcileOrphans()` (after migrations, before serving). |

**`spawn.yml` vs the DB cache (roast D14):** the DB cache (`app_ref`, `app_version`, `model`,
mounts) is **authoritative-in-practice for routing/provisioning until E5** gives the CP the ability
to read `spawn.yml`. The auto-upgrade hook (if `pinned=0`, bump to `LatestReviewed`) mutates the DB
cache; `spawn.yml` reconciliation is explicitly E5. This inverts the system-design "spawn.yml
authoritative" claim *for now*, by necessity, and is stated so.

**Seeding (roast D12/D13):** `Open` idempotently `Upsert`s, from `cmd/cp/main.go`'s config: an owner
**for every `CP_DEV_TOKENS` entry** (so `auth`'s token→owner always resolves to a real row — today's
config is `dev-token=dev`, so seed owner `dev`), plus the demo app(s) + version(s) + declared mounts
(today's app is `secret-app`→ref — the seed must match the actual config, not an aspirational
`zork`). E4/E5 replace the seed; no schema change.

---

## 8. Migrations & config

- **goose** with `//go:embed migrations/<dialect>/*.sql`; `Open` runs `goose.Up` then reconciles
  then seeds.
- **Config (env):** `CP_DB_DRIVER=sqlite|postgres`, `CP_DB_DSN`. **SQLite** DSN carries pragmas
  (`file:cp.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)`); **Postgres** DSN carries **none
  of those** (WAL/foreign_keys pragmas are SQLite-only). `Open(ctx, Config)` wires the modernc
  driver (`"sqlite"`) + `sqlitedialect`, or pgx + `pgdialect`.
- Forward-only; staging carries data across versions. `atlas migrate lint` on the pg tree is a later
  CI nicety.

---

## 9. Testing

- **Hermetic unit (`:memory:`):** `NewTestStore(t)` opens
  `file:<test>?mode=memory&cache=shared&_pragma=foreign_keys(1)` (no WAL in `:memory:`), `goose.Up`,
  clean `Store`. Covers every repo method, **status-guarded transition rejections** (e.g. `SetActive`
  on a `suspended` row fails), the deleted-filter, version-existence validation, and reconciliation.
  No Docker/cgo; `t.Parallel()`-safe.
- **Schema-drift snapshot test:** `goose.Up` on `:memory:`, then assert each table's columns match the
  Bun struct fields (Bun doesn't generate the DDL — goose does — so nothing else reconciles them).
- **CP handler tests:** real `:memory:` store replaces the in-memory maps; assert transitions,
  ownership rejection (both gRPC + WS), list output, reconciliation.
- **Postgres schema-soundness test (Decision 3):** a dedicated build-tagged test (CI provides a
  Postgres service container, **not** the app stack) that runs `goose.Up` on Postgres and does a
  **write-then-read-back per table** to prove the pg DDL tree is sound and the dialect round-trips.
  This keeps the second tree honest without running the whole CP on Postgres.
- **Build-tagged e2e (`//go:build e2e`, fail-loud, never skip):** drives `create→list→suspend→
  resume→delete` through the stub agent. **Honest scope:** under `Scratch` + stub teardown this
  asserts **CP-index state-machine bookkeeping** (status flips, ownership, reconciliation,
  per-transition guards) — **not** data survival. Lossless suspend/resume is verified once E3's
  persistent backend + `sp-gd9` land.

---

## 10. Scope boundary & dependencies

- **`sp-mqj` (contracts) is a hard predecessor:** `cp.v1` lifecycle RPCs; `node.v1` `Suspend`
  message; `StartSpawn` repeated mount field; a `SUSPENDED` `SpawnPhase` + a node→CP suspend-complete
  signal. `sp-pc4` cannot land without it.
- **`sp-gd9`** consumes this store: node-side **per-mount** persist (writes `spawn_mounts.persist_marker`)
  + restore, the idle timer, the takeover fence, the web UI.
- **E3** supplies the persistent backend → lossless suspend/resume (lifecycle §8 gates the demo
  lifecycle on it).
- **E5** supplies real version registration (populating `app_version_mounts` from manifests) +
  `spawn.yml` reconciliation; the schema is ready for it.

---

## 11. Success criteria

1. `go test ./...` (hermetic) green: repos, **status-guarded transitions**, deleted-filter,
   version-existence validation, reconciliation, schema-drift snapshot — on `:memory:`, no Docker/cgo.
2. The CP uses the store for create/ownership(**gRPC + WS**)/list/suspend/resume/delete +
   boot/evict reconciliation; the in-memory `apps`/owner-authority maps are gone; cleanup uses
   **Destroy**, not Suspend.
3. The Postgres schema-soundness test migrates + round-trips every table on a CI Postgres.
4. A CP restart reconciles orphans to `suspended` (marked `recovered`) and `suspending` to `error`.
5. App versioning: a spawn pins `app_version`/`app_ref`; create validates the version exists;
   delisting a version doesn't break existing spawns.
6. The build-tagged e2e asserts CP-index transitions through create→suspend→resume→delete
   (**data-loss expected under scratch**; lossless gates on E3 + `sp-gd9`).
