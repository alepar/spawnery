# Fake PodBackend + Cross-Lane Contract Suite (SE1)

- **Bead:** `sp-2tx8.1` (epic `sp-2tx8`)
- **Status:** draft
- **Mode:** one-shot (Mode B)
- **Supersedes nothing.** Sibling of `2026-07-12-suspend-torn-snapshot-fix-design.md`.

## 1. Problem

Spawn lifecycle orchestration — fork, suspend, resume, delta capture, reap — is written against the
`runtime.PodBackend` interface (17 methods, `internal/runtime/pod.go`), but almost none of it is
reachable from the hermetic test suite. Exercising it needs Docker or containerd/runsc, built images,
and in the highest-fidelity case a libvirt VM. The consequence is measurable: every bug found in the
2026-07-11 session had been sitting in `master` for weeks, and each one was found by a human running a
VM, not by a test.

- fork **destroyed its own source** (stop → capture → re-launch) on the CRI lane.
- the assembled delta image was never `Unpack`ed into the snapshotter, so CRI could not launch it.
- the delta image was recorded under a non-canonical ref while CRI normalises the ref it queries.
- an `agentSpecs` in-memory cache made re-launch depend on state a restarted process could not have.
- suspend captures a torn snapshot (see the SE2 spec).

The fakes we *do* have are ad-hoc and drifting: `fakePodBackend` (`manager_sandbox_test.go`),
`noSizeFakeBackend` (`manager_quota_test.go`, a hand-forwarded copy of the former), and
`scriptedPodBackend` (`internal/node/attach_lifecycle_test.go`). Each is maintained by hand against a
17-method interface; when `RestoreForkedSource` was added, **all three silently failed to compile** and
the break was only caught because someone ran the tests. They also model nothing: `CaptureDelta` returns
a tag and records a string. They cannot express "the source is still running", "this write landed after
the snapshot", or "that image is not launchable" — which is exactly why none of them caught the bugs.

## 2. Main challenges

The hard part is not writing a fake; it is writing one whose **fidelity is enforced rather than
assumed**. A fake that lies is worse than no fake: it produces green tests for broken lanes, which is
precisely the failure mode we are trying to end. The fake must therefore (a) model enough state that
lifecycle bugs are *observable* — container liveness, image chains, and the **content** of what gets
captured, at the time it gets captured — and (b) be held to the same contract as the two real backends
by a suite both must pass.

The second challenge is honesty about coverage. A fake cannot prove durability, `fsync`, crash
survival, gVisor's syscall surface, or containerd's ref-normalisation quirks. Two of this session's
bugs (the un-unpacked image, the non-canonical ref) are *precisely* of that kind — real-lane semantics
a fake would never have surfaced. Claiming this suite makes the e2e lanes redundant would repeat the
mistake in a new form. It does not, and the spec says where the line is.

## 3. Key decisions

One behaviour-faithful in-memory `PodBackend` in a new **`internal/runtime/fakepod`** package, modelling
container state machines, an image/layer chain, and a **per-container content view split into rootfs and
mount paths** — the last being what makes capture-time consistency observable. It replaces all three
ad-hoc fakes. Alongside it, **`internal/runtime/podbackendtest`** exports `RunContract(t, Factory)`, a
table-driven suite run by three callers: the fake (hermetic, default suite), the Docker backend (under
`e2e`), and the CRI backend (under `cri_delta_e2e`). Anything the fake asserts, the real lanes must also
assert, or it is fiction. The interface itself does **not change** — that is SE4's problem, and
deliberately not this one's.

## 4. Decision points

### 4.1 Where the fake lives, and what it is called

**Chosen: a new non-test package `internal/runtime/fakepod`, constructor `fakepod.New(opts...)`.**

It must be importable from `internal/spawnlet`'s tests, `internal/node`'s tests, and the contract suite,
so it cannot live in a `_test.go` file or a `_test` package. A non-test package under `internal/runtime`
is the only option that all three can import; it never enters a production binary because no production
code imports it.

It is **not** called `FakeRuntime` and does not go in `internal/runtime`, because
`runtime.NewFake() *FakeRuntime` **already exists** (`internal/runtime/runtime.go:158`) and is a fake of
the *other*, legacy `Runtime` abstraction — the one `NewManager` takes. Today's tests construct
`NewManager(runtime.NewFake(), …)` and then white-box-overwrite `m.pod`. Two fakes of two different
interfaces with near-identical names in one package is a trap; the package boundary keeps them apart.

*Considered:* putting it in `internal/runtime` as `FakePodBackend` — rejected for the name collision
above. *Considered:* one fake per consuming package — that is the status quo, and it is what drifted.

### 4.2 What the fake models

**Chosen: three pieces of state — container lifecycle, image chain, and content.**

**Container lifecycle.** An explicit state machine per container (`absent → created → running ⇄ paused →
stopped → removed`) for the sandbox, the sidecar, and the agent, with the pod's two-phase structure
enforced: `StartAgent` into a non-existent sandbox is an error, `Attach` to a stopped agent is an error,
`Pause` of an already-paused container is an error, and `exec` (the scrub hook) on a paused container is
an error. **Illegal transitions must fail, not no-op.** This is the whole point: a fake that cheerfully
accepts `CaptureDelta` on a removed container cannot catch a fork that removed its source. Real-lane
behaviour is what the contract suite pins, so where the two real backends disagree on an edge (they do —
e.g. Docker's `Cmd` overrides `CMD` while CRI's `Command` overrides `ENTRYPOINT`, per the interface's own
doc comment), the fake models the **weaker** guarantee and the suite records the divergence rather than
papering over it.

**Image chain.** `map[ref] → {base, layers, depth}`. `CaptureDelta`/`CaptureDeltaAs` create an image
whose parent is `h.BaseImageRef` and enforce `layers(delta) > layers(base)` — the moby#47065 guard the
real path depends on. `EnsureImage(base, deltaRef)` returns `deltaRef` **only if that ref was actually
created and is launchable**, else `base`. A capture that produces an unlaunchable image is therefore a
test failure, not a silent pass — which is the class the "missing local delta image" bug belonged to.

**Content — the load-bearing part.** Each container has a writable view: `map[path] → bytes`, partitioned
by the mount table into **rootfs paths** and **mount paths**. Tests can write through a test-only hook
(`b.AgentWrite(spawnID, path, data)`) and, crucially, install a **background writer** (`b.StartAgentWriter(spawnID)`)
that keeps writing until the container is paused — modelling exactly what a real agent's build/LSP/autosave
processes do. `CaptureDelta` snapshots a **copy** of the rootfs view *at the instant it is called*; the
mount snapshot (driven by the journaler, not the backend) captures the mount view at *its* instant. A test
can then assert the two artifacts correspond to the same point in time. Without this, the SE2 torn-snapshot
bug is not expressible; with it, it is a three-line test.

*Considered:* a stateless recorder that just logs calls and returns canned values (today's design). It
cannot observe any of the five bugs above. *Considered:* modelling a real overlay filesystem — far more
than is needed; a flat path→bytes map with a mount partition is sufficient to express every consistency
property we care about.

### 4.3 Fidelity: the cross-lane contract suite

**Chosen: `internal/runtime/podbackendtest` exporting `RunContract(t *testing.T, f Factory)`,** where
`Factory` produces a live `PodBackend` plus the test hooks it needs (a way to write into the agent, and a
way to read a captured artifact's contents). Three callers:

| caller | build tag | runs in |
|---|---|---|
| `fakepod` | *(none)* | the hermetic default suite, on every `go test ./...` |
| Docker backend | `e2e` | needs Docker + images |
| CRI backend | `cri_delta_e2e` | needs containerd + runsc + images |

Per the project convention, the tagged lanes **fail** (never `t.Skip`) when their dependency is missing.

The suite is the fake's fidelity contract in both directions: a behaviour the fake implements that the
real lanes do not is a bug in the fake, and a behaviour the real lanes have that the fake does not is a
gap in the fake. Concretely it pins at minimum: two-phase start ordering; `Attach` liveness; pause/unpause
semantics including `exec`-on-paused failing; `CaptureDeltaAs` **leaving the source running and
unremoved** (the fork bug); `EnsureImage` returning a **launchable** delta ref after a capture (the image
bugs); the layer-count guard; `ListManaged` round-tripping the labels it claims to; and `Stop` being
idempotent. Where a real lane cannot support an assertion, the suite records it as an explicit,
named exception rather than dropping it.

*Considered:* trusting the fake and testing the real lanes separately — that is today, and today's fakes
model behaviour the real lanes do not have.

### 4.4 What this explicitly does **not** cover

Stated up front so nobody mistakes a green hermetic suite for a green lane:

- **Durability and crash semantics.** The fake has no `fsync` and no process to kill. "The artifact is
  durable before teardown" cannot be proven here — it stays an e2e/VM property.
- **Runtime-specific quirks.** containerd's ref normalisation, snapshotter unpacking, gVisor's syscall
  surface, the `runsc` pause regression (gVisor #12647) — a fake by construction cannot have these. Two
  of this session's five bugs were of exactly this kind. **The e2e and VM lanes remain load-bearing and
  are not reduced in scope by this epic.**
- **Concurrency against real kernel objects.** The fake serialises; a real lane does not.

The claim this spec makes is narrower and defensible: **every lifecycle-orchestration bug — who gets
stopped, what gets captured, when, and from which state — becomes catchable in milliseconds without
Docker.** That is four of the five bugs above, and both SE2 blockers.

### 4.5 Consolidating the existing fakes

**Chosen: delete all three, port their tests onto `fakepod` with options.** `fakePodBackend`'s recording
(`ops`, `capturedRefs`) becomes `b.Ops()`; its fault knobs (`captureErr`, `pauseErr`) become
`fakepod.FailOn(op, err)`; `scriptedPodBackend`'s scripted returns become the same. `noSizeFakeBackend`
exists solely to *not* have a `DeltaSize` method (Go interface satisfaction being static, it must be a
distinct type); it becomes a one-line `fakepod.WithoutDeltaSize(b)` wrapper in the fake package — one
maintained type instead of a hand-forwarded 17-method copy that must be updated on every interface change.

### 4.6 The tests this unlocks (the actual deliverable)

The fake is not the deliverable; the tests are. At minimum, each of this session's bugs gets a regression
test that fails against the pre-fix code:

1. **fork preserves its source** — after `CaptureDeltaAs`, the source agent is `running`, not removed, and
   its content is unchanged. *(Would have caught the CRI fork bug.)*
2. **fork's artifact inherits the source's content** — the fork's rootfs contains what the source had at
   capture time.
3. **suspend is not torn** — with a background agent writer running, the rootfs artifact and the mount
   snapshot reflect the same instant. *(This is SE2's regression test; it is why §4.2's content model
   exists.)*
4. **a captured delta is launchable** — `EnsureImage(base, DeltaTag(id))` returns the delta after a
   capture. *(The image-visibility class.)*
5. **resume replays the delta** — a spawn resumed from a captured delta sees the writes.
6. **failure arms** — capture fails → the source is restored to running; `StartAgent` fails → the pod is
   rolled back, not leaked.

## 5. Acceptance criteria

- `internal/runtime/fakepod` and `internal/runtime/podbackendtest` exist; `RunContract` passes against
  the fake in the default hermetic suite (`go test -race ./...`, no Docker).
- `RunContract` passes against the Docker backend under `e2e` and the CRI backend under `cri_delta_e2e`.
- `fakePodBackend`, `noSizeFakeBackend`, and `scriptedPodBackend` are **deleted**; their tests pass on
  `fakepod`.
- The six regression tests in §4.6 exist. Tests 1–3 **fail** when reverted against the pre-fix code
  (verified by actually reverting, not by assertion).
- `golangci-lint run ./...` = 0 issues.

## 6. Out of scope

Any change to the `PodBackend` interface (SE4). Any change to suspend ordering (SE2 — this epic only
supplies the harness that makes SE2's fix testable). Reducing the e2e/VM lanes.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from the
assumptions above — append a dated note here, whether or not a formal debugging skill was used.*

### 2026-07-12 — sp-2tx8.1.3: the real-lane arms

`RunContract` now runs against three backends: `fakepod` (hermetic), `DockerPodBackend` (`e2e`), and
`CRIPodBackend` (`cri_delta_e2e`, via the new `just test-cri-contract` fixture — the pre-existing
`test-cri-delta` containerd has no CRI plugin and no CNI, so it cannot run a pod sandbox).

Both real lanes passed every one of the 13 contract cases on the first run against real infrastructure
(a real Docker daemon; a dedicated CRI+CNI containerd, `runc` handler) — no lane bugs surfaced, and no
contract case had to be fixed in the arm.

Lane divergences the suite records rather than papers over:

- **Cmd vs Command.** Docker maps `AgentSpec.Cmd` to `Config.Cmd` (overrides CMD, keeps ENTRYPOINT); CRI
  maps it to `Command` (overrides ENTRYPOINT). Both arms drive `Cmd = nil` and fall through to the image
  entrypoint, so the contract does not exercise the divergence. A contract case that pins argv semantics
  would need per-lane expectations; none exists, and that is deliberate.
- **Stop.** Docker stops without removing (stopped pods linger in `ListManaged`); CRI removes the
  sandbox. The contract only pins idempotence; the Docker arm force-removes its containers in cleanup
  (matched by the `spawnery.node-id` label), which also confirmed the daemon is left clean after a run.
- **ListManaged ids.** Docker: sidecar+agent ids. CRI: sandbox id only. The contract asserts "at least
  one id is set" (sp-2tx8.3.1 tightens this).
- **The zero-layer guard is not the same guard.** Docker compares committed layer count to the pinned
  base's (moby#47065); CRI rejects an empty delta layer (`deltaSize <= 0`) and releases the half-made
  image. `ArmZeroLayerCapture` is implemented per lane against the guard that lane actually has: Docker's
  arm repoints the handle's pinned base at an image one layer deeper than any commit can produce
  (`buildDeepImage`, asserted `base+1` layers before use); CRI's arm wraps the `deltaEngine` seam
  (`armableEngine`) so `Capture` reports a zero-byte diff while armed.

Registered contract exceptions: none — both real lanes satisfy the whole contract.

The CRI arm was run under the default `runc` handler only (`RUNTIME_HANDLER=runsc` is wired in
`just test-cri-contract` for a future gVisor pass but was not exercised in this task).
