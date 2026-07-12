# Spawnlet Restart Re-adoption (SE3)

- **Bead:** `sp-2tx8.3` (epic `sp-2tx8`)
- **Status:** draft
- **Mode:** collaborative (Mode A)
- **Depends on:** `sp-2tx8.1` (SE1) — the fake is what makes a restart hermetically testable.

## 1. Problem

A spawnlet restart destroys every spawn on the node, and the control plane has no automatic recovery.
This is not an edge case: `systemctl restart spawnery-spawnlet` is the **documented upgrade path**
(`docs/e2e-vm-testing.md`). Shipping a new node binary today means killing every running spawn on it.

Both death paths destroy, for different reasons:

- **Graceful (SIGTERM — the upgrade path).** `cmd/spawnlet/main.go:142` calls `gracefulStopAll(mgr)`
  after `node.Run` returns → `manager.go:849 StopAll` → `Stop` → `teardown` → `pod.Stop`, which *removes*
  the containers. **Every spawn is torn down before the process exits.** Nothing survives to be adopted.
- **Unclean (SIGKILL/OOM/panic).** The pods survive the process, and then `ReapOrphans` destroys them
  all at the next startup.

Either way the CP marks the spawns Unreachable, and nothing brings them back.

The graceful path is the one that matters, and it is the one the previous (superseded) design missed: it
proposed re-adoption but only ever touched `ReapOrphans`, so by the time its recovery loop ran there was
nothing left to recover. That was a confirmed roast blocker, and this spec exists partly to not repeat it.

## 2. Why it is hard

Almost all per-spawn node state lives in memory and cannot be recovered from the runtime:

| state | where it lives today | recoverable from the runtime? |
|---|---|---|
| launch spec, mounts, model, image | in-memory `Spawn` | no |
| mount finalizers | in-memory, hold **live `storage.Backend` values** | no — not serialisable at all |
| per-spawn MITM CA **private key** | `internal/node/githubcontrol.go:41`, `cas map[string]caPair` — silently **regenerated on miss**, while the agent still trusts the old cert | no |
| journal repo password | in-memory, owner-sealed, delivered once by the CP | no |
| ACP session registry (ids, mosh ports, pumps) | `internal/node/sessions.go`, node-local, node-allocated | no |
| egress floor | applied to the pod IP at launch | partially |
| agent/sidecar container ids, pod IP | **CRI's `ListManaged` returns only `{SpawnID, Generation, NodeID, SandboxID}`** | not today |

The node's durable JSON stores hold almost nothing: `journalRecord` is `{Generation, Manifests}` and
`deltaRecord` is `{Depth}`. Any design claiming "the existing stores are enough" is simply wrong — the
superseded spec claimed exactly that.

## 3. Approach

**Re-adopt, with the CP as the source of truth.** The pods stay alive across the restart and the node
rebuilds its in-memory state from what the CP re-delivers. Three decisions frame everything below.

**Why re-adopt rather than drain.** The alternative was to auto-suspend every spawn on SIGTERM and resume
after — which reuses machinery we already have and sidesteps the state problem entirely. It was rejected
because it kills every in-container process: a build in flight does not survive an upgrade. Re-adoption
preserves the container and everything running in it, which is the whole point.

**Why the CP re-delivers rather than the node persisting.** Giving the node a durable per-spawn record
duplicates the CP's ledger — two sources of truth that can diverge — and would put the journal password
and the control token on node disk. The CP already holds the authoritative spawn ledger and already has
the owner-sealed key-travel machinery from migrate. So the node asks, and needs **no new durable store**.

**What continuity we promise.** The pod, the agent process, its files, and any in-flight work survive.
The live session on top does not: the node's session registry is rebuilt empty, the client's connection
drops and it reconnects. Preserving live session continuity (same mosh ports, resuming the agent's
existing ACP session) is a much larger problem and buys only that the terminal does not blink.

## 4. Design

### 4.1 Shutdown stops destroying

`gracefulStopAll` becomes **`gracefulDetachAll`**. On SIGTERM the node closes its pumps and sessions,
stops the journal watchers, closes the CP stream, and **exits with the pods still running**.

The spawn-deletion path (`Manager.Stop`, `StopAll` as called by an actual delete) is unchanged; only the
process-shutdown path changes. This is the load-bearing edit — without it, everything else in this spec is
dead code.

*Consequence to accept:* if the node never comes back, its pods run unsupervised. They are labelled, so any
future spawnlet on that machine finds and reconciles them, and the CP sees the spawns as Unreachable and
can act. This is strictly better than today, where the same scenario destroys them unconditionally.

### 4.2 Startup re-adopts instead of reaping

`ReapOrphans` becomes **`ReconcileManagedPods`**, still running inside `node.Run` **before the node begins
serving** — so re-adoption is immediate on process start, not polled or deferred.

1. `ListManaged()` → the set of managed pods, with `{spawn id, generation}` from the labels.
2. The node reports that set to the CP. The CP answers **per spawn**: **adopt** (the full launch spec, the
   mount table, and the owner-sealed journal key) or **reap** (unknown spawn, stale generation, or the CP
   considers it gone).
3. **Adopt:** rebuild the in-memory `Spawn`; re-open the journal repo with the re-delivered key; restart the
   journal watchers; re-apply the egress floor (idempotent); re-dial ACP; report the spawn Active.
4. **Reap**, *or any failure to rebuild*: fall back to today's behaviour — **capture-before-reap**, then
   destroy. There is no half-adopted state: a spawn is fully rebuilt or it is gone.

### 4.3 The node never self-adopts

Adoption requires the CP's confirmation, matched on **generation**, reusing the CP's existing `adoptOrStop`
fencing. A node returning after the CP re-created its spawns elsewhere is told to reap, so split-brain is
not expressible.

**The corollary is the important one: if the CP is unreachable at startup, the node reaps nothing and leaves
the pods running.** A CP blip must never become data loss. (Today it silently would — the node reaps on its
own authority.) The node retries; the pods wait.

A pod whose `LabelNodeID` does not match this node is not ours: leave it, log it, do not reap it.

### 4.4 Secrets

- **The MITM CA private key is persisted** into the spawn's existing on-disk dir, alongside the
  `spawn-ca.crt` the node already writes there (`internal/spawnlet/gitproxy.go`). This is not a new class of
  secret: the node **is** the git proxy and already sees that traffic in plaintext, and `node.key`
  (`cmd/spawnlet/main.go:246`) is already at rest. The alternative — deriving the key deterministically from
  `node.key` via HKDF — is cleverer and avoids the file, but it also requires a deterministic certificate
  (serial, validity) to keep the agent's cached bundle valid, and buys ~zero marginal security given
  `node.key` is on that disk anyway. Simplest thing that works.
  *Why this matters:* today the CA is **regenerated on a cache miss** while the agent still trusts the old
  cert — so any long-lived in-agent process that cached the CA bundle gets TLS failures after a restart.
- **The model API key never touches node disk, and does not need to.** The sidecar container never stopped;
  it still holds the key in its env. Same for `SIDECAR_CONTROL_TOKEN`, which the node reads back from the
  sidecar's env rather than re-deriving.
- **The journal repo password** is re-delivered by the CP, owner-sealed, via the existing key-travel path.

### 4.5 Runtime gaps to close first

Re-adoption cannot name the containers it is adopting until these land:

- **CRI `ListManaged`** must return the **agent id, the sidecar id, and the pod IP** — it must query the
  sandbox's containers and `PodSandboxStatus`, not just list sandboxes.
- **Docker `ListManaged`** must offer the same guarantees (it has `LabelRole` grouping; the contract suite
  from SE1 pins that both lanes really do).

These are prerequisites, not follow-ups.

### 4.6 Sessions

The session registry starts empty. Clients reconnect and open a fresh session against the still-alive agent
— which works because **the agent is the ACP server and the node dials it**, so the agent survives the
node's death and merely sees a client disconnect. Lingering in-container session servers are reaped at adopt
so they do not squat their ports.

## 5. Testing

**Hermetic (the payoff from SE1).** In `fakepod`, container state outlives the `Manager` — so *a restart is
just constructing a new Manager against the same `fakepod` instance*. Re-adoption becomes a unit test:

- graceful shutdown leaves the pods running (asserts `gracefulDetachAll` does not destroy);
- a new Manager over the same fake re-adopts them, with the spawn Active and its content intact;
- a stale generation is reaped, with capture-before-reap having run;
- **a CP that is unreachable at startup reaps nothing** — the pods are still there afterwards;
- a rebuild failure (fault-injected) falls back to capture-then-destroy, leaving nothing half-adopted.

**VM (the honest one).** `systemctl restart spawnery-spawnlet` against the prod-stack VM with a live spawn
that holds a marker file *and* a long-running process: both must survive, the spawn must return to Active
with no manual intervention, and git-over-HTTPS must still work inside the agent (proving §4.4's CA fix).

## 6. Acceptance criteria

- A `systemctl restart` of the spawnlet leaves every running spawn running, and every spawn returns to
  **Active** without operator action. **No `MarkUnreachable` that sticks.**
- An in-flight process inside the agent survives the restart.
- git-over-HTTPS inside the agent still works after the restart (the CA did not change under it).
- A CP that is unreachable when the node starts causes **zero** pods to be destroyed.
- A stale-generation pod is capture-before-reaped, as today.
- CRI's `ListManaged` returns the agent id, sidecar id, and pod IP; the SE1 contract suite pins it on both
  lanes.
- `golangci-lint run ./...` = 0 issues.

## 7. Out of scope

Live session continuity (same ports, resumed ACP session). Surviving an unclean node death with the *same*
guarantees — an unclean death still leaves the pods running, and this design adopts them the same way, but
the node's own in-flight bookkeeping (a suspend mid-gate, say) is not made crash-safe here. The
`PodBackend` interface itself (SE4).

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from the
assumptions above — append a dated note here, whether or not a formal debugging skill was used.*

### 2026-07-12 — sp-2tx8.3.4 (ReconcileManagedPods)

- §4.2's "re-apply the egress floor (idempotent)" is implemented as **Remove-then-Apply**. Neither applier
  is idempotent (both insert with `iptables -I`), and the previous process never removed its rules
  (`DetachAll` deliberately doesn't) — so a bare re-Apply would install a duplicate rule set that teardown's
  single `-D` pass would leave behind (a stale DROP for whatever pod next recycles the IP). `ReapPod` now
  also removes the floor for the pod it reaps, which `ReapOrphans` never did (it had no pod IP until 3.1).
- Adoption **derives** each mount's host dir through a new, pure `storage.Backend.HostDir` rather than
  re-running `Prepare` — `Prepare` re-seeds (scratch) or re-clones (github) a directory the live agent is
  actively writing to.
- An adopted spawn's `ControlToken` is empty and its GitHub control listener is not re-served (both are
  sp-2tx8.3.5's scope): until that lands, `SetModel` and in-agent GitHub token minting do not work on an
  adopted spawn. Marked `TODO(sp-2tx8.3.5)` in `internal/spawnlet/adopt.go`.
- `StartSpawn` is gated behind the reconcile: the CP must not be able to `StartPod` a spawn id whose pod the
  node is still holding.
- The planner's file list did not include `internal/spawnlet/manager_reconcile_capture_test.go`, which also
  referenced `ReapOrphans` (capture-before-reap R1-R4 matrix). Re-expressed it over `UntrackedPods`/`ReapPod`
  in the same commit as the split — leaving it would have kept `ReapOrphans` alive in the tree and failed
  the acceptance grep.
- `startSpawn`'s agent-death `exitFn` was extracted into `attacher.agentDeathReclaim` (behaviour-preserving —
  `TestStartSpawnAgentDeathSelfCleans` still passes) and is shared with the ACP re-dial in `adoptPod`.
  `Manager.DetachSpawn` was added as `Adopt`'s undo (store removal + watcher handoff, never `Stop`).
