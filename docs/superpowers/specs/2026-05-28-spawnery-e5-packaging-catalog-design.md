# Spawnery E5 — App Packaging & Catalog (Design)

**Bead:** `sp-7hl`
**Status:** Draft v1 (interview complete; pending user review)
**Date:** 2026-05-28
**Depends on:** [E0](2026-05-26-spawnery-e0-contracts-design.md),
[E3](2026-05-28-spawnery-e3-storage-design.md),
[E4](2026-05-28-spawnery-e4-identity-secrets-design.md)

Owns: the **`spawneryapp.yml`** manifest spec, **definition-repo conventions**, **version
resolution + auto-upgrade**, the **publish trigger** (MVP: poll), **registration + catalog**,
and the **open vs private** gate (review *content* spec'd in **E8**).

---

## 1. Manifest & definition repo

- Manifest schema: see **[E0 §3](2026-05-26-spawnery-e0-contracts-design.md)** (`apiVersion`,
  `kind: App`, `id`, `agents`, `tools`, `persona`, `skills`, `model`, `runtime`, `storage`,
  `visibility`). `storage:` block contributed by [E3 §8](2026-05-28-spawnery-e3-storage-design.md).
  Personalization + permissions are **post-MVP** (`TODO.md`).
- **Definition-repo layout (convention):**
  ```
  <definition repo root>
  ├── spawneryapp.yml           (manifest at known path)
  ├── persona.md
  ├── skills/                   (instruction files imported by the agent natively)
  ├── seed/                     (scaffold copied into new spawns' /data; per E3 §4 step 5)
  ├── icon.png
  └── README.md
  ```
- **Identity:** `creator/app` handle = repo owner/repo name; **version = semver git tag**
  resolving to an immutable **commit SHA** (E0 §2).

---

## 2. Publish trigger — **poll model (MVP)**

- **No webhook, no `publish` action, no CLI.** Creators just **push semver git tags** to their
  definition repos.
- **CP resolves from a cache, not a live call on the hot path (roast `sp-7fj`):**
  1. A **periodic refresh** job polls registered Apps' latest semver tag → SHA and stores it
     (`latestKnownSha` in the catalog index, §3a) with a short TTL.
  2. **`createSpawn` / auto-resume reads the cached SHA** — it does **not** block on a synchronous
     GitHub call. On a cache miss it resolves once and caches; if **GitHub is down or
     rate-limited** (installation limit ~5000/hr, shared with clone/push), fall back to the
     **last-known-good SHA** so spawns still start. This keeps GitHub latency + outages off the
     spawn-start path.
- **Webhook-driven push discovery** (creator installs the Spawnery GitHub App on the
  definition repo; GitHub pushes events) is **post-MVP** and removes polling entirely.

---

## 3. Registration & catalog — **hybrid**

- **Optional formal registration (catalog listing):** a creator (logged-in user with the
  `creator` capability) submits the definition-repo URL via the web UI / CP API. CP fetches +
  validates the manifest, **lists the App** in the catalog, then polls per §2. Registered Apps
  get the full catalog surface: discovery/search, ratings, flags, listing UX.
- **Ad-hoc URL spawn (no listing):** any user can paste a **public GitHub URL** at spawn-create;
  CP fetches + validates the manifest on demand and proceeds. Useful for power users / sharing,
  but the App doesn't appear in the catalog and accumulates no metadata.
  - **Abuse guard (roast `sp-iui`/SSRF):** ad-hoc fetches are **per-user rate-limited**,
    restricted to validated **`github.com` hosts only** (no arbitrary URLs → no SSRF probe), and
    the **scanner (E8 §5) must gate the manifest before any ad-hoc spawn executes** — same bar as
    a registered App, just without listing.
- **Private Apps require registration**: closed source ⇒ Spawnery's GitHub App must be installed
  on the repo so the CP can access it, AND human review is required before listing/sale (§5).

### 3a. Catalog index (CP-side)
- CP-side DB (Postgres / equivalent), one entry per registered App:
  `{ id (creator/app), repoUrl, manifestCache, visibility, status (listed|review|hidden),
    latestKnownTag, latestKnownSha, stars, flags, createdAt, updatedAt }`.
- Basic browse + text search (title/description/tags) for MVP. Faceted search post-MVP.
- Catalog API per [E0 §5b](2026-05-26-spawnery-e0-contracts-design.md).

---

## 4. Versioning & auto-upgrade (recap + filling)

Carried over from the [system design §8](2026-05-26-spawnery-system-design.md) and E0:

- **Immutable, content-addressed versions** — `spawn.yml.app.ref = creator/app@<sha>` always
  references a SHA. Tags resolve to SHAs CP-side; tags themselves are display-only.
- **`versionPolicy: auto`** (default) — CP resolves latest tag's SHA at session start; uses it.
- **`versionPolicy: pinned`** — CP uses `pinnedSha` from `spawn.yml`; new tags are ignored
  until the user un-pins.
- **Guardrails:**
  - **Permission escalation breaks auto-upgrade → re-consent** (post-MVP per E0; manifest
    `permissions` block is post-MVP, so this guard is wired up when permissions land).
  - **Pre-upgrade snapshot** — node creates a git tag at the pre-upgrade commit under
    `.spawnery/snapshots/<oldSha>-pre-<newSha>` for free rollback (E3 §6).
  - **Changelog notice** — surfaced to the user in the chat UI even on silent upgrades
    (rendered from the tag's commit message / GitHub release notes).

---

## 5. Publish gating — open vs private

- **Open Apps → automated checks only, instant listing on pass.** The CP runs **automated checks**
  on the fetched manifest at each new tag:
  - JSON-Schema validation (well-formed; correct types; required fields present).
  - Reference checks (declared `tools` are in the common-toolset catalog; `agents.support` are
    known agents; `model.requires` are valid capabilities).
  - Seed sanity (`storage.seed` path exists in the repo; size cap).
  - Manifest-side trust: no path traversal, no oversized files, no suspicious links.

  Pass → **listed/usable immediately**. Fail → surfaced to the creator via the catalog UI; the
  bad tag is **excluded from auto-upgrade** until fixed (older passing tags remain valid).

- **Private Apps → automated + human review queue.** Same automated checks gate entry to the
  review queue; **a human reviewer** then gates listing/sale. Review **content** (what's
  inspected for prompt-injection / abuse / scope sanity) is spec'd in **E8**; E5 owns only the
  queue mechanics:
  - Each new tag → review queue entry with `App@sha`, manifest diff vs last-approved.
  - Reviewer can **approve**, **reject (with notes)**, or **request changes**.
  - Approved tag → usable for spawning + listing.
  - SLA target (post-MVP): TBD; MVP review = best-effort by the Spawnery team.

---

## 6. Validation & schema versioning

- Manifest schema is JSON Schema; the CP ships the schema; validation runs at registration, at
  every tag poll, and at every spawn-create.
- **Manifest `apiVersion: spawnery/v1`**; schema bumps (`v2`, …) follow semver; older versions
  accepted until deprecated; never silently coerced.
- **Strict by default**: unknown top-level fields → warning (not failure) for forward-compat;
  unknown required-shape fields → failure.

---

## 7. Deferred (post-MVP)

Webhook-driven push discovery (GitHub App on definition repos) · GitLab / Gitea / self-hosted git
definition repos · CLI `spawnery` for creators · faceted/full-text search at scale · creator
analytics · ratings/reviews UX hardening · staged rollouts / canary tags · review SLA / paid
fast-track lane · automated prompt-injection scanner (overlaps with E8).

---

## Appendix — E5 decision log

| # | Decision | Choice |
|---|---|---|
| E5.1 | Publish trigger | **Poll model** at `createSpawn` + periodic refresh (CP resolves latest semver tag → SHA, dispatches SHA to node). Webhook = post-MVP |
| E5.2 | Registration | **Hybrid**: optional formal registration for catalog listing + ad-hoc URL spawn for public repos; private requires registration |
| E5.3 | Catalog (asserted) | CP-side DB; basic browse + text search; ratings/flags; visibility-scoped |
| E5.4 | Open publish (asserted) | Automated manifest + reference checks; pass → instant list; bad tag excluded from auto-upgrade |
| E5.5 | Private review (asserted) | Same automated gate + human review queue; review content owned by **E8** |
| E5.6 | Versioning (recap) | Immutable SHAs; `auto` = latest tag's SHA at session start; `pinned` = SHA; escalation breaks (when permissions land); pre-upgrade snapshot tag |
| E5.7 | Schema versioning | `apiVersion: spawnery/v1`; strict-with-warnings on unknown top-level fields; bumps follow semver |
