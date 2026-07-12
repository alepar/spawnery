# Skill Catalog Lifecycle — Update, Provenance Visibility, Safe Delete

**Date:** 2026-07-12
**Status:** draft
**Epic:** `sp-mwco.3` *(Mode B one-shot spec — decisions made solo from the 2026-07-11 audit; review before implementation)*
**Extends:** [Profile Skill Ingestion from a GitHub URL](2026-06-22-profile-skill-url-ingestion-design.md),
[Skill Bundles](2026-07-12-skill-bundles-design.md)

## 1. Problem

Catalog skills are write-once and opaque. Re-ingesting a URL whose upstream changed creates a
second catalog row with the same name (idempotency keys on `(owner, sha256)` only) — nothing
links old to new, nothing updates profiles, and the two rows are indistinguishable because the
persisted provenance (`source_url/ref/subdir/sha256/size`, `ingest_skill.go:127-148`) is absent
from every proto response, so neither the web UI nor spawnctl can display it. Deleting a catalog
entry (spawnctl-only today) leaves dangling profile entries that hard-fail spawn creation with
`unknown catalog ref` until manually cleaned. URL ingest itself has no CLI parity.

## 2. Main challenges

Update semantics must not recreate the silent-update channel the bundle design rejected: profile
entries pin content; updates are explicit. Delete must protect running state (profiles referencing
the entry) without inventing referential-integrity machinery the store doesn't have. Everything
here is additive proto surface — no breaking changes.

## 3. Key decisions

Provenance goes on the wire (added fields). Single-skill update reuses the ingest machinery and
provenance-lineage queries rather than a new versioning entity — bundles already own the
versioned-update story; single skills get a lighter "newer row exists" affordance. Delete becomes
referentially safe via a reference check + explicit force. spawnctl gains ingest/provenance
parity.

## 4. Decision points, by section

### 4.1 Provenance on the wire

**Chosen:** add `source_url`, `source_ref`, `source_subdir`, `sha256`, `size` to
`CustomizationCatalogEntry` and `CatalogEntrySummary` (added proto fields; empty for non-URL
entries). Web catalog card and `spawnctl catalog show`/`list` display them. This is the
prerequisite for every other affordance here — users must be able to tell two same-name rows
apart. **Discarded:** a separate `GetCatalogProvenance` RPC (needless second round-trip for data
the row already holds).

### 4.2 Single-skill update — lineage by provenance, explicit re-pin

**Chosen:** `ReingestSkill(catalog_id)` re-fetches the row's `(source_url, source_ref,
source_subdir)`; unchanged sha → "up to date" (no writes, idempotent); changed → a NEW catalog row
(content identity stays content-addressed) returned alongside the old id. Lineage is *derived*,
not stored: rows sharing `(creator_id, source_url, source_ref, source_subdir)` form a family,
newest by `created_at`. UI shows an "update available" badge on profile entries whose catalog ref
has a newer family member, with an explicit "update entry" action that swaps the ref
(the Gap 1 pin-and-explicit-update model, applied to single skills). Applies the standard
per-owner ingest quota. **Discarded:** mutating the existing row's content in place (breaks
content-addressing, silently changes any other profile referencing it, and races running spawns);
a `supersedes` FK column (write-time bookkeeping duplicating what the provenance query answers);
wrapping every single skill in a bundle-of-1 (user pinned single = plain row; bundle machinery is
overhead here).

### 4.3 Safe delete

**Chosen:** `DeleteCatalogEntry` checks references first — profile entries (`catalog_ref`) and
bundle-version memberships — and fails `FailedPrecondition` listing the referencing
profiles/bundles; `force=true` overrides, and the existing kill-switch/suspend behavior for
running spawns is unchanged. Assembly keeps failing loud on a dangling ref (defense in depth),
but the normal path can no longer create one. The web UI gains delete (with the reference list in
the confirmation dialog); the Garage object is never removed (content-addressed, shared,
append-only per the parent spec — GC stays out of scope). **Discarded:** cascade-delete of
referencing profile entries (silently mutates profiles); soft-delete/tombstones (state machine
creep for no consumer).

### 4.4 spawnctl parity

**Chosen:** `spawnctl catalog ingest <url> [--ref --subdir --name --description]` (calls
`IngestSkillFromURL`; prints catalog/bundle ids), `spawnctl catalog reingest <catalog-id>`, and
provenance columns in `show`/`list`. Bundle verbs (`bundle list/show/reingest`) ride along once
Gap 1 lands. **Discarded:** a separate `spawnctl skill` noun (the catalog noun already models
this).

### 4.5 Web UI

Catalog card: provenance block (source URL as link, ref, short sha, size), delete button
(reference-aware confirm), "check for updates" on URL-provenance rows. Profile entry chip:
"update available" badge + one-click re-pin. All error paths surface as actionable toasts, as the
ingest dialog does today.

### 4.6 Testing

Hermetic: provenance fields round-trip through both RPC response types; reingest unchanged/changed
paths (no-write vs new-row + family lineage query); delete blocked with correct reference list /
force override; dangling-ref assembly still fails loud. CLI: golden output for
ingest/show/list. Web: vitest for badge + delete confirm flows. No new e2e — the existing ingest
e2e covers the only external dependency, and everything here is CP/store/UI logic.

## 5. Out of scope

Garage object GC/refcounting; automatic profile updates; catalog sharing/marketplace listing
semantics; edit-in-place of skill content (`UpdateCatalogEntry` keeps its current
name/description/content contract for non-URL entries).

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged
from the assumptions above — append a dated note here, whether or not a formal debugging skill was
used.*
