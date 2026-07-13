# Skill Catalog Lifecycle — Provenance, Revocation, Safe Delete, Admin Publish

**Date:** 2026-07-12 (revised 2026-07-12 after roast BLOCK)
**Status:** draft (roasted → BLOCK → revised)
**Epic:** `sp-mwco.3`
**Extends:** [Profile Skill Ingestion from a GitHub URL](2026-06-22-profile-skill-url-ingestion-design.md),
[Skill Bundles](2026-07-12-skill-bundles-design.md) (`sp-mwco.1`)

## 1. Problem

Catalog skills are write-once, opaque, globally visible, and un-revocable.

- **Opaque:** the persisted provenance (`source_url/ref/subdir/sha256/size`,
  `ingest_skill.go:127-148`) is absent from every proto response, so neither the web UI nor
  spawnctl can display it — two same-name rows are indistinguishable.
- **Globally visible:** rows are created `listed=true` and `List()` has **no tenant filter**, so a
  skill pasted from a random GitHub repo is immediately offered in **every other tenant's** picker.
  With bundles that becomes ~63 rows per paste. The mirror hazard: a creator's
  `SetCatalogListing(false)` **kill-switches other users' live spawns**, with no confirmation and no
  reference list.
- **Un-revocable:** `DeleteCatalogEntry` removes only the row. The Garage object is never removed
  (by design), and **existing spawns replay from their own persisted artifact rows (`object_key` +
  sha) at resume/fork without re-consulting the catalog** — so after a "delete", a malicious skill
  is still fetchable by every spawn that ever bound it. Delete is a UI affordance, not a kill
  switch.
- **Unsafe delete:** dangling profile entries hard-fail spawn creation (`unknown catalog ref`).
- **No CLI parity:** URL ingest is web-only.

## 2. Superseded: the derived-lineage update model

An earlier draft of this spec gave *single* skills a **derived** lineage (rows sharing the
provenance tuple form a family; newest by `created_at`) while `sp-mwco.1` gave bundles **stored**
versions. The roast killed this, correctly, on two counts: it shipped **two update models for one
user concept** (which one you got depended invisibly on whether the repo happened to have a root
`SKILL.md`), and the derived model is **simply wrong on an upstream revert** — with content-addressed
idempotency, re-ingesting A after A→B→A returns the *original* (oldest) row and writes nothing, so
"newest by created_at" stays the stale B row: a permanent phantom "update available" whose re-pin
moves the profile to content that no longer matches upstream.

**Resolution (user-pinned): bundle-of-one everywhere.** *Every* URL ingest creates a bundle with
N ≥ 1 members (`sp-mwco.1` §3). Update, versioning, pinning, and diffing are **`sp-mwco.1`'s
`ReingestBundle` alone**. This spec no longer defines `ReingestSkill`, lineage, or a second badge.
What remains here is everything bundles do *not* cover: provenance display, revocation, delete
safety, listing/publish policy, object repair, and CLI parity.

## 3. Key decisions

Provenance (including the **source commit**) goes on the wire. Revocation becomes real via a
**sha denylist consulted at presign**. Delete becomes referentially safe and tenant-safe.
URL-ingested rows default to **unlisted**, and **publishing to the global catalog is admin-only**.
A missing Garage object is repairable.

## 4. Design

### 4.1 Provenance on the wire

Add `source_url`, `source_ref`, `source_subdir`, `source_commit`, `sha256`, `size`, `bundle_member`
to `CustomizationCatalogEntry` and `CatalogEntrySummary` (additive; empty for non-URL entries).

Two corrections the roast forced:

- **`source_url` is not a URL.** `ingest_skill.go:127` stores `owner + "/" + repo`. Either persist
  the full URL going forward (with a migration) or render as `https://github.com/` + slug — but do
  not ship a "clickable source link" over a bare slug. (The finding's sub-claim that this hardcodes
  github.com into re-ingest is **wrong**: `ParseRepoURL` already accepts the bare slug.)
- **`sha256` is the packed-tar content hash, not a git commit.** Label it as such in the UI, and
  display `source_commit` (new, from `sp-mwco.1` §4.9) as the upstream identity. Version comparison
  keys on the commit, never `created_at`.

### 4.2 Revocation — a sha denylist at presign *(the actual kill switch)*

Deleting or unlisting a row does not stop a spawn that already bound the object. Add a CP-side
**`skill_object_denylist(sha256, reason, created_at)`**, consulted in `presignNodeArtifacts` on
**every** start path (create/resume/fork/recreate/migrate). A denylisted sha cannot be re-presigned,
so no spawn can re-materialize it — cheap (one lookup per artifact), needs no GC, and works even
though the Garage object remains. Admin action; reason is recorded and surfaced.

This is what makes "revoke a malicious skill" true. Note `sp-mwco.4`'s StatObject gate would
otherwise happily pass a revoked-but-present object.

### 4.3 Safe delete

`DeleteCatalogEntry` checks references first — profile entries (`catalog_ref`) **and bundle-version
memberships** — and fails `FailedPrecondition`; `force=true` overrides.

- **Tenant safety:** the catalog is global and `AddProfileEntry` does no ownership check, so the
  reference check spans owners. It therefore returns **counts only** — "referenced by 3 profiles
  across 2 owners" — never profile names or ids. (Leaking other tenants' identifiers in an error
  message is a real disclosure.)
- **Honest claim:** check-then-delete with no FK and no transaction cannot guarantee "the normal
  path can no longer create a dangling ref" — a concurrent attach still can. Either wrap check+delete
  in `WithTx` with a lock on the referencing tables (preferred), or state the weaker property. The
  spec picks `WithTx`; assembly keeps failing loud on a dangling ref as defense in depth.
- `force=true` on a row that is a **bundle-version member** would orphan a live version: it is
  rejected outright (delete the bundle version instead), not forced.
- The Garage object is never removed. Revocation is §4.2's denylist.

**Bundle deletion** (absent from every earlier draft — bundles were create-only, forever): add
`DeleteBundle(bundle_id)` and `DeleteBundleVersion(version_id)` with the same reference check over
`bundle_ref` profile entries. Without these, `sp-mwco.1` §4.5's "deleted bundle/version fails
assembly loud" branch is unreachable code.

### 4.4 Kill-switch scope *(correcting a false claim)*

The earlier draft declared "the existing kill-switch/suspend behavior for running spawns is
unchanged". That is wrong and dangerous: `resolveAffectedSpawns` walks `catalog_ref` profile entries
only, so a skill delivered via a `bundle_ref` is **invisible** to it — revoking it would terminate
**zero** spawns. The kill-switch must resolve affected spawns through **bundle-version membership →
bundle_ref entries** as well (specified in `sp-mwco.1` §4.5; restated here because this spec owns
revoke).

### 4.5 `UpdateCatalogEntry` guard

`UpdateCatalogEntry` overwrites `Content` while leaving `SHA256` set — and assembly prefers
`SHA256`, so the edit is either rejected by the tar validator or **silently ignored at spawn time**;
it also lets a caller rename/redescribe a URL row, polluting the provenance display. Guard it:
`FailedPrecondition` for any row with a non-empty `SHA256` (URL-ingested rows are immutable; change
them upstream and re-ingest).

### 4.6 Listing policy — unlisted by default, admin-only publish

URL-ingested rows (single skills and bundle members alike) are created **`listed=false`** —
creator-visible only. **Publishing to the global catalog is an admin-only action**
(`PublishCatalogEntry` / `PublishBundle`, admin-gated), since `List()` is untenanted and a listed row
is visible to everyone.

`SetCatalogListing(false)` gains the same reference-count confirmation as delete (it already has the
cross-tenant blast radius — it kill-switches other users' live spawns — with no guard at all today).

### 4.7 Object repair

Ingest short-circuits on `GetByCreatorSHA` **before** `PutIfAbsent`, so an unchanged-sha re-ingest
writes nothing — meaning a **lost Garage object permanently bricks every profile referencing that
sha**, and the one verb that could fix it does nothing. Fix: the unchanged-sha branch still calls
`PutIfAbsent` (a cheap StatObject-then-PUT). Contract: "no catalog writes, but re-uploads a missing
object."

### 4.8 spawnctl parity

`spawnctl catalog ingest <url> [--ref --subdir --name --description]`, `spawnctl bundle
list|show|reingest`, provenance columns in `catalog show`/`list`, and `spawnctl catalog
deny <sha>` (admin) for §4.2.

### 4.9 Web UI

Catalog card: provenance block (source URL, ref, **commit**, content-sha labelled as such, size),
delete (reference-aware, counts-only confirm), admin publish/unpublish, admin deny. Profile entry
chips get the bundle badge/diff/re-pin from `sp-mwco.1` (not a second single-skill mechanism).

### 4.10 Testing

Hermetic: provenance round-trip through both response types; denylist blocks presign on every start
path (incl. resume + fork); delete blocked with counts-only message / `WithTx` race test / force
rejected for bundle members; bundle+version delete reference checks; `UpdateCatalogEntry` guard;
unlisted-by-default + admin-only publish authz; unchanged-sha re-ingest re-uploads a deleted object;
kill-switch resolves through bundle membership. CLI golden output. Web vitest for delete/publish/deny
flows. No new e2e lane needed.

## 5. Out of scope

Garage object GC/refcounting (revocation is the denylist, not deletion); automatic profile updates;
marketplace semantics beyond admin publish; editing URL-ingested content in place (§4.5).

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged
from the assumptions above — append a dated note here, whether or not a formal debugging skill was
used.*

### Changes vs. original design (2026-07-13, as implemented)

- **The derived-lineage update model was DELETED** in favour of bundle-of-one everywhere: it duplicated
  `sp-mwco.1`'s model and was wrong on an upstream revert (A→B→A returns the *oldest* row under
  content-addressing, so "newest by created_at" showed a permanent phantom update).
- **Revocation is a presign-time sha denylist, not delete.** Deleting a row does not stop a spawn that
  already bound the object — spawns replay from their own persisted artifact rows without re-consulting
  the catalog.
- **`listed=false` is now the default for ALL catalog entries (inline included), and publishing is
  admin-only.** This extends beyond URL ingest — noted as a behavior change for existing callers of
  `CreateCatalogEntry` (see `sp-mvz4`).
- The kill-switch had to be taught to resolve through bundle membership; it previously saw only
  `catalog_ref` entries and would have terminated *zero* spawns for a bundle-delivered skill.

- **2026-07-12 — roasted (BLOCK) and revised.** The derived-lineage update model was **deleted**
  (§2) in favour of bundle-of-one everywhere: it duplicated `sp-mwco.1`'s model and was wrong on an
  upstream revert. False claims corrected: "kill-switch behavior unchanged" (it is blind to bundle
  members — §4.4); "the normal path can no longer create a dangling ref" (check-then-delete has no
  FK/tx — §4.3); "source URL" is a bare `owner/repo` slug and the displayed sha is a content hash,
  not a commit (§4.1). Majors folded: real revocation via a presign-time sha denylist (§4.2);
  tenant-safe counts-only delete errors (§4.3); bundle/version deletion (§4.3); `UpdateCatalogEntry`
  guard (§4.5); unlisted-by-default + admin-only publish (§4.6); missing-object repair (§4.7).
