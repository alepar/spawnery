# Suspend Captures a Torn Snapshot — Fix (SE2)

- **Bead:** `sp-2tx8.2` (epic `sp-2tx8`) — **bug, in shipped code**
- **Status:** draft
- **Mode:** one-shot (Mode B)
- **Depends on:** `sp-2tx8.1` (SE1) for the hermetic regression test.
- **Provenance:** surfaced by the `superpowers:roast` of the (now superseded) runtime-abstraction spec;
  3/3 judges CONFIRM, severity blocker. Verified in code, not inferred.

## 1. Problem

A suspend produces a **torn snapshot**: the spawn's journaled **mounts** are captured at one instant and
its container **rootfs** at a later one, with the agent running in between. On resume the spawn gets a
rootfs from time T2 married to mounts from time T1, and any agent write in that window exists in one and
not the other.

The suspend is a two-RPC gate. `SnapshotForSuspend` (`internal/spawnlet/manager.go:1951`) stops the
journal watchers, **pauses the agent**, takes the final journal/mount snapshot while quiesced, and returns
its mount markers to the CP. It deliberately leaves the agent paused. The CP then closes the gate and
calls `FinishSuspend`, which — via `teardown` — does this (`manager.go:2091-2109`):

```go
_ = m.pod.Unpause(ctx, h)              // <-- the agent is running again
… scrubFn(ctx, h, m.cfg.DeltaScrubPaths)   // rm -rf /tmp … via `exec`
… m.pod.CaptureDelta(ctx, h)           // Docker commits with Pause:false — a LIVE container
```

The unpause is not incidental; the in-line comment explains it: the scrub is an `exec` into the agent
container, and **`exec` cannot run on a paused container** (`"container is paused"`). So the scrub, a
layer-hygiene feature, forced the gate open.

The giveaway is that `FinishSuspend`'s own doc comment (`manager.go:2005`) still claims it

> *"captures the rootfs delta (on the paused container — commit works on paused containers)"*

which is what the code used to do, and no longer does. The scrub broke the invariant and the comment was
never updated — so the code has been silently violating its own stated contract.

**Consequence.** Every agent process still writing between T1 and T2 tears the snapshot: builds, LSP
servers, `git` background work, editor autosave, cron. The code's in-line defence — *"sessions are already
reaped by the time FinishSuspend calls teardown, so the agent has no driver"* — is true and irrelevant: it
removes the *ACP driver*, not the agent's own processes. The scrub has the same problem in reverse: a live
process can recreate a scrubbed path before the diff runs, so the scrub is not even reliably doing its job.

## 2. Main challenges

The naive fix — "capture while paused" — collides head-on with the reason the unpause exists. The scrub
needs `exec`; `exec` needs a running container. Any fix must either move the scrub somewhere it can run
against a live container *without* opening the quiescence window, do the scrub without `exec`, or drop it.

Second, moving the scrub **earlier** (before the pause) makes a latent hazard worse: `DeltaScrubPaths`
defaults to paths like `/tmp`, and a journaled mount can be mounted **at or under** a scrub path. Today
the scrub runs *after* the mount snapshot, which accidentally protects the snapshot's contents. Move it
before, and an `rm -rf /tmp` would delete the user's mount data and then faithfully snapshot the deletion.
The fix must not trade a torn snapshot for a data-destroying one.

Third, fork. The scrub lives only in the suspend path, so a **fork captures an unscrubbed layer** — the two
lanes produce artifacts under different content policy. That asymmetry needs a decision, not an accident.

## 3. Key decisions

**Move the scrub to before the pause, and never unpause.** The new order is: stop watchers → scrub (live,
`exec` works) → **pause** → journal/mount snapshot → *[CP gate]* → **capture rootfs while still paused** →
stop → finalise. This restores exactly the invariant `FinishSuspend`'s comment already advertises, and it
puts the *best-effort* operation (hygiene) outside the *correctness* invariant (consistency) rather than
inside it. Guard the scrub so it can never touch a path at or under a mount. Leave fork **deliberately
unscrubbed**, because scrubbing a fork's source would delete a live user's files.

## 4. Decision points

### 4.1 The ordering fix

**Chosen: scrub before `Pause`; delete the `Unpause` in `teardown`; capture on the paused container.**

The scrub's guarantee is *best-effort layer hygiene* — a smaller delta. It can tolerate a race (a live
process recreating `/tmp/x` between the scrub and the pause costs us a few bytes of layer, and nothing
else). The snapshot's guarantee is *consistency*, and it cannot tolerate a race at all. The correct move
is therefore to put the racy, tolerant operation outside the quiesced window and keep the intolerant one
strictly inside it. After the fix, everything that lands in an artifact — mounts and rootfs alike — is
captured from a frozen agent, in one window, across the CP gate.

Both lanes support capture-while-paused, and this is not assumed:

- **Docker** commits a paused container (which is what the pre-scrub code did, and what the doc comment
  still describes).
- **CRI/containerd** `rootfs.CreateDiff` was **spike-proven** this session to produce a byte-identical
  layer (same digest, same size) whether the container is RUNNING, PAUSED, or STOPPED — the spike that
  killed the previous design's central premise. Capturing a *paused* container is the easy case.

*Considered — scrub from the node's filesystem instead of `exec`* (reach into the graphdriver/snapshotter
dir and `rm` there, so no unpause is needed): rejected. It is runtime-specific
(`/var/lib/docker/overlay2/<id>/diff` vs a containerd snapshotter mount), hostile to the rootless +
userns-remap lane, and trades a clean bug for a fragile layering violation.

*Considered — scrub the captured layer, not the container* (capture while paused, then rewrite the layer
tar to drop the scrub paths): this is the theoretically cleanest option — no live mutation at all, works
identically for fork, and no abort side effect. Rejected on cost: Docker has no API to edit a committed
image's layer, so it means export → rewrite → import for every suspend, in both lanes. Disproportionate to
what the scrub buys. Worth revisiting only if layer surgery is ever needed for another reason.

*Considered — snapshot the mounts after the capture instead* (so both happen live at T2): rejected. It
moves the tear rather than removing it, and gives up the one quiescent window we already have.

### 4.2 The scrub-vs-mount guard

**Chosen: refuse to scrub any path that is at, or under, a journaled or bind mount — skip it and log.**

With the scrub now running *before* the mount snapshot, a `DeltaScrubPaths` entry that overlaps a mount
would delete the user's persistent data and then snapshot the deletion as authoritative. That is a
data-loss bug strictly worse than the one being fixed, and it is created by this change, so the guard ships
**with** it, not after.

The check is a path-prefix test against the spawn's mount table (`sp.MountBindings` / `JournalMounts`),
evaluated per scrub path at scrub time. A skipped path is logged at warn — silently not-scrubbing is how
this class of thing hides. This also closes roast finding M23, which flagged the intersection as
unspecified even in today's ordering.

### 4.3 Fork stays unscrubbed — deliberately

**Chosen: the scrub remains suspend-only. The asymmetry is intentional and gets a comment saying so.**

The roast filed "fork captures an unscrubbed layer, so fork and suspend produce artifacts under different
content policy" as a major. The finding is correct; the implied fix is not. On **suspend**, the spawn is
being torn down, so deleting `/tmp` costs nobody anything. On **fork**, the source keeps running and the
user is still using it — scrubbing it would **delete a live user's files out from under them** to make a
*different* spawn's image smaller. That is not a trade worth making, and the source-preserving fork we just
shipped exists precisely so the source is untouched.

So the artifacts genuinely differ, for a stated reason: a suspend artifact is scrubbed, a fork artifact is
not. The alternative — having the *forked child* scrub its inherited `/tmp` on first boot — cleans the
running fork but does not shrink the captured layer, so it buys nothing the fork lane cares about. Not
doing it. (YAGNI: if fork layer size ever becomes a real problem, §4.1's rejected layer-rewrite option is
the principled answer, and it would serve both lanes.)

### 4.4 The aborted-suspend side effect (accepted)

`SnapshotForSuspend` already has an abort arm: if the journal snapshot fails it unpauses, restarts the
watchers, and the spawn keeps running. With the scrub moved earlier, an aborted suspend now leaves the
spawn with its scrub paths **already cleaned**.

Accepted, and documented in the code. `DeltaScrubPaths` are by definition disposable (`/tmp`, package
caches) — content the agent must already tolerate losing, since a *successful* suspend/resume cycle wipes
them anyway. The alternative (defer the scrub until after the CP gate commits) is precisely the ordering
that causes the bug. Trading a certain torn snapshot for an unlikely, harmless `/tmp` clean is the right
side of that trade.

### 4.5 Fix the lying comment

`FinishSuspend`'s doc comment describes behaviour the code stopped having. After this change the comment
becomes true again, and the `teardown` sequence gets an explicit note that **the capture runs on a paused
container and the unpause was removed on purpose** — so the next person who needs an `exec` in that path
does not silently re-open the window.

## 5. Acceptance criteria

- `teardown` no longer calls `Unpause` before capture; the rootfs delta is captured from the paused agent.
- The scrub runs in `SnapshotForSuspend`, before `Pause`.
- A scrub path at or under a mount is skipped and logged; a test proves mount data survives a
  `DeltaScrubPaths` entry that overlaps it.
- **Regression test (hermetic, on SE1's `fakepod`):** with a background agent writer running, suspend →
  resume yields a rootfs and a mount view from the **same instant** — no write exists in one and not the
  other. This test must **fail** against the pre-fix code.
- The suspend/resume e2e (`garage_e2e`) and the VM acceptance `git-persistence` scenario stay green.
- `FinishSuspend`'s doc comment matches what the code does.
- `golangci-lint run ./...` = 0 issues.

## 6. Out of scope

Fork's content policy beyond §4.3's decision. Layer rewriting. The `PodBackend` interface (SE4). The
question of whether `Pause` fully quiesces *every* writer — it does not (the sidecar and the spawnlet-side
mount writers are not paused; roast §5), but the journal watchers **are** stopped by
`SnapshotForSuspend` before the pause, and the sidecar does not write to the agent's rootfs or mounts.
Full-system quiescence is SE4's problem, not this bug's.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from the
assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
