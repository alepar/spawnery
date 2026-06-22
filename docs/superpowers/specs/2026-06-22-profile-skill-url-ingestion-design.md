# Profile Skill Ingestion from a GitHub URL (Garage-backed, by-ref delivery)

**Date:** 2026-06-22
**Status:** draft
**Extends:** [Profiles — The Customization Tool (v2)](2026-06-14-profiles-customization-tool-design.md) (`sp-nrzf.3`),
[Artifact-Injection + Cross-Agent Installer](2026-06-14-cross-agent-installer-design.md) (`sp-l5sx`/`sp-1bia`)

## 1. Problem

The Profiles feature lets a user assemble a named bundle of customizations (skills/MCPs/configs/plugins)
that get injected into a spawn's agent harness at creation. The whole downstream chain is built and
tested end-to-end:

```
profile entry → internal/cp/profiles_assembly.go → manifest.json + tar payloads
  → artifact delivery (sp-l5sx) → in-pod agentinstall (sp-1bia) → ~/.claude/skills/<dir>/
```

But there is **no way to get an arbitrary skill into the system from the UI.** A skill is a tar archive
with a top-level `SKILL.md`; the web UI deliberately excludes skills from its custom-entry form
(`web/src/views/ProfilesView.tsx:76` — "a plain textarea cannot produce a tar; use catalog-ref for
skills instead"). The only existing path is `spawnctl catalog create --kind skill --content-file
<tar>`, which requires a human to clone, tar, and upload out of band.

The goal: a user pastes a GitHub URL (e.g. `https://github.com/199-biotechnologies/claude-deep-research-skill`)
in the UI, the skill is ingested, and it becomes selectable in a profile — so the literal flow
"create a profile in the UI with this skill → create a spawn with that profile → the agent comes up
with the skill available" works.

> **Note on implementation status.** `sp-nrzf.3` reads 57% complete in beads, but the code is far
> ahead: `internal/cp/profiles_assembly.go` exists (the `.7` "assembly layer"), and
> `CreateSpawnRequest.profile_id` is wired through `server.go:1208`/`:1273` (the `.8` "profile_id on
> CreateSpawn"). Those beads should be reconciled (see §9). This spec adds the missing front door,
> not the back end.

## 2. Main challenges

A skill ingested from a URL can be larger than the **1 MiB inline cap** (`maxArtifactInlineBytes`)
that the catalog's `content BLOB`/`bytea` column and the inline-only artifact substrate impose —
`sp-l5sx` deliberately dropped by-ref delivery from MVP. So this work **re-introduces a by-ref
delivery path**, scoped narrowly to non-sensitive public skills (which sidesteps the original roast
objection that "by-ref + sensitive breaks CP-blind": public skills carry no secrets). Skill bytes
move out of the relational store into a dedicated **Garage** bucket; delivery becomes a node-side
pull. The second challenge is doing the URL fetch safely from the CP — a component not previously in
the business of fetching arbitrary user-supplied URLs (SSRF, tar-bombs, symlink escapes).

## 3. Key decisions

Skill bytes live as **zstd-compressed tar objects in a dedicated, unencrypted Garage bucket**
(public skills are non-sensitive). The CP fetches the GitHub tarball over HTTPS (allowlisted hosts),
unpacks it safely, validates a top-level `SKILL.md`, repacks a plain tar, computes `sha256` over that
plain tar (the content identity + integrity check), zstd-compresses it, and `PutObject`s it to Garage
under a **global content-addressed key** `skills/<sha256>.tar.zst` (cross-user dedup). Authoritative
ownership/provenance lives in the **catalog DB rows** (`source url/ref/subdir, owner, sha256, size`);
S3 object tags (`source`, `owner`, `catalog_id`) are a best-effort first-writer convenience. At
`CreateSpawn`, the assembly layer emits a **by-ref `ArtifactSpec`** (`object_key` + `sha256`) for the
skill payload (the `manifest.json` index stays inline); at `StartSpawn` the CP **presigns a
short-lived GET URL per object** and passes it to the node. The node fetches the presigned URL,
zstd-decodes, **verifies the sha256**, and unpacks into the existing staging dir — `agentinstall`
runs unchanged. **Garage is a hard dependency** for ingesting or spawning with a URL skill (no inline
fallback). **Sensitive artifacts keep the inline E2E sealed path**; by-ref is non-sensitive-only.

## 4. Decision points, by section

### 4.1 Fetch mechanism — HTTPS tarball, not git clone

**Chosen:** the CP does an HTTPS GET of GitHub's repo tarball
(`https://api.github.com/repos/{owner}/{repo}/tarball/{ref}`, following to `codeload.github.com`),
gunzips, strips GitHub's `owner-repo-<sha>/` wrapper dir, and (if `subdir` given) descends into it.
No `git` binary, no subprocess; egress is narrowed to GitHub hosts only.

**Discarded:** server-side `git clone` (go-git or the git binary) — works for any host and extends to
private repos, but a larger outbound-fetch + subprocess surface than MVP needs; deferred as the
auth/private-repo extension. A **sandboxed node job** to do the clone (CP keeps zero egress) was
rejected as overkill — it needs a new async job type and node plumbing for a fetch the CP can do
directly in-process.

### 4.2 Input shape — URL + optional ref + optional subdir

**Chosen:** `url` required; `ref` (branch/tag/commit) optional, defaulting to the repo's default
branch; `subdir` optional, for repos where `SKILL.md` is nested or one repo holds several skills;
`name` optional, defaulting to the repo (or subdir leaf) name sanitized to a clean single path
segment (this becomes the skill's directory identity); `description` optional.

**Discarded:** URL-only (no ref/subdir) — fails on nested or multi-skill repos and gives no
reproducible pinning. URL + ref only — middle ground, but the example survey shows nested layouts are
common enough to want `subdir` in MVP.

### 4.3 Storage backend — Garage, not the relational store

**Chosen:** a dedicated Garage bucket (e.g. `spawnery-skills`), unencrypted. Lifts the 1 MiB DB-blob
ceiling, gives content-addressed dedup, and matches the node-pull pattern the transient journal tier
already uses. The CP already talks to Garage S3 directly (`internal/cp/fork.go` creates per-fork
buckets via `journal.S3Config` on `minio-go`), and nodes already pull from Garage on resume — both
legs are proven.

**Discarded:** keeping the `content BLOB` inline (the pre-change design) — simplest and Garage-free,
but caps skills at 1 MiB and bloats the relational store. A **hybrid** (inline under 1 MiB, Garage
above) was rejected to avoid two delivery code paths; URL skills commit fully to Garage.

### 4.4 Object key + ownership — global content-addressed, DB-authoritative ownership

**Chosen:** key `skills/<sha256(plain-tar)>.tar.zst` — global, so identical skills ingested by
different users store once. Ownership and provenance are tracked in the **catalog DB rows**
(creator-scoped; many rows may reference the same `sha256`). S3 object tags
(`source=<url@ref:subdir>`, `owner=<userId>`, `catalog_id`) are set best-effort on first write as a
Garage-side convenience, not the source of truth.

**Discarded:** owner-namespaced keys (`skills/<ownerId>/<sha256>.tar.zst`) — makes object-level
ownership 1:1 and taggable truthfully and enables trivial per-owner GC, but loses cross-user dedup.
The user chose to keep cross-user dedup; the DB already models ownership many-to-one, so object-level
tags being lossy/first-writer is acceptable.

### 4.5 Compression — zstd over the plain tar

**Chosen:** stored object is `tar.zst` (zstd, moderate level ~3 — good ratio, modest CPU/mem) via
`klauspost/compress/zstd` (already in the tree, pure-Go, streaming). **Identity and integrity hash
the plain (uncompressed) tar**, not the compressed bytes — zstd output is not byte-stable across
versions/levels, so hashing it would break content-addressing. The node zstd-decodes, then verifies
`sha256(plain tar)` before unpacking.

**Discarded:** storing the uncompressed tar (simpler, but larger objects) and gzip (worse ratio).
Hashing the compressed blob (rejected for non-determinism, as above).

### 4.6 Delivery + node credentials — presign-per-start

**Chosen:** the spawn row persists the durable `object_key` + `sha256`; the CP **presigns a fresh,
short-lived GET URL at every `StartSpawn`** (create/resume/migrate) via `minio-go`
`PresignedGetObject`. Nodes never hold Garage credentials for skills — they receive only
time-bounded GET URLs for specific objects. The node integrity-checks via the `sha256` in the by-ref
spec.

**Discarded:** giving nodes static read creds for the skills bucket (weaker posture for untrusted
self-hosted nodes; broader blast radius). Persisting a presigned URL on the spawn row (would expire
before a months-later resume — presign-per-start avoids this).

### 4.7 By-ref `ArtifactSpec` — re-add, non-sensitive only

**Chosen:** `ArtifactSpec` gains `oneof source { bytes inline | ObjectRef objectref }`, where
`ObjectRef = { string object_key; string sha256; }`. The by-ref variant is used **only for
non-sensitive skill payloads**; `manifest.json` and all sensitive artifacts stay inline. The node's
materialize step dispatches on the variant: inline → write bytes (today's path); objectref →
presigned GET → zstd-decode → sha256-verify → unpack.

**Discarded:** a parallel artifact list or a new message type — reusing `ArtifactSpec` with a `oneof`
keeps the assembly/delivery/materialize seams intact and the `content_type` (TAR) semantics unchanged.

### 4.8 RPC surface — new `IngestSkillFromURL`

**Chosen:** a new CP RPC `IngestSkillFromURL({url, ref?, subdir?, name?, description?}) →
{catalog_id}` that fetches/unpacks/validates/repacks/zstd/PutObjects, then writes a catalog row via
the **same creator-scoped store path as `CreateCatalogEntry`**. `CreateCatalogEntry` (raw inline
bytes) is left untouched for the existing `spawnctl --content-file` path. Validation is **fail-loud at
ingest** (`no SKILL.md found at <subdir>`, size-cap, host-not-allowed, fetch errors) — strictly better
than today's silent pass that only fails at `CreateSpawn`.

**Discarded:** overloading `CreateCatalogEntry` with an optional URL field (muddies a clean
raw-bytes RPC and mixes fetch concerns into it).

### 4.9 Fetch safety (the security-bearing core)

- **Host allowlist:** only `github.com` / `api.github.com` / `codeload.github.com`. Anything else,
  and any cross-host redirect, → `InvalidArgument`. This is the SSRF containment — the CP cannot be
  pointed at internal addresses.
- **URL normalization:** accept `https://github.com/owner/repo[.git][/tree/<ref>/<subdir>]`, parse
  `owner`/`repo` strictly, map to the tarball endpoint at the chosen `ref`.
- **Bounded fetch:** HTTP timeout + context cancel; `io.LimitedReader` on the compressed stream
  (reject > ~20 MiB on the wire); a decompressed-size + file-count ceiling (tar-bomb guard).
- **Unpack safety:** regular files + dirs only — reject symlinks, hardlinks, device nodes, absolute
  paths, and any `..` escape (same rule as `confineDestPath`).
- **Size cap:** the repacked **plain tar** must be ≤ a configurable bound (~50 MiB, up from the 1 MiB
  DB-blob limit); hard-reject over.

### 4.10 Web UI

In `ProfilesView`'s add-entry flow, skill stops being excluded (remove the `:76` comment/guard).
Instead an **"Add skill from GitHub URL"** dialog collects `url` (required) + optional
`ref`/`subdir`/`name`/`description`, calls `IngestSkillFromURL`, refreshes the catalog list, and
offers to attach the new entry to the profile being edited. Ingest failures surface as actionable
toasts. The existing capability preview (which agents a skill applies to) is reused unchanged.

### 4.11 Config

Skills bucket name, Garage endpoint/creds (reuse `JOURNAL_S3_*`), zstd level, wire/decompressed/tar
size caps, and the host allowlist are surfaced through the config framework
([config-framework-design](2026-06-20-config-framework-design.md)).

### 4.12 Testing

- **Hermetic units:** URL parse/normalization + allowlist rejection; tar-bomb / symlink / hardlink /
  `..` / absolute-path rejection; wrapper-strip + subdir descent; `SKILL.md`-presence; repack + zstd
  round-trip; `sha256` identity; the RPC with a **fake fetcher + fake S3** (no network).
- **Build-tagged e2e:** a real GitHub fetch (the example repo) + real Garage Put/presign/Get +
  node materialize + skill present in `~/.claude/skills/`. Per project convention it **fails, not
  skips**, when GitHub or Garage is unreachable (`t.Fatalf` naming the missing dep).

### 4.13 Out of scope (named explicitly)

Private repos / authenticated fetch (future, via the auth service which already holds GitHub OAuth
tokens); GC / refcounted reclamation of Garage skill objects (MVP bucket is append-only — accept the
leak); by-ref delivery for **sensitive** artifacts (stays inline E2E-sealed); non-GitHub git hosts;
raising the size cap further or a generic by-ref blob store.

## 5. Availability consequence

By-ref makes **Garage a hard runtime dependency** for ingesting or spawning with a URL skill,
including on resume — inline skills had no external dependency at provision time. This is consistent
with the transient journal tier, which already requires node↔Garage. A plain `just dev` without
`just garage` will not support URL skills; the dev-env ergonomics work
([dev-env-ergonomics-design](2026-06-20-dev-env-ergonomics-design.md)) could later fold Garage into
`just dev`.

## 6. Decomposition (proposed epic)

A new epic extending `sp-nrzf.3` (profiles) and `sp-l5sx` (substrate), with children roughly:

1. **Substrate by-ref `ArtifactSpec` + node materialize fetch** — proto `oneof source`, `make gen`;
   node presigned-GET → zstd-decode → sha256-verify → unpack (`internal/spawnlet`). Proto-touching:
   serialize.
2. **CP Garage skill store + `skillfetch` + RPC + catalog migration** — `skillfetch` pkg
   (allowlist/fetch/unpack/validate/repack/zstd); minio-go skill store (Put-if-absent + presign);
   `IngestSkillFromURL` RPC; catalog schema migration (`source_url/source_ref/source_subdir/sha256/
   size`, sqlite + pg). Proto-touching: serialize.
3. **CP assembly: emit by-ref skill payloads** — `profiles_assembly.go` emits `ObjectRef` specs for
   skills; presign at `StartSpawn`.
4. **Web UI** — "Add skill from GitHub URL" dialog; un-exclude skill kind.
5. **e2e** — build-tagged real-GitHub + real-Garage load-proof.
6. **Beads reconciliation** — close/realign `sp-nrzf.3.7`/`.8` (implemented but read OPEN).

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
