# Spawn Transition Coordination — DB-Claim Lease + `status_seq` Optimistic CAS

**Status:** designed 2026-06-13 · **Beads:** `sp-u53.7` (epic), supersedes the narrow `sp-u53.7.1`;
absorbs `sp-csks`; narrows `sp-u53.7.2`; revisits `sp-iuo1`.
**Supersedes/reshapes:** the spawn-state-machine consistency protocol's "CP = intent, node =
ground-truth-for-running-only" split, for the suspend/resume transition window specifically.

## Problem

Suspend/resume is a multi-step dance across several goroutines **and two processes** (CP + node).
Splitting suspend into a fail-closed gate→reap→finish sequence (`sp-ei4.1.15`) broke three implicit
invariants the old single-step suspend relied on — `store.Claim` = single-owner exclusion, "the row
stays Active until the node confirms", and "the agent is frozen until stop" — and every actor that
still assumes them now races:

1. **CP suspend handler vs CP inventory reconcile** — `SetSuspending` was *deferred* until the node's
   reply, so the row stayed `Active` across the whole round-trip; a heartbeat's `reconcileInventory`
   sees the torn-down container missing and flips `Active→Unreachable`, and the later `SetSuspending`
   (guarded on `Active`) fails with `store: transition conflict`. Patched with an in-memory `inFlight`
   exemption set (`66a6b85`) — lost on restart, invisible to other actors, a band-aid.
2. **Node suspend goroutine vs node quota-watchdog / idle-reaper** — both call the node's local
   `Claim`; the gate uses `store.Get` (not `Claim`), leaving the spawn claimable across gate→finish;
   plus a true data race on `sp.journalWatchers` (gate nils it while teardown ranges it). (`sp-csks`)
3. **CP 30s timer vs CP late `SuspendComplete`** — two independent deadlines reach inconsistent
   terminal states (CP `Errored`, node `Suspended`); the late reply is dropped, markers lost. (`sp-iuo1`)
4. **Agent process vs teardown across the unpause** — pause was the synchronisation that made the
   journal snapshot a consistent cut; unpausing for the scrub lets the agent write after the snapshot. (`sp-o5ka`, out of scope here)

The ad-hoc `inFlight` set only quiets CP-side actors; it is invisible to the node's own actors and
does not survive a restart. We want a principled coordination model that works **both** in-process
(CP goroutines) **and** cross-process (CP ↔ node).

## Main challenges

A lifecycle transition performs a **network round-trip in the middle of a state change** (pause →
snapshot → teardown on a remote node). That forbids holding a lock across the transition (deadlock /
unavailability), and it opens a window where every background sweeper (reconcile, quota watchdog, idle
reaper) can observe the entity and act on a stale view. The coordination must: (a) make "mid-transition"
**observable and crash-safe** so sweepers back off; (b) provide **hard mutual exclusion** for the one
actor that mutates; (c) close the **read/modify/write TOCTOU** between a sweeper's decision and an
operator's write; (d) **revert cleanly** on failure; (e) **recover** an entity stranded by a dead
driver; and (f) keep the **two processes converging** when their independent deadlines disagree.

## Key decisions

The research (`deep-research`, 2026-06-13, 24/25 claims confirmed) is unambiguous on the shape: make
the **entity's own persisted state the single source of truth for "mid-transition"** (the Kubernetes
`deletionTimestamp`+finalizer pattern — a crash-safe marker every reconciler reads, vs. an in-memory
set lost on restart), serialise mutations per entity, fence every write with **optimistic
compare-and-swap on a version column** (the `resourceVersion`/409 pattern, closing TOCTOU), and **never
hold a lock across the round-trip** (SEI CERT POS52-C). A lease with a timeout is **unsafe on its own**
(Kleppmann) — it must be paired with a **fencing token checked at the resource**. We already have that
token: the episode **generation**.

The chosen model (after evaluating a node-authoritative inversion and rejecting it — see Decision 1):

- **The CP is the sole brain.** Every lifecycle transition is decided and driven by a CP goroutine.
  The node never transitions a spawn on its own.
- **One locking substrate: the CP store** (bun-accessed; sqlite/postgres). The claim/lease is **columns
  on the spawn row**, acquired by CAS — no second lock mechanism, no split-lock state. The old
  in-memory `s.locks` per-spawn lock is retired.
- **The node is a pure generation/epoch-fenced executor + reporter.** It runs CP-driven commands
  (idempotent, rejects stale generations), and reports running inventory + resource metrics + progress.
  It never locks and never decides. This **dissolves** the node-side `Claim` contention (race 2).
- **`status_seq` optimistic CAS** closes TOCTOU at finer grain than generation: every status/activity
  mutation bumps it; every guarded write CAS-es on the `status_seq` the caller read.
- **No CP ⇒ no transition** (safety invariant). Node-local detectors become **reporters**; a partition
  means no signal is acted on, so the spawn is *preserved* for the user to resume — never autonomously
  reaped under degraded connectivity.

## Decision points, by section

### 1. Authority model — CP-authoritative, not node-authoritative

**Chosen:** the CP store stays the durable source of truth for status (UI + reconcile read it,
unchanged); the node holds *no* status authority. **Rejected:** a node-authoritative inversion (CP
store becomes a node-pushed projection, authority handing off at pod boundaries). The inversion buys
single-writer-per-live-spawn but at the cost of handoff/dormant/reconnect-merge complexity — and,
decisively, it implies **node autonomy under partition**, which is *actively harmful*: a node that
loses the CP and then idle-reaps would tear down a pod whose snapshot/delta-shipping cannot reach the
journal anyway, destroying in-flight work the user expected to resume. Centralising the decision in the
CP makes "no CP, no transition" structural. The race-safety win does **not** require the inversion — it
comes from the claim/lease + fencing, which both models share.

### 2. Locking substrate — DB claim columns, single mechanism

**Chosen:** the claim/lease lives as columns on the spawn row, acquired by CAS, because once the
watchdog/reaper become CP-side reporters (§6) **every transition driver is a CP goroutine** — there are
no node-local claimants left, so nothing needs a node-side lease. One substrate eliminates the
split-lock risk of an in-memory CP lock *plus* a node lease diverging. **Rejected:** a node-side lease
manager (needed only if node-local actors claimed directly — they no longer do) and the existing
in-memory `s.locks` lock (retired in favour of the DB claim so all locking goes through one place).

Spawn row gains: `status_seq BIGINT NOT NULL`, `claim_holder TEXT NULL`, `claim_lease_id TEXT NULL`,
`claim_deadline` (nullable timestamp). The episode **generation** stays on the `spawn_containers` row —
it fences the *episode* and is the only token that crosses the wire; `status_seq` fences *row/status
mutations* and is CP-store-internal. Two counters, two scopes.

### 3. The CAS protocol

**Acquire** (caller reads the row first, then CAS on the `status_seq` it read):
```sql
UPDATE spawns
SET claim_holder=?, claim_lease_id=?, claim_deadline=now()+TTL, status_seq=status_seq+1
WHERE id=? AND status_seq=?
  AND (claim_holder IS NULL OR claim_deadline < now())
-- rowcount 1 → acquired; 0 → conflict → re-read, re-decide, retry/abort
```

**Transition under a held claim** (fenced by lease + `status_seq` + **generation**; the `status`
predicate is dropped because status cannot have changed if `status_seq` did not):
```sql
UPDATE spawns SET status='suspending', status_seq=status_seq+1
WHERE id=? AND status_seq=? AND claim_lease_id=?
  AND ? = (SELECT generation FROM spawn_containers WHERE spawn_id=spawns.id AND ended_at IS NULL)
-- caller's expected generation in the subquery: a recreate that ended this episode returns a
-- different gen → rowcount 0 → fenced out.
```

**Heartbeat** (liveness only — does **not** bump `status_seq`):
```sql
UPDATE spawns SET claim_deadline=now()+TTL WHERE id=? AND claim_lease_id=?
-- rowcount 0 ⇒ the claim was lost (expired/preempted/released): the driver MUST bail out
-- immediately — stop driving, commit no further transitions.
```

**Release** clears the claim columns and bumps `status_seq`.

**TOCTOU closed (the idle-reaper example):** reaper reads `(Active, seq=42)` and decides to reap.
Concurrently the activity updater runs `SET last_activity_at=…, status_seq=status_seq+1` → seq=43.
The reaper's Acquire `WHERE status_seq=42` → rowcount 0 → forced re-read → sees fresh activity → stands
down. **Any** mutation bumping `status_seq` invalidates every stale read; this generalises beyond the
reaper.

### 4. Transient statuses — `Suspending` (used correctly) + new `Resuming`

**Chosen:** keep `Suspending`/`PhaseSuspending` (already exist) but write `Suspending` **before** the
round-trip (not deferred), and add a **new** `Resuming` status + `PhaseResuming` for the symmetric
resume path — distinct from `Starting` so the UI and sweepers tell a resume-restore apart from a fresh
create. The claim is the *hard exclusion*; the transient status is the cheap *read-signal* sweepers
consult to skip. **Rejected:** relying on the claim alone (sweepers would have to acquire a claim per
spawn per tick to learn "busy" — too heavy; a status read is free) and reusing `Starting` for resume
(loses the resume-vs-create distinction the UI wants). `Resuming` is surfaced on the wire
(`SpawnStatus`) for the UI.

### 5. Flows

**Suspend (CP-driven)** — replaces the deferred `SetSuspending` + 30s `select`:
1. Read `(Active, seq=S, liveGen=G)` → **Acquire** claim (CAS on `seq=S`; 0 → re-read/abort).
2. **`Active→Suspending`** (CAS, lease+gen fenced) — sweepers now read `Suspending` and skip.
3. `Suspend(id, G)` to node; **heartbeat the lease while waiting**; any heartbeat → 0 rows ⇒ bail.
4. Reply: **gate-abort** → revert `Suspending→Active`, release, return `FailedPrecondition` (spawn
   alive). **Success** → `Suspending→Suspended` + record markers + end container + release (authority
   back to CP, dormant).

**Resume (CP-driven):** Acquire claim on the (dormant) row → `ClaimStarting` bumps generation →
`Suspended→Resuming` → provision → `Resuming→Active` + release. Failure reverts `Resuming→Suspended`
(migration) or `→Errored` (plain resume), reusing the existing `failResume` path.

**Reconcile:** drop the `inFlight` hack. The lost-sweep skips any spawn whose status is transient
(`Suspending`/`Resuming`) or currently claimed — a read, no claim taken. Only `Active`, unclaimed,
unreported → `Unreachable`. **Reconnect resync:** the node reports `(spawn, gen)` + metrics; the CP
reconciles against its authoritative status with **generation as the tiebreak** — node gen behind live
→ destroy stale pod; gen matches & status live → confirm/adopt; status dormant but node still running
gen → orphan → destroy.

### 6. Node-local detectors → CP-side reporters

**Chosen:** the node's quota watchdog and idle reaper stop transitioning spawns autonomously. The node
heartbeat carries per-spawn resource usage + idle facts (**data, not decisions**); CP-side evaluators
consume them, decide, and drive suspend/stop through the *same* claim+transition path. **Rejected:**
keeping them as node-side initiators gated behind a synchronous CP claim+status-write (still enforces
"no CP, no transition" but keeps two initiator sites and re-introduces node-side claiming). A partition
→ no reports → no action → spawns preserved (the §Key-decisions safety invariant, made structural).

### 7. Liveness split — driver (7.1) vs worker (7.2)

**Chosen:** the lease + heartbeat is **driver** liveness (is the CP goroutine still alive?) — handled
here. If the driver dies, the lease expires and a **recovery sweep** (CP, periodic + on startup) finds
`status IN (Suspending,Resuming) AND (claim_holder IS NULL OR claim_deadline < now())` and reconciles
it to a safe state against node ground truth (pod paused → unpause + `Active`; pod gone → finalise).
Node→CP **progress events** — is the snapshot advancing vs wedged? — are **worker** liveness, deferred
to `sp-u53.7.2`. Together they replace the blunt 30s timeout: lease-expiry (driver dead) here,
progress-stall (worker stuck) in 7.2. A generous backstop deadline remains in 7.1 since the lease
handles CP-death cleanly. **Rejected:** folding worker-progress into 7.1 (keeps the two liveness
concerns distinct and the change reviewable).

### 8. Failure / recovery matrix (every path lands in a defined state)

| Failure | Outcome |
|---|---|
| Node gate abort | revert `Suspending→Active`, spawn alive, `FailedPrecondition` |
| CP driver goroutine dies | lease expires → recovery sweep reconciles vs node ground truth |
| Node dies mid-suspend | `SuspendComplete` never arrives; stream close → `Unreachable`; recover on reconnect |
| Claim preempted/expired | heartbeat 0 rows → driver bails; new owner drives |
| Stranded `Suspending`/`Resuming` | recovery sweep finds transient + expired claim → reconcile to safe state |

### 9. Cross-process convergence

There is **no distributed lock protocol**. Convergence reduces to three things, mostly already present:
the DB claim serialises CP drivers; **generation+epoch fencing on node commands** (re-checked *at the
node, at execution time*) rejects stale/superseded commands; and reconnect reconcile (§5) resyncs using
generation as the tiebreak. `sp-csks` thus **dissolves** to its residual `journalWatchers` data-race
fix (a plain node-internal mutex), kept in scope; `sp-iuo1` (late success dropped) is largely resolved
by lease/recovery + gen fencing and is revisited under 7.2.

## Implementation decomposition (child tasks under `sp-u53.7`)

- **(i)** Store primitive: `status_seq` + claim columns + `Acquire`/`Heartbeat`/`Release`/transition
  CAS + recovery sweep + migrations. *(internal/cp/store)*
- **(ii)** Rewrite suspend/resume on the primitive; retire `inFlight` and `s.locks`; add `Resuming`.
  *(internal/cp/lifecycle.go, server.go)*
- **(iii)** Reconcile: drop the `inFlight` exemption; honour transient/claimed; reconnect resync.
  *(internal/cp/server.go)*
- **(iv)** Watchdog/reaper → CP-side reporters (node reports metrics; CP decides + drives).
  *(internal/spawnlet, internal/cp)*
- **(v)** `journalWatchers` data-race fix (absorbs `sp-csks`). *(internal/spawnlet/manager.go,
  internal/node/attach.go)*
- **(vi)** Proto: `Resuming` wire status + node metrics fields. *(proto/, gen/)*

## Testing

- **Store CAS unit tests** (hermetic, in-memory store): Acquire conflict on stale `status_seq`; the
  idle-reaper TOCTOU (activity bump invalidates a pending reaper Acquire); transition fenced out by a
  recreated generation; heartbeat-0-on-lost-claim; recovery sweep reverts a stranded transient.
- **Lifecycle concurrency tests** (the gap that let races 1/3 ship): run suspend **concurrently** with
  `reconcileInventory` and a simulated late `SuspendComplete`; assert no `Unreachable` flip mid-suspend,
  no `transition conflict`, defined terminal state on every interleaving.
- **Reconnect resync tests**: node reports stale/current/dormant generations; assert
  destroy/confirm/orphan per §5.
- **e2e** (`e2e` tag): a real suspend/resume round-trip with the reconcile loop live, asserting the
  status timeline `Active→Suspending→Suspended→Resuming→Active` and marker persistence.
