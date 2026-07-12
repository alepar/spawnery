# Skill Delivery Hardening — Re-Presign, Live Error Taxonomy, Staging Budget, Config Surface

**Date:** 2026-07-12 (revised 2026-07-12 after roast BLOCK)
**Status:** draft (roasted → BLOCK → revised)
**Epic:** `sp-mwco.4`
**Extends:** [Profile Skill Ingestion from a GitHub URL](2026-06-22-profile-skill-url-ingestion-design.md) §4.6/§4.9/§4.11
**Siblings:** [Skill Bundles](2026-07-12-skill-bundles-design.md) (`sp-mwco.1`) — bundles are what break the current staging budget

## 1. Problem

The parent spec mandated four pieces that shipped incomplete, and bundles (`sp-mwco.1`) turn two of
them from latent debt into **happy-path failures**:

- The node→CP **re-presign RPC** does not exist; the 30-minute TTL is the only defense against
  expiry, and spike S5 (cold-start latency) never ran.
- The node's **error taxonomy is built but dead**: `spawnlet/artifacts.go` classifies 404 → terminal,
  transport → retryable, 403/5xx → retryable, but **nothing consumes `FetchError.Terminal`** — no
  retry, no clean terminal failure; everything bubbles up as a generic create error.
- **Size caps and the host allowlist are hardcoded** despite §4.11 promising a config surface.
- The ingest quota has no backoff guidance.
- `sp-nrzf.3.14.8` (resume hard-depends on Garage even with a delta image) belongs to this theme.

## 2. What the roast changed

Three of this spec's original claims were **false**, and one mechanism was **dead on arrival**:

1. **"Backoff ceiling well under the CP stall window" was wrong three ways.** It counted only ~13 s
   of sleeps while ignoring the **5-minute per-attempt HTTP timeout** (`spawnlet/artifacts.go:30`)
   → ~15 min *per artifact*; `Materialize` is a **serial** loop with **no aggregate deadline** → ×N
   members; and **no progress event is emitted inside staging**, so the CP's 30 s no-progress stall
   timer never resets. With a 20-member bundle this breaks the **happy path**, not just the retry
   path.
2. **Re-presign on "403 and only 403" is dead on the exact failure it exists to fix.** Garage returns
   **400 / `InvalidRequest` ("Date is too old")** for an *expired* presign; 403 (`AccessDenied` /
   `SignatureDoesNotMatch`) means clock skew, proxy Host rewriting, or endpoint mismatch — a
   *persistent config fault* that would burn the retry budget and then misreport "Garage
   unreachable".
3. **"Hook it where the placeholder comment sits (artifacts.go:191)"** — line 191 is inside a **doc
   comment**, in a package that deliberately holds no stream handle and imports no proto.
4. **"node-side `maxPlainTarBytes` follows via the existing spawnlet config path"** is false:
   `defaultFetcher` (a package func, not a method) bounds the compressed body against the
   **hardcoded const**, never `tarCap()` — a hidden 50 MiB gate would survive any config raise.

## 3. Key decisions

The staging budget (deadline + progress events + parallel fetch + a node-side content-addressed
cache) is now the **first** piece, because bundles break it. Re-presign triggers on the **parsed S3
error code**, not an HTTP status, and is **key-scoped to the spawn's own artifacts** (it is otherwise
a cross-tenant presign oracle). The resume-gating fold is **demoted to spike-gated**.

## 4. Design

### 4.1 Staging budget: deadline, progress, parallelism, cache *(blocker fix — bundles break the happy path)*

- **Per-attempt HTTP timeout** reduced to fit the budget (from 5 min) and stated explicitly.
- **Aggregate staging deadline** across all artifacts of a spawn (not per-artifact), enforced in
  `Materialize`.
- **A progress event per artifact**, emitted from inside `Materialize`. This is the load-bearing
  one: without it the CP's no-progress stall timer fires regardless of how the retries are tuned.
- **Parallel fetch** (bounded, e.g. 4 concurrent) instead of the serial loop — a 20-member bundle is
  20 serial GETs today.
- **Node-local sha-keyed artifact cache.** Object keys are immutable content hashes, so caching is
  trivially correct. `Materialize` currently wipes staging and re-downloads **every** artifact on
  **every** start; bundles multiply per-spawn Garage reads 20–60×. Cache hits also make a Garage
  brownout survivable for a re-start of the same content.
- **Retry budget** (after the above): retryable → 3 jittered attempts within the aggregate deadline;
  terminal → immediate.

### 4.2 Consume the taxonomy — and trigger re-presign on the S3 error code, not the status

`FetchError.Terminal` becomes load-bearing (it is set correctly today; only consumption is new):

- **Expired presign** — detected by parsing the **S3 error `Code` from the XML body**
  (`InvalidRequest` / "Date is too old"), *not* by HTTP status → one re-presign round (§4.3), then
  the retry budget.
- **`AccessDenied` / `SignatureDoesNotMatch` (403)** → **terminal**, with a distinct message naming
  the likely cause (clock skew / endpoint or Host mismatch). Retrying a config fault is pointless.
- **404 / sha mismatch / over-cap** → terminal, immediate clean failure, user-visible message.
- **Transport (connection refused/reset, 5xx)** → retryable within the budget; exhaustion surfaces
  as "Garage unreachable after N attempts".

**Spike (gates this section):** with `just garage`, presign with a 1 s TTL, sleep, `curl -i`; repeat
with a mangled signature. Record the exact status + XML `Code` for each. Kill criterion: if expiry is
not distinguishable from a signature fault by code, the trigger must be redesigned.

**Do not leak the presigned URL into the spawn error.** `url.Error.Error()` embeds the full URL
including `X-Amz-Signature`, and the chain `manager.go:1251 → attach.go:709 → server.go:1461
SetError` **persists it to the DB and the web UI** — a 30-minute bearer capability in plain sight.
The user-facing message is formatted from `FetchError.msg` **alone**, never `.Error()`/`Unwrap()`.
*(This may already be a live bug in shipped code — file it separately.)*

### 4.3 Re-presign over the Attach stream

Message pair on the existing node↔CP Attach stream:
`RepresignArtifactsRequest{request_id, spawn_id, generation, object_keys[]}` (node→CP) and
`RepresignArtifactsResponse{request_id, object_key → presigned_url}` (CP→node).

Details the earlier draft omitted:

- This would be the **first node-initiated request/response** on Attach (every existing node→CP
  variant is an unsolicited event or an ack). It therefore needs: `request_id` + `generation` on
  both messages (the "generation-fenced" claim was made without carrying a generation), a node-side
  **pending-request map**, a CP-side spawn→node ownership resolver, a **response timeout**, and
  defined behavior on **Attach reconnect mid-fetch**.
- **Key-scoping (security, one line, do not omit):** the CP **intersects the requested `object_keys`
  with the spawn's persisted artifact rows** and rejects any key not in that set. Without it, the
  only check is "does the spawn belong to this node" — and since the skills bucket is one global
  content-addressed namespace with **guessable keys** (anyone who can fetch the same public repo
  reproduces the sha), any untrusted self-hosted node with one live spawn could have the CP mint
  30-minute bearer GETs for **arbitrary** skill objects.
- `object_keys[]` length and call rate are bounded.
- The **denylist from `sp-mwco.3` §4.2 is consulted here too** — a revoked sha is never re-presigned.
- The node-side seam is a real cross-layer callback + response demux (the package holds no stream
  handle today), not a one-line hook at a comment.

### 4.4 CP-side HEAD before presign

`StatObject` per **distinct** object key at StartSpawn, failing early with `FailedPrecondition
"skill object missing"` rather than letting the node discover it.

- **`StatObject` is not on the `SkillStore` interface** (`PutIfAbsent` + `PresignedGet` only) — the
  interface and its fake must change. It was costed as "free"; it is not.
- **No `>8` carve-out.** The earlier draft switched the gate off above 8 by-ref artifacts — i.e. for
  every bundle, the flagship case. If the node-side 404 backstop is adequate for bundles it is
  adequate for 1–8. Objects are immutable and content-addressed, so make the HEADs **parallel** and
  memoize a "known-present" sha set; then apply the gate uniformly, or drop it entirely and rely on
  §4.2's node-side contract. (The spec keeps it, parallel + memoized.)
- It gets its **own timeout** and a **transport-vs-missing** error split — a Garage brownout must
  report `Unavailable`, not mass-report "skill object missing".
- **Interaction with §4.5:** §4.5's resume-gating spike was answered KILLED — re-materialize is
  never skipped, on resume or otherwise, so this gate applies uniformly on every start including
  resume.

### 4.5 Resume gating (`sp-nrzf.3.14.8`) — SPIKE ANSWERED: KILLED, not implemented

The earlier draft *chose* to skip by-ref re-materialize on a delta-image resume. Demoted to
spike-gated (below), because it rested on an unrun assumption and, if implemented naively, **breaks
every resume**:

- The premise "a resume's delta image contains the agent home" came from parent-spec spike **S4,
  which was never run** — and `sp-mwco.2` simultaneously moves the install target to
  `~/.agents/skills`, which may not be in the delta at all.
- Naive gating breaks the staging/manifest contract: `Materialize` wipes and recreates staging every
  start, `manifest.json` is an **inline** artifact still delivered every start, and the launcher
  re-runs `apply-artifacts` every launch → a gated resume hands `agentinstall` a manifest listing
  payload dirs that were never fetched → `StatusFailed` ("skill source directory does not exist") for
  **every skill, on every resume**. Worse, `installSkillTree`'s unconditional `RemoveAll(dest)` would
  **delete the delta-image copy** it was supposed to reuse.
- Therefore, if the spike said gating was viable, it would have needed to suppress **the manifest
  artifact and the apply phase together** (or skip the staging wipe), state whether the decision is
  CP- or node-side, and reconcile stale skill trees already baked into existing delta images.

**The spike ran (sp-mwco.2.2) and the kill criterion fired.** Two probes, both at the real
same-node-resume mechanism (`Manager.Suspend` + same-spawn-ID `CreateWithSelection`, `EnsureImage`
preferring `DeltaTag(id)` — a strictly stronger probe than the CP+Garage variant originally sketched,
since it needs no CP/Garage at all):

| Probe | Outcome |
|---|---|
| D1 image-level (`CaptureDeltaAs`, the fork primitive — does not stop the source) | both `~/.claude/skills` and `~/.agents/skills` captured intact in the delta image, mode 0700, content byte-identical |
| D2 lifecycle, `DeltaCapture=true` | BOTH trees survive the suspend+relaunch cycle |
| D2 lifecycle, `DeltaCapture=false` (**verified production default**) | **NEITHER tree survives** |

`DeltaCapture=false` is the production default: `cmd/spawnlet/config.go` registers **no default** for
`delta.capture` (only the `DELTA_CAPTURE` env binding), nothing under `deploy/` sets it, and only
`Justfile:59` / `Justfile:223` (dev recipes) opt in. `internal/spawnlet/manager.go:1606` / `:2108` gate
capture on `m.cfg.DeltaCapture`.

**Conclusion: gating is unsound on the arm production actually runs, per the kill criterion this
section already specified. No gating code was written.** By-ref `Materialize` runs on **every** start
— first-create and resume alike, unconditionally, exactly as it does today (`internal/spawnlet/
artifacts.go:341`/`:1261` in `manager.go`). §4.3's re-presign is load-bearing, as predicted. What
*does* make a same-node resume survive a Garage brownout is **not** gating but §4.1's node-local
sha-keyed cache (`ArtifactStager.CacheDir`, wired at `manager.go:348`, node-local and per-spawn-
independent, so it survives suspend/resume and a spawnlet restart): `stageByRef` tries `cacheLoad`
first and skips the GET entirely on a hit. The residual Garage dependency on resume therefore moved
**CP-side** — `statSkillObjects` (§4.4) re-stats and re-presigns on every start; only a sha already
memoized in `s.skillPresent` from an earlier stat on a warm CP tolerates a transport failure. It did
not vanish.

Tests: `internal/spawnlet/artifacts_resume_test.go` (`TestMaterialize_ResumeReStagesEveryByRefArtifact`
pins the anti-gating invariant — verified to go red under a scratch naive gate;
`TestMaterialize_ResumeServedFromShaCacheWhenFetchFails` pins the cache-not-gating survival property)
and `internal/cp/artifacts_resume_test.go` (`TestNodeArtifactsForStart_StatsAndPresignsOnEveryCall`,
`TestStatSkillObjects_MemoizedShaSurvivesTransportFailure`).

If a node ever opts into `DeltaCapture=true`, gating becomes conditionally viable again, but would
still need to separately close the three items above (manifest-as-inline-artifact, `installSkillTree`'s
unconditional `RemoveAll`, the staging-dir wipe) — none of which this bead touched, since the premise
for needing them (gating being live) did not hold. Do not treat `DeltaCapture=true` as the target
state to design for; nothing under `deploy/` sets it today.

**Original spike (recorded here for history):** create → suspend → **stop Garage** → resume → `ls`
the install path + read `apply-report.json`. Kill criterion: skill absent, or home not in the delta →
gating is unsound; by-ref materialize stays on every start and re-presign (§4.3) becomes load-bearing.
This is exactly the result the sp-mwco.2.2 D1/D2 probes above produced on the production
(`DeltaCapture=false`) arm.

### 4.6 Config surface for caps

Move `WireCapBytes`, `DecompressedCapBytes`, `PlainTarCapBytes`, `FileCountCap`, `HTTPTimeout` into
the `skills.*` config block (koanf + env, `SKILLS_*`), defaults = current values.

- **Fix `defaultFetcher`** to consult `tarCap()` rather than the hardcoded const (§2.4), or the
  node keeps a hidden 50 MiB gate.
- **The cap must be on the wire.** Otherwise it is two independently-deployed sources of truth: raise
  it CP-side (as bundles invite) without redeploying every self-hosted node and ingest succeeds while
  the node fetch fails terminally. Carry the effective cap in the `ArtifactSpec`, or have the node
  advertise its cap at registration and the CP refuse to schedule above it.
- Any cap raise is bounded by `sp-mwco.1` §4.2's CP memory budget (~3× decompressed per in-flight
  ingest) and its concurrent-ingest semaphore.

**The host allowlist is NOT config-surfaced** (reversing the earlier draft). It cannot enable ingest
from a new host at all — `ParseRepoURL` rejects non-github.com and `tarballURL` is hardcoded — so it
only widens the set of hosts a **redirect chain may leave GitHub for**. As a config knob it is inert
for its stated purpose and a pure security downgrade (the "append-only, so a typo can't disable
containment" rationale defends the wrong failure mode: here **addition** is the risk). It stays a
code constant. A test asserts a config-added host cannot originate a fetch.

### 4.7 Quota backoff

The quota rejection message gains the window reset time (`retry after ~Nm`), mirroring the GitHub 429
handling. Note `sp-mwco.1` §4.8 adds the **CP-wide** refetch budget that the per-owner quota does not
provide.

### 4.8 S5 — measure the right thing

The presign TTL must cover the **last** artifact's GET, not the first: all URLs are minted at
StartSpawn and (today) fetched serially. Restate S5 as **"StartSpawn → *last* artifact GET, p99, on
an N-member bundle, cold image cache"**, run it in the VM e2e lane, record the number here, and
assert TTL headroom against *that*. §4.1's parallel fetch shrinks it; §4.3's re-presign is the
backstop.

**Implementation status (sp-mwco.4.6):** the node now emits the exact StartSpawn → last-GET
duration as `slog.Info("artifacts: by-ref staging complete", "count", N, "elapsed", d)` from
`materializeByRef` (`internal/spawnlet/artifacts.go`), and
`acceptance/tests/customization/skill-staging-s5.spec.ts` measures the create → ACTIVE upper bound
(an upper bound on StartSpawn → last-GET: ACTIVE also waits out post-staging agent-install/health
work) over R=5 iterations on a configurable K-member bundle, asserting `max < PresignTTL/3`. **The
VM-lane run itself has not been executed** — it needs a provisioned e2e VM with real GitHub egress
(see `docs/e2e-vm-testing.md`) plus a curated set of small public GitHub skill repos
(`ACC_SKILL_SOURCE_REPOS`), neither of which was available in this implementation session. **This
gap — no measured number in this section — is tracked as bd `sp-mwco.4.6.1`** (run the VM lane, or
its `just dev` + `just garage` fallback per the plan's Step 8, and record the number here); it is
not fabricated in its absence.

### 4.9 Testing

Hermetic: taxonomy consumption (retryable exhaust / expired-code → re-presign → success /
`AccessDenied` terminal / 404 terminal); re-presign fencing (wrong node, wrong generation, **key not
in the spawn's artifact set** → rejected); denylist blocks re-presign; presigned URL absent from the
spawn error string; aggregate deadline + per-artifact progress events; cache hit skips the GET; config
plumb-through incl. the wire cap; allowlist cannot originate a fetch. E2e: 1 s-TTL expiry recovered
via re-presign; VM-lane S5 on a bundle.

## 5. Out of scope

Garage object GC (revocation is `sp-mwco.3`'s denylist); by-ref for sensitive artifacts; non-GitHub
hosts; a general log-scrubbing middleware (§4.2 fixes the specific leak that reaches the DB/UI).

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged
from the assumptions above — append a dated note here, whether or not a formal debugging skill was
used.*

- **2026-07-12 — roasted (BLOCK) and revised.** Blockers: the retry/timeout math was wrong by ~60×
  and emits no progress events, so bundles break the **happy path** (§4.1); re-presign triggered on
  403 while Garage returns **400/`InvalidRequest`** for an expired presign — dead on arrival (§4.2).
  False claims corrected: "hook at artifacts.go:191" (a doc comment, in a package with no stream
  handle) (§4.3); "node-side cap follows via config" (`defaultFetcher` uses the const) (§4.6);
  "StatObject is free" (not on the `SkillStore` interface) (§4.4). Majors folded: re-presign is a
  **cross-tenant presign oracle** without key-scoping to the spawn's own artifacts (§4.3);
  re-presign needs `request_id`/`generation`/pending-map/timeout/reconnect semantics (§4.3); the
  presigned URL **leaks into the persisted spawn error and the web UI** (§4.2); the HEAD gate was
  switched off exactly where needed (§4.4); resume gating would break **every** resume and is
  demoted to spike-gated (§4.5); the config allowlist is inert and a security downgrade — dropped
  (§4.6); S5 measured the first GET instead of the last (§4.8); node-side content-addressed cache
  added (§4.1).

- **2026-07-12 — sp-mwco.4.2 Phase 0 spike results** (live dev Garage, `internal/cp/skillstore/presign_expiry_spike_test.go`,
  build tag `garage_e2e`). Confirms expiry is cleanly distinguishable from a signature/config fault by
  the parsed S3 error `Code`+`Message` (never the bare HTTP status):

  | Case | HTTP | Code | Message |
  |---|---|---|---|
  | Expired presign (1s TTL, slept 2s) | 400 | `InvalidRequest` | `Bad request: Date is too old` |
  | Tampered `X-Amz-Signature` (fresh presign) | 403 | `AccessDenied` | `Forbidden: Invalid signature` |
  | Nonexistent key (fresh presign) | 404 | `NoSuchKey` | `Key not found` |

  Note Garage's `AccessDenied` message for a bad signature carries no "expired"/"too old" wording —
  the classifier's expiry markers (`InvalidRequest`+"too old", or `AccessDenied`+"expired") do not
  collide with the tampered-signature case. `classifyS3Error` (artifacts.go) implements this table.
- **2026-07-12 — sp-mwco.4.6 implemented §4.6/§4.7/§4.8.** (a) `WireCapBytes`/`DecompressedCapBytes`/
  `PlainTarCapBytes`/`FileCountCap`/`HTTPTimeout` moved from `skillfetch` package consts to
  `skills.*` config (koanf + `SKILLS_*` env aliases), defaults unchanged. `defaultFetcher` (the
  actual §2.4 roast finding) now takes an explicit `capBytes` parameter instead of closing over the
  hardcoded const. The effective plain-tar cap is carried on the wire via a new
  `node.v1.ObjectRef.max_plain_tar_bytes` field, stamped by the CP at `nodeArtifactsForStart` time
  (`Server.effectiveSkillPlainTarCap`) and consumed verbatim by the node (`ArtifactStager.capFor`) —
  no local min() against the node's own default, so a CP-side raise takes effect without a node
  redeploy. (b) Confirmed and pinned: the host allowlist (`fetch.go`'s `allowedHosts`) stays a code
  constant; `TestConfigCannotAddFetchOriginatingHost` (skillfetch/caps_test.go) asserts origination
  is fixed by `ParseRepoURL`/`tarballURL`, not by anything in `Config`. (c) `ingestQuota.allow` now
  returns the remaining window; the rejection message reads `retry after ~Nm`. (d) Node
  instrumentation for the corrected S5 metric landed (`materializeByRef`'s
  `"artifacts: by-ref staging complete"` slog line, elapsed since the first by-ref fetch dispatched
  — i.e. since StartSpawn began staging) plus an acceptance spec
  (`skill-staging-s5.spec.ts`) that measures create → ACTIVE (an upper bound) over 5 iterations on a
  configurable K-member bundle and asserts headroom against `PresignTTL/3`. **The VM-lane run was
  NOT executed in this implementation session** (no provisioned e2e VM + real GitHub egress + curated
  skill-repo fixture list available); §4.8's "record the number here" is therefore still open —
  running `GOLDEN_IMAGE=… scripts/e2e-vm/run.sh --profile fake` (or the local dev-stack fallback) and
  recording the observed max is filed as **bd `sp-mwco.4.6.1`**, a child of this bead, so it isn't
  silently lost.
- **2026-07-12 — sp-mwco.4.5: resume gating spike-answered, KILLED, no gating code shipped.** The
  sp-mwco.2.2 fork/suspend delta spike (D1 image-level + D2 lifecycle-level, same-node-resume
  mechanism) hit §4.5's own kill criterion on the production (`DeltaCapture=false`) arm — neither
  `~/.claude/skills` nor `~/.agents/skills` survives a suspend+relaunch without delta capture, and
  nothing in `deploy/` turns delta capture on. §4.5 was rewritten in place to record the D1/D2 result
  table and the conclusion (by-ref `Materialize` runs on every start; §4.1's node sha-cache, not
  gating, is what lets a same-node resume survive a Garage brownout; the residual Garage dependency on
  resume moved CP-side to `statSkillObjects`); §4.4's now-dangling "interaction with §4.5" bullet was
  reconciled to match. `internal/cp/lifecycle.go` and `internal/spawnlet/manager.go` are **untouched**
  — there was no gating code to add. Regression guards for the invariants naive gating would have
  broken landed as new tests: `internal/spawnlet/artifacts_resume_test.go`
  (`TestMaterialize_ResumeReStagesEveryByRefArtifact`, verified to go red under a scratch
  early-return-on-existing-staging-dir gate before being reverted;
  `TestMaterialize_ResumeServedFromShaCacheWhenFetchFails`) and `internal/cp/artifacts_resume_test.go`
  (`TestNodeArtifactsForStart_StatsAndPresignsOnEveryCall`,
  `TestStatSkillObjects_MemoizedShaSurvivesTransportFailure`).

  Two follow-ups surfaced by this work, **not filed as beads here** (the coordinator files them):
  (a) `DeltaCapture` has no registered default anywhere under `deploy/` — worth an explicit decision
  on whether silently discarding all agent-home state (not just skills) across every suspend/resume on
  every production node is intentional, or whether `deploy/` should set `delta.capture=true` and pay
  the resulting capture-time/storage cost; (b) resume-with-Garage-down is still blocked CP-side by
  `statSkillObjects` on a COLD CP (nothing memoized yet) even when every node already has the objects
  cached locally — worth considering persisting the present-sha set across CP restarts, or downgrading
  a transport-only stat failure to a warning on the resume path specifically, given the node's 404
  backstop (§4.4's own fallback reasoning) already exists as a second line of defense.
