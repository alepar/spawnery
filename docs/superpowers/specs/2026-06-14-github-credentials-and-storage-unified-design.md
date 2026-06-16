# GitHub Credentials + Storage Backend - Unified Design

> **Status:** revised 2026-06-16 after empirical GitHub-App spikes. The MVP now explicitly accepts
> installation-selection-scoped GitHub App user tokens rather than relying on refresh-time
> `repository_id` narrowing. Keystone decisions are in the Revision Log (Section 0) and Decision Log
> (Section 15); spike verdicts are recorded in Section 14.
>
> **Round-3 revision (2026-06-16):** after a round-2 adversarial review (`superpowers:roast`, BLOCK),
> the custody posture is changed from **node-retains-refresh** to **AS-custodial refresh with
> CP-coordinated fanout**: the node never holds the refresh token; the AS is the sole rotation
> authority; the suspend backstop is deferred from MVP. **Section 16 is authoritative and supersedes
> Sections 1, 3, 3.1, 8 (credential path), 9, 10–11, and Decision 5/19 where contradicted.** Read
> Section 16 first.
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
- **Token blast radius.** Docs suggested a `repository_id` generation-time parameter could scope a
  user token to a **single repo**, but the empirical spike showed the load-bearing MVP path
  (`grant_type=refresh_token` + `repository_id`) does **not** narrow a rotated token. **Resolution:**
  the MVP stops promising GitHub-enforced per-repo token scope. Live access tokens are scoped by the
  App installation's repository selection; Spawnery enforces the bound repo through mount validation
  and an exact-repo credential helper (Sections 5, 7, 9).
- **Single-use refresh + availability.** Docs confirmed refresh tokens are strictly single-use (using
  one invalidates both the old refresh and old access token) and that expiry is an opt-out App setting.
  The previous strict owner-online, memoryless refresh had an unrecoverable loss window and no
  long-running-spawn story. **Resolution:** hybrid minting -- the node retains the owner-delivered
  refresh credential for the spawn's life and refreshes short-lived, installation-selection-scoped
  access tokens via a **new AS mint/refresh API** that keeps the App `client_secret` AS-side
  (Section 3). This is a deliberate, documented
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
production GitHub user-token path ships before these gates. Section 14 records the resolved spike
verdicts that shape the implementation contract.

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

### 3.1 Sustaining a running spawn (the AS mint/refresh API)

> **Superseded by §16.2–16.5 (round-3).** The node does **not** retain the refresh token; the AS is
> the durable custodian and sole refresher, and the node authenticates by node identity (not by
> presenting the refresh token). The flow below is retained for history; read §16 for the binding
> design.

The node, not the owner client, sustains a running spawn:

1. At spawn start the node receives the owner-sealed GitHub credential (refresh token tuple) over the
   A4-folded delivery path and **unseals it into the per-spawn secrets tmpfs, retaining it for the
   spawn's life** (see Section 9 for the at-rest tradeoff).
2. When the node needs a fresh access token (current one near/at its 8h expiry, or before a
   pre-pod/backend operation), it calls a **new AS mint/refresh API**, presenting the current refresh
   token plus expected target repo metadata (`host`, `owner`, `repo`, and `repository_id` when known).
3. The AS combines the presented refresh token with the App `client_secret` (which never leaves the
   AS), calls GitHub's token endpoint, and returns the rotated tuple (new access token + new refresh
   token) to the node. The returned access token is treated as **installation-selection-scoped**; the
   `repository_id` field is validation/audit metadata, not a GitHub narrowing guarantee.
4. The node atomically updates its retained refresh token (single-use rotation) and the credential
   provider's live access token.

**Single-use durability on the node<->AS channel.** Because each refresh rotates the refresh token, a
lost response would otherwise brick the spawn. The exchange is therefore **persist-before-confirm /
idempotent**: the node durably records the in-flight refresh attempt; the AS refresh is safe to retry;
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
| `app_metadata` | App/account metadata needed to verify the tuple and call the AS mint/refresh API. |

An access token (`access_token` / `access_expires_at`) may be cached transiently for an active link
but is not the durable credential; the node refreshes fresh installation-selection-scoped access
tokens via the AS mint/refresh API. There is **no** `permission_profile` field on the token -- see
Section 5; the token always carries the App's full permission set intersected with the user's access
and the App installation's repository selection.

The CP catalog stores non-secret metadata in cleartext as routing and validation facts:
`host`, `login`, `github_user_id`, `refresh_expires_at`, display name, and version. The sealed tuple
carries the same non-secret metadata so the owner client/node can verify that clear routing metadata
was not spliced.

`PutSecret(expected_version)` updates the sealed payload and clear metadata atomically. A refresh
writer that loses the CAS re-reads the newer tuple and discards stale results.

## 5. Permission Model (Spawnery-enforced, not GitHub-enforced)

**Doc-confirmed facts (deep-research, primary GitHub docs) plus spike correction:** user-to-server
tokens do not use OAuth scopes; they use the App's fine-grained permissions, fixed at App
registration; a token's effective permission is the intersection of App permissions, user
permissions, and installation repository selection; a single App cannot vary permissions per
authorization. `repository_id` can narrow some initial user-token generation flows, but the empirical
MVP refresh path did **not** narrow a rotated token.

Consequences for the design:

- The MVP ships **one GitHub App** whose registered permission set is the **union** of what backend
  operations need: `Contents: write` (clone/fetch/push) and -- validated by the repo-create spike --
  `Administration: write` (create). Users consent once.
- **"Permission profiles" are a Spawnery-enforced mount policy, not a GitHub token capability.** A
  mount declares whether it may create a missing repo; Spawnery's prepare logic enforces that policy
  before any GitHub call. GitHub does **not** enforce an `existing-repo` vs `repo-create` distinction
  -- the same token is technically capable of both; Spawnery is the gate.
- **Least privilege is achieved by installation selection plus Spawnery policy, not per-token GitHub
  repo scope.** The raw access token can reach repositories selected in the App installation. Normal
  Spawnery operation is bounded to the mount's repo by structured binding validation, backend access
  verification, and an exact-repo credential helper (Sections 7/9). This is a deliberate MVP
  relaxation; raw token exfiltration has installation-selection blast radius.

A mount that asks to create a missing repo while the configured App lacks `Administration: write`
fails before agent start with a typed "repo-create not available" error. The production MVP App keeps
that permission because the 2026-06-16 create spike passed.

The exact registered permission set is now tied to the spike verdicts and `github_e2e`: minimum
permissions for access verification, clone/fetch, push, create, and refresh rotation.
Current external docs record: expiring user tokens have an 8-hour access token and ~6-month refresh
token, refresh uses the App `client_secret`
([refreshing user access tokens](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/refreshing-user-access-tokens));
`POST /user/repos` requires `Administration: write`
([create repository for the authenticated user](https://docs.github.com/en/rest/repos/repos#create-a-repository-for-the-authenticated-user)).
The fine-grained permissions reference marks that endpoint with `x` for GitHub-App user tokens, but
the 2026-06-16 empirical spike proved a GitHub App user-to-server token can create a private
personal repo. `create_if_missing` remains in MVP (Section 14).

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
  initialize according to the app manifest/binding seed behavior; after create, verify the refreshed
  token can reach the new repo. The 2026-06-16 spike showed selected-repository installations
  immediately covered the newly-created repo, so create-capable links do not require an
  all-repositories install (Section 14).
- existing empty repo: leave empty unless the app manifest/binding says to seed or initialize

## 7. Delivery Model And Node Lifecycle

`github-token` attachments declare two consumers for one sealed tuple:

- `node-storage`: eligible for targeted pre-pod unseal for `github:` mount preparation, on-demand
  mint/refresh via the AS API, and suspend backstop.
- `agent-render`: an installation-selection-scoped access token rendered into the agent secrets tmpfs
  behind an exact-repo credential helper for normal `git`/`gh` use after startup. The agent receives
  an access token, never the durable refresh credential. The helper enforces the bound repo in normal
  operation; direct raw-token exfiltration retains installation-selection blast radius.

Storage credentials are not `spawn_artifacts` and are not `ArtifactTarget` container payloads.
GitHub storage credentials are carried by the A4-folded secret delivery path and mount binding
metadata.

`StartSpawn.secrets` is self-contained for the node. Each entry carries sealed bytes plus
non-secret routing metadata: secret id, type (`github-token`), version, delivery id, usages
(`node-storage`, `agent-render`), consuming mount names, render profile/paths, and clear metadata for
host/profile validation. **These rich routing fields plus the new AS mint/refresh API require proto
changes**
(today `SealedSecret={target_path,sealed,secret_id}` and `StartSpawn` has no secrets field); a single
bead owns the proto delta and proto-touching tasks are serialized (Section 13).

Lifecycle:

```text
StartSpawn(secrets, mounts)
  -> pre-pod unseal only declared node-storage github-token secrets referenced by github mounts;
     retain the refresh credential in per-spawn secrets tmpfs for the spawn's life
  -> refresh/mint an installation-selection-scoped access token via the AS mint/refresh API,
     carrying target repo metadata for validation/audit
  -> GitHub backend verify/clone/create/prepare using the tmpfs credential provider
  -> StartPod(sidecar)
  -> normal secret injection/rendering, including the agent's exact-repo GitHub helper config
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

The node-side credential path is explicit: a credential provider rooted at a **node-only directory**.
No plaintext token field is added to `spawnlet.Spawn`. The provider is atomically refreshed when a
mint rewrites the live access token.

> **Amended by §16.6 (round-3).** This directory must **not** be `SecretInjector.DirFor(spawnID)`:
> that path is bind-mounted into the agent container (`internal/spawnlet/manager.go:946-959`), so
> rooting node-storage credential material there would expose it to the agent (round-2 F30). The
> node-side provider uses a node-only directory; the `agent-render` token is delivered to the agent
> tmpfs separately.

Backend prepare:

1. Validate host support and token-host match.
2. Validate the mount's Spawnery create policy for the requested action (Section 5).
3. Refresh/mint an installation-selection-scoped access token via the AS API; include the bound repo
   metadata and `repository_id` as expected-target validation/audit data, not as a scope guarantee.
4. Verify repo access via GitHub REST and/or credentialed git fetch before `StartAgent`.
5. If repo exists, clone/fetch into the mount host dir.
6. If repo is missing and create is available + allowed, create a private user-owned repo, verify the
   live token covers the new repo, and initialize according to manifest/binding seed behavior.
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
- The helper returns a token only for exact `{host, owner, repo}` matches. This helper is the primary
  in-spawn boundary for normal Git operations.
- No `~/.git-credentials`.
- Node-side Git commands ignore or override repo-controlled credential helpers/config includes where
  feasible.

**At-rest tradeoff (documented).** The node retains the owner-delivered refresh credential in secrets
tmpfs for the spawn's life (needed for on-demand minting and the suspend backstop). This widens the
node-compromise exposure window from "zero immediately after inject" to session-lifetime. It is bounded
by: sealed in transit, tmpfs-only (never durable disk, never journaled), and wiped on spawn end. Accepted.

This hardening protects Spawnery-driven backend operations and normal in-spawn Git use. It does not
make a leaked raw GitHub token repo-scoped: the access token can reach any repository selected in the
App installation, subject to the App's permissions and the user's access. This raw-token exfiltration
blast radius is the accepted MVP relaxation. It is bounded by short-lived access tokens, GitHub's token
and grant revocation APIs, selected-repository installs, tmpfs-only storage, and no journal/log
persistence. Agent token availability is intentional capability and is documented as residual risk;
the egress reconciliation with the per-pod egress floor for the required github.com channel is noted in
Section 12/14.

## 10. Suspend Backstop

> **Deferred from MVP — see §16.7 (round-3).** Sections 10–11 are not implemented in MVP; for
> journaled `github:` mounts the Kopia journal already captures `.git`. MVP `github:` mounts MUST use
> a journaled durability class. The backstop machinery below moves to a follow-up epic and is retained
> here as its design baseline.

GitHub backstop is a spawnlet suspend mechanism. It preserves already-committed local Git work that
has not reached any **real remote branch**, by pushing it to a namespaced machine-ref namespace for
human recovery. It does **not** synthesize WIP commits for dirty working-tree state; dirty state and
committed work are both already in the Kopia journal (`.git` is journaled), so the backstop is a
**cross-node / human-recovery convenience layer, not the sole durability path**. Reconsidering a WIP
dirty-state backstop is tracked by backlog epic `sp-lqld`.

The node self-refreshes a token via the AS mint/refresh API at suspend time, so a system-triggered suspend
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
- `node-storage` pre-pod routing constraints; AS refresh treats `repository_id` as expected-target
  metadata, not as a scope guarantee
- credential provider rendering and atomic refresh rewrite; secrets-tmpfs journal-exclusion assertion
- exact-repo credential helper refuses non-bound repo URLs even when the raw token would be accepted
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
environment: App user-token handoff, the AS mint/refresh API (single-use rotation, installation-selection
scope, and an assertion that refresh+`repository_id` is **not** assumed to narrow), Spawnery
create-policy gating, actual clone/fetch/push/create behavior, newly-created-repo coverage, token/grant
revocation, and refresh API behavior. The lane is also where the Section 14 spike verdicts are
mechanized.

Token expiry tests use fake/forced-expiry paths for MVP. Do not add an 8-hour sleep test.

The required github.com egress channel must be reconciled with the per-pod egress floor in the
backend/egress design (whether the floor permits the node's mint/clone/push and the agent's
exact-repo helper push); this is called out as an integration requirement, not left implicit.

## 13. Beads Update Plan

After this spec lands:

- keep `sp-v40s` and `sp-u53.1`; update both epics' notes to point to this revised spec
- update `sp-v40s` children for: single-App union-permission set, the **new AS mint/refresh API**
  (installation-selection-scoped access tokens, single-use rotation, `client_secret` AS-side,
  `repository_id` as validation/audit metadata only), node-retained refresh credential, owner-side
  refresh + inactivity-based relink, response-wrap handoff hardening, web and spawnctl surfaces
- update `sp-u53.1` children for: Spawnery-enforced create policy (not GitHub permission profiles),
  installation-selection-scoped agent token behind an exact-repo helper, journal-excluded secrets tmpfs,
  backstop leaf-hash ref namespace, reachability detection, multi-branch partial-failure warnings,
  backstop enumeration/recovery, defined fail-closed outcomes
- **retire** the impl-design LWW/safety-ref scope on `sp-u53.1.3` (no push rails; agent owns real pushes)
- assign a single proto-owner bead for the `StartSpawn.secrets` fields + AS mint/refresh API; serialize
  proto-touching tasks
- assign a bead to own the GitHub agent-cred config render (`GH_CONFIG_DIR`/helper/`hosts.yml` ->
  journal-excluded tmpfs)
- update/close the Section 14 spike beads and add `github_e2e` backfill coverage for their verdicts
- add production-token dependencies from `sp-u53.1` to the `sp-7h6.1` gates named in Section 2
- record `sp-vd5w` as immediate prerequisite to merge first

## 14. Spike Verdicts And E2E Backfill

These empirical spikes were run on 2026-06-16 against a throwaway GitHub App before implementation.
The verdicts below are binding design inputs; `github_e2e` should later mechanize them so drift is
caught in CI/lane runs.

1. **Repo-create via user-to-server token: PASS.** A GitHub App user-to-server token with
   `Administration: write` and `Contents: write` successfully called `POST /user/repos` and created a
   private personal repo. MVP implication: keep `create_if_missing` for private user-owned repos and
   keep `Administration: write` in the App permission set, with Spawnery create policy as the product
   gate.
2. **Newly-created-repo coverage: PASS, with token-scope caveat.** With a selected-repositories App
   installation, a repo created via the user-to-server token was immediately listed in the
   installation's repositories and was clonable using an access token. Create-capable links do not
   require an all-repositories install. However, `grant_type=refresh_token` plus `repository_id` did
   **not** narrow the rotated token; the returned token could still access both selected repos. MVP
   implication: create-then-clone is feasible, but the design must use installation-selection-scoped
   live tokens plus Spawnery exact-repo controls.
3. **Token revocation: PASS.** `DELETE /applications/{client_id}/token` revoked a user-to-server
   access token before its 8h expiry. `DELETE /applications/{client_id}/grant` revoked the App
   authorization and broke further refresh. Single-use refresh invalidated the predecessor access
   token immediately. Independent re-link/new authorization did not invalidate an earlier still-valid
   token. MVP implication: the exfil kill-switch story is token revoke for a known access token and
   grant revoke for the whole GitHub App authorization; do not claim re-link alone kills every prior
   access token.
4. **Response-wrap nonce under a scaled AS: DESIGN RESOLVED.** Production multi-instance AS
   deployments must use a shared volatile response-wrap store with atomic redeem-and-delete,
   encrypted payloads, short TTL, auth-bound redemption, and no durable plaintext tuple persistence.
   Sticky/in-memory redemption is acceptable only for single-instance development.

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
5. **A new AS mint/refresh API** rotates the GitHub App user token with the App `client_secret`
   AS-side and returns short-lived, installation-selection-scoped access tokens; `repository_id` is
   expected-target validation/audit metadata only. The node retains the refresh credential for the
   spawn's life and is the single refresher for a running spawn; the node<->AS exchange is
   persist-before-confirm/idempotent to survive single-use rotation loss.
6. **A single GitHub App** with the union of permissions; there is no GitHub-enforced permission
   profile. "Profiles" are a **Spawnery-enforced mount create-policy**. Least privilege is achieved by
   selected-repository installations plus Spawnery binding/helper enforcement, not per-link permission
   selection or refresh-time `repository_id` scoping.
7. Mount bindings use structured repo identity and options, not URI query strings; repo references are
   fully qualified; MVP supports only `github.com`.
8. Existing org/other-owner repos are allowed only where the **App is installed** (access = the
   intersection of App, user, and installation); MVP repo creation is private user-owned only and
   enabled by the passing repo-create spike.
9. `node-storage` GitHub credentials may be unsealed pre-pod, only when declared by a `github:` mount;
   the agent receives an installation-selection-scoped access token via the `agent-render` route,
   never the refresh credential, behind a credential helper that only answers for the exact bound repo.
10. Storage credentials are not `spawn_artifacts`; `StartSpawn.secrets` carries self-contained routing
    metadata and sealed bytes; the rich fields + AS mint/refresh API require a proto delta with a
    single owner.
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
20. Empirical spikes (Section 14) are resolved: repo-create and selected-install new-repo coverage
    passed, refresh-time `repository_id` narrowing failed and drove the relaxed token-scope design,
    token/grant revocation passed, and scaled-AS nonce redemption requires a shared volatile store.

**Round-3 decisions (2026-06-16) — supersede 5 and 19 where contradicted:**

21. **AS-custodial refresh, CP-blind, AS≠CP.** The AS durably holds the refresh chain and is the sole
    rotation authority; the node holds only short-lived installation-selection-scoped access tokens,
    never the refresh token. The CP stays blind (owner-sealed copy for DR only). AS≠CP is a hard
    invariant: co-located only for single-tenant dev with a documented downgrade; multi-tenant/prod
    requires separate trust domains. (§16.2; resolves round-2 F2/F6/F30; accepts F4/F5.)
22. **Node→AS refresh is node-identity-bound.** The call authenticates with the Register/Heartbeat
    node identity (`nodeKeyCache`) plus CP confirmation that the node hosts the spawn; the access
    token is a link reference, not the authorization. Resolves off-node replay and resume-after-8h.
    (§16.3; resolves round-2 F7.)
23. **Refresh is proactive and CP-coordinated.** AS rotates+seals → notifies CP → CP fans out the
    sealed token to every node on the link → each node re-renders the agent token. Reactive-on-401 is
    impossible (git runs in the agent); a transparent git proxy is a tracked future upgrade, not MVP.
    (§16.4; resolves round-2 F11.)
24. **One link = one single-active shared access token** (spike 3: rotation invalidates the
    predecessor). The brief per-refresh stale window is accepted (in-flight agent git ops may 401 and
    are re-run; the next op self-heals via the credential helper). Routine revoke is skipped (rotation
    already kills the old token); the compromise kill switch is `DELETE /grant`. (§16.4–16.5.)
25. **Node-side credential provider lives in a node-only directory**, never the agent-bind-mounted
    `SecretsMountPath`; `agent-render` is delivered separately. (§16.6; resolves round-2 F30.)
26. **Suspend backstop deferred from MVP** to a follow-up epic; MVP `github:` mounts MUST use a
    journaled durability class so committed `.git` is cross-node durable; the key-independent WIP floor
    stays at `sp-lqld` with the key-dependency asymmetry stated. (§16.7; resolves round-2
    F16/F17/F18/F19/F20/F32.)
27. **AS is a sustained refresh-path dependency** (not bootstrap-only); clone2leak hardening
    (`gh>=2.63.0`, `protectProtocol`, `useHttpPath`, refuse repo-injected helpers) and an enforcing
    replay-guard CI gate are required; `github_e2e` mechanizes the relaxed-scope + custody assertions.
    (§16.8; resolves round-2 F22/F26/F36; documents F34.)

## 16. Round-3 Revision (2026-06-16): AS-custodial refresh, CP-coordinated fanout

This revision follows a round-2 adversarial review (`superpowers:roast`, **BLOCK** — 29 confirmed
findings; report `docs/superpowers/specs/2026-06-16-github-storage-backend-adversarial-review-r2.md`)
plus the 2026-06-16 spike verdicts. It **supersedes the node-retains-refresh custody posture** in
Sections 1, 3, 3.1, 8 (credential path), 9, 10–11, and Decision 5/19 where contradicted. The
storage-mechanics, scope-relaxation, and spike-verdict content elsewhere is unchanged.

### 16.1 Why the prior posture failed review
- The node-held refresh credential made node-compromise = whole-user-surface minting + owner lockout
  (F6), leaked the refresh token into the agent via the bind-mounted secrets tmpfs (F30, code-grounded
  at `internal/spawnlet/manager.go:946-959`), and could never be written back to durable owner custody
  after a node-side single-use rotation (F2). "Persist-before-confirm" was self-contradictory: a
  forgetful AS plus a tmpfs-only node cannot recover a lost rotation (F1/F12).
- Spike `sp-v40s.3` confirmed refresh rotation **immediately invalidates the predecessor access
  token**. With one shared link this makes independent per-spawn tokens impossible: **one link = one
  single-active access token shared by all of the user's concurrent spawns.**

### 16.2 Custody: AS-custodial, CP-blind, AS≠CP
- The **AS durably holds the refresh chain** and is the **sole rotation authority**. The node NEVER
  receives the refresh token — only short-lived, installation-selection-scoped **access tokens**.
- The CP stays **blind**: it stores only an owner-sealed copy of the credential for owner
  disaster-recovery / cross-device relink, never plaintext, and relays only sealed bytes during fanout.
- **AS≠CP is a hard trust-boundary invariant.** Co-located AS/CP is acceptable only for a single-tenant
  development deployment with a documented threat-model downgrade; any multi-tenant or production
  deployment MUST run the AS and CP in separate trust/deployment domains — a CP that can read the AS
  keystore collapses the residual "compromised CP cannot mint" property (round-2 F4).
- This pulls a bounded slice of `sp-6pqt` into MVP. Fully-CP-blind custody (no AS-readable refresh
  token) remains the `sp-6pqt` north star.
- **Accepted threat (round-2 F5):** an AS compromise exposes every live link's refresh chain. This is
  bounded by the AS≠CP isolation invariant and the grant-wide kill switch (§16.5) and is the explicit
  price of node+AS-to-sustain custody.

### 16.3 Node→AS authorization (node-identity-bound)
A node calling the AS mint/refresh API authenticates with its **established node identity** (the
Register/Heartbeat node certificate / signed sub-key already in the CP `nodeKeyCache`). The AS
authorizes a refresh only when (1) the node identity is valid, (2) the CP confirms that node currently
hosts the spawn/link, and (3) the presented link reference maps to a held refresh chain. The access
token is a link reference, not the authorization. Consequences: an access token stolen off-node cannot
drive a refresh (round-2 F7), and a spawn resumed after >8h (dead access token) can still authorize
its first refresh because node identity does not expire with the GitHub token.

### 16.4 Refresh flow (proactive, CP-coordinated fanout)
`git`/`gh` run in the **agent**, not the spawnlet, so the node cannot intercept a 401 to refresh
reactively (a transparent git proxy that would enable reactive refresh is a tracked future upgrade,
not MVP). MVP refresh is **proactive and CP-coordinated**:

1. A node, near the ~8h access-token expiry, requests a refresh from the AS (node-identity authZ,
   §16.3).
2. The AS **dedupes per link** (refreshed within a recent window → return the current shared token
   without re-rotating; else rotate once), seals the new access token per recipient, and **notifies
   the CP**.
3. The **CP fans out** the sealed access token to every node currently hosting a spawn on that link,
   over the existing Attach / sealed-delivery substrate (CP relays opaque ciphertext, stays blind).
   Each node atomically updates its node-side credential provider **and re-renders the agent's
   `agent-render` token file** (round-2 F11).
4. **No explicit revoke on routine refresh:** rotation already invalidated the predecessor (spike 3),
   so there is nothing to revoke and no make-before-break is possible with a single shared chain. The
   CP may notify the AS once all currently-connected nodes acknowledge the new token, but the targeted
   `DELETE /token` is redundant for a rotated token and is **skipped in MVP**. Nodes that were
   disconnected/suspended during a fanout re-sync the current token on reconnect/resume.

**Accepted per-refresh window.** At rotation the old shared token dies immediately, so until the
fanout reaches a node its in-flight agent git operations may `401`. With no reactive path these fail
and are re-run by the user; the **next** git invocation self-heals because the credential helper reads
the freshly re-rendered token from tmpfs. Refresh happens < once per 8h per link and the window is the
fanout latency. This is the accepted MVP cost; the git-proxy upgrade removes it.

**Agent must use the credential helper, not an embedded token.** The `agent-render` path installs a git
credential helper that answers only for the exact bound repo from journal-excluded tmpfs. This is both
the rotation-pickup mechanism (the next op reads the current token) and the journaled-`.git/config`
mitigation. Residual (round-2 F34): an untrusted agent can still embed a token into a journaled
`.git/config` via `git remote set-url https://<token>@…`; Spawnery cannot prevent this without the git
proxy. Documented residual.

### 16.5 Revocation / kill switch
- Routine generation supersession does **not** revoke — it would break sibling spawns sharing the live
  token, and rotation already kills superseded tokens. Tokens lapse by TTL after nodes migrate.
- The compromise kill switch is **`DELETE /applications/{client_id}/grant`** (grant-wide; it
  intentionally breaks all of the user's spawns and the refresh chain), proven by spike `sp-v40s.3`.

### 16.6 Backend credential-path correction (round-2 F30)
The node-side credential provider is rooted in a **node-only directory that is NOT bind-mounted into
the agent container**. Section 8's `SecretInjector.DirFor(spawnID)` is bind-mounted at
`SecretsMountPath` (`internal/spawnlet/manager.go:946-959`) and must not host node-storage credential
material. The `agent-render` token is delivered to the agent tmpfs separately.

### 16.7 Backstop deferred from MVP
The GitHub suspend backstop (Sections 10–11) is **deferred from MVP** to a follow-up epic. For
journaled `github:` mounts the Kopia journal already captures `.git`, so committed work is cross-node
durable without it (round-2 F20). Therefore **MVP `github:` mounts MUST use a journaled durability
class**; a non-journaled `github:` mount is rejected at bind validation until the backstop epic lands
(round-2 F19). The key-independent WIP-commit floor remains deferred to `sp-lqld`; the asymmetry is
explicit — in MVP dirty working-tree state is recoverable only from the owner-sealed (key-dependent)
journal (round-2 F18).

### 16.8 Availability and testing deltas
- The AS is now a **sustained refresh-path dependency** (a proactive refresh ~once per 8h per link plus
  fanout), not bootstrap-only (round-2 F26). A brief AS outage is tolerated within the proactive
  refresh budget; mint/refresh failures are fail-closed and retryable; deployments must state the AS
  availability target.
- clone2leak / CVE-2024-53858 (round-2 F22): node-side git pins `gh >= 2.63.0`, sets
  `credential.protectProtocol` and `credential.useHttpPath=true`, and refuses repo-injected
  `credential.helper` / config-includes (`.gitmodules`, `.lfsconfig`, `.git/config`) including
  recursive submodules. The untrusted agent's own git on hostile content retains documented residual
  exfil capability bounded by the installation-selection blast radius.
- The replay-guard release gate (round-2 F36) is an **enforcing test/CI gate** — the github real-token
  path must fail to build/run until the `sp-vd5w` / `sp-7h6.1.8` guards are present — not just a beads
  ordering note.
- The `github_e2e` lane is now runnable: the 2026-06-16 throwaway App (`app_id=4065493`) exists. The
  lane asserts installation-selection scope (refresh+`repository_id` does **not** narrow), node-identity
  authZ on refresh, CP-coordinated fanout + the accepted per-refresh window, and token/grant revocation.

## Post-Implementation Notes

*As this design is implemented and iterated on -- bug fixes, adjustments, anything that diverged from
the assumptions above -- append a dated note here, whether or not a formal debugging skill was used.*

- 2026-06-15: Resolved spike `sp-v40s.4` for horizontally scaled AS response-wrap redemption.
  Production multi-instance AS deployments must use a shared volatile response-wrap store with
  atomic redeem-and-delete semantics, encrypted payloads, short TTL, auth-bound redemption, and no
  durable plaintext tuple persistence. Sticky-session pinning is acceptable only as a single-instance
  or temporary development posture, not as the production scaling mechanism. See
  `deploy/authsvc/README.md`.
- 2026-06-16: Resolved spikes `sp-v40s.1` through `sp-v40s.3` against a throwaway GitHub App.
  Repo-create via GitHub App user-to-server token passed, selected-install new-repo coverage was
  immediate, token/grant revocation worked, and refresh rotation invalidated the predecessor token.
  The critical correction is that `grant_type=refresh_token` plus `repository_id` did **not** narrow
  the rotated token; the MVP design is relaxed to installation-selection-scoped access tokens with
  Spawnery exact-repo guards.
- 2026-06-16 (round-3): A round-2 `superpowers:roast` returned BLOCK (29 confirmed findings). The
  node-retains-refresh posture was the root of the worst findings (node-compromise blast radius,
  refresh-leak-to-agent, no rotation writeback). Reworked to **AS-custodial refresh + CP-coordinated
  fanout** (Section 16): the AS owns the refresh chain and is the sole refresher; the node holds only
  short-lived access tokens and authenticates by node identity; the CP fans out rotated tokens to all
  nodes sharing a link; the suspend backstop is deferred from MVP (journaled mounts required). The
  per-refresh stale window is accepted; a transparent git proxy is the tracked future upgrade.
