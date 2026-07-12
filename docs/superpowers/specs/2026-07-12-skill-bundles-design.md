# Skill Bundles — Multi-Skill Repo Ingestion (versioned, pinned attach)

**Date:** 2026-07-12
**Status:** draft
**Epic:** `sp-mwco.1`
**Extends:** [Profile Skill Ingestion from a GitHub URL](2026-06-22-profile-skill-url-ingestion-design.md) (`sp-nrzf.3.14`),
[Profiles — The Customization Tool (v2)](2026-06-14-profiles-customization-tool-design.md) (`sp-nrzf.3`)

## 1. Problem

URL skill ingestion (sp-nrzf.3.14) handles exactly one skill per call: it requires a top-level
`SKILL.md` after wrapper-strip + subdir descent (`internal/cp/skillfetch/skillfetch.go:217-231`).
A superpowers-style repo (`skills/*/SKILL.md`, no root `SKILL.md`) fails outright with
`no SKILL.md found in repository root`; the only workaround is one `IngestSkillFromURL` call per
subdir (~20 dialogs for superpowers), each producing an unrelated catalog row that must be attached
to the profile one at a time. Worse, a repo with BOTH a root `SKILL.md` and nested skills silently
repacks the entire tree — nested skills and all — into one skill object, undetected.

The goal: paste one URL, get the whole bundle into a profile, and have every member skill land in
the spawn's agent harness. Bundles are versioned so upstream changes can be picked up deliberately,
never silently.

## 2. Main challenges

A bundle must not regress the per-skill properties the parent spec fought for: content-addressed
per-skill dedup (canonical repack, `skills/<sha256>.tar.zst`), the duplicate-skill-dir-name
rejection, the 64-artifact cap, and the by-ref delivery path. It must also not create a
silent-update channel: a re-ingested bundle changing every profile that references it (and every
future spawn) without user intent was explicitly rejected. The versioning model threads this
needle: re-ingestion cuts new *versions*; profiles pin a version at attach; updates are explicit.

## 3. Key decisions (user-pinned)

1. **Bundle = N per-skill catalog rows + a grouping entity** — each member skill is an ordinary
   content-addressed `customization_catalog` row (per-skill dedup and by-ref delivery unchanged);
   the bundle groups them.
2. **Discovery = recursive walk, prune at skill dirs** — any directory containing `SKILL.md` is a
   skill; its subtree belongs to it (no deeper descent). Root `SKILL.md` → single-skill repo →
   today's plain path (backward compatible, no bundle entity).
3. **Bundle is versioned; profile pins a version** — the profile entry stores
   `(bundle_id, version_id)` captured at attach. Re-ingestion (manual in MVP) cuts a new version;
   no profile changes until the user explicitly updates the entry's pin. Spawns bind to whatever
   the pinned version resolves to at creation time.
4. **Manual re-ingest only in MVP** — a "Check for updates" action per bundle; the schema is
   versioned from day one so a periodic poller can be added later without migration.

## 4. Design

### 4.1 Entities & schema

Two new tables (sqlite + pg migrations):

- **`skill_bundle`** — `bundle_id, creator_id, name, source_url, source_ref, source_subdir,
  created_at, updated_at`. Unique on `(creator_id, source_url, source_ref, source_subdir)`:
  re-pasting the same URL re-ingests (possibly cutting a new version) rather than duplicating.
- **`skill_bundle_version`** — `version_id, bundle_id, seq (monotonic per bundle), created_at`,
  plus a member join table `skill_bundle_member (version_id, catalog_id, position)`.

Members are ordinary `customization_catalog` rows: content-addressed, per-skill Garage objects,
provenance columns as today (each member's `source_subdir` records its directory within the repo).
Member rows are created with `listed=0` so the catalog UI shows one bundle, not 20 loose rows.
The catalog schema itself is unchanged.

**Version-cut rule:** re-ingest fetches + discovers + canonically repacks each skill separately
(per-skill sha256). If the member set and every member sha match the latest version → no new
version (idempotent; returns the existing version). Otherwise a new version row is created,
reusing unchanged member rows and creating catalog rows only for changed/new skills (the
`(owner, sha256)` idempotency from the parent spec makes this reuse automatic).

### 4.2 Discovery

Runs on the already-fetched, already-validated entry list inside `skillfetch` (all existing
SSRF/tar-safety/streaming-cap machinery reused unchanged; discovery adds no new I/O):

- Walk the (wrapper-stripped, subdir-descended) tree. Any directory containing `SKILL.md` is a
  skill root; its entire subtree belongs to that skill; do not descend past it.
- Root `SKILL.md` → the whole tree is one skill → **plain single-skill ingest, no bundle entity**
  (exactly today's behavior, including for repos that also contain nested `SKILL.md`s below the
  root one — those are that skill's payload, as today; the mixed case is resolved by construction).
- Per-skill name: `SKILL.md` frontmatter `name` (sanitized), fallback = skill dir basename
  (frontmatter pinning per the cross-agent research; mismatch warning as today).
- Fail-loud at ingest on: duplicate skill names within the bundle; per-skill plain-tar cap
  (existing 50 MiB); bundle caps — max 100 skills, max 200 MiB total plain tar.
- Zero skills discovered → the existing `no SKILL.md found` error (now phrased to mention nested
  discovery was attempted).

### 4.3 RPC surface

- `IngestSkillFromURL` request unchanged; response gains `bundle_id`, `version_id`, and repeated
  member `catalog_id`s (added fields; single-skill repos return one `catalog_id` as today, no
  bundle fields).
- New: `ReingestBundle(bundle_id) → {version_id, changed, added/updated/removed counts}` — the
  manual update button. Applies the same per-owner ingest quota as `IngestSkillFromURL`.
- New: `ListBundles`, `GetBundle(bundle_id)` (bundle + versions + members with provenance).

All additive; no breaking proto changes. `make gen` after.

### 4.4 Profile attachment

`ProfileEntry` gains a **`bundle_ref`** source alongside `catalog_ref`/custom: stores
`(bundle_id, version_id)` — **pinned at attach time**. Re-ingestion never touches existing
profiles. The web UI shows an "update available" badge on the entry chip when the bundle's latest
`seq` exceeds the pinned version's, with an explicit "update to latest" action that re-pins (and
re-runs the cap + duplicate-name checks).

Entry-cap accounting: a bundle_ref counts as its member count against `maxArtifactsPerSpawn` (64),
checked at attach (UX) and authoritatively at assembly. Cherry-picking needs no special support:
members are ordinary catalog rows, so attaching individual skills instead of the bundle already
works (they are `listed=0`, so the picker surfaces them under the bundle, not in the main list).

### 4.5 Assembly & spawn binding

At spawn create, `profiles_assembly.go` resolves `bundle_ref → pinned version → member rows` and
emits N by-ref `ArtifactSpec`s exactly as if they were individual catalog refs. The spawn's
persisted artifact rows (object_key + sha per skill) ARE the version binding — resume/fork already
replay from those rows, so "spawn bound to the version at creation" falls out of the existing
substrate with zero new mechanism. The existing duplicate-dir-name rejection runs over the fully
expanded set (bundle members + standalone entries together). A `bundle_ref` whose bundle or
version row was deleted fails assembly loud (`unknown bundle ref`), mirroring the catalog_ref
behavior; safe-delete guards live in the lifecycle spec (`sp-mwco.3`).

### 4.6 Web UI

Same ingest dialog. A multi-skill result shows the discovered member list before/after attach.
The bundle renders as one catalog card (expandable member list, provenance) and one profile-entry
chip with member count + pinned version. Bundle card gets **"Check for updates"** →
`ReingestBundle` → toast: "up to date" or "v3 created: +2 skills, 3 updated" (profiles unchanged
until their entries are explicitly updated).

### 4.7 Out of scope

Periodic/webhook auto-reingest (schema-ready, not built); pinning a profile to a non-latest
version at attach time (attach always pins latest; older pins only arise by later versions being
cut); bundle sharing/marketplace listing; GC of orphaned member objects (parent spec's
append-only stance stands); nested bundles.

### 4.8 Testing

- **Hermetic:** discovery fixtures (root-only / `skills/*` nested / mixed root+nested / duplicate
  member names / >100 skills / zero skills); version-cut idempotency (unchanged → no new version;
  one member changed → new version reusing others); bundle_ref assembly expansion + dup-name over
  the expanded set; attach-time cap accounting; pinned-version resolution (a newer version exists,
  assembly still uses the pin).
- **Build-tagged e2e:** extend `TestCPSkillIngestE2E`: ingest the real superpowers repo → bundle
  with N members → profile attach → spawn → assert every member skill directory present in
  `~/.claude/skills/`. Fails (never skips) when GitHub/Garage are down, per project convention.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged
from the assumptions above — append a dated note here, whether or not a formal debugging skill was
used.*
