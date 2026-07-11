# Container Runtime Abstraction — one fork() for every runtime

**Status:** draft (v2 — rewritten after the premise spike + roast BLOCK) · **Date:** 2026-07-11

> **v1 was built on a false premise and was BLOCKed by roast (38 confirmed findings, 6 blockers).**
> A spike then killed the premise outright. This v2 is much smaller as a result. §2 records what died
> and why, so nobody resurrects it.

## 1. Problem

Spawnery runs spawns on two container lanes — **Docker/runc** (`DockerPodBackend`) and
**CRI/containerd + runsc** (`CRIPodBackend`) — behind `runtime.PodBackend`. That interface abstracts
the *mechanics* (start/stop/attach) but not the *semantics* of the lifecycle operations, so
`manager_fork.go`, the suspend gate, resume and migrate carry lane-specific knowledge, and the
fork/suspend logic is reachable only via e2e — which is why real bugs survived in it for months.

**Goal:** one runtime-agnostic implementation of fork / suspend / resume / migrate, composed from
primitives that hide every lane difference, and that is **hermetically testable**.

## 2. The premise that died (read this before proposing a redesign)

v1's entire complexity — an *identity swap*, a durable launch spec, a new-`Handle` contract,
`ErrSourceDown` rollback, a sqlite node DB — existed to serve one claimed asymmetry:

> *"Docker can commit a running container; CRI must stop → CreateDiff → re-launch, so on CRI the
> source's container identity changes underneath you."*

**That is false.** Spike (2026-07-11, runsc `release-20260601.0`, `overlay2=none`, systrap):
`rootfs.CreateDiff` of a **RUNNING** container, of a **PAUSED** container, and of a **STOPPED** one
each produced a **byte-identical layer** (same digest `sha256:bffee207…`, same size), with the
container's writes present, and the task still `RUNNING` afterwards.

`CreateDiff` never required a stopped container. The belief was an artifact of gVisor
[#12647](https://github.com/google/gvisor/issues/12647) corrupting `task pause` — the
`no running task found` it produced was the *pause bug*, not evidence a stop was mandatory.

**Consequence: the lanes are symmetric.** Both capture-while-live. `CaptureDeltaAs` now mirrors
Docker's `CommitContainerPreserving` exactly (committed on master; fork·cli verified green, source
stays ACTIVE and never restarts). Deleted with it: the identity swap, the source re-launch, the
in-memory `agentSpecs` cache, the stale-`AgentID` hazard, and the runsc "source restarts" UX caveat.

Two latent bugs surfaced and were fixed at the same time: the assembled delta image was never
**unpacked** into the snapshotter (CRI only surfaces unpacked images), and it was recorded under a
**non-canonical ref** while CRI normalises its lookups.

**So this design no longer needs:** a durable launch spec (nothing re-launches), the sqlite node DB
(its whole rationale was atomicity *across an identity swap*), `EnsureRunning` (no stale ids), or
restart re-adoption (see §8).

## 3. Primitives

```go
type Runtime interface {
    Create(ctx, spawn SpawnRef, spec PodSpec) (Handle, error)
    Destroy(ctx, spawn SpawnRef, h Handle) error

    // FORK: one-shot. Captures the rootfs delta; the spawn KEEPS RUNNING, same container, same Handle.
    SnapshotPreserving(ctx, spawn SpawnRef, h Handle, whileQuiesced Hook) (Artifact, error)

    // SUSPEND / MIGRATE: two-phase, because the real suspend is a CP-GATED protocol (roast-confirmed):
    // Begin quiesces + runs the hook (mount journal) and RETURNS STILL QUIESCED, so the CP gate can
    // decide and the node can reap ACP sessions / relay per-mount progress.
    BeginFinal(ctx, spawn SpawnRef, h Handle, whileQuiesced Hook) (Token, error)
    FinishFinal(ctx, tok Token, scrubPaths []string) (Artifact, error) // scrub → capture → teardown
    AbortFinal(ctx, tok Token) error                                   // un-quiesce, spawn keeps running

    Materialize(ctx, spawn SpawnRef, spec PodSpec, chain ArtifactChain) (Handle, error)
    Export(ctx, Artifact, io.Writer) error
    Import(ctx, io.Reader, desc ArtifactDesc) (Artifact, error) // desc pins the CP's ContentDigest
    ListManaged(ctx) ([]ManagedPod, error)
}
```

`Pause`/`Unpause` are **not** on the interface. Quiescence is reachable only *inside* the snapshot
primitives — so the orchestration structurally cannot call a pause, and a repeat of #12647 could not
reach it. But see §5: what quiescence actually guarantees is narrower than v1 claimed.

**The scrub has a home.** The live delta scrub (`exec rm -rf <DeltaScrubPaths>` inside the agent)
requires a *running* agent, so it is a step of `FinishFinal` — un-quiesce → scrub → capture → teardown —
not something the caller does around a snapshot. (roast: it had no home in v1.)

## 4. Composition — every op, zero lane branches

| op | composition |
|---|---|
| create | `Create(spec)` |
| **fork (same-node)** | `SnapshotPreserving(source, hook: journal mounts)` → `Materialize(fork, spec, chain+art)` |
| **fork (cross-node)** | `SnapshotPreserving(source, hook)` → `Export` → ship → `Import(desc)` → `Materialize` on target |
| suspend | `BeginFinal(hook)` → *CP gate* → `FinishFinal(scrub)` → persist artifact |
| resume | `Materialize(spec, chain)` |
| migrate | `BeginFinal(hook)` → **artifact durable** → `FinishFinal` → `Export` → ship → `Import` → `Materialize` |

Cross-node fork (`manager_fork_transfer.go`, ~300 lines) is a **first-class row**, not an omission —
roast caught v1 silently dropping it.

## 5. Quiescence: what the hook actually guarantees (honest)

v1 claimed `whileQuiesced` gives a window in which the journaled mounts have no writers. **That is
false and roast confirmed it:** `docker pause` freezes the *agent container's* cgroup only. The
**sidecar is not paused**, and **spawnlet-side writers touch the same host dirs** (`storage.Backend`
Prepare/Finalize, the GitHub backend's `RunGit(hostDir)`, gitenv/credential rendering, the journal).

**The contract is therefore:** `whileQuiesced` guarantees **the agent is not writing**. It does *not*
make the mount globally quiescent. Excluding the *other* writers is the **orchestration's** job (§7's
per-spawn op lock), not the runtime's. The runtime must not pretend otherwise — a hook that silently
under-delivers consistency is worse than one whose limits are stated.

## 6. Artifact is a CHAIN, not a value

Roast: the real rootfs state is an **ordered chain** — `RootfsArtifacts` (with `Sequence`),
`DeltaDepth`, `BaseImageDigest`, `LaunchImageRef` (which, not the base, is what the moby#47065
layer-count guard compares against), plus gap/duplicate validation, the portable-history check, a
squash-at-depth heuristic, and chain inheritance on fork. A singular `Artifact` cannot express this.

So: `Artifact` is one link; **`ArtifactChain`** is what `Materialize` takes and what a fork inherits.
Chain policy (depth, squash, portability) stays **orchestration-level** — the runtime does not own it.
`Import` takes an `ArtifactDesc` so the CP-pinned `ContentDigest` is verified at the one boundary
where bytes from another node's untrusted agent rootfs enter this node's image store.

## 7. Concurrency, durability, fencing

- **Per-spawn op lock.** Nothing today excludes `Create`/`Destroy`/snapshot/reconcile/attach from
  racing on one spawn. A single-writer lock per spawn is required, and it is also what excludes the
  spawnlet-side mount writers during a `whileQuiesced` window (§5).
- **Durable-before-destructive.** `FinishFinal` must not tear the pod down until the artifact is
  **durable** (journal ack / fsynced + pointer committed). Migrate must not destroy the source until
  the artifact has *left the node*. (roast blocker: v1 could lose a spawn entirely on a crash between
  capture and persistence.)
- **Migrate fencing.** The source must be fenced (generation bump) before the target materialises, or
  a partitioned source and a live target both write the same journal generation.

## 8. Restart re-adoption — REINSTATED (v2 corrected a factual error)

> **v1 (and the roast that confirmed it) got this wrong.** v1 claimed a spawnlet restart "costs a
> resume cycle and the live session, not the work", and cut re-adoption as scope creep on an
> overstated harm. **The harm was understated, not overstated.**

**What actually happens today** (code-verified):

1. `ReapOrphans` runs **once at spawnlet process startup, before the node connects to the CP**
   (`node.Run()`). The in-mem store is empty then, so **every** managed pod is an "orphan":
   best-effort `CaptureDelta` (work preserved) → **`Stop` — the pod is destroyed**.
2. The node reports an **empty inventory**.
3. CP `reconcileInventory`: those spawns are `PhaseActive` but unreported → `rt.Drop()` +
   **`MarkUnreachable`**.
4. **Nothing auto-recovers them.** The only route back to Active is `adoptOrStop` flipping
   `Unreachable→Active` *when a node reports the spawn as running* — which can never happen, because
   the pod was destroyed.

So a spawnlet restart/upgrade **destroys every spawn on the node and leaves them Unreachable until a
human resumes them.** The work survives in a delta; the spawn does not.

**Two of the roast's objections to re-adoption are stale or misplaced:**

- **Fencing is already CP-side and correct.** `adoptOrStop` fences on generation: *gen matches + live
  row → adopt (flip Unreachable→Active); gen behind live → `StopSpawn(stale_gen)`.* The node must
  therefore **not** fence locally (v1 did — that is what made it split-brain-prone). It **reports what
  it is running**; the **CP adjudicates**. The machinery exists and is untouched by this design.
- **Re-attach is a plain TCP re-dial on BOTH lanes** — `AttachTCP(podIP:acpPort)` in *both*
  `docker_pod.go` and `cri/backend.go`. The roast's "raw byte pump / Docker stdio hijack with no
  replay" rests on **stale code**: the stdio hijack is gone. An in-flight ACP turn may still be lost,
  which is a *session* concern, not a *spawn-liveness* one.

**Design.** On startup the node does **not** blanket-reap:

```
for pod := range rt.ListManaged():             // labels: spawnID, generation
    spawn, err := rebuildSpawn(pod)            // see below
    if err != nil: rt.Destroy(pod)             // cannot re-adopt -> reap (as today)
    else:          mgr.ReAdopt(spawn); redialACP(spawn)   // then REPORT it to the CP
// the CP's adoptOrStop then adopts (gen matches) or orders StopSpawn (gen stale)
```

`rebuildSpawn` needs no new at-rest secret and no new durable store:

| field | recovered from |
|---|---|
| container ids, generation | runtime **labels** (`ListManaged`) |
| PodIP / NetnsPath | runtime status (`PodSandboxStatus` / `ContainerIP`+`ContainerPID`) |
| **ControlToken** (per-pod sidecar bearer **secret**) | the **sidecar container's env** — the node sets `SIDECAR_CONTROL_TOKEN=…` at start, so **the runtime already stores it**; read it back rather than persist it |
| mounts / journal pins / delta depth | the existing per-spawn JSON stores |

**Load-bearing assumption (spike before building):** that the sidecar's env is readable back on
**both** lanes (Docker `inspect` → `Config.Env`; CRI `ContainerStatus(verbose)` → OCI spec env).
*Cheapest test:* start a pod on each lane, restart nothing, read `SIDECAR_CONTROL_TOKEN` back through
the runtime API. *Kill criteria:* if CRI does not expose it, re-adoption needs the token persisted
(secret at rest) — which changes the security posture and must be re-decided.

## 8b. Explicitly cut (and why)

- **sqlite node DB** — its sole justification was atomic writes *across the identity swap*. No swap ⇒
  no cross-record transaction ⇒ no need. Roast also showed v1's own rationale contradicted its state
  table. Keep the existing per-spawn JSON stores.
- **Durable launch spec** — existed only to re-launch the source. Nothing re-launches.
- **`EnsureRunning`** — existed to repair a stale `AgentID`. Ids no longer change.

## 9. Testing — what actually proves "one fork() everywhere"

1. **`fakeRuntime`** implementing the primitives ⇒ fork/suspend/resume/migrate become **hermetically
   unit-testable** (no Docker, no containerd). This is the single biggest win: that logic is currently
   e2e-only, which is precisely why these bugs lived so long.
2. **Shared runtime contract suite** — postcondition tests both impls must pass under their lane tag:
   `SnapshotPreserving` leaves the spawn **running and addressable** (container-level *and*
   agent-ready — roast: a container-running assertion can pass on a spawn no client can talk to);
   `whileQuiesced` runs with the agent genuinely not writing **during** the hook (not "after");
   `BeginFinal` leaves it quiesced and `AbortFinal` restores it; `FinishFinal` scrubs then tears down;
   `Materialize(chain)` reproduces the rootfs; artifact is durable before teardown.
3. **Cross-lane `Export`/`Import`** cannot run under a single lane tag. Use a **golden artifact
   fixture** (a checked-in layer captured on each lane) so the portable-artifact claim inherited from
   sp-ei4.1.3 is actually tested, rather than asserted.

## 10. Migration

1. `Runtime` interface + `Artifact`/`ArtifactChain`/`Handle`/`Token` types.
2. Wrap the (now symmetric) backends as `DockerRuntime` / `CRIRuntime` — behaviour-preserving.
3. Per-spawn op lock (§7).
4. Rewrite orchestration onto the primitives; delete lane branches; move the scrub into `FinishFinal`.
5. Restart re-adoption (§8): rebuild-from-runtime + report; CP adjudicates. Narrow `ReapOrphans` to
   "cannot rebuild" / CP-disowned pods only.
6. `fakeRuntime` + contract suite + golden cross-lane fixture; delete the old capture/pause surface.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*

- **2026-07-11 — premise spike + fork fixed on master (before this design ships).** `CreateDiff` works
  on a live/paused container (§2). The CRI lane now preserves the source; `fork·cli` is green, source
  stays ACTIVE and never restarts. Also fixed: delta image not unpacked into the snapshotter; delta
  recorded under a non-canonical ref. These landed as small fixes to the existing `PodBackend`, so
  this design is now a *refactor for testability and uniformity*, not a bug fix.
