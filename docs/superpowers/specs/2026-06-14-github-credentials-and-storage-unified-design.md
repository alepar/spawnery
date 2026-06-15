# GitHub Credentials + Storage Backend - Unified Design

> **Status:** revised 2026-06-15 after adversarial review (`superpowers:roast`, BLOCK) plus a
> `deep-research` pass on the GitHub-App token model. Keystone decisions re-made with the user;
> see the Revision Log (Section 0) and Decision Log (Section 15). Repo-create and three smaller
> facts remain gated on empirical spikes (Section 14).
>
> **Beads:** `sp-v40s` (GitHub token provisioning), `sp-u53.1` (GitHub storage backend),
> gated by `sp-7h6.1` children and `sp-vd5w`.
>
> **Supersedes where contradicted:** [GitHub Storage Backend - Implementation Design](2026-06-14-github-storage-backend-impl-design.md)
> and its blocked assumptions. In particular this spec **retires the impl-design's last-write-wins
> real-branch overwrite + pre-overwrite safety-ref scheme** (see Section 8 / Decision 14).
>
> **Builds on:** [User Secrets Store - Owner-Online Auto-Injection](2026-06-14-user-secrets-store-design.md),
> [Owner-Sealed Secrets](2026-06-10-owner-sealed-secrets-design.md),
> [Transient Tier - Kopia Journal + Migration](2026-06-10-transient-tier-kopia-journal-design.md),
> [Auth & Identity](2026-06-11-auth-identity-design.md).

## 0. Revision Log

This spec was BLOCKed by an adversarial review. The blocking findings and their resolutions:

- **GitHub App permission model (keystone).** `deep-research` against primary GitHub docs confirmed
  user-to-server tokens use the App's **fine-grained permissions fixed at registration**, NOT
  per-authorization OAuth scopes; a single App cannot issue two permission sets. The previous
  `existing-repo` / `repo-create` "re-linkable permission profiles on one App" design was infeasible.
  **Resolution:** one App with the union of permissions; permission *profiles* become a
  **Spawnery-enforced mount policy**, not a GitHub-enforced token capability (Section 5).
- **Token blast radius.** Docs confirmed a `repository_id` generation-time parameter that scopes a
  user token to a **single repo**. **Resolution:** the agent's token is minted scoped to exactly the
  bound repo (Sections 7, 9), converting the "broad token to untrusted agent" finding into a bounded one.
- **Single-use refresh + availability.** Docs confirmed refresh tokens are strictly single-use (using
  one invalidates both the old refresh and old access token) and that expiry is an opt-out App setting.
  The previous strict owner-online, memoryless refresh had an unrecoverable loss window and no
  long-running-spawn story. **Resolution:** hybrid minting -- the node retains the owner-delivered
  refresh credential for the spawn's life and mints short-lived, repo-scoped tokens via a **new AS
  mint API** that keeps the App `client_secret` AS-side (Section 3). This is a deliberate, documented
  relaxation of strict CP-blind custody (fully-CP-blind custody stays deferred to `sp-6pqt`).
- **Conflict handling contradiction.** Resolved in favor of this spec's **no-push-rails** model:
  Spawnery never force-updates a user's real branch; the agent owns real pushes; Spawnery writes only
  namespaced backstop machine refs (Section 8, Decision 14). The impl-design's LWW/safety-ref scope is
  retired.
- **Durability of committed work.** The Kopia journal captures `.git` (committed-but-unpushed work is
  already in the owner-sealed journal), so the GitHub backstop is a **cross-node / human-recovery
  convenience layer**, not the sole durability path (Section 10).

## 1. Problem

GitHub-backed storage and GitHub token provisioning are one security boundary, not two separable
implementation details. The storage backend needs a user GitHub token to clone, verify, create,
and suspend-backstop repos. The token provisioning system must preserve the core user-secrets
property as far as the chosen availability posture allows.

**Custody posture (MVP).** The owner is online to **bootstrap** a link (capture + seal the GitHub
credential) and to start a spawn (deliver the sealed credential). Thereafter a node **sustains** the
spawn by minting short-lived access tokens through the AS. The strict "a compromised CP can never
read or mint user tokens" property is therefore **relaxed by design**: the AS holds the App
`client_secret` and, when a node presents the owner-delivered refresh token, can mint user tokens for
that actively-running spawn. The App `client_secret` never leaves the AS; nodes never hold it. Fully
CP-blind custody (no AS-side minting) is out of MVP and tracked by backlog epic `sp-6pqt`.

This spec is the single design contract for GitHub credentials plus the GitHub storage backend.
Implementation remains split by bead ownership:

- `sp-v40s` owns GitHub token provisioning, the App permission set, the AS mint/refresh API, and
  web/spawnctl surfaces.
- `sp-u53.1` owns GitHub storage backend mechanics, mount binding, prepare/provision, suspend
  backstop, and recovery.
- `sp-7h6.1.*` owns the generic CP-blind secret catalog and delivery substrate.

## 2. Binding Dependencies And Gates

Production GitHub-token handling is blocked until the generic secret-delivery floor is ready:

- `sp-vd5w` lands first: `SealedSecret` carries `version` and `delivery_id`; node reconstructs
  in-flight AAD from wire fields instead of guessing zero values.
- `sp-7h6.1.8` still owns the stateful guards: per-live-generation version monotonicity and
  delivery-id-once.
- `sp-7h6.1.9` folds owner-online delivery into the A4 intent round-trip.
- `sp-7h6.1.4` renders and injects secrets for the node/agent/sidecar consumers described here.
- `sp-7h6.1.11` replaces the no-op node revocation checker before real user tokens are sealed to
  nodes in production.

Backend mechanics may be built and tested earlier behind local/static-token seams, but no
production GitHub user-token path ships before these gates **and** before the Section 14 spikes
resolve (in particular the repo-create spike gates `create_if_missing`).

## 3. Custody Model

GitHub custody is **owner-online-to-bootstrap, node+AS-to-sustain** for MVP. The AS never durably
stores plaintext GitHub access or refresh tokens; the durable credential lives owner-sealed in the
CP-blind secrets store.

Initial token capture uses a response-wrapping handoff:

1. Owner starts GitHub link/re-link from web or `spawnctl`.
2. AS performs the GitHub App OAuth code exchange.
3. AS holds the resulting tuple only in process memory behind a short-lived, single-use nonce.
4. AS redirects the owner client; the nonce is delivered to the client **without landing in the
   redirect URL / browser history / Referer / AS access logs** (e.g. via an authenticated fetch the
   client initiates, not a query-string nonce). The redemption path must resist a leaked-nonce replay.
5. Owner client redeems the nonce over an authenticated same-site request.
6. Owner client immediately seals the tuple into the CP-blind user secrets store.

The wrapped tuple must not be written to AS DB rows, logs, redirect URLs, crash reports, or metrics.
If the AS process restarts before redemption, the owner repeats the link flow. (The single-use,
in-memory nonce assumes a single AS instance or sticky session for the redemption window; a
horizontally-scaled AS must share the nonce out-of-band or pin redemption to the issuing instance --
tracked as a deployment constraint, see Section 14.)

### 3.1 Sustaining a running spawn (the AS mint API)

The node, not the owner client, sustains a running spawn:

1. At spawn start the node receives the owner-sealed GitHub credential (refresh token tuple) over the
   A4-folded delivery path and **unseals it into the per-spawn secrets tmpfs, retaining it for the
   spawn's life** (see Section 9 for the at-rest tradeoff).
2. When the node needs a fresh access token (current one near/at its 8h expiry, or a per-mount
   `repository_id`-scoped token is required), it calls a **new AS mint API**, presenting the current
   refresh token plus the target `repository_id`.
3. The AS combines the presented refresh token with the App `client_secret` (which never leaves the
   AS), calls GitHub's token endpoint, and returns the rotated tuple (new access token + new refresh
   token, repo-scoped where requested) to the node.
4. The node atomically updates its retained refresh token (single-use rotation) and the credential
   provider's live access token.

**Single-use durability on the node<->AS channel.** Because each mint rotates the refresh token, a lost
mint response would otherwise brick the spawn. The mint exchange is therefore **persist-before-confirm
/ idempotent**: the node durably records the in-flight refresh attempt; the AS mint is safe to retry;
on an ambiguous outcome the node reconciles (if GitHub already rotated, adopt the new tuple; only a
genuinely lost rotation forces a relink). A single running spawn has exactly **one** refresher (its
node), so the web+spawnctl concurrent-refresh race does not apply to running spawns.

### 3.2 Owner-side refresh (no spawn running)

When no spawn is running to sustain the link, the owner client refreshes:

1. Owner client unseals the stored refresh token locally and calls the AS mint/refresh API.
2. AS uses `client_secret` to rotate and returns the new tuple; AS forgets it.
3. Owner client updates the `github-token` secret with strict CAS.
4. If a refresh call's result is lost, the client re-reads the secret; if the stored version is now
   newer, treat as a benign concurrent-refresh race; otherwise surface `relink_required`.

**Re-link triggers are inactivity/breakage based, not a periodic cliff.** Each successful refresh
issues a new refresh token that resets the ~6-month window, so an actively-used link never hits the
cliff. Re-link is required only after **>6 months of no refresh** (window actually elapsed) or a
**broken/revoked chain** (a refresh that GitHub rejects).

Configurable AS-side/durable custody beyond this posture is out of MVP and tracked by backlog epic
`sp-6pqt`.

## 4. GitHub Token Shape

`github-token` is one sealed, atomically versioned tuple. The durable owner-sealed credential is the
refresh token (plus metadata needed to mint); access tokens are ephemeral and minted on demand. The
sealed payload includes:

| field | notes |
|---|---|
| `host` | MVP supports `github.com`; future GitHub Enterprise splice guard. |
| `login` | GitHub login for rendered `gh` config and UX. |
| `github_user_id` | Immutable numeric user id when available. |
| `refresh_token` | GitHub App user refresh token (durable credential; single-use, rotates). |
| `refresh_expires_at` | ~6-month window; reset on each refresh. Warn/re-link on inactivity. |
| `app_metadata` | App/account metadata needed to verify the tuple and call the mint API. |

An access token (`access_token` / `access_expires_at`) may be cached transiently for an active link
but is not the durable credential; the node mints fresh, `repository_id`-scoped access tokens via the
AS mint API. There is **no** `permission_profile` field on the token -- see Section 5; the token always
carries the App's full permission set intersected with the user's access.

The CP catalog stores non-secret metadata in cleartext as routing and validation facts:
`host`, `login`, `github_user_id`, `refresh_expires_at`, display name, and version. The sealed tuple
carries the same non-secret metadata so the owner client/node can verify that clear routing metadata
was not spliced.

`PutSecret(expected_version)` updates the sealed payload and clear metadata atomically. A refresh
writer that loses the CAS re-reads the newer tuple and discards stale results.

## 5. Permission Model (Spawnery-enforced, not GitHub-enforced)

**Doc-confirmed facts (deep-research, primary GitHub docs):** user-to-server tokens do not use OAuth
scopes; they use the App's fine-grained permissions, fixed at App registration; a token's effective
permission is the intersection of App permissions, user permissions, and installation repository
selection; a single App cannot vary permissions per authorization; `repository_id` can narrow a token
to one repo at mint time.

Consequences for the design:

- The MVP ships **one GitHub App** whose registered permission set is the **union** of what backend
  operations need: `Contents: write` (clone/fetch/push) and -- gated on the repo-create spike --
  `Administration: write` (create). Users consent once.
- **"Permission profiles" are a Spawnery-enforced mount policy, not a GitHub token capability.** A
  mount declares whether it may create a missing repo; Spawnery's prepare logic enforces that policy
  before any GitHub call. GitHub does **not** enforce an `existing-repo` vs `repo-create` distinction
  -- the same token is technically capable of both; Spawnery is the gate.
- **Least privilege is achieved by token scoping, not permission selection.** Per-mount tokens are
  minted `repository_id`-scoped to the bound repo (Section 7/9), so a given spawn's token cannot reach
  other repos even though the App's registered permission set is broad.

A mount that asks to create a missing repo while the App lacks `Administration: write` (i.e. the
repo-create spike failed and create was not enabled) fails before agent start with a typed
"repo-create not available" error.

The exact registered permission set is a **binding spike before production** (Section 14): minimum
permissions for access verification, clone/fetch, push, create (if feasible), and refresh rotation.
Current external docs record: expiring user tokens have an 8-hour access token and ~6-month refresh
token, refresh uses the App `client_secret`
([refreshing user access tokens](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/refreshing-user-access-tokens));
`POST /user/repos` requires `Administration: write`
([create repository for the authenticated user](https://docs.github.com/en/rest/repos/repos#create-a-repository-for-the-authenticated-user)),
but the fine-grained permissions reference marks that endpoint with `x` for GitHub-App user tokens --
**creation via a user-to-server token is unproven** and gated (Section 14).

## 6. Mount Binding And Repo Identity

GitHub mount binding uses structured fields, not security-sensitive URI query strings. Repo
identity is always fully qualified:

```text
{ host: "github.com", owner: "<owner-or-org>", repo: "<repo>" }
```

MVP accepts only `host == "github.com"`; other hosts return `unsupported_host`.

Binding fields:

- repo identity `{host, owner, repo}`
- `credential_secret_id`
- `create_if_missing` (the mount-level create policy enforced by Spawnery per Section 5)
- manifest/binding-controlled seed/initialize behavior
- durability class from the existing manifest model

Repo owner does not need to match the credential login. Because token access is the intersection of
App permissions, user access, and installation (doc-confirmed), reaching an existing org or
other-account repo requires the **App to be
installed** on that account and the user to have access; access verification (Section 8) surfaces the
typed failure when the App is not installed there. Repo creation is MVP-conservative: private
user-owned repos only, `create_if_missing=true`, the mount's create policy allowed, and the
repo-create spike passed. Org repo creation is deferred.

Missing repo behavior:

- missing + `create_if_missing=false`: fail before agent start
- missing + `create_if_missing=true` (and create available): create private user-owned repo, then
  initialize according to the app manifest/binding seed behavior; after create, mint/refresh the token
  so it can reach the new repo (see the newly-created-repo-coverage spike, Section 14)
- existing empty repo: leave empty unless the app manifest/binding says to seed or initialize

## 7. Delivery Model And Node Lifecycle

`github-token` attachments declare two consumers for one sealed tuple:

- `node-storage`: eligible for targeted pre-pod unseal for `github:` mount preparation, on-demand
  minting via the AS API, and suspend backstop.
- `agent-render`: a `repository_id`-scoped access token rendered into the agent secrets tmpfs for
  normal `git`/`gh` use after startup. The agent receives a **repo-scoped** token, not the durable
  refresh credential and not an installation-wide token.

Storage credentials are not `spawn_artifacts` and are not `ArtifactTarget` container payloads.
GitHub storage credentials are carried by the A4-folded secret delivery path and mount binding
metadata.

`StartSpawn.secrets` is self-contained for the node. Each entry carries sealed bytes plus
non-secret routing metadata: secret id, type (`github-token`), version, delivery id, usages
(`node-storage`, `agent-render`), consuming mount names, render profile/paths, and clear metadata for
host/profile validation. **These rich routing fields plus the new AS mint API require proto changes**
(today `SealedSecret={target_path,sealed,secret_id}` and `StartSpawn` has no secrets field); a single
bead owns the proto delta and proto-touching tasks are serialized (Section 13).

Lifecycle:

```text
StartSpawn(secrets, mounts)
  -> pre-pod unseal only declared node-storage github-token secrets referenced by github mounts;
     retain the refresh credential in per-spawn secrets tmpfs for the spawn's life
  -> mint a repository_id-scoped access token via the AS mint API for each github mount
  -> GitHub backend verify/clone/create/prepare using the tmpfs credential provider
  -> StartPod(sidecar)
  -> normal secret injection/rendering, including the agent's repo-scoped GitHub config
  -> StartAgent
```

Pre-pod unseal is narrow. Generic app secrets, MCP secrets, and BYOK sidecar keys remain in the
normal pre-agent injection path. If pre-pod prepare fails, the node wipes the per-spawn secrets tmpfs
before returning an error.

Replay state for delivered secrets is in-memory per `(spawn_id, generation, secret_id)` for MVP:
highest accepted version and seen delivery ids. Generation fencing requires fresh delivery on
resume/recreate and prevents cross-generation opens.

**Failure outcomes (defined, not implied):**

- **Partial secret delivery** (a referenced secret is unsealable at start): the in-container
  `secretwait` times out and the spawn **fails closed with a typed error**; the per-spawn secrets
  tmpfs is wiped.
- **AS unavailable** at mint time (or for the node-revocation check): **fail closed** -- the spawn
  cannot obtain/refresh a token and surfaces a typed, retryable error rather than proceeding.
- **Owner-offline mid-run** no longer forces capability loss, because the node self-refreshes via the
  AS API. An `Errored` start/resume caused by a transient mint/AS failure is **re-drivable** once the
  dependency recovers; only a genuinely lost refresh rotation (relink required) is terminal. The
  resume path states which case applies.

## 8. Backend Mechanics

The GitHub backend uses direct GitHub REST for API actions and plain `git` for clone/fetch/push.
It does not depend on `gh` for backend mechanics. `gh` config is rendered for the agent's
convenience only.

The node-side credential path is explicit: a credential provider rooted at
`SecretInjector.DirFor(spawnID)`. No plaintext token field is added to `spawnlet.Spawn`. The
provider reads only from the per-spawn secrets tmpfs and is atomically refreshed when a mint rewrites
the live access token.

Backend prepare:

1. Validate host support and token-host match.
2. Validate the mount's Spawnery create policy for the requested action (Section 5).
3. Mint a `repository_id`-scoped access token via the AS API for the bound repo.
4. Verify repo access via GitHub REST and/or credentialed git fetch before `StartAgent`.
5. If repo exists, clone/fetch into the mount host dir.
6. If repo is missing and create is available + allowed, create a private user-owned repo, mint a
   token covering the new repo, and initialize according to manifest/binding seed behavior.
7. If repo is missing without create available/allowed, fail typed before agent start.

Node-daemon GitHub egress is allowed only for declared `github:` mounts using bound `node-storage`
credentials. This is a deliberate node-trusted action.

**Agent-driven Git is not intercepted, and Spawnery does not write user branches.** After
`StartAgent`, native Git semantics apply; the agent/user is responsible for real branch pushes.
Spawnery does **not** force-update real branches, run last-write-wins overwrite of a user branch,
create pre-overwrite safety refs for agent pushes, or wrap git in push rails. (This retires the
superseded impl-design's LWW scheme; the suspend backstop in Section 10 is the only thing Spawnery
writes to the remote, and it writes only namespaced machine refs.)

## 9. Credential Hardening

Spawnery-delivered GitHub credential material lives only in secrets tmpfs and node memory.
Spawnery must not write token plaintext to env vars, durable disk, user home directories, data
mounts, or Kopia snapshots.

**Journal exclusion (load-bearing).** Because the Kopia journal captures `.git` and the agent home,
all rendered GitHub credential material (`GH_CONFIG_DIR`, the git credential helper config, `hosts.yml`,
the retained refresh credential) lives in a **secrets tmpfs that is explicitly excluded from the Kopia
journal**, so no token reaches a snapshot at rest.

Rendered config:

- `GH_CONFIG_DIR` points into the journal-excluded secrets tmpfs.
- Git credential config points into the same tmpfs; `credential.useHttpPath=true`.
- The helper returns a token only for exact `{host, owner, repo}` matches, and the token itself is
  `repository_id`-scoped to that repo, so even direct token use cannot reach other repos.
- No `~/.git-credentials`.
- Node-side Git commands ignore or override repo-controlled credential helpers/config includes where
  feasible.

**At-rest tradeoff (documented).** The node retains the owner-delivered refresh credential in secrets
tmpfs for the spawn's life (needed for on-demand minting and the suspend backstop). This widens the
node-compromise exposure window from "zero immediately after inject" to session-lifetime. It is bounded
by: sealed in transit, tmpfs-only (never durable disk, never journaled), and wiped on spawn end. Accepted.

This hardening protects Spawnery-driven backend operations. The `repository_id`-scoped agent token
reduces -- but does not eliminate -- exfiltration capability: a user-authorized agent can still use its
repo-scoped token against the one bound repo (push to it, read it). Agent token availability is
intentional capability and is documented as residual risk; the egress reconciliation with the per-pod
egress floor for the required github.com channel is noted in Section 12/14.

## 10. Suspend Backstop

GitHub backstop is a spawnlet suspend mechanism. It preserves already-committed local Git work that
has not reached any **real remote branch**, by pushing it to a namespaced machine-ref namespace for
human recovery. It does **not** synthesize WIP commits for dirty working-tree state; dirty state and
committed work are both already in the Kopia journal (`.git` is journaled), so the backstop is a
**cross-node / human-recovery convenience layer, not the sole durability path**. Reconsidering a WIP
dirty-state backstop is tracked by backlog epic `sp-lqld`.

The node self-refreshes a token via the AS mint API at suspend time, so a system-triggered suspend
while the owner is offline can still authenticate the backstop push (this resolves the prior
owner-offline backstop-auth gap).

Backstop detection is reachability-based across all local branches/refs, not tracking-status-based:

- fetch/list remote refs
- for each local branch/ref, compute commits reachable locally but not remotely
- branches without upstreams are still considered
- detached HEAD commits are considered when not remote-reachable

Backstop refs are non-branch machine refs:

```text
refs/spawnery/backstop/<yyyy-mm-dd>/<spawn-id>/<generation>/<branch-name>/<branch-hash>
```

`<branch-name>` is sanitized and length-bounded for quick human identification. `<branch-hash>` (the
branch tip commit) is the **leaf** path component, which both avoids collisions and avoids the
directory/file ref conflict a branch-name-leaf scheme would hit (`feat` vs `feat/x` coexist because the
hash, not the branch name, is the leaf). Pushing a backstop ref is idempotent when the tip is unchanged.
Full original branch name, source commit, produced ref, and recovery metadata are persisted separately
keyed by `(spawn_id, generation, mount)`; the ref path is helpful but not the source of truth.

**Multi-branch atomicity.** A suspend may push N ahead-branches as separate ref updates. Pushes are
**best-effort per branch**; per-branch success/failure is recorded in the `SuspendComplete` structured
warnings (Section 11), so a mid-loop failure leaves a recorded, recoverable state rather than a silent
partial result.

GC is conservative and date-prefix based:

- keep backstop refs while the spawn exists
- after spawn deletion, default 30-day retention
- active-spawn refs are pruned only when superseded by a newer successful durability point for the
  same generation/branch
- GC is best-effort; inability to authenticate to GitHub during GC is warning-only. (GC after spawn
  deletion has no live spawn to mint through; the GC credential path is an open item -- Section 14.)

## 11. Suspend Failure, Recovery, And Backstop Surfacing

Suspend order is:

1. final Kopia snapshot
2. GitHub backstop

If the final snapshot fails, suspend fails normally. If final snapshot succeeds and GitHub backstop
fails, suspend fails closed unless owner-sealed journal durability is already complete for that
mount. The exception requires a successful owner-sealed final snapshot and durable CP-held
owner-sealed journal-key ciphertext. When it applies, suspend may complete with structured degraded
warnings: GitHub committed-work backstop failed, but owner-sealed journal recovery is available.

`SuspendComplete` carries structured warning records (including per-branch backstop results). The CP
persists them and UI/CLI surfaces them. Logs alone are not sufficient.

**Backstop refs are not write-only.** On resume (and on demand), Spawnery **enumerates** the backstop
refs for `(spawn_id, generation, mount)`, re-associates them with their original branch names from the
persisted recovery metadata, and **surfaces them to the user** as recovery options. There is a defined
read/recovery path, not just a write path.

On resume, owner-sealed journal restore is authoritative. If journal restore fails, Spawnery presents
the failed journal generation/mount, error details, a likely transient/permanent classification when
possible, and the available GitHub backstop refs. The user chooses: retry journal restore, recover
from backstop, start from repo, or abort/keep suspended. Spawnery does not auto-merge, auto-checkout,
or clobber anything.

All Garage/Kopia data, journal metadata, recovery records, and UI references are keyed by
`(spawn_id, generation, mount)`. Starting a later generation may be allowed by user choice, but it
must never overwrite or hide a failed generation's snapshot lineage.

## 12. Testing

Hermetic tests:

- `github-token` tuple CAS and metadata mirroring
- owner-side refresh race handling; re-link trigger logic (inactivity/breakage, not periodic cliff)
- node<->AS mint idempotency / persist-before-confirm (lost-response reconcile, no brick on single-use)
- mount binding validation; unsupported host rejection
- Spawnery-enforced create-policy gating (mount may/may-not create)
- `node-storage` pre-pod routing constraints; `repository_id`-scoped mint per mount
- credential provider rendering and atomic refresh rewrite; secrets-tmpfs journal-exclusion assertion
- replay high-water/delivery-id guards
- reachability-based backstop detection, including no-upstream branches and detached HEAD
- backstop ref leaf-hash scheme (no `feat`/`feat/x` D/F conflict); multi-branch partial-failure warnings
- structured suspend warnings; backstop enumeration + resume-time surfacing
- generation-keyed recovery metadata
- fail-closed outcomes: partial secret delivery, AS-down at mint, AS-down at revocation check

Local/static-token Git server tests cover backend mechanics without the production OAuth/mint path:
existing repo clone/fetch/push, missing repo fail-closed, create-if-missing through a fake/local
provider seam, suspend backstop ref production, GC behavior.

`github_e2e` lane covers real GitHub semantics and fails loudly when opted into without required
environment: App user-token handoff, the AS mint API (`repository_id`-scoped + single-use rotation),
Spawnery create-policy gating, actual clone/fetch/push/create behavior, and refresh API behavior. The
lane is also where the Section 14 spikes are mechanized once an App exists.

Token expiry tests use fake/forced-expiry paths for MVP. Do not add an 8-hour sleep test.

The required github.com egress channel must be reconciled with the per-pod egress floor in the
backend/egress design (whether the floor permits the node's mint/clone/push and the agent's
repo-scoped push); this is called out as an integration requirement, not left implicit.

## 13. Beads Update Plan

After this spec lands:

- keep `sp-v40s` and `sp-u53.1`; update both epics' notes to point to this revised spec
- update `sp-v40s` children for: single-App union-permission set, the **new AS mint API**
  (`repository_id`-scoped, single-use rotation, `client_secret` AS-side), node-retained refresh
  credential, owner-side refresh + inactivity-based relink, response-wrap handoff hardening, web and
  spawnctl surfaces
- update `sp-u53.1` children for: Spawnery-enforced create policy (not GitHub permission profiles),
  `repository_id`-scoped agent token, journal-excluded secrets tmpfs, backstop leaf-hash ref namespace,
  reachability detection, multi-branch partial-failure warnings, backstop enumeration/recovery, defined
  fail-closed outcomes
- **retire** the impl-design LWW/safety-ref scope on `sp-u53.1.3` (no push rails; agent owns real pushes)
- assign a single proto-owner bead for the `StartSpawn.secrets` fields + AS mint API; serialize
  proto-touching tasks
- assign a bead to own the GitHub agent-cred config render (`GH_CONFIG_DIR`/helper/`hosts.yml` ->
  journal-excluded tmpfs)
- file the Section 14 spikes as blocking pre-impl gates on `sp-u53.1` / `sp-v40s`
- add production-token dependencies from `sp-u53.1` to the `sp-7h6.1` gates named in Section 2
- record `sp-vd5w` as immediate prerequisite to merge first

## 14. Spikes (pre-implementation gates)

These are empirical (not doc-answerable) and gate production implementation. Each is filed as a
blocking bead.

1. **Repo-create via user-to-server token (THE gate).** Question: can a GitHub App user-to-server
   token with `Administration: write` call `POST /user/repos` to create a *personal* repo, despite the
   docs' `x` marker? Cheapest test: register a throwaway App, complete the user-authorization flow,
   call the endpoint. Kill criteria: if it cannot, **drop `create_if_missing` from MVP** (require the
   repo to pre-exist) and remove `Administration: write` from the App permission set.
2. **Newly-created-repo coverage.** Question: after creating a repo (or for a "selected repositories"
   installation), is the new repo immediately reachable by a freshly minted token, or must the
   installation's repo selection update first? Lever: require an **"all repositories" installation** for
   create-capable links. Kill criteria: if neither auto-coverage nor a programmatic selection update is
   possible, create-then-clone in one prepare flow is infeasible -- gate create accordingly.
3. **Token revocation.** Question: is there an API to revoke a user-to-server access token (or the App
   authorization) before its 8h expiry, and does re-link/refresh invalidate previously issued access
   tokens beyond the single-use-refresh case? Informs the exfil kill-switch story.
4. **Response-wrap nonce under a scaled AS.** Question (deployment): how is the single-use in-memory
   nonce redeemed when the AS is horizontally scaled (shared store vs sticky redemption)? Resolve
   before multi-instance AS deployment.

(Branch-protection / ruleset bypass for force pushes is **out of scope** for Spawnery -- it never pushes
user branches; an agent's own pushes to a protected branch are the agent/user's concern.)

## 15. Decision Log

1. One unified spec for GitHub credentials plus GitHub storage; existing implementation epics remain.
2. Custody is owner-online-to-bootstrap, node+AS-to-sustain; strict fully-CP-blind custody deferred to
   `sp-6pqt`. The App `client_secret` never leaves the AS.
3. Initial OAuth token handoff uses single-use in-memory response wrapping, with the nonce kept out of
   the redirect URL/history/logs; redemption resists leaked-nonce replay.
4. The durable owner-sealed credential is the (single-use, rotating) refresh token; access tokens are
   ephemeral and minted on demand. Refresh resets the ~6-month window; relink is inactivity/breakage
   based, not a periodic cliff.
5. **A new AS mint API** mints `repository_id`-scoped, short-lived access tokens; the node retains the
   refresh credential for the spawn's life and is the single refresher for a running spawn; the
   node<->AS exchange is persist-before-confirm/idempotent to survive single-use rotation loss.
6. **A single GitHub App** with the union of permissions; there is no GitHub-enforced permission
   profile. "Profiles" are a **Spawnery-enforced mount create-policy**. Least privilege is achieved by
   `repository_id` token scoping, not per-link permission selection.
7. Mount bindings use structured repo identity and options, not URI query strings; repo references are
   fully qualified; MVP supports only `github.com`.
8. Existing org/other-owner repos are allowed only where the **App is installed** (access = the
   intersection of App, user, and installation); MVP repo creation is private user-owned only and
   gated on the repo-create spike.
9. `node-storage` GitHub credentials may be unsealed pre-pod, only when declared by a `github:` mount;
   the agent receives a `repository_id`-scoped access token via the `agent-render` route, never the
   refresh credential or an installation-wide token.
10. Storage credentials are not `spawn_artifacts`; `StartSpawn.secrets` carries self-contained routing
    metadata and sealed bytes; the rich fields + AS mint API require a proto delta with a single owner.
11. Node backend uses REST + plain git; `gh` is agent convenience only.
12. **No push rails** for agent-driven Git; Spawnery never force-updates or LWW-overwrites a user
    branch. The impl-design LWW/safety-ref scheme is retired. Spawnery writes only namespaced backstop
    machine refs.
13. Backstop preserves committed local work to machine refs for human recovery; the Kopia journal
    (which captures `.git`) is the actual durability path, so the backstop is a cross-node/recovery
    convenience, not the sole guarantee. No WIP commit floor in MVP (`sp-lqld`).
14. Backstop refs live under `refs/spawnery/backstop/<yyyy-mm-dd>/<spawn-id>/<generation>/<branch-name>/<branch-hash>`
    with the tip-hash as the leaf (collision- and D/F-conflict-safe); pushes are best-effort per branch
    with per-branch results in `SuspendComplete`.
15. Backstop refs have a defined enumeration/recovery path surfaced at resume; they are not write-only.
16. GitHub backstop failure can be downgraded only when owner-sealed journal final snapshot and
    owner-sealed key custody succeeded.
17. Journal restore failure is user-directed; all journal data/metadata is generation-keyed and must
    not be overwritten by later generations.
18. Defined fail-closed outcomes: partial secret delivery, AS-down at mint, AS-down at revocation
    check; owner-offline mid-run is re-drivable (node self-refreshes), only a lost rotation is terminal.
19. Credential material lives only in a **journal-excluded** secrets tmpfs; the node retains the refresh
    credential for the spawn's life (documented at-rest tradeoff), wiped on spawn end.
20. Empirical spikes (Section 14) gate production: repo-create feasibility (THE gate), newly-created-repo
    coverage, token revocation, and the scaled-AS nonce redemption.

## Post-Implementation Notes

*As this design is implemented and iterated on -- bug fixes, adjustments, anything that diverged from
the assumptions above -- append a dated note here, whether or not a formal debugging skill was used.*

- 2026-06-15: Resolved spike `sp-v40s.4` for horizontally scaled AS response-wrap redemption.
  Production multi-instance AS deployments must use a shared volatile response-wrap store with
  atomic redeem-and-delete semantics, encrypted payloads, short TTL, auth-bound redemption, and no
  durable plaintext tuple persistence. Sticky-session pinning is acceptable only as a single-instance
  or temporary development posture, not as the production scaling mechanism. See
  `deploy/authsvc/README.md`.
