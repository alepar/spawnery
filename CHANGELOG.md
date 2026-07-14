# Changelog

This project has no tagged releases yet. Changes accumulate under `[Unreleased]`; when a release
process exists, that section will be cut into a dated release. Append new entries to
`[Unreleased]` under the relevant subheading (`Breaking changes`, `Added`, `Fixed`, ...) — don't
create a new `## [Unreleased]` block.

## [Unreleased]

### Breaking changes

- **Catalog entries are now created unlisted.** `CreateCatalogEntry` sets `listed=false` for
  **all** entries, inline content included — not just URL-ingested skills. Previously entries
  were created listed and appeared in every tenant's catalog immediately.
- **`ListCatalogEntries` is owner-scoped.** It now returns `listed=true` entries UNION the
  caller's own (including unlisted) — was an untenanted "all listed entries". `GetCatalogEntry`
  applies the same "listed OR mine" gate and returns **NotFound** for another owner's unlisted
  entry (deliberately not `PermissionDenied`: no existence probe).
- **Publishing is admin-only.** `PublishCatalogEntry` / `PublishBundle` are the only doors onto
  the global catalog, and `SetCatalogListing(listed=true)` now returns `PermissionDenied` for
  non-admins. The admin allowlist is `CP_ADMIN_OWNERS` (`admin_owners`), **empty by default —
  nobody is an admin until an operator sets it**. Unlisting is unchanged for creators, and
  remains reference-guarded (counts-only `FailedPrecondition` unless `confirm=true`; a confirmed
  unlist terminates referencing spawns).
- **Impact on existing callers:** a client that called `CreateCatalogEntry` and expected the
  entry to be immediately visible in the shared catalog will now see it as unlisted. **Nothing
  breaks for the creator** — you can still Get/List your own unlisted entries and attach them to
  profiles/spawns (the profile-entry resolve path applies the same "listed OR mine" gate). Only
  *global* visibility now needs an admin.
- **Why:** the catalog `List()` was untenanted — a pasted skill landed in every tenant's picker
  (×63 per bundle). See
  `docs/superpowers/specs/2026-07-12-skill-catalog-lifecycle-design.md` §4.6.
- **CLI:** `spawnctl catalog create` / `catalog ingest` now say so in their output and
  `--help`; an admin publishes with `spawnctl catalog set-listing <catalog-id> --listed`. A
  `--publish` flag was deliberately **not** added to `catalog create`/`catalog ingest`:
  publishing requires admin, so the flag would `PermissionDenied` for essentially every caller
  (and needs a second RPC) — a footgun advertising a capability nobody has. An admin who wants
  create-then-publish already has `spawnctl catalog set-listing <id> --listed`.
