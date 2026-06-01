# Spawnery — Control-Plane State / DAO Layer (Design)

**Bead:** `sp-pc4`
**Status:** Draft v1 (approved in brainstorming; pending user review)
**Date:** 2026-05-31
**Supersedes:** the preliminary schema in
[State/DAO Research Brief](2026-05-31-state-dao-layer-research-brief.md) (that doc remains the
requirements + research record; this is the design).
**Depends on:** [Spawn Lifecycle](2026-05-31-spawn-lifecycle-design.md) (fixes the status machine
+ index fields), [System Design](2026-05-26-spawnery-system-design.md) §2/§3.
**Hands off to:** `sp-gd9` (node-side suspend/resume mechanics + idle timer + web UI),
E3 (persistent storage backend), E5 (manifest/catalog caching).

---

## 1. Goal & scope

Replace the CP's ephemeral in-memory maps with a durable, transactional **state layer**, and on
top of it implement the **entire CP-side spawn lifecycle**. Today the CP has *no durable record of
a spawn*; ownership and routing live only in `router.Router`'s in-memory `route`. This layer makes
the `spawns` table the **CP index** (system design §2) and the source of truth for ownership and
lifecycle.

**In scope (`sp-pc4`):**
- The `store` package: schema, migrations, domain types + interfaces, the Bun implementation,
  hermetic `:memory:` tests.
- The full **CP-side state machine**: status transitions, boot + node-evict reconciliation,
  idempotent seeding.
- The lifecycle **RPCs**: `CreateSpawn` (rewired), `ListSpawns`, `SuspendSpawn`, `ResumeSpawn`,
  `DeleteSpawn`, and `Session` (ownership via the store + auto-resume-on-attach).
- The **CP→node message plumbing** for suspend/resume (a new `Suspend` message; `StartSpawn`
  carries mounts) — the node *accepts* these; its real persist/restore is `sp-gd9`.

**Out of scope (→ `sp-gd9` / E1 / E3):** the node-side WIP-ref persist + restore, the two-stage
idle timer, the web UI, and a real persistent storage backend. **Honesty note:** until those land,
the node's suspend/resume is a **stub teardown** — the CP state machine is real and testable, but
data does not actually survive a suspend (current storage is `Scratch`). Suspend→resume becomes
lossless when E3 (persistent backend) + `sp-gd9` (WIP ref) ship.

**Stays in-memory (unchanged):** `registry.Registry` (live nodes), `router.Router` (live relay
senders + routing), `scheduler` pending signals, the node's `spawnlet.Store` (container handles).

---

## 2. Stack (from the research brief)

**Bun** (uptrace/bun, thin ORM, single dialect source) over **modernc.org/sqlite** (pure-Go,
cgo-free embedded) and **PostgreSQL/pgx** (server, later) — **both backends from day one**;
**goose** migrations (`//go:embed`, two dialect trees); **SQLite `:memory:`** for hermetic tests;
**repository interfaces over `bun.IDB`** with `Store.WithTx`. Opaque **TEXT** ids (uuidv7 for
spawns), **INTEGER unix-seconds** timestamps, status as **TEXT + CHECK**.

---

## 3. Schema

Five tables. SQLite DDL shown; Postgres is the same shape with `text`/`bigint` (Bun emits the
dialect; only the goose DDL trees differ).

```sql
-- owners — identity (token map seeds these for the demo; E4 OAuth writes them later)
CREATE TABLE owners (
  id         TEXT PRIMARY KEY,            -- opaque handle ("alice") now; uuidv7 under OAuth
  email      TEXT,
  created_at INTEGER NOT NULL
);

-- apps — catalog entry (identity only)
CREATE TABLE apps (
  id           TEXT PRIMARY KEY,          -- public app_id, e.g. "zork"
  display_name TEXT,
  created_at   INTEGER NOT NULL
);

-- app_versions — immutable, content-addressed versions (git tags)
CREATE TABLE app_versions (
  app_id     TEXT NOT NULL REFERENCES apps(id),
  version    TEXT NOT NULL,               -- semver / git tag, immutable
  ref        TEXT NOT NULL,               -- content-addressed ref the node mounts at /app
  reviewed   INTEGER NOT NULL DEFAULT 0,  -- 0/1; "latest reviewed tag" channel (sys-design §8)
  created_at INTEGER NOT NULL,
  PRIMARY KEY (app_id, version)
);
CREATE INDEX idx_app_versions_reviewed ON app_versions(app_id, reviewed, created_at DESC);

-- spawns — the CP index: durable lifecycle record + thin resume-critical config pointer
CREATE TABLE spawns (
  id           TEXT PRIMARY KEY,          -- uuidv7, stable across the whole lifecycle
  owner_id     TEXT NOT NULL REFERENCES owners(id),
  app_id       TEXT NOT NULL REFERENCES apps(id),     -- FK to apps only (NOT composite to versions)
  app_version  TEXT NOT NULL,             -- the version this spawn currently runs (pinned snapshot)
  app_ref      TEXT NOT NULL,             -- denormalized content ref; survives version delisting
  pinned       INTEGER NOT NULL DEFAULT 0,-- 1 = locked to app_version; 0 = auto-upgrade to latest reviewed
  model        TEXT NOT NULL,
  status       TEXT NOT NULL
               CHECK (status IN ('starting','active','suspending','suspended','error','deleted')),
  node_id      TEXT,                       -- current active episode; NULL when suspended
  suspend_ref  TEXT,                       -- refs/spawnery/suspend/<id> marker; filled by sp-gd9
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL,           -- = created_at at create; bumped on attach/activity
  suspended_at INTEGER,                    -- NULL when active
  deleted_at   INTEGER                     -- NULL unless soft-deleted
);
CREATE INDEX idx_spawns_owner  ON spawns(owner_id, last_used_at DESC);
CREATE INDEX idx_spawns_status ON spawns(status);
CREATE INDEX idx_spawns_node   ON spawns(node_id);

-- spawn_mounts — per-mount storage bindings (the "repo location + provider binding" of sys-design §2)
CREATE TABLE spawn_mounts (
  spawn_id    TEXT NOT NULL REFERENCES spawns(id),
  name        TEXT NOT NULL,               -- mount name (path under /app)
  backend_uri TEXT NOT NULL,               -- managed:<repo> | github:owner/repo | gdrive:<id> | scratch
  PRIMARY KEY (spawn_id, name)
);
```

**Design notes**
- `spawns.app_id` FKs `apps` only; `app_version`/`app_ref` are a **denormalized pinned snapshot**,
  so a spawn survives a version being delisted (system design §8 reconstruction caveat) with no
  cascade blocking.
- `app_versions` is minimal; the full manifest stays authoritative in `spawneryapp.yml` — caching
  it is **E5**'s job. "Latest reviewed" = `MAX(created_at) WHERE reviewed=1`.
- Soft-delete (`status='deleted'` + `deleted_at`), filtered out of lists — audit/append-friendly.
- `suspend_ref` is written empty by this layer (stub teardown) and filled once `sp-gd9` persists
  the real WIP ref.

---

## 4. Domain types & interfaces (`internal/cp/store/store.go`)

Bun tags live on the domain types (agreed: minimal boilerplate). Repos run on `bun.IDB`.

```go
type Status string
const (
    Starting   Status = "starting"
    Active     Status = "active"
    Suspending Status = "suspending"
    Suspended  Status = "suspended"
    Errored    Status = "error"
    Deleted    Status = "deleted"
)

type Owner      struct { ID, Email string; CreatedAt int64 }
type App        struct { ID, DisplayName string; CreatedAt int64 }
type AppVersion struct { AppID, Version, Ref string; Reviewed bool; CreatedAt int64 }
type Mount      struct { Name, BackendURI string }
type Spawn struct {
    ID, OwnerID, AppID, AppVersion, AppRef, Model string
    Pinned      bool
    Status      Status
    NodeID      string   // "" when suspended
    SuspendRef  string   // "" until sp-gd9 fills it
    CreatedAt, LastUsedAt int64
    SuspendedAt, DeletedAt *int64
}

type OwnerRepo interface {
    Get(ctx context.Context, id string) (Owner, error)
    Upsert(ctx context.Context, o Owner) error
}
type AppRepo interface {
    Get(ctx context.Context, id string) (App, error)
    List(ctx context.Context) ([]App, error)
    Upsert(ctx context.Context, a App) error
    UpsertVersion(ctx context.Context, v AppVersion) error
    GetVersion(ctx context.Context, appID, version string) (AppVersion, error)
    LatestReviewed(ctx context.Context, appID string) (AppVersion, error) // resolution at create
}
type SpawnRepo interface {
    Create(ctx context.Context, s Spawn, mounts []Mount) error // status=starting; row + mounts
    Get(ctx context.Context, id string) (Spawn, error)
    GetMounts(ctx context.Context, id string) ([]Mount, error)
    ListByOwner(ctx context.Context, ownerID string) ([]Spawn, error) // excludes deleted

    SetActive(ctx context.Context, id, nodeID string) error
    SetSuspending(ctx context.Context, id string) error
    SetSuspended(ctx context.Context, id, suspendRef string) error
    SetError(ctx context.Context, id string) error
    Touch(ctx context.Context, id string, ts int64) error
    MarkDeleted(ctx context.Context, id string, ts int64) error

    ReconcileOrphans(ctx context.Context) (int, error)          // boot: {starting,active,suspending}->suspended
    ReconcileNode(ctx context.Context, nodeID string) (int, error) // node evict: same -> suspended
}
type Store interface {
    Owners() OwnerRepo
    Apps()   AppRepo
    Spawns() SpawnRepo
    WithTx(ctx context.Context, fn func(Store) error) error
    Close() error
}
```

Repos never open their own transaction; `Store.WithTx` composes them (so `Create`'s row+mounts
inserts are made atomic by the caller wrapping in `WithTx`).

---

## 5. Transaction boundary

```go
func (s *Store) WithTx(ctx context.Context, fn func(store.Store) error) error {
    return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
        return fn(&Store{db: tx}) // tx is a bun.IDB; repos compose inside it
    })
}
```
Real uses now: `CreateSpawn` (spawn row + N mount rows atomically). Future: spawn write + audit
event atomically.

---

## 6. Package layout

```
internal/cp/store/
  store.go            # domain types + interfaces (this §4)
  open.go             # Open(ctx, Config) -> Store: driver+dialect, goose.Up, ReconcileOrphans, seed
  bunstore/
    bunstore.go       # Store impl over *bun.DB / bun.Tx; Owners()/Apps()/Spawns()/WithTx/Close
    owners.go apps.go spawns.go   # repos over bun.IDB
    testing.go        # NewTestStore(t) -> :memory: + goose.Up
  migrations/
    sqlite/0001_init.sql
    pg/0001_init.sql
```

---

## 7. CP-side lifecycle (integration)

The store replaces `apps.Resolver` and the router's `owner` authority. `Server` gains `st
store.Store`, drops `*apps.Resolver`; `router.Bind` drops its `owner` param and `Router.Owner()` is
removed (DB is authoritative for ownership). New CP→node messages: **`Suspend`** (persist+teardown;
stub teardown until `sp-gd9`), and **`StartSpawn` carries the mount bindings** so create/resume tell
the node what to materialize.

| RPC / event | Behavior |
|---|---|
| **`CreateSpawn`** | resolve `Apps().LatestReviewed(appID)`; mint **uuidv7**; resolve mounts (app's declared mounts × chosen storage backend; demo defaults managed/scratch); `WithTx{ Spawns().Create(starting, mounts) }`; `sched.Create(id, app_ref, model, mounts)`; on ACTIVE → `SetActive(id, nodeID)`, on err/timeout → `SetError(id)`. |
| **`ListSpawns`** | `Spawns().ListByOwner(owner)` — the UI home surface. |
| **`SuspendSpawn`** | owner check via `Get`; `SetSuspending`; send node `Suspend`; on teardown → `SetSuspended(id, "")` + `router.Drop`. |
| **`ResumeSpawn`** | owner check; require `suspended`; `GetMounts`; `sched.Create(id, app_ref, model, mounts)` (re-provision, possibly new node); `SetActive(id, nodeID)`. Identical to create minus the row insert. |
| **`DeleteSpawn`** | owner check; if active, send node `Stop` + `router.Drop`; `MarkDeleted(id, now)`. Data backend preserved (destroy is a future opt-in). |
| **`Session`** (attach) | owner check via `Get`; if `suspended`, **auto-resume** (the `ResumeSpawn` path) then attach; **single-session takeover** evicts a stale client; `Touch(id, now)` on attach. |
| **node evict** (`DropNode`) | `Spawns().ReconcileNode(nodeID)` → those spawns `suspended`. |
| **CP boot** (`Open`) | after migrations: `Spawns().ReconcileOrphans()` → orphaned `{starting,active,suspending}` `suspended`. |

**Auto-upgrade hook (designed, enforcement deferred):** on create/resume, if `pinned=0`, resolve
`LatestReviewed`; if newer than the spawn's `app_version`, update `app_version`/`app_ref`. The
permission-escalation re-consent guard (system design §8) is **E8**; this layer only carries
`pinned` + the version fields.

**Seeding:** `Open` idempotently `Upsert`s the demo owner(s) + app(s) + version(s) from the same
static config in `cmd/cp/main.go` today (`zork`→ref, `dev`→owner), so `auth`'s static token map
resolves to real owner rows. E4/E5 replace the seed later — no schema change.

---

## 8. Migrations & config

- **goose** with `//go:embed migrations/<dialect>/*.sql`; `Open` runs `goose.Up` then reconciles.
- **Config** (env): `CP_DB_DRIVER=sqlite|postgres`, `CP_DB_DSN` (`file:cp.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)` or `postgres://…`). `Open(ctx, Config)` selects the driver + Bun dialect.
- Forward-only migrations; staging carries data across versions. `atlas migrate lint` as a CI gate
  on the pg tree is a later nicety.

---

## 9. Testing

- **Hermetic unit tests:** `NewTestStore(t)` opens `file:<test>?mode=memory&cache=shared&_pragma=foreign_keys(1)`,
  runs `goose.Up`, returns a clean `Store`. No Docker, no cgo. Covers every repo method + the
  state-machine transitions + reconciliation. `t.Parallel()`-safe (per-test DB).
- **CP handler tests:** the existing `runNode`/server tests get a real `:memory:` store instead of
  the in-memory maps; assert status transitions, ownership rejection, list output, reconciliation.
- **Build-tagged e2e (`//go:build e2e`, fail-loud, never skip):** the CP-side lifecycle against the
  stub agent through the node — create → list → suspend (stub teardown) → resume → delete — asserts
  the index reflects each transition. A Postgres-backed run of the same suite is the dialect-drift
  guard (CI, build-tagged).

---

## 10. Scope boundary & dependencies

- **`sp-gd9`** consumes this store unchanged: node-side WIP-ref persist (writes the real
  `suspend_ref`) + restore, the two-stage idle timer (calls `SetSuspended` via the node→CP path),
  and the web UI (consumes `ListSpawns` + lifecycle RPCs).
- **E3** supplies a persistent storage backend so suspend/resume is lossless (replacing `Scratch`).
- **E5** supplies real app publishing/versioning + manifest caching (replacing the seed; the
  `app_versions` table is ready for it).
- **E0 contracts** must add the new RPCs + the `Suspend` node message + `StartSpawn` mounts field;
  filed as a contracts task under this epic.

---

## 11. Success criteria

1. `go test ./...` (hermetic) green: store repos, state transitions, reconciliation, seeding — on
   `:memory:` SQLite, no Docker/cgo.
2. The CP uses the store for create/ownership/list/suspend/resume/delete + boot/evict reconciliation;
   the in-memory `apps`/owner-authority maps are gone.
3. Build-tagged e2e drives create→list→suspend→resume→delete through the stub agent and asserts the
   index; the same suite passes against Postgres.
4. A CP restart reconciles orphaned spawns to `suspended`; they re-list and (stub-)resume.
5. App versioning: a spawn pins `app_version`/`app_ref`; `LatestReviewed` resolves create; delisting
   a version doesn't break existing spawns.
6. Both backends build from one codebase (Bun dialects); only the goose DDL trees differ.
