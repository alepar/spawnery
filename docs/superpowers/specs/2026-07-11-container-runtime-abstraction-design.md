# Container Runtime Abstraction — one fork() for every runtime

**Status:** draft · **Date:** 2026-07-11 · **Epic:** (bd epic created at handoff)

## 1. Problem

Spawnery runs spawns on two container lanes — **Docker/runc** (`DockerPodBackend`) and
**CRI/containerd + runsc** (`CRIPodBackend`) — behind a `runtime.PodBackend` interface. That
interface abstracts the *mechanics* (start/stop/attach) but **not the semantics that matter for
lifecycle operations**, so `internal/spawnlet/manager_fork.go` (and the suspend/resume/migrate paths)
are riddled with lane-specific branching, and the lanes diverge in ways that leak upward.

The divergence is one specific, load-bearing asymmetry:

| | capture mechanism | does the source survive it? |
|---|---|---|
| **Docker/runc** | `docker commit` (commit-while-running) | **yes** — container never stops |
| **CRI/runsc** | stop container → containerd `CreateDiff` | **no** — must stop, then **re-launch** |

On CRI, **the source's container identity changes underneath the orchestration**. Every lane-specific
branch, the fragile in-memory `agentSpecs` cache, and the stale-`AgentID` recovery bug all trace back
to that single fact.

Three concrete defects motivated this design (all observed on the prod-stack VM):

- **(a) Stale-identity recovery.** The CP crash-recovery path sends `UnpauseIfPaused`, which unpauses
  a stored `AgentID` that no longer exists after a capture → `load container …: not found`.
- **(b) Fragile launch spec.** Re-launching the source needs its `AgentSpec`; it lived in an
  **in-memory map** (`agentSpecs`, keyed by sandbox id) which is lost on any spawnlet restart and
  failed in practice (`no cached agent spec for sandbox …`).
- **(c) Orchestration-level `Pause`.** `manager_fork.go` calls `m.pod.Pause` for spec-§3 quiescence.
  A gVisor containerd-shim regression (gVisor
  [#12647](https://github.com/google/gvisor/issues/12647), fixed by `55b3fd17`, first shipped in
  `release-20260601.0`) made `task pause` corrupt the task **and the sandbox** — task → `PID 0 /
  UNKNOWN`, resume → `no running task found`, sandbox teardown → wedged. That single upstream bug was
  the root cause of *both* the fork `StartContainer` hang **and** the M4 suspend
  `RemovePodSandbox` wedge. It reached us **only because the orchestration calls `Pause` at all.**

**Goal:** one runtime-agnostic implementation of `fork()` / `suspend()` / `resume()` / `migrate()`,
composed from primitives whose contracts hide every lane difference — and which structurally cannot
reproduce defects (a)–(c).

## 2. Prior art this builds on (not re-litigated)

- **`2026-06-12-writable-rootfs-survival-design.md` (sp-ei4.1.3)** already established:
  *"One portable artifact across lanes: an OCI layer tar. **Lane-specific capture code, lane-agnostic
  artifact**."* The **data** boundary is therefore already abstracted — both lanes emit the same OCI
  layer tar. What was never abstracted is the **control flow**. This design supplies that half.
  It also fixed the per-lane mechanisms (Docker userns-remap + `docker commit`; runsc `overlay2=none`
  + containerd `DiffService`) — we keep them as *implementation details*, now hidden.
- **`2026-05-27-spawnery-e1-runtime-core-design.md` (E1 §7)** defined the pluggable isolation-backend
  seam. This design *raises* that seam from mechanics to semantics; it does not replace the idea.

## 3. The primitive set

```go
// internal/runtime
type Runtime interface {
    Create(ctx, spawn SpawnRef, spec PodSpec) (Handle, error)
    Destroy(ctx, spawn SpawnRef) error

    // Contract-based: defined by POSTCONDITION, not mechanism.
    SnapshotLive(ctx, spawn SpawnRef, whileQuiesced func(context.Context) error) (Artifact, Handle, error)
    SnapshotFinal(ctx, spawn SpawnRef, whileQuiesced func(context.Context) error) (Artifact, error)

    Materialize(ctx, spawn SpawnRef, spec PodSpec, art Artifact) (Handle, error)

    Export(ctx, art Artifact, w io.Writer) error
    Import(ctx, r io.Reader) (Artifact, error)

    ListManaged(ctx) ([]ManagedPod, error) // labels carry spawnID + generation
}
```

**Contracts (the whole point):**

- `SnapshotLive` — **postcondition: the spawn is still RUNNING.** The runtime may swap the agent
  container's identity to satisfy this; it therefore returns a **new `Handle`** *explicitly*. Callers
  must persist it. (Explicit return, not in-place mutation — this is what kills defect (a).)
- `SnapshotFinal` — **postcondition: the spawn is torn down**, artifact captured.
- `whileQuiesced` runs **inside the runtime's own quiesced window**, so the journal can snapshot
  mounts at an instant consistent with the rootfs delta — *without* `Pause` being on the interface.
- **`Pause`/`Unpause` are NOT primitives.** Quiescence is an internal detail of the snapshot
  primitives. The orchestration structurally *cannot* call pause → defect (c) becomes unreachable.

### 3.1 Lane implementations

| | `SnapshotLive` | `SnapshotFinal` |
|---|---|---|
| **Docker** | `pause` → `whileQuiesced()` → `commit` (in-place) → `unpause`; Handle unchanged | `stop` → `whileQuiesced()` → `commit` → teardown |
| **CRI/runsc** | `stop agent` → `whileQuiesced()` → `CreateDiff` → **re-launch agent from the durable launch spec** (sandbox + sidecar survive) → **new Handle** | `stop` → `whileQuiesced()` → `CreateDiff` → teardown |

Docker keeps its zero-downtime capture. CRI's identity swap is **contained inside the impl**.

### 3.2 Composition — every op, zero lane branches

| op | composition |
|---|---|
| create | `Create(spec)` |
| suspend | `SnapshotFinal(hook: journal mounts)` → persist artifact |
| resume | `Materialize(spec, artifact)` |
| **fork** | `SnapshotLive(source, hook: journal mounts)` → `Materialize(fork, spec, artifact)` |
| migrate | `SnapshotFinal` → `Export` → ship → `Import` → `Materialize` |

## 4. Node-durable state — sqlite

The node today persists only `journalState` (mount manifest pins) and `deltaState` (delta depth) as
per-spawn JSON files. The `Spawn` record itself is an **in-memory map**; on restart it is empty and
`ReapOrphans` **destroys every running pod**.

**Decision: one transactional node-state DB** — `modernc.org/sqlite` (**pure-Go, already vendored**
via authsvc; the spawnlet stays **cgo-free**), replacing the two JSON stores.

**Why sqlite, not a third JSON file:** during an identity swap the node must update *together* the
launch spec, the new artifact pointer, and the spawn's container ids. Across loose files a crash
between writes leaves precisely the inconsistent half-state this design exists to eliminate. One
transaction makes recovery deterministic.

**What is persisted:**

| record | purpose | secrets? |
|---|---|---|
| **launch spec** (`AgentSpec`) | re-launch the agent (CRI swap) / `Materialize` on recovery | **none** — sidecar env (which holds the model API key) is *never* persisted; the sidecar survives a relaunch, and full pod create/resume still receives its spec from the CP |
| **artifact pointer** | rootfs delta ref + journal mount pins (today's delta/journal state) | none |

**Not persisted:** container ids — the runtime's **labels already carry spawn id + generation**
(that's how `ListManaged` works), so ids are derivable and must not be duplicated.

### 4.1 Restart re-adoption (replaces reap-everything)

With a durable launch spec + artifact pointer, a restarted spawnlet can **re-adopt** running pods
instead of destroying them:

```
for pod := range rt.ListManaged():        // labels: spawnID, generation
    spec, art, ok := nodeDB.Load(pod.SpawnID)
    if !ok || pod.Generation < nodeDB.Generation(pod.SpawnID):  // stale/unknown
        rt.Destroy(pod)                   // genuine orphan — reap
    else:
        mgr.ReAdopt(pod, spec, art)       // rebuild Spawn record, re-attach pumps/relays
```

A node restart/upgrade **no longer destroys every running spawn.** Requires **generation fencing**
(never adopt a generation older than the node's record) and **pump/relay re-attach**. `ReapOrphans`
narrows to: reap only pods with no record or a stale generation.

## 5. Error handling & recovery contract

- **`SnapshotLive` owns its rollback.** On any failure it must restore the source to RUNNING, or
  return typed **`ErrSourceDown`** stating it could not. No implicit half-states.
- **`EnsureRunning(spawn)` replaces `UnpauseIfPaused`.** The CP's crash-recovery no longer unpauses a
  stale `AgentID`; the node re-establishes liveness from the **durable launch spec + last artifact**
  via `Materialize`. Runtime-agnostic, no stale ids, no pause. **Fixes defect (a).**
- **Reconciler hardening.** In `Server.reconcileInventory`, move the **claim/lease check *before*** the
  `store.Forking` branch, so an in-flight *leased* fork is never `rt.Drop()`-ed + `MarkForkingLost`-ed
  on a transient unreport. `MarkForkingLost` then fires only for a genuinely abandoned fork
  (lease expired) — its actual intent.
  *Note:* investigation showed the reconciler was a **consequence** of the failed restore, not its
  cause (the source stays in the node's in-memory store during a fork, so it stays reported). This is
  hardening, not the root fix — recorded honestly so nobody designs against a misdiagnosis.

## 6. Testing — what actually guarantees "one fork() everywhere"

1. **`fakeRuntime`** implementing the 6 primitives → the entire fork/suspend/resume/migrate
   orchestration becomes **hermetically unit-testable** (no Docker, no containerd). Today that logic
   is reachable only via e2e, which is why these bugs survived so long.
2. **A shared runtime *contract* suite** — one table of postcondition tests that **both** impls must
   pass, run under each lane's build tag (`e2e`, `cri_delta_e2e`):
   - `SnapshotLive` leaves the spawn **running** and returns a Handle that actually addresses it
   - `SnapshotLive` on failure restores the source, or reports `ErrSourceDown`
   - `whileQuiesced` runs with the agent genuinely quiesced (no writes land after it returns)
   - `Materialize(art)` reproduces the captured rootfs + mounts
   - `SnapshotFinal` tears the pod down
   - `Export`/`Import` round-trips across lanes (Docker↔runsc), per sp-ei4.1.3's portable-artifact rule

   **This suite is the mechanism that proves a lane honors the contract — and it is exactly the test
   that would have caught the gVisor pause regression on day one.**

## 7. Migration (incremental, no big-bang)

1. `Runtime` interface + `Artifact`/`Handle`/`SpawnRef` types + sqlite node-state store (migrate the
   two JSON stores into it).
2. Wrap the existing backends as `DockerRuntime` / `CRIRuntime` — behavior-preserving.
3. Rewrite the orchestration onto the primitives; **delete every lane branch and every
   orchestration-level `Pause`**.
4. CP: `EnsureRunning` recovery + reconciler lease ordering.
5. Restart re-adoption (generation fencing + pump re-attach); narrow `ReapOrphans`.
6. `fakeRuntime` + contract suite; delete the old fork/pause/capture surface from `PodBackend`.

## 8. Trade-offs & non-goals

- **On runsc, a fork still restarts the source's agent** (filesystem preserved; live tmux/ACP session
  drops — "resume semantics"). Accepted previously. The abstraction does not remove this; it
  **contains** it so it stops leaking into the orchestration.
- **Not a goal:** a third runtime (CRI+runc) today — but the seam makes it additive.
- **Not a goal:** changing the artifact format. sp-ei4.1.3's OCI-layer-tar stays the wire.
- The runsc pause bug is fixed upstream (pin bumped to `release-20260601.0`); removing `Pause` from
  the interface is **defense in depth**, not a workaround for a live bug.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
