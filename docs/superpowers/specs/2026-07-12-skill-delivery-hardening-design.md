# Skill Delivery Hardening — Re-Presign, Live Error Taxonomy, Config Surface

**Date:** 2026-07-12
**Status:** draft
**Epic:** `sp-mwco.4` *(Mode B one-shot spec — decisions made solo from the 2026-07-11 audit; review before implementation)*
**Extends:** [Profile Skill Ingestion from a GitHub URL](2026-06-22-profile-skill-url-ingestion-design.md) §4.6/§4.9/§4.11

## 1. Problem

The parent spec mandated four pieces that shipped incomplete. (a) The node→CP **re-presign RPC**
does not exist — the 30-minute presign TTL is the only defense against expiry on a cold start, and
spike S5 (cold-start StartSpawn→first-GET latency) never ran, so that TTL is unvalidated.
(b) The node's **error taxonomy is built but dead**: `spawnlet/artifacts.go` correctly classifies
404 → terminal `skill object missing` vs transport → retryable `Garage unreachable` vs 403/5xx →
retryable, but nothing consumes `FetchError.Terminal` — no retry happens on retryable, no clean
terminal fail on terminal; both bubble up as a generic create failure. (c) The **size caps and
host allowlist are hardcoded** package consts despite §4.11 listing them as config-surfaced.
(d) The per-owner ingest **quota has no backoff guidance**. Related: `sp-nrzf.3.14.8` (resume
hard-depends on Garage even with a delta image) belongs to this hardening theme.

## 2. Main challenges

The node↔CP channel is a single `Attach` bidi stream (`proto/node/v1/node.proto`) — re-presign
must ride it without a new dialing surface. Retry logic must respect the CP-side suspend/create
stall windows (a node retrying for minutes while the CP's 30s stall window expires helps nobody).

## 3. Key decisions

Re-presign becomes a message pair on the existing Attach stream. The node consumes its own
taxonomy: bounded in-node retries for retryable, immediate clean failure for terminal, re-presign
on 403. Caps/allowlist move into the skills config block with current values as defaults. S5 runs
in the VM e2e lane and its measurement is recorded here. `sp-nrzf.3.14.8` is folded into this
epic.

## 4. Decision points, by section

### 4.1 Re-presign over the Attach stream

**Chosen:** two new messages on the existing node↔CP Attach stream:
`RepresignArtifactsRequest{spawn_id, object_keys[]}` (node→CP) and
`RepresignArtifactsResponse{object_key → presigned_url}` (CP→node). CP handler re-presigns via the
existing `skillStore.PresignedGet` (same TTL, same node-endpoint client) after checking the spawn
belongs to that node (generation-fenced like other node messages). The node hooks it exactly where
the placeholder comment sits (`artifacts.go:191`): on `403` — and only 403 — request fresh URLs
once, then resume the bounded retry loop. **Discarded:** a new unary CP RPC callable by nodes
(nodes deliberately have no CP-facing unary auth surface; the stream already carries fenced
node-scoped messages); persisting longer-lived URLs (bearer-capability lifetime creep, rejected in
the parent spec).

### 4.2 Consume the taxonomy (make `FetchError.Terminal` load-bearing)

**Chosen:** in the node's materialize path: **retryable** (connection refused/reset, 5xx) → up to
3 attempts with jittered backoff (~1s/3s/9s, ceiling well under the CP stall window); **403** →
one re-presign round (§4.1) then the retry budget; **terminal** (404, sha mismatch, over-cap) →
fail immediately, no retries. `manager.go` stops wrapping generically: a terminal artifact error
fails the create/resume with the taxonomy's message surfaced to the spawn error (user-visible
"skill object missing (404): skills/<sha>.tar.zst"), and a retryable exhaustion surfaces as
"Garage unreachable after 3 attempts". Hermetic tests fake the fetcher and assert the branch
behavior — the audit showed the flag is set correctly; only consumption is new. **Discarded:**
CP-driven retries (the CP can't distinguish node-side transport errors; the node owns the fetch);
unbounded retry (wedges create under the stall window).

### 4.3 Optional CP-side HEAD before presign

**Chosen (cheap, included):** `StatObject` per distinct object key at StartSpawn presign time,
failing the start with `FailedPrecondition "skill object missing"` before scheduling the node —
the parent spec deferred this "unless free"; with the minio client and keys already in hand it is
one call per skill, and it converts a late node-side terminal failure into an early, clearer CP
error. Bounded: skipped when a spawn has >8 by-ref artifacts (bundles) to avoid serial HEAD storms
— the node-side 404 contract (§4.2) remains the backstop. **Discarded:** always-HEAD (latency
scales with bundle size for an error that is rare).

### 4.4 Config surface for caps + allowlist

**Chosen:** move `WireCapBytes`, `DecompressedCapBytes`, `PlainTarCapBytes`, `FileCountCap`,
`HTTPTimeout`, and the fetch host allowlist from consts/vars (`skillfetch.go:29-40`,
`fetch.go:18-22`) into the `skills.*` config block (koanf + env, matching the existing
`SKILLS_*` names), defaults = current values. The allowlist stays append-only semantics-wise:
config *extends* known-good GitHub hosts rather than replacing them, so a typo cannot silently
disable SSRF containment. Node-side `maxPlainTarBytes` follows via the existing spawnlet config
path so the two ends agree. **Discarded:** replacing the allowlist wholesale from config
(footgun); leaving caps hardcoded (spec debt, and bundles change the size calculus).

### 4.5 Quota backoff

**Chosen:** the quota rejection message gains the window reset time (`retry after ~Nm`), mirroring
the GitHub 429 handling; the in-memory counter is otherwise unchanged. **Discarded:** token-bucket
smoothing / persistent quota state (no evidence MVP needs it).

### 4.6 S5 measurement + `sp-nrzf.3.14.8`

**Chosen:** run S5 in the VM e2e lane (cold image cache: StartSpawn→first Garage GET, p99),
record the number in this spec's notes, and assert TTL headroom (>10× p99) in the lane test.
Fold `sp-nrzf.3.14.8` here: when a resume has a delta image containing the agent home, skip
re-materialize of by-ref skill artifacts (first-create-only gating per parent spec §5/S4) — with
the re-presign RPC as the fallback when gating is off. **Discarded:** raising the TTL blindly
(the re-presign RPC is the correct long-tail fix).

### 4.7 Testing

Hermetic: taxonomy consumption branches (retryable exhaust / 403→re-presign→success / terminal
immediate); re-presign handler fencing (wrong node/generation rejected); config plumb-through
(caps + allowlist extension); HEAD-gate skip at >8 refs. E2e (existing lanes): 403-expiry
simulation (presign with 1s TTL override, node recovers via re-presign); VM lane S5 measurement.

## 5. Out of scope

GC/refcounting of Garage objects; by-ref for sensitive artifacts; non-GitHub hosts (the allowlist
extension mechanism exists, but vetting new hosts is a product decision); log-scrubbing middleware
for presigned URLs (today's by-omission redaction stands; noted as accepted risk).

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged
from the assumptions above — append a dated note here, whether or not a formal debugging skill was
used.*
