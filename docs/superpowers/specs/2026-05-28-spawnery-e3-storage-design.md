# Spawnery E3 — Storage Layer (Design)

**Bead:** `sp-u53`
**Status:** Draft v1 (interview complete; pending user review)
**Date:** 2026-05-28
**Depends on:** [E0](2026-05-26-spawnery-e0-contracts-design.md),
[E1](2026-05-27-spawnery-e1-runtime-core-design.md)

> **⚠️ Demo-MVP overlay** ([Demo MVP Scope](2026-05-28-spawnery-demo-mvp-scope.md)): the demo ships
> **three storage options** — **managed default** (Spawnery-hosted repo, web-browsable in-app +
> one-click download/clone), **BYO-cloud** (Google Drive/iCloud/OneDrive via `git bundle`), and
> **GitHub** (power-user). **Managed is the default** (zero-setup for non-techs, `sp-0a2`); spawned
> repos default **private**. The incremental-blob + debounced-persist notes (§5) are in scope.

Owns the data side of "your data stays yours": the uniform `/data` substrate, the provider-adapter
interface and implementations, provisioning, credential custody + refresh, persistence cadence,
conflict handling, and the on-repo layout.

---

## 1. Substrate & adapter interface

- **Universal substrate = a git repo of files.** The container always sees `/data` as a **git
  working tree** the agent reads/writes (the agent is, by construction, a coding-style agent).
- **Adapter interface** (in-process, inside the node agent; canonical signature in
  [E0 §8b](2026-05-26-spawnery-e0-contracts-design.md)):
  ```
  materialize(binding, dataDir) -> working tree at /data
  persist(binding, dataDir, checkpoint)
  capabilities() -> { gitNative | blob, ... }
  ```
- **Storage is per-App optional.** Apps that need data declare it via the manifest `storage`
  block (§9); Apps like zork can omit it.

---

## 2. Adapter implementations (MVP)

### 2a. GitHub-native (MVP-primary)
- Backed by **Spawnery's GitHub App** with **fine-grained, per-repo installation** (never the
  broad `repo` scope).
- **Binding URI:** `github:owner/repo`.
- **`materialize`:** `git clone` (or `git fetch` if cache present) into `/data`.
- **`persist`:** the agent makes **semantic commits**; on checkpoint the node `git push`es.

### 2b. Blob (Drive / OneDrive / iCloud)
- **Binding URI:** `gdrive:<id>` / `onedrive:<id>` / `icloud:<id>`.
- **`materialize`:** download the spawn's `.bundle` from the provider; `git clone` from the
  bundle into `/data`.
- **`persist`:** `git bundle create --all` into a fresh `.bundle`; upload, then swap (overwrite
  the binding). Human-readable mirror = post-MVP.
- **Order:** GitHub first; Drive is the second adapter shipped.

---

## 3. Credential custody & refresh

- **All durable storage creds live at the CP** (Spawnery's GitHub App private JWT signer;
  OAuth client secrets + per-user OAuth refresh tokens).
- At `createSpawn` the CP issues a **short-lived, repo-scoped access token** and delivers it to
  the node in the command. The node uses it; **never persists it**.
- **Refresh-on-demand:** the node calls `refreshStorageToken{spawnId, provider}` over its
  existing outbound gRPC stream when needed. For GitHub the CP mints a new installation token
  from the App JWT; for OAuth providers the CP uses its stored refresh token to mint a new
  access token. **MVP = reactive refresh on 401** (cheap, correct); proactive refresh near
  `exp` is a nicety.
- **Privacy posture:** the CP *can* mint tokens scoped to your authorized repo — inherent to a
  one-click GitHub-App grant. It relays tokens to the node, which does the git ops. Durable
  refresh creds stay at the CP. (User-held / E2E-sealed storage creds for self-host-style
  storage privacy → post-MVP, see TODO.)

---

## 4. Provisioning flow (spawn create)

1. Client calls `POST /spawns` (App@sha, agent, model, storage choice + provider auth).
2. **CP provisions the destination** via the provider API:
   - GitHub: create a new private repo in the owner's account via the App.
   - Blob: create the spawn's folder + an empty initial `.bundle`.
3. CP records the binding (URI + scope) in the CP index.
4. CP issues a fresh short-lived storage token and sends `createSpawn` to the node, carrying:
   App@sha, agent + tools, model config, storage binding + token, and the bridge/node session
   keys (E0 §10).
5. **Node clones the empty destination into `/data`**, scaffolds from the App's **`storage.seed`**
   (copied out of `App@sha`), writes **`.spawnery/spawn.yml`** (§6), commits, **pushes**.
6. Spawn enters ACTIVE on the first session connect.

---

## 5. Persist mechanics & cadence

- **Cadence:** agent makes **semantic commits** as part of its work (via a `git`-tool in the
  bundled common toolset); the node **persists per completed agent turn**, and again on
  **idle/teardown / explicit close** as a safety net. One-turn data-loss window.
- **Debounce + background (roast `sp-7fj`):** persistence is **debounced/coalesced** (a short
  window, or batched across rapid turns) and the push runs **in the background** so it never
  blocks the next turn; the idle/teardown flush guarantees durability. This bounds GitHub
  push frequency (rate-limit pressure) without widening the loss window meaningfully.
- **Blob is NOT full-bundle-per-turn (roast `sp-7fj`):** `git bundle --all` re-uploads the entire
  history every turn → cost/latency grow linearly with repo size. MVP blob writes an
  **incremental bundle** (objects since the last persisted ref) and only periodically (or on
  teardown) consolidates to a full `--all` baseline. Full folder-as-git-remote incremental object
  transfer stays the post-MVP target. *(For the demo, GitHub-only storage sidesteps this entirely.)*
- **Token freshness:** the node checks the in-memory token's `exp`; on push failure or
  near-expiry, calls `refreshStorageToken` and retries. (Proactive refresh near `exp` + jitter to
  avoid refresh storms is the post-MVP hardening.)

---

## 6. On-repo layout

```
<data repo root>
├── README.md                          ← App-declared user-visible content (e.g. wiki landing)
├── <App data paths>                   ← from spawneryapp.yml `storage.schema`
└── .spawnery/                         ← Spawnery internals
    ├── spawn.yml                      ← spawn metadata (source of truth — see E0 §4)
    ├── threads/                       ← optional conversation history (per spawn.yml flag)
    └── snapshots/                     ← pre-upgrade git tags / safety snapshots
```

- **App data lives at the repo root** at paths the App declares — so when the user browses the
  repo on GitHub they see their actual files (Markdown wiki pages, etc.), not Spawnery internals.
- **Spawnery internals are under `.spawnery/`** (single hidden dir).
- **`.spawnery/spawn.yml`** is the source of truth for the spawn configuration (E0 §4).
- **Conversation threads** at `.spawnery/threads/` if persisted (per `spawn.yml.conversation`).
- **Snapshots:** before an auto-upgrade migration touches `/data`, the node creates a git
  **tag** at the pre-upgrade commit (recorded under `.spawnery/snapshots/`) → free rollback.

---

## 7. Conflict handling

Spawns are single-writer + ephemeral, so concurrent writes from another agent process are not
the common case. The realistic conflict is the **user editing the repo externally** between
sessions.

- **Detection:** on persist, detect **non-fast-forward** (GitHub) or bundle-version mismatch
  (blob).
- **Policy (MVP):** **last-write-wins**, **but surfaced** to the user via an ACP notification
  ("your repo had changes since the spawn started; we updated based on the spawn's view — diff
  here"). No silent overwrite without notification.
- **Auto-merge is post-MVP.**

---

## 8. Manifest additions (extends E0 §3)

The App manifest gains a `storage` block:

```yaml
storage:
  required: true                # false for e.g. zork
  schema: ./storage-schema.md   # documentation of the expected layout under /data
  seed: ./seed/                 # scaffold dir copied verbatim on first materialize
```

E0 §3 is updated to reference this block under E3's authority (instead of "owned by E3, not
specified here").

---

## 9. Deferred (post-MVP)

User-held / E2E-sealed storage creds (storage-privacy max) · readable mirror for blob providers ·
folder-as-git-remote (incremental object transfer) · bidirectional sync with external edits ·
auto-merge on conflict · GitLab / Gitea / self-hosted git adapters · DB-backed storage adapter
for high-write Apps · proactive token refresh near `exp`.

---

## Appendix — E3 decision log

| # | Decision | Choice |
|---|---|---|
| E3.1 | Storage cred custody | CP-managed; **short-lived** tokens delivered in `createSpawn`; refresh-on-demand from CP via the node's gRPC stream (reactive on 401, MVP) |
| E3.2 | Provisioning | CP creates the destination (GitHub repo / blob folder) + records binding; node clones empty + scaffolds from `storage.seed` + writes `spawn.yml` + commits + pushes |
| E3.3 | On-repo layout | `.spawnery/` for internals (spawn.yml, threads/, snapshots/); App data at the repo root via `storage.schema` |
| E3.4 | Adapters MVP (asserted) | GitHub-native first; blob (Drive) second (`git bundle` only); storage optional per App |
| E3.5 | Persist cadence (asserted) | Semantic commits + persist per completed turn + idle/exit autosave; one-turn loss window |
| E3.6 | Conflict (asserted) | Non-fast-forward → **last-write-wins, surfaced via ACP**; no auto-merge in MVP |
| E3.7 | Snapshots (asserted) | Pre-upgrade git tag under `.spawnery/snapshots/` for free rollback |
