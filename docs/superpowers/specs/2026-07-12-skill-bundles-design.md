# Skill Bundles — Multi-Skill Repo Ingestion (versioned, pinned attach)

**Date:** 2026-07-12 (revised 2026-07-12 after roast BLOCK)
**Status:** draft (roasted → BLOCK → revised)
**Epic:** `sp-mwco.1`
**Extends:** [Profile Skill Ingestion from a GitHub URL](2026-06-22-profile-skill-url-ingestion-design.md) (`sp-nrzf.3.14`),
[Profiles — The Customization Tool (v2)](2026-06-14-profiles-customization-tool-design.md) (`sp-nrzf.3`)
**Siblings:** [All-Agent Skill Install](2026-07-12-all-agent-skill-install-design.md) (`sp-mwco.2`),
[Skill Catalog Lifecycle](2026-07-12-skill-catalog-lifecycle-design.md) (`sp-mwco.3`),
[Skill Delivery Hardening](2026-07-12-skill-delivery-hardening-design.md) (`sp-mwco.4`)

## 1. Problem

URL skill ingestion (sp-nrzf.3.14) handles exactly one skill per call: it requires a top-level
`SKILL.md` after wrapper-strip + subdir descent (`internal/cp/skillfetch/skillfetch.go:217-231`).
A superpowers-style repo (`skills/*/SKILL.md`, no root `SKILL.md`) fails outright; the only
per-skill workaround is one `IngestSkillFromURL` call per subdir (~20 dialogs for superpowers),
each producing an unrelated catalog row that must be attached one at a time.

**The motivating repo cannot be ingested at all today** (roast blocker, verified live against the
real tarball): `obra/superpowers` contains a top-level `AGENTS.md` as a **symlink** (mode 120000 →
`CLAUDE.md`), and `skillfetch/fetch.go:214-226` hard-rejects any symlink/hardlink entry **before**
wrapper-strip and subdir filtering. The reject therefore aborts the whole fetch regardless of which
subdir is requested — so the bundle walk **and** the per-subdir workaround both fail. Fixing the
tar-entry policy (§4.2) is task one of this epic; nothing else in the family works without it.

The goal: paste one URL, get the whole bundle into a profile, and have every member skill land in
the spawn's agent harness. Bundles are versioned so upstream changes are picked up deliberately,
never silently.

## 2. Main challenges

A bundle must not regress the per-skill properties the parent spec fought for: content-addressed
per-skill dedup, the duplicate-skill-dir-name rejection, the 64-artifact cap, and by-ref delivery.
It must not create a silent-update channel. And — the roast's central lesson — **it must not assume
the existing pipeline already does things it does not do**: the tar policy fails closed on
symlinks, the caps bind on the *whole repo tarball* (not per skill), one `ProfileEntry` cannot
today emit N artifacts, and bundle members are invisible to the catalog kill-switch. Each is
scoped explicitly below.

## 3. Key decisions (user-pinned)

1. **Bundle-of-one, always.** *Every* URL ingest creates a bundle with N ≥ 1 members — including a
   single-skill repo. One stored, versioned, pinned update model across the whole feature; no
   second derived-lineage model (this supersedes the earlier split with `sp-mwco.3`, whose derived
   lineage is deleted).
2. **Bundle = N per-skill catalog rows + a versioned grouping entity** — each member is a
   content-addressed catalog row (per-skill dedup and by-ref delivery unchanged).
3. **Discovery = recursive walk, prune at skill dirs** — any directory containing `SKILL.md` is a
   skill; its subtree belongs to it (no deeper descent).
4. **Profiles pin a version** — `(bundle_id, version_id)` captured at attach. Re-ingestion cuts a
   new version; **no profile changes until the user explicitly re-pins, and only after seeing a
   content diff** (§4.9).
5. **Manual re-ingest only in MVP** (schema versioned from day one; a poller is a later addition).
6. **Skill content is untrusted third-party instruction** — the family gets a real (if minimal)
   content-trust posture: commit pinning, diff-on-update, and frontmatter sanitization (§4.9).

## 4. Design

### 4.1 Tar-entry policy — skip non-regular entries, do not fail closed *(blocker fix)*

`skillfetch` today errors on `tar.TypeSymlink`/`TypeLink` (`fetch.go:214-226`), which makes the
flagship repo unfetchable. Change the policy to **skip-with-report**, mirroring the pattern
`internal/agentinstall/skill.go:146-178` (`copyTree`) already uses:

- Non-regular entries (symlink, hardlink, device, fifo, socket) are **skipped**, not fetched, and
  their paths are collected into a `SkippedEntries []string` on the fetch result.
- Skipped entries are surfaced in the ingest response and in the web ingest result ("3 entries
  skipped: AGENTS.md (symlink), …") so a skill that *depends* on a symlink fails visibly at use
  rather than mysteriously at ingest.
- The safety properties that actually matter are unchanged and stay fail-closed: absolute paths and
  `..` escapes still **reject** the whole fetch (`safeRelPath`), because those are attacks, not
  repo hygiene. A symlink is neither followed nor written — skipping is strictly safer than the
  status quo *and* strictly more useful.
- PAX/global headers continue to be skipped (already fixed in `afcfb4c`).

**Hermetic test:** a fixture tarball containing a top-level symlink ingests successfully with the
symlink reported as skipped; a fixture containing `../escape` still hard-rejects.

### 4.2 Discovery

Runs on the fetched entry list inside `skillfetch` (SSRF/streaming-cap machinery reused; the
tar-entry policy is the one change, §4.1):

- Walk the (wrapper-stripped, subdir-descended) tree. Any directory containing `SKILL.md` is a
  skill root; its entire subtree belongs to that skill; do not descend past it.
- **Root `SKILL.md`**: the repo is a single skill — but **still walk the rest of the tree**. Nested
  `SKILL.md`s below a root skill are reported as a warning listing them ("this repo has a root
  SKILL.md and 12 nested skills; ingesting the root skill only — set `subdir` to ingest a nested
  one"). The earlier "resolved by construction" claim was wrong: silently packing nested skills
  into one opaque blob is the very corruption §1 named.
- Per-skill name: `SKILL.md` frontmatter `name` (sanitized), fallback = skill dir basename.
- **Per-skill repack rebases paths** so `SKILL.md` sits at the tar root — `agentinstall` stats
  `SKILL.md` at the source-dir root, so an unrebased `skills/foo/SKILL.md` prefix fails install.
  This repack **MUST be byte-identical to a `subdir=<member path>` ingest of the same directory**,
  or the same skill gets two different sha256s and the `(owner, sha256)` dedup the whole design
  rests on stops firing. **Hermetic test:** `sha(bundle member) == sha(standalone subdir ingest)`.
- Fail-loud at ingest on: duplicate skill **names** within the bundle; duplicate skill **content**
  under different directory names (two byte-identical skill dirs would otherwise collapse into one
  catalog row under the first one's name and one member would silently vanish); per-skill plain-tar
  cap (existing 50 MiB).

**The real caps (correcting a dead-text error).** The reused pipeline enforces, *streaming, on the
whole repo tarball before any per-skill split*: **20 MiB wire, 50 MiB decompressed, 10 000 tar
entries** (`skillfetch.go:29-40`). The earlier "200 MiB total plain tar" bundle cap was
unreachable. Consequences that must be stated, not discovered:

- **`FileCountCap` (10 000) is the binding limit**, not size: ~100 skills × ~100 files each hits it
  first. The error must name the cap and suggest `subdir` scoping.
- A repo hosting skills *beside a large codebase* is unfetchable even when its skills are tiny —
  the exact "paste one repo URL" case. Accepted for MVP; the mitigation is `subdir`, and the error
  message must say so. Raising the caps is `sp-mwco.4`'s config work (§4.4 there), bounded by the
  memory note below.
- **Bundle member cap = 63**, not 100: a spawn is capped at `maxArtifactsPerSpawn` = 64 *including*
  the inline `manifest.json`. A 64+-member bundle would ingest (permanently uploading never-GC'd
  Garage objects) and then fail **every** `CreateSpawn`. The cap is enforced **at ingest**, not
  only at assembly.
- **Memory**: whole-repo ingest is fully buffered in CP memory (~3× the decompressed cap per
  in-flight ingest: entry list + plain tar + zstd buffer, all live at `EncodeAll`), and the 20/hr
  per-owner quota does not bound concurrency. Add a **concurrent-ingest semaphore** (max 4
  in-flight, CP-wide). Any future cap raise is bounded by this budget.

### 4.3 Entities & schema

Two new tables (sqlite + pg migrations):

- **`skill_bundle`** — `bundle_id, creator_id, name, source_url, source_ref, source_subdir,
  created_at, updated_at`. Unique on `(creator_id, source_url, source_ref, source_subdir)`.
  **`source_ref`/`source_subdir` are `NOT NULL DEFAULT ''`** — the existing catalog columns persist
  `NULL` in the common case (`nullableString`, `ingest_skill.go:171-176`), and NULLs are distinct
  in a unique index and never `=` in a predicate, so a NULL-bearing key would silently never match:
  re-pasting a URL would mint a new bundle every time. A migration backfills existing NULLs to `''`.
- **`skill_bundle_version`** — `version_id, bundle_id, seq (monotonic per bundle), source_commit,
  created_at`, plus member join table **`skill_bundle_member (version_id, catalog_id,
  source_subdir, position)`**, unique on **`(version_id, source_subdir)`** (the member's directory
  within the repo is its stable identity; keying on `catalog_id` alone would let two identical
  skill dirs collapse and would make an upstream *rename* undetectable).

**Member rows are a distinct kind, not "listed=0 catalog rows".** The catalog gains a
`bundle_member BOOLEAN NOT NULL DEFAULT false` column. This decouples membership from `Listed`,
which is load-bearing: `listed=false` **means revoked** (it drives the kill-switch), and content
identity `(owner, sha256)` carries neither a name nor a listing flag. Without the split, reusing a
previously-standalone skill as a member would either leave it listed (defeating the UI goal) or
flip it to `listed=0` — i.e. fire a **revoke that kills the user's running spawns**. Rule: **a
content-identity hit NEVER mutates `Listed`, `Name`, or `SourceSubdir` of the existing row.**

**Version-cut rule:** re-ingest fetches, discovers, and repacks each skill separately. A new
version is cut iff the member set (by `source_subdir`) or any member sha changed — compared against
the **`source_commit` + member shas**, never `created_at` (see §4.9). Unchanged → no new version
(idempotent). Changed → a new version reusing unchanged member rows, creating catalog rows only for
new/changed skills.

**Listing default (security).** URL-ingested rows (members and single skills alike) are created
**`listed=false`** — creator-visible only. The catalog `List()` has no tenant filter, so today's
`listed=true` default would offer a skill pasted from a random repo in **every other tenant's**
picker (×100 per bundle paste). **Publishing to the global catalog is an admin-only action**
(`sp-mwco.3` §4.6).

### 4.4 Profile attachment

`ProfileEntry` gains a **`bundle_ref`** source alongside `catalog_ref`/custom, storing
`(bundle_id, version_id)` **pinned at attach**. `ProfileEntry.CatalogID` (a single NOT NULL column
today) is **not** reused for this — bundle_ref entries carry their own `BundleID`/`VersionID`
columns and leave `CatalogID` empty; the store's NOT NULL constraint is relaxed accordingly in the
migration.

- Entry-cap accounting: a bundle_ref counts as **its member count** against
  `maxArtifactsPerSpawn`, checked at attach (UX) and authoritatively at assembly.
- **Per-member `exclude` and `rename` overrides** live on the bundle_ref entry. Without them a
  duplicate skill-dir name between a bundle member and any other profile entry would hard-fail the
  whole spawn with no repair path (member names come from upstream frontmatter on rows the user
  cannot edit). A collision is a **warn-and-rename** at attach, never a spawn-creation abort.
- "Update available" badge when the bundle's latest `seq` exceeds the pinned version's, with an
  explicit re-pin action **gated on the diff view** (§4.9). The badge's freshness comes from a
  cheap `MAX(seq)` per bundle joined at profile-read time — it does **not** poll GitHub.

### 4.5 Assembly & spawn binding *(blocker fix: N artifacts from one entry)*

**The naive claim "emits N by-ref ArtifactSpecs exactly as if they were individual catalog refs"
is unbuildable**: `profiles_assembly.go:139-158` derives an artifact's `Id`, `DestPath`, and
manifest `Name` from a **single** `ProfileEntry`. One bundle_ref is one entry, so all N members
would collide on one payload dir — and `unpackTar` does not wipe its destination, so the payloads
would **silently merge**. The design therefore specifies **synthetic per-member artifact identity**:

- `Artifact.Id` and `DestPath` = `<entry_id>/<catalog_id>` (unique per member; `skill_bundle_member`
  supplies both `catalog_id` and `position`).
- `Artifact.Name` = the member's own catalog `Name` (post-rename-override) — this is the on-disk
  skill dir name, and it is what the duplicate-name check must run over.
- The duplicate-dir-name check runs over the **fully expanded** set (bundle members + standalone
  entries), on the **post-dedup catalog Names** — the same values assembly will use. (Today §4.2's
  ingest-time check and assembly's check run at different times on different values, so a colliding
  bundle passes ingest cleanly and explodes at spawn creation.)
- Payload destinations are guaranteed disjoint by construction; assembly asserts it.

Spawn binding then falls out of the existing substrate: the spawn's persisted artifact rows
(object_key + sha per member) **are** the version binding, and resume/fork already replay from those
rows.

**Kill-switch must see bundle members** *(blocker fix)*: `resolveAffectedSpawns` walks
`catalog_ref` profile entries only, so revoking or de-listing a skill delivered via a bundle would
terminate **zero** spawns — silently voiding the revoke guarantee shipped in `sp-nrzf.3.9`. Revoke/
de-list must resolve affected spawns through **bundle-version membership → bundle_ref profile
entries** as well as catalog_ref.

**Layout change on re-ingest**: if a re-ingest no longer discovers a member set (upstream flipped
between bundle and single-skill layout, or dropped all skills), it fails loud
(`FailedPrecondition: upstream layout changed`) and leaves the pinned version intact. A bundle never
silently changes entity kind.

### 4.6 RPC surface

- `IngestSkillFromURL` request unchanged; response gains `bundle_id`, `version_id`, member
  `catalog_id`s, `skipped_entries`, and `warnings` (nested-skill warning, cap hints). Every ingest
  now returns a bundle (bundle-of-one).
- `ReingestBundle(bundle_id) → {version_id, changed, added/updated/removed, diff_token}`.
- `ListBundles`, `GetBundle(bundle_id)` (versions + members + provenance).
- `GetBundleDiff(bundle_id, from_version, to_version)` → per-member added/removed/changed with
  **SKILL.md body diffs** (§4.9).

All additive; no breaking proto changes.

### 4.7 Web UI

Same ingest dialog; the result lists discovered members, skipped entries, and warnings. A bundle is
one catalog card (expandable members, provenance incl. **source commit**) and one profile-entry chip
(member count + pinned version + per-member exclude/rename). "Check for updates" → `ReingestBundle`
→ if changed, the **diff view** (§4.9) is the gate before re-pin.

### 4.8 Rate budget for update checks

"Check for updates" refetches a whole tarball per bundle per user against GitHub's **~60 req/hr per
source IP, shared across all tenants** (the CP egresses from one IP; ETag/304 was deferred by the
parent spec). The per-owner 20/hr quota does not bound that shared budget. MVP therefore: update
checks are **ETag/`304`-conditional** (store the ETag on `skill_bundle`; a 304 costs no body and
short-circuits to "up to date"), and a **CP-wide refetch budget** guards the shared limit. A
configured `GITHUB_TOKEN` raises it to 5000/hr and is strongly recommended for any deployment
enabling update checks.

### 4.9 Threat model — skill content is untrusted third-party instruction

Every control shipped so far is **transport-level** (SSRF, tar-bomb, symlink, sha256, caps). Nothing
addresses what the *content* does. These specs multiply that exposure ~100× (one paste → 100
third-party `SKILL.md`) and 6× (one canonical dir → six harnesses, `sp-mwco.2`) into agents holding
the user's workspace and git credentials. MVP posture (the rest is explicitly accepted risk):

- **Commit pinning.** GitHub hands the CP the exact commit for free in the tarball wrapper dir
  (`owner-repo-<sha>/`), which `fetch.go:228-238` currently parses **and discards**. Persist it as
  `source_commit` on catalog rows *and* `skill_bundle_version`. Refs default to a mutable branch
  head, so without this a force-push or tag-move is undetectable and incident response is
  impossible. **Cost: one string.** Version-cut and "is this current" comparisons key on it (never
  `created_at`).
- **Diff-on-update.** A re-pin must show **what changed in the instructions** — changed `SKILL.md`
  bodies, added/removed members, old→new commit — before it commits. A one-click un-diffed update
  channel with a nagging badge is the same supply-chain outcome as the silent-update channel §3
  rejects, and it cannot distinguish an upstream advance from a compromise or a rollback.
- **Frontmatter sanitization.** `description` is today never parsed, bounded, or sanitized — yet
  Claude loads **every installed skill's description into the system prompt at startup**. One
  pasted bundle = up to 63 unbounded attacker-controlled strings in every spawn's system prompt,
  before any skill is ever invoked. Parse it, cap it (1 KiB), strip XML/tag markup and
  reserved/role words; apply length+charset caps to `name` too (`sanitizeName` does neither today).
  A bundle member's description comes from its own `SKILL.md` frontmatter.
- **Name-squat guard** (with `sp-mwco.2`): reject or namespace an ingested skill whose name collides
  with a skill the image or harness already ships — install is an unconditional clobber-by-name
  (`agentinstall/skill.go:78-113`) into a namespace shared by all six harnesses, and the name is
  attacker-influenceable via frontmatter.
- **Accepted risk, stated plainly:** no signature/attestation, no publisher allowlist, no content
  scanning, no first-ingest human review, and the **executable bit survives the pipeline**
  (`canonicalRepack` preserves 0755). Skill content is trusted to the extent the user trusts the
  URL they pasted. Admin-only publishing (§4.3) is what keeps that blast radius per-tenant.

### 4.10 Out of scope

Periodic/webhook auto-reingest (schema-ready); pinning at attach to a non-latest version; bundle
marketplace/sharing semantics beyond admin publish; GC of orphaned member objects (append-only
stance stands — but see `sp-mwco.3`'s sha-denylist revoke, which is the actual kill switch); nested
bundles; content scanning/attestation (above).

### 4.11 Testing

- **Hermetic:** symlink-skip + escape-reject fixtures; discovery fixtures (root-only, root+nested
  warn, `skills/*`, duplicate names, duplicate content, >63 members, >10k files, zero skills);
  `sha(bundle member) == sha(subdir ingest)`; version-cut on commit+sha (incl. the revert case
  A→B→A); bundle_ref assembly emitting N disjoint artifacts with correct Ids/DestPaths/Names;
  dup-name over the expanded set; exclude/rename overrides; kill-switch resolving through bundle
  membership; `NOT NULL DEFAULT ''` key matching; frontmatter description cap/sanitize.
- **Build-tagged e2e:** ingest the **real `obra/superpowers`** (the repo whose symlink currently
  breaks ingest — this test is the proof §4.1 works) → bundle with N members → profile attach →
  spawn → assert every member skill present **at whatever path `agentinstall` canonically resolves
  for the agent** (a shared helper, not a hardcoded `~/.claude/skills` — `sp-mwco.2` may move it).
  Fails (never skips) when GitHub/Garage are down.

## 5. Dependencies

`sp-mwco.2` (all-agent install) **owns the install destination** this spec's e2e asserts against —
both epics extend `TestCPSkillIngestE2E`. Land `sp-mwco.1`'s ingest/assembly first, share the
path-resolution helper, and sequence the e2e edits to avoid a collision. `sp-mwco.2` also owns the
**all-or-nothing install contract** (a partially-installed bundle must fail the spawn, not come up
missing 3 of 20 skills silently).

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged
from the assumptions above — append a dated note here, whether or not a formal debugging skill was
used.*

- **2026-07-12 — roasted (BLOCK, 52 confirmed findings across the family) and revised.** Blockers
  folded: the symlink hard-reject makes the flagship repo unfetchable today (§4.1); one
  `ProfileEntry` cannot emit N artifacts (§4.5); bundle members are invisible to the kill-switch
  (§4.5). Majors folded: real caps stated + 200 MiB dead text deleted + member cap 63 (§4.2);
  per-member repack must be byte-identical to a subdir ingest or dedup dies (§4.2); membership
  decoupled from `Listed` (§4.3); `NOT NULL DEFAULT ''` on the provenance key (§4.3);
  exclude/rename overrides (§4.4); layout-change branch (§4.5); ETag + shared rate budget (§4.8);
  content threat model (§4.9); `listed=false` default with admin-only publish (§4.3). Bundle-of-one
  adopted family-wide, superseding `sp-mwco.3`'s derived-lineage model.

- **2026-07-12 — sp-mwco.1.11: §4.9's frontmatter sanitization only closed the catalog row; the
  installed SKILL.md a harness actually loads was still raw.** The whole-epic review caught that
  `sanitizeDescription` was wired to `Result.Description` / bundle `Member.Description` only — the
  **catalog row's** metadata. `canonicalRepack` ships every member's `SKILL.md` byte-for-byte, on
  purpose: `sha(bundle member) == sha(subdir ingest)` (§4.2, §4.11) is the content-addressed dedup
  identity, and rewriting `SKILL.md` at repack time would redefine that identity per-skill. So the
  raw, unbounded, attacker-controlled `description:` (and `name:`) shipped in the stored tar was
  exactly what every harness loaded into its system prompt at startup — the threat this section
  describes was **not** closed by what landed.

  Resolution: sanitize at **install time**, in `agentinstall`, not at repack. `installTreeAt`
  rewrites the staged (temp-dir) copy's `SKILL.md` frontmatter — description capped/stripped via
  the (now-shared) `spec.SanitizeDescription`, `name` pinned to the installed artifact name and
  length-capped (`spec.MaxSkillNameBytes = 64`) — immediately before the atomic rename, so both the
  canonical (`~/.agents/skills/<name>/`) and any native per-agent copy get the sanitized file. The
  source staging dir, the stored tar, and its sha are never touched: the dedup invariant survives
  by construction, pinned by `internal/cp/skillfetch/rawtar_test.go`.

  Residual, stated plainly: the raw description/name **do** persist — in the stored tar (dedup
  identity) and in the node's staging dir — but neither is ever read by a harness; only the
  install-time-rewritten copy is. A malformed frontmatter block (unclosed `---`, or not a YAML
  mapping) fails the install closed (`StatusFailed`, nothing written) rather than let unbounded
  content reach a system prompt. See `internal/agentinstall/frontmatter.go` and
  `internal/agentinstall/spec/skillmeta.go`.

- **2026-07-13 — sp-mwco.1.13: §4.9's diff-token gate could dead-end a stale-pinned bundle entry
  with no way to re-pin.** As shipped, `ReingestBundle` minted a diff token only when it detected a
  real change in-process; `GetBundleDiff` could only mark an already-minted token as viewed, never
  mint one itself. An entry pinned to v1 while the bundle was already at v2 — CP restart (the gate
  is in-memory, no persistence), the 30-minute token TTL having lapsed, or another session having
  already cut v2 — hit a dead end: "check for updates" reported `changed=false` (nothing new to
  mint), `GetBundleDiff(v1, v2)` returned `diff_token=""`, and `RepinProfileBundle` failed
  `FailedPrecondition` forever. The "update available" badge became a permanent, un-actionable nag.

  Resolution: **the view IS the gate.** `GetBundleDiff` now mints-and-marks-viewed a token for the
  exact `(owner, bundle, fromVersion, toVersion)` pair it actually serves (`diffGate.mintViewed`),
  independent of whether `ReingestBundle` ever ran in this CP process. `ReingestBundle`'s mint is
  now an optimization only (dedupe: a subsequent `GetBundleDiff` of the same pair reuses that
  token rather than minting a second one), not the sole path to a satisfiable token. The security
  property is unchanged and, incidentally, tightened: `assertDiffViewed` now also pins the token's
  `fromVersionID` to the entry's **currently pinned** version — a token for a narrower pair (e.g.
  v2→v3) no longer satisfies a re-pin from an earlier pin (v1) onto v3, closing a latent loophole
  where the caller could skip an intermediate delta. An un-diffed re-pin still fails closed
  (unknown/unviewed/expired/wrong-pair token). `diffGate` sweeps expired records on every mint and
  dedupes live records per tuple, keeping the map bounded despite `GetBundleDiff` being an
  unrate-limited read. See `internal/cp/bundles.go` (`diffGate`, `assertDiffViewed`,
  `GetBundleDiff`) and `internal/cp/bundles_test.go`.
