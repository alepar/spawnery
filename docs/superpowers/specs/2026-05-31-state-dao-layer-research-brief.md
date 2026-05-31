# Control-Plane State / DAO Layer — Requirements & Research Brief

**Date:** 2026-05-31
**Status:** Requirements converged; awaiting deep research before design + plan
**Tracking:** beads `sp-` epic (see project tracker)

## Why this exists

Today **all** control-plane (CP) state lives in ephemeral in-memory maps guarded by
mutexes — `cp/registry` (live nodes), `spawnlet.Store` (live spawns on a node),
`cp/apps` (static config map), `cp/auth` (static token map). Nothing survives a CP
restart. There is no `database/sql` anywhere. This brief captures the requirements for
introducing a durable, transactional **Data Access (DAO) layer** behind one
storage-agnostic abstraction. It pairs with the open `sp-jf7` "CP-state backup."

> Note: `internal/storage/` is already taken — it realizes a spawn's **data mounts**
> (user files bind-mounted into the pod). That is NOT this layer. This layer is
> control-plane **metadata** persistence.

## Consolidated understanding

| Dimension | What we settled |
|---|---|
| **What** | A durable, transactional control-plane DAO for Spawnery (Go). |
| **Entities v1** | owners/identity, spawns (durable record), apps/catalog. Forward-compatible with audit/consent/trust later (append-only, time-ordered). |
| **Mode 1 — test/e2e** | In-memory embedded engine, clean slate per run, hermetic + Docker-free. |
| **Mode 2 — staging** | Persistent embedded engine on the box; schema migrated forward across versions; data survives upgrades. Behaves like prod. |
| **Mode 3 — true prod (later)** | Client/server RDBMS; transactional, strongly consistent; modest scale but wants headroom + future features (CDC/logical event stream, views, read replicas, eventual sharding). |
| **Hard constraints** | Ergonomic cross-repo transactions; thin-ORM scanning (no manual row→struct); SQL stays visible (no query-hiding magic); one codebase over two dialects; strong (not absolute) preference for pure-Go/cgo-free; mature/boring, no bleeding edge. |
| **Open for research** | The embedded engine (SQLite = seed), the server RDBMS (Postgres = seed), the access approach (thin ORM vs codegen-from-SQL vs builder+scanner), migration tooling, the in-memory test pattern, and the Go shape of the abstraction so embedded→server is a backend swap not a rewrite. |

### The spawn record (durable vs ephemeral)
Only the **durable record** of a spawn is persisted — id, owner_id, app_id, model, node,
status, storage choice, created/stopped timestamps. The **live container handles**
(sidecar/agent IDs, mount dirs, relay state) stay in memory; they are reconstructed or
reaped on restart, not persisted.

## Entities (v1) — relationships

- `owners` 1—N `spawns`
- `apps` 1—N `spawns` (a spawn launches one app version)
- `apps` 1—N `app_versions` (catalog versioning)
- *(later)* append-only `audit` / `consent` / `trust` — time-ordered, insert-heavy

## Deliverable of the research

A firm, justified recommendation for a single reference stack (embedded engine + server
RDBMS + access layer + migration tool + in-memory test pattern + Go abstraction shape),
with runner-ups and a 12-month maintenance-risk read. The research must **challenge the
seed engine choices**, not accept them.

## Deep-research prompt (verbatim, as issued)

```
# Deep Research: Go persistence/DAO stack for a control-plane state layer
# (mature, boring, portable embedded→server)

## Role
You are a senior Go backend architect. Survey the CURRENT (2025–2026) state of the
art for persisting relational control-plane state in a Go service, then make a firm,
justified recommendation for the specific case below. Bias hard toward MATURE,
POPULAR, BATTLE-TESTED, well-maintained options. Explicitly flag and avoid anything
bleeding-edge, low-adoption, single-maintainer, or recently-rewritten. "Boring that
gets out of the way" beats "clever."

## The system
A Go service (the "control plane") for an AI-agent platform. Today ALL of its state
lives in ephemeral in-memory maps guarded by mutexes; nothing survives a restart. We
are introducing a durable, transactional Data Access layer. Scale is modest and NOT
distributed-first: think thousands→low-millions of rows, low write rate, single
region — but we want headroom to grow 10–100x and add features without re-platforming.

### Entities (v1)
- owners / identity (accounts; referenced by almost everything)
- spawns: durable record of a launched agent instance — id, owner_id, app_id, model,
  node, status, storage choice, created/stopped timestamps. (Live container handles
  stay in memory; only the durable record is persisted.)
- apps / catalog: published app definitions + versions + manifests.
Forward-compatible (designed-for-but-not-built-now): an append-only, time-ordered
audit/consent/trust log.

### Three deployment modes from ONE codebase (this is the core constraint)
1. TEST/E2E: in-memory, ephemeral, CLEAN SLATE per run. Must be hermetic — no Docker,
   no external daemon, no C toolchain ideally — so unit/e2e tests "just run."
2. STAGING: persistent EMBEDDED engine (file on the box), schema migrated FORWARD
   across app versions, data survives upgrades. Behaves like prod.
3. PROD (later, not now): a client/server RDBMS — transactional, strongly consistent.
   Not aimed at large distributed scale, but we'd like a credible future path to:
   a logical/CDC change-event stream, views, standby/read replicas, and eventual
   sharding/partitioning, without abandoning the stack.

## Hard constraints (deal-breakers)
- Cross-repository TRANSACTIONS / unit-of-work must be first-class and ergonomic in Go
  (e.g. create owner + first spawn atomically). Not bolted on.
- THIN-ORM ergonomics: result-set→struct mapping must be handled for us (hand-scanning
  rows is the pain we're eliminating), BUT the SQL must stay visible and reviewable.
  Rule OUT heavy reflection/struct-tag ORMs that hide query behavior. Rule OUT raw
  database/sql hand-scanning.
- ONE codebase must serve an embedded engine AND a server RDBMS. Maintaining two SQL
  dialects is acceptable; a full second implementation is not.

## Strong preferences (weigh, don't treat as absolute)
- Pure-Go / cgo-free toolchain (driver + deps) so hermetic tests and cross-compilation
  work with no C compiler. Call out exactly where cgo sneaks in.
- Minimal, stable dependency surface; slow-moving, widely-adopted libraries.

## OPEN questions you must actually evaluate — do NOT assume our seeds
These are SEED examples only; challenge them:
- Embedded engine: SQLite is the obvious seed. Is it the right one? Compare credible
  alternatives for an embedded, transactional, RDBMS-shaped store in Go (e.g. SQLite
  via modernc.org/sqlite vs mattn/go-sqlite3; libSQL/Turso embedded; rqlite; any
  others). Which best matches "embedded now, server later, same SQL"?
- Server RDBMS: Postgres is the seed. Compare to credible "or-equivalent" options
  (e.g. CockroachDB, YugabyteDB, MySQL/MariaDB) specifically on: transactional
  strong consistency, the future-feature wishlist above, operational simplicity at
  modest scale, and how cleanly it pairs with the chosen embedded engine's dialect.
- Access layer: compare the live approaches head-to-head on OUR constraints —
    * thin ORMs / query-mappers (e.g. Bun, ent, sqlx, scany/pgx)
    * codegen-from-SQL (e.g. sqlc) — incl. its multi-dialect story
    * type-safe SQL builders (e.g. go-jet, squirrel + a scanner)
  For each: how it handles (a) two-dialect portability, (b) cross-repo transactions,
  (c) struct mapping without hand-scanning, (d) cgo, (e) migrations, (f) maturity/adoption.
- Migrations / schema evolution: compare goose, golang-migrate, atlas, ent-native (or
  others). Which gives a clean "forward-migrate staging across versions" story AND runs
  on both the embedded and server engine?
- In-memory/test strategy: same-engine :memory: vs alternatives. What's the proven
  pattern for clean-slate-per-test that still exercises real SQL/constraints?
- The Go SHAPE of the abstraction: repository interfaces vs a Store/Querier handle vs a
  generated client. What pattern lets us swap embedded↔server as a backend change (not a
  rewrite) AND compose transactions across multiple repositories? Show idiomatic
  examples from real, popular projects.

## Portability landmines (cover explicitly)
Enumerate the concrete SQLite↔Postgres (or whichever pair you recommend) dialect
differences that bite a single-codebase DAO, and how the recommended stack handles each:
upsert syntax, RETURNING, JSON/JSONB columns, autoincrement/identity, boolean & time/
timestamp handling, foreign-key enforcement defaults, concurrency/locking model,
transaction isolation levels.

## Deliverable / output format
1. Landscape map: the credible options in each category (engine, access layer, migrations)
   with a maturity/adoption signal (stars, release cadence, who uses it in prod, age).
2. Comparison tables scoring each against our hard constraints + strong preferences.
3. A SINGLE recommended reference stack for our case: embedded engine + server RDBMS +
   access layer + migration tool + in-memory test pattern + the Go abstraction shape —
   with a short, real code sketch showing: a repository interface, a transaction spanning
   two repositories, and the same query compiling against both dialects.
4. The top 1–2 runner-up stacks and the specific conditions under which we'd pick them.
5. A "things that will bite you in 12 months" section: maintenance risk, dialect drift,
   the cgo/cross-compile story, and the realistic effort to add Postgres later if we
   ship on the embedded engine first.
6. Cite sources (docs, adoption data, notable prod users, benchmarks) inline.
```

## Next step

Run the research → land findings here → brainstorm the concrete DAO design (entities,
schema, interfaces, transaction boundary, migration story) → writing-plans → implement.
No implementation until the stack is chosen.
