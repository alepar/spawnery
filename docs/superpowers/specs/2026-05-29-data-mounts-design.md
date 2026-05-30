# Configurable Per-Mount Data Backends (Design)

**Status:** Approved in brainstorming — pending written-spec review
**Date:** 2026-05-29
**Amends:** [E3 storage](2026-05-28-spawnery-e3-storage-design.md),
[E0 §3 manifest](2026-05-26-spawnery-e0-contracts-design.md),
[system design §5](2026-05-26-spawnery-system-design.md),
[spawnlet slice](2026-05-29-spawnlet-slice-design.md).

Replaces the single read-write `/data` mount with a **configurable set of named data mounts living
inside the read-only `/app` tree**, each seeded from an app-provided seed dir and bound to a
user-chosen storage backend.

---

## 1. Motivation

The current model mounts one `/app` (ro, the App definition) + one `/data` (rw, the user's data) as
sibling trees, and copies `AGENTS.md` from `/app` into `/data`. Limitations: only one data location;
the data tree is detached from the app tree; and a copy step that has to be maintained. The new
model lets an App declare **multiple named data folders inside its own tree**, each independently
seeded and independently backed (e.g. one folder backed by GitHub, another scratch).

---

## 2. Mount model

- **`/app` (read-only)** = the App definition: `spawneryapp.yml`, `AGENTS.md`, persona/skills, and
  the seed dirs.
- **cwd = `/app`.** The agent runs in `/app`, so Goose reads `/app/AGENTS.md` **in place — no copy**.
- Each declared mount is a **read-write bind-mount overlaid at `/app/<path>`** (e.g. `/app/data`).
  Docker nested binds make these rw islands on the otherwise-ro `/app`. **The agent writes only into
  its mounts; the app definition stays immutable.** (Goose's own scratch/state goes to `$HOME`, not
  cwd, so a read-only cwd is fine — **verified: Goose v1.36 runs fine with a read-only `/app` cwd**.)
  - **Mountpoint must pre-exist in the ro `/app` source (runc constraint, `sp-f4v`):** runc cannot
    create the nested mount destination `/app/<path>` inside an already-read-only `/app`, so the
    `<path>` dir must exist in the bind source. **Slice convention:** the app ships the (empty) mount
    dir (e.g. `examples/secret-app/data/.gitkeep`). **Full design:** when `/app` is assembled per-spawn
    from `App@sha`, assembly creates the mountpoints — or the node `mkdir`s them in the (host-writable)
    app dir before the ro bind. Tracked as a follow-up.
- **The only copy operation is seeding** a fresh mount from its seed dir.

> Note: a mount's seed dir (e.g. `/app/seed`) remains visible read-only in `/app` alongside the
> seeded rw mount (`/app/data`). Harmless; the app's `AGENTS.md` simply points the agent at the
> mount path, not the seed path.

---

## 3. Manifest (`spawneryapp.yml`) — the App declares structure

The App declares which named data folders it needs and how to scaffold them. Replaces the old
`storage: {required, schema, seed}` block.

```yaml
storage:
  mounts:
    - name: main          # stable identifier; spawn.yml binds a backend BY THIS NAME
      path: data          # mount point relative to /app  ->  /app/data  (rw)
      seed: seed          # seed source relative to /app   ->  /app/seed (ro); scaffolds a fresh mount
    # - name: notes
    #   path: notes
    #   seed: notes-seed
```

- `name` is the stable key. The app may change `path` across versions without breaking a user's
  binding (which references `name`). `path` and `seed` are relative to `/app`.
- An app with no data needs (e.g. zork) declares `storage: { mounts: [] }` or omits it.

---

## 4. `spawn.yml` — the user binds a backend per mount, BY NAME

```yaml
storage:
  mounts:
    - name: main
      backend: scratch                  # slice: scratch
    # - name: notes
    #   backend: github:alepar/app-notes # full design: github:/blob:/managed/local:<path>
```

- Binding is **by `name`**, not path → app path changes don't break user bindings.
- A mount the app declares but the user doesn't bind: default to `scratch` for the slice (an
  unbound mount is ephemeral). Policy for required-but-unbound persistent mounts is a full-design
  concern (E3).

---

## 5. Backend abstraction — the seam

Each mount is realized by a **`StorageBackend`** (per-mount):

```
StorageBackend:
  Prepare(ctx, spawnID, mountName, seedDir) -> hostDir    // host dir to bind rw at /app/<path>
  Finalize(ctx, hostDir)                                  // on teardown
```

- **`Prepare`** produces a host directory to bind-mount read-write. It materializes the backing
  store, **seeding from `seedDir` when the store is empty/new** (Q2 rule), and returns the host path.
- **`Finalize`** runs at teardown.

**Backend types:**

| Backend | `Prepare` | `Finalize` | Slice? |
|---|---|---|---|
| **scratch** | mkdir a fresh temp dir; copy `seedDir/*` into it | **`rm -rf` the dir (nuke)** | ✅ **slice** |
| local:`<path>` | use `<path>`; if empty, seed it | persist = leave on disk; cleanup transient | full design |
| github:`owner/repo` | clone repo; if empty, seed + initial commit | commit + push, then drop the clone | full design |
| blob / managed | materialize from blob / managed store; seed if new | persist back | full design |

The **seed-when-empty-else-materialize** rule (Q2) applies to all **persistent** backends; **scratch
always seeds and never persists** (it's the always-fresh, nuke-on-exit type).

---

## 6. Lifecycle

**CreateSpawn:**
1. Read the manifest's `storage.mounts`; resolve each to its `spawn.yml` backend binding (default
   `scratch` if unbound).
2. For each mount, `backend.Prepare(...) -> hostDir` (scratch: temp dir seeded from `/app/<seed>`).
3. Build the agent container mounts: `appPath → /app (ro)` **+** for each mount
   `hostDir → /app/<path> (rw)`.
4. Start sidecar, then agent (cwd `/app`, stdio attached). Record the spawn + its mounts' hostDirs.

**Teardown (`StopSpawn` → `Manager.Stop`):**
1. Stop + remove the agent and sidecar containers (existing behavior).
2. **For each mount, `backend.Finalize(hostDir)`** — scratch nukes the temp dir; persistent backends
   persist (commit/push) then clean up. **(New: teardown now finalizes mounts, not just containers.)**

---

## 7. Teardown (answering the open question)

- **There is an explicit teardown API:** `StopSpawn` (ConnectRPC) → `Manager.Stop`. `spawnctl` calls
  it on exit; **the e2e tests call `StopSpawn` (via `defer`), not `docker stop`.**
- The mount change makes `Stop` responsible for **finalizing mounts** (§6) in addition to killing
  containers — scratch dirs are nuked here, so a normal stop leaves no residue.
- **Gap (deferred):** a client that crashes/disconnects without calling `StopSpawn` leaks the
  containers *and* the scratch dirs. Tracked by `sp-8hf` (orphan reaper: label containers +
  reap-on-startup + idle reaper). Scratch makes this marginally more pressing; still deferred.

---

## 8. Slice implementation scope

- **Build:** the named multi-mount structure, `cwd=/app`, the `StorageBackend` interface, and the
  **`scratch` backend only**. Persistent backends (local/git/blob/managed) are the full design.
- **Manager:** drop the single `/data` mount, the `copySeed`-to-`/data`, and the entrypoint
  `AGENTS.md` copy. Instead: `/app` ro + per-manifest-mount `backend.Prepare` → rw bind at
  `/app/<path>`; `Stop` calls `backend.Finalize` per mount.
- **Runtime:** unchanged (already accepts N mounts).
- **Client/agent:** `session/new` cwd `/data` → **`/app`**.
- **Entrypoint (`deploy/agent/entrypoint.sh`):** **remove** the `cp /app/AGENTS.md /data/AGENTS.md`
  line (cwd is `/app` now; Goose reads it in place).
- **`examples/secret-app`:** manifest declares `mounts: [{name: main, path: data, seed: seed}]`;
  `seed/README.md` holds the secret; `AGENTS.md` instruction → *"read `data/README.md`"* (relative
  to cwd `/app`).
- **e2e:** same intent — createSpawn secret-app, ask the secret, assert `QUOKKA-4417` (now via a
  scratch mount at `/app/data`, cwd `/app`).

---

## 9. Spec amendments

- **E3 storage:** the substrate becomes **N named per-mount backends** (was a single data repo); add
  the `StorageBackend` interface + backend types; the GitHub/blob adapters become *backend
  implementations* of this interface, per-mount.
- **E0 §3 manifest:** `storage:` becomes `storage.mounts: [{name, path, seed}]`.
- **system design §5:** "universal substrate = a git repo" → **a set of named data mounts, each
  backed independently** (scratch / git / blob / managed); cwd `/app`.
- **spawnlet slice design:** cwd `/app`, scratch backend, named mounts, no `AGENTS.md` copy.

---

## Appendix — decision log

| # | Decision | Choice |
|---|---|---|
| M.1 | Data location | Named rw mounts **inside** `/app` (`/app/<path>`); cwd `/app`; `/app` ro |
| M.2 | Declare vs bind | App manifest declares `{name, path, seed}`; user binds backend **by name** in spawn.yml |
| M.3 | Seed vs materialize | Persistent backends: seed-when-empty-else-materialize; **scratch: always seed, never persist** |
| M.4 | Copies | Only seeding; **drop the AGENTS.md copy** (read in place from `/app`) |
| M.5 | Backend seam | `StorageBackend{Prepare, Finalize}` per mount; backend URI in spawn.yml |
| M.6 | Slice scope | Multi-mount structure + `StorageBackend` interface + **scratch only**; persistent backends = full design |
| M.7 | Teardown | Explicit `StopSpawn` API (e2e uses it, not `docker stop`); `Stop` now **finalizes mounts** (scratch nukes); crash-without-stop leak = `sp-8hf` |
