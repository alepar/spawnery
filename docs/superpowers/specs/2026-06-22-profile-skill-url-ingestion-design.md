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
(`https://api.github.com/repos/{owner}/{repo}/tarball/{ref}`, which **302-redirects to
`codeload.github.com`** — a cross-host hop on the happy path, see §4.9 for how the SSRF rule must
permit it per-hop), gunzips, strips GitHub's `owner-repo-<sha>/` wrapper dir, and (if `subdir` given)
descends into it. No `git` binary, no subprocess; egress is narrowed to GitHub hosts only.

**Auth + rate limits (roast §4.1/§4.13).** Unauthenticated `api.github.com` is capped at **~60
requests/hour per source IP**; a single CP egresses from one IP, so unauthenticated ingest lets any
one user starve all tenants. MVP therefore: (a) support an **optional `GITHUB_TOKEN`** (raises the
shared budget to 5000/hr) via config; (b) a **per-user ingest quota + backoff**; (c) surface GitHub
`429` as an actionable error, not an opaque failure. ETag/`304` caching is a noted follow-up, not
MVP.

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

**URL parsing rules (roast §4.2/§4.9).** `ref` and `subdir` are **first-class explicit inputs**;
the canonical input is `owner/repo` (or `https://github.com/owner/repo`) **plus** the separate `ref`
and `subdir` fields. A `/tree/<ref>/<subdir>` deep URL is *ambiguous* — GitHub branch names legally
contain `/` (`release/1.2`), so `tree/release/1.2/skills/x` cannot be split into ref vs subdir
without enumerating refs. MVP therefore: accept bare `owner/repo` and `https://github.com/owner/repo`
(strip `.git`, trailing slash, `?query`, `#fragment`); **reject** `/tree/...` and `/blob/...` deep
paths with an actionable error ("paste the repo URL and set ref/subdir explicitly") rather than
mis-parsing. **Default-branch resolution:** omit `{ref}` from the tarball endpoint entirely —
GitHub's `/tarball` (no ref) serves the default branch, avoiding an extra rate-limited
`repos/{owner}/{repo}` call.

**Discarded:** URL-only (no ref/subdir) — fails on nested or multi-skill repos and gives no
reproducible pinning. URL + ref only — middle ground, but the example survey shows nested layouts are
common enough to want `subdir` in MVP.

### 4.3 Storage backend — Garage, not the relational store

**Chosen:** a dedicated Garage bucket (e.g. `spawnery-skills`), unencrypted. Lifts the 1 MiB DB-blob
ceiling, gives content-addressed dedup, and matches the node-pull pattern the transient journal tier
already uses.

> **Correction (roast §4.3/§3).** An earlier draft claimed "the CP already talks to Garage S3
> directly … both legs are proven." That is **false**: `internal/cp/fork.go` only *computes bucket
> names* (`forkBucketName`) — it instantiates no S3 client. The only `minio.New` is node-side
> (`internal/storage/journal/genkey.go`) and in e2e tests. **The CP has never spoken S3.** So the
> CP-side minio client, `MakeBucket`, `PutObject`, and `PresignedGetObject` are all **new code on a
> component with no S3 history** — not a proven leg. This is gated by spike **S1** (§7) and the
> §6.2 task is scoped accordingly. The node→Garage leg (journal restore) is genuinely proven, but
> see spike **S2** — it does not prove the *presign-from-CP* path.

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

**Catalog-row idempotency (roast §4.4/§4.8).** The Garage object is content-addressed and dedups,
but the catalog table does not — an RPC retry, UI double-submit, or re-pasted URL would create
duplicate rows. Add a **unique constraint on `(owner, sha256)`**; `IngestSkillFromURL` returns the
**existing `catalog_id` on conflict** (idempotent). The dedup hit-rate this assumes is itself gated on
the deterministic-repack fix in §4.5 / spike S3 — without it, `sha256` is not stable and neither
object nor row dedup fires.

### 4.5 Compression — zstd over the plain tar

**Chosen:** stored object is `tar.zst` (zstd, moderate level ~3 — good ratio, modest CPU/mem) via
`klauspost/compress/zstd` (a **new direct dependency** — it is currently only an *indirect* dep via
minio-go, imported nowhere in first-party code; pure-Go, streaming). **Identity and integrity hash
the plain (uncompressed) tar**, not the compressed bytes — zstd output is not byte-stable across
versions/levels, so hashing it would break content-addressing. The node zstd-decodes, then verifies
`sha256(plain tar)` before unpacking.

**The repack MUST be canonical/deterministic (roast §4.5/§4.4 — major).** `sha256(plain tar)` is only
a stable content identity if the tar bytes are reproducible. Go's `archive/tar` and a naive repack
encode per-entry **mtime, uid/gid, mode, and filesystem-walk order**; GitHub tarball entries carry
commit/extraction mtimes and directory order is not guaranteed, so the *same skill tree* repacked
twice (or fetched at two commits with an identical tree) yields *different* bytes → different
`sha256` → dedup never fires and the no-GC bucket (§4.13) grows unbounded. The repack therefore
canonicalizes: **zero mtime (SOURCE_DATE_EPOCH-style), uid/gid = 0, normalized mode (0644 files /
0755 dirs preserving the executable bit only), entries sorted by path, no PAX/GNU extension
variance.** Verified by spike **S3** (§7).

**Discarded:** storing the uncompressed tar (simpler, but larger objects) and gzip (worse ratio).
Hashing the compressed blob (rejected for non-determinism, as above).

### 4.6 Delivery + node credentials — presign-per-start

**Chosen:** the spawn row persists the durable `object_key` + `sha256`; the CP **presigns a fresh
GET URL at every `StartSpawn`** (create/resume/migrate) via `minio-go` `PresignedGetObject`. Nodes
never hold Garage credentials for skills — they receive only time-bounded GET URLs for specific
objects. The node integrity-checks via the `sha256` in the by-ref spec.

The roast surfaced four sharp edges here (three major); the design pins each:

- **TTL with cold-start headroom + node-side re-presign (roast §4.6 — major).** "Short-lived" is
  under-specified and collides with the cold resume/migrate path the design exists for: between
  presign-at-StartSpawn and the node's actual GET there can be an image pull + scheduling queue
  (**minutes**). A too-short TTL 403s before the fetch starts, and nodes hold no creds to re-sign.
  Fix: TTL = **30 min** (well above the cold-node StartSpawn→first-byte budget measured in spike
  **S5**) **and** a **node→CP re-presign RPC** on `403 expired` / transient fetch failure (bounded
  retries). One of the two would do; we take both — the TTL covers the common case, re-presign covers
  the long tail without a heroic TTL.
- **Presign signed-host must equal the node-reachable Garage host (roast §4.6 — major, S2).** SigV4
  binds the signature to the exact endpoint host + region + addressing style. The CP's *internal*
  Garage endpoint (reused `JOURNAL_S3_*`, e.g. `garage:3900`) may be neither routable from nor
  signature-valid at an untrusted self-hosted node. The CP MUST presign against the **node-reachable
  Garage hostname** (a distinct config value from the CP's internal endpoint), **path-style**, fixed
  region. Verified by spike **S2**.
- **Offline presign + object-missing contract (roast §4.6/§4.13 — major + minors).**
  `PresignedGetObject` computes a local HMAC and **never contacts Garage**, so the CP mints URLs (and
  reports `StartSpawn` success) even when Garage is down or the object is absent — the failure only
  appears at the node GET. The design defines the catch: the node maps a **`404` to a terminal
  `skill object missing` spawn error** (distinct from a retryable **connection error =
  `Garage unreachable`**), surfaced to the user; a missing-object spawn is failed cleanly, not left
  half-created. An optional CP-side **HEAD before presign** is a cheap upgrade to fail at StartSpawn
  instead of at the node (deferred unless S1/S2 make it free).
- **The presigned URL is a bearer capability** relayed CP→node over ACP; bound the TTL as above and
  **redact it from request logs**. Impact is low (non-sensitive object, per-start URL) but stated.

**Discarded:** giving nodes static read creds for the skills bucket (weaker posture for untrusted
self-hosted nodes; broader blast radius). Persisting a presigned URL on the spawn row (would expire
before a months-later resume — presign-per-start avoids this).

### 4.7 By-ref `ArtifactSpec` — re-add, non-sensitive only

**Chosen:** `ArtifactSpec` gains an **added sibling field** `ObjectRef objectref = 9` (presence
discriminates: `objectref` set → by-ref, else inline), where `ObjectRef = { string object_key;
string sha256; string presigned_url; }` (the CP fills `presigned_url` at StartSpawn; `object_key`/
`sha256` are the durable identity). The by-ref variant is used **only for non-sensitive skill
payloads**; `manifest.json` and all sensitive artifacts stay inline. The node's materialize step
dispatches on presence: inline → write bytes (today's path); objectref → presigned GET → bounded
zstd-decode → sha256-verify → unpack.

**Corrected from oneof (roast §4.7 — minor but right).** An earlier draft used
`oneof source { bytes inline | ObjectRef objectref }`. That is a **breaking change**: it renames the
generated Go field, so every `&ArtifactSpec{Inline: x}` construction (≥5 sites:
`internal/cp/artifacts.go:92,134`, `profiles_assembly.go:79,132`, `server.go:908`) and ~30 `.Inline`
reads stop compiling — the opposite of the "seams intact" property the choice was justified by. A
plain **added field** (presence-discriminated) preserves all existing inline sites.

**Node-side bounded decode (roast §4.7/§6.1 — minor, defense-in-depth).** The reused
`unpackTar` (`internal/spawnlet/artifacts.go:150`) is marked safe only because inline artifacts are
≤1 MiB; its symlink/hardlink/device and `..`/absolute-path rejection (via `safeRel`) **does** cover
by-ref too. But by-ref payloads are up to ~50 MiB plain tar and **zstd-decode is unbounded**, and
`sha256` can only be verified *after* full decode (identity is over the plain tar) — so a corrupt or
high-ratio object would OOM an untrusted self-hosted node before the hash gate fires. Fix: a
**streaming `io.LimitedReader` on the zstd-decoded stream and the per-file copy**, bounded by the
same ~50 MiB ceiling, aborting before exhaustion.

**Discarded:** a parallel artifact list or a new message type — reusing `ArtifactSpec` keeps the
assembly/delivery/materialize seams and the `content_type` (TAR) semantics unchanged.

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

- **Host allowlist, per-hop (roast §4.1-vs-§4.9 — major, fixes a self-contradiction):** the tarball
  endpoint *requires* a 302 to `codeload.github.com`, so "reject all cross-host redirects" would make
  every ingest fail. The correct rule: **each redirect hop's target host must re-pass the allowlist**
  (`github.com` / `api.github.com` / `codeload.github.com`), and the **resolved IP of every hop must
  not** fall in private / loopback / link-local / cloud-metadata (169.254.0.0/16, 100.64.0.0/10,
  RFC1918, `::1`, fc00::/7) ranges. Any hop failing either check → `InvalidArgument`. This is the
  actual SSRF containment (per-hop + IP-range), not a blanket cross-host ban.
- **URL normalization:** accept `https://github.com/owner/repo[.git][/tree/<ref>/<subdir>]`, parse
  `owner`/`repo` strictly, map to the tarball endpoint at the chosen `ref`.
- **Bounded fetch, enforced *streaming* (roast §4.9):** HTTP timeout + context cancel;
  `io.LimitedReader` on the compressed stream (reject > ~20 MiB on the wire). The decompressed-size +
  file-count ceiling MUST be enforced by a `LimitedReader` **wrapping the gzip reader before tar
  parsing** — gzip's ~1032:1 max ratio means 20 MiB on the wire can inflate to ~20 GB, so a
  check-after-buffering would OOM the CP before it fires. Peak memory stays bounded.
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

**Skill-name collision (roast §4.10 — major).** The skill `name` becomes the on-disk
`~/.claude/skills/<name>/` directory (`agentinstall` installs into `<home>/skills/<a.Name>/`,
`internal/agentinstall/skill.go`), and `profiles_assembly.go` does **not** dedup entry names. Common
auto-derived leaf names (`skill`, `skills`, `claude-skill`) collide across repos, and two distinct
skills sharing a `name` in one profile would silently overwrite/merge at install. Fix: **reject
duplicate skill directory names within a single profile** (a uniqueness check at profile-assembly,
fail-loud). Additionally, reconcile the catalog `name` with the **`SKILL.md` frontmatter `name`** at
ingest: default the catalog name *from* the frontmatter when present, and warn on mismatch — so the
install directory matches the skill's declared identity (discoverability/invocation).

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
tokens); GC / refcounted reclamation of *completed* Garage skill objects (MVP bucket is append-only —
accept the leak); by-ref delivery for **sensitive** artifacts (stays inline E2E-sealed); non-GitHub
git hosts; raising the size cap further or a generic by-ref blob store; ETag/`304` ingest caching.

**Not deferrable — incomplete multipart uploads (roast §4.4/§4.13 — minor).** minio-go `PutObject`
auto-switches to multipart above ~16 MiB, and a ~50 MiB tar of incompressible content can exceed
that. An aborted/failed ingest leaves dangling MPU parts that are **not** a completed object, **not**
content-addressed, and **not** reclaimable by re-ingest — a distinct leak the "completed-object
append-only" stance does not cover, and Garage does not abort stale MPUs by default. Fix (MVP):
`AbortMultipartUpload` on `PutObject` error in the skill store, and/or a Garage bucket MPU-expiry
lifecycle.

## 5. Availability consequence

By-ref makes **Garage a hard runtime dependency** for *ingesting* or *first-creating* a spawn with a
URL skill — inline skills had no external dependency at provision time. This is consistent with the
transient journal tier, which already requires node↔Garage. A plain `just dev` without `just garage`
will not support URL skills; the dev-env ergonomics work
([dev-env-ergonomics-design](2026-06-20-dev-env-ergonomics-design.md)) could later fold Garage into
`just dev`.

**Resume may NOT need Garage — gate on spike S4 (roast §5/§4.6 — major).** The draft asserted the
dependency holds "including on resume." That may be **self-inflicted**: `agentinstall` writes the
skill into `~/.claude/skills/<dir>/`, and if the journal snapshot captures the agent rootfs, the
skill files are **already present** on a journaled restore — re-fetching from Garage at resume is
redundant and needlessly turns a Garage outage into a resume failure for every URL-skill spawn.
Spike **S4** decides: if a suspend→resume with Garage *stopped* still comes up with the skill, the
by-ref materialize is **gated to first-create only** and resume/migrate skip it. This also defuses
the worst case of the TTL/object-missing findings (§4.6) — they then bite only at first create, never
on a months-later resume.

## 6. Decomposition (proposed epic)

A new epic extending `sp-nrzf.3` (profiles) and `sp-l5sx` (substrate), with children roughly:

1. **Substrate by-ref `ArtifactSpec` + node materialize fetch** — proto `oneof source`, `make gen`;
   node presigned-GET → zstd-decode → sha256-verify → unpack (`internal/spawnlet`). Proto-touching:
   serialize.
2. **CP Garage skill store + `skillfetch` + RPC + catalog migration** — *gated on spikes S1/S2/S3.*
   The CP-side minio client is **net-new** (the CP has never spoken S3). `skillfetch` pkg
   (per-hop-allowlisted streaming fetch + GitHub token/rate-limit + canonical repack + zstd); minio-go
   skill store (Put-if-absent + `AbortMultipartUpload` on error + presign against the node-reachable
   host); `IngestSkillFromURL` RPC (idempotent on `(owner,sha256)`); catalog schema migration
   (`source_url/source_ref/source_subdir/sha256/size` + unique `(owner,sha256)`, sqlite + pg).
   Proto-touching: serialize.
3. **CP assembly: emit by-ref skill payloads** — `profiles_assembly.go` emits `ObjectRef` specs for
   skills; presign at `StartSpawn`.
4. **Web UI** — "Add skill from GitHub URL" dialog; un-exclude skill kind.
5. **e2e** — build-tagged real-GitHub + real-Garage load-proof.
6. **Beads reconciliation** — close/realign `sp-nrzf.3.7`/`.8` (implemented but read OPEN).

## 7. Pre-implementation spikes (from the roast)

The roast (2026-06-22, **REVISE** — 0 blockers, 13 majors confirmed 3/3) surfaced load-bearing
assumptions to de-risk *before* building. Each is cheap and decisive:

- **S1 — CP can drive Garage S3 at all (kills the feasibility unknown).** The CP has never spoken
  S3. Write ~30 lines: build a `minio.Client` from `JOURNAL_S3_*` against `just garage`,
  `MakeBucket(spawnery-skills)` + `PutObject` a small object. *Kill criteria:* can't construct a
  working client → the §6.2 scope and the whole Garage approach need rethink.
- **S2 — CP-presigned GET is node-usable.** `cl.PresignedGetObject(bucket, key, 30m, nil)`, then
  plain `http.Get` the URL from a separate process **with no creds**, against the **node-reachable**
  Garage address (not the CP's internal endpoint). *Kill criteria:* `403 SignatureDoesNotMatch` /
  host unroutable → CP must presign against a separate node-facing hostname (path-style, fixed
  region); if unfixable, fall back to node-held read creds.
- **S3 — deterministic repack.** Repack the same extracted skill dir twice via the planned code;
  `diff` the two `sha256`. Then repack two commits with a byte-identical tree. *Kill criteria:*
  hashes differ → the canonicalization in §4.5 (zero mtime/uid/gid, sorted entries) is mandatory
  before ship, else dedup + the unique `(owner,sha256)` constraint are fiction.
- **S4 — does resume need Garage?** Suspend then resume a URL-skill spawn with Garage **stopped**.
  *Kill criteria:* it resumes with the skill present → gate by-ref materialize to first-create only
  (§5); it does not → keep the resume dependency and the §4.6 TTL/re-presign protocol must cover
  resume.
- **S5 — cold-node StartSpawn→first-byte latency.** Measure p99 time from StartSpawn to the node's
  first Garage GET on a **cold image cache**; set the §4.6 TTL above it (with the re-presign retry as
  backstop).

S1–S3 are hermetic-ish (dev distrobox + `just garage`); S4–S5 need the e2e lane. Per the roast's
REVISE verdict, these are de-risking spikes, not blockers — but S1 and S3 gate real correctness and
should run first.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
