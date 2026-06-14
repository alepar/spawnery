# GitHub Credentials + Storage Backend - Unified Design

> **Status:** approved in collaborative design 2026-06-14; adversarial review deferred to a
> separate session.
>
> **Beads:** `sp-v40s` (GitHub token provisioning), `sp-u53.1` (GitHub storage backend),
> gated by `sp-7h6.1` children and `sp-vd5w`.
>
> **Supersedes where contradicted:** [GitHub Storage Backend - Implementation Design](2026-06-14-github-storage-backend-impl-design.md)
> and its blocked assumptions.
>
> **Builds on:** [User Secrets Store - Owner-Online Auto-Injection](2026-06-14-user-secrets-store-design.md),
> [Owner-Sealed Secrets](2026-06-10-owner-sealed-secrets-design.md),
> [Transient Tier - Kopia Journal + Migration](2026-06-10-transient-tier-kopia-journal-design.md),
> [Auth & Identity](2026-06-11-auth-identity-design.md).

## 1. Problem

GitHub-backed storage and GitHub token provisioning are one security boundary, not two separable
implementation details. The storage backend needs a user GitHub token to clone, verify, create,
and suspend-backstop repos. The token provisioning system needs to preserve the core user-secrets
property: a compromised CP cannot read or mint user tokens. The previous `sp-u53.1` design was
blocked because the node-side credential path, branch backstop detection, refresh custody, and
GitHub permission model were not coherent against the codebase.

This spec is the single design contract for GitHub credentials plus the GitHub storage backend.
Implementation remains split by bead ownership:

- `sp-v40s` owns GitHub token provisioning, permission profiles, refresh, and web/spawnctl surfaces.
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
production GitHub user-token path ships before these gates.

## 3. Custody Model

GitHub custody is strict owner-online for MVP. The AS never durably stores plaintext GitHub access
or refresh tokens.

Initial token capture uses a response-wrapping handoff:

1. Owner starts GitHub link/re-link from web or `spawnctl`.
2. AS performs the GitHub App OAuth code exchange.
3. AS holds the resulting tuple only in process memory behind a short-lived, single-use nonce.
4. AS redirects the owner client with the nonce.
5. Owner client redeems the nonce over an authenticated same-site request.
6. Owner client immediately seals the tuple into the CP-blind user secrets store.

The wrapped tuple must not be written to AS DB rows, logs, redirect URLs, crash reports, or metrics.
If the AS process restarts before redemption, the owner repeats the link flow.

Refresh is also memoryless:

1. Owner client unseals the stored GitHub refresh token locally.
2. Owner client calls AS `/github/refresh` with the refresh token.
3. AS uses the GitHub App `client_secret` to call GitHub's refresh endpoint.
4. AS returns the rotated tuple and forgets it.
5. Owner client updates the `github-token` secret with strict CAS.
6. If active spawns use the secret, owner client re-delivers the new version.

Configurable AS-side/durable custody is explicitly out of MVP and tracked by backlog epic `sp-6pqt`.

## 4. GitHub Token Shape

`github-token` is one sealed, atomically versioned tuple. The sealed payload includes:

| field | notes |
|---|---|
| `host` | MVP supports `github.com`; future GitHub Enterprise splice guard. |
| `login` | GitHub login for rendered `gh` config and UX. |
| `github_user_id` | Immutable numeric user id when available. |
| `access_token` | 8-hour GitHub App user access token. |
| `refresh_token` | GitHub App user refresh token. |
| `access_expires_at` | Used by owner-client refresh scheduling. |
| `refresh_expires_at` | Warn/re-link before the six-month cliff. |
| `permission_profile` | `existing-repo` or `repo-create`. |
| `app_metadata` | App/account metadata needed to verify the tuple. |

The CP catalog stores non-secret metadata in cleartext as routing and validation facts:
`host`, `login`, `github_user_id`, `permission_profile`, `access_expires_at`,
`refresh_expires_at`, display name, and version. The sealed tuple carries the same non-secret
metadata so the owner client/node can verify that clear routing metadata was not spliced.

`PutSecret(expected_version)` updates the sealed payload and clear metadata atomically. A refresh
writer that loses the CAS re-reads the newer tuple and discards stale results.

If a GitHub refresh call fails, the client re-reads the secret. If the stored version is now newer,
the failure is treated as a benign concurrent-refresh race. If not, surface `relink_required`.

## 5. Permission Profiles

Permission profiles are explicit and never silently upgraded:

- `existing-repo`: clone/fetch/push existing repos that the GitHub user can access.
- `repo-create`: elevated profile required for `create_if_missing`.

Repo creation requires an explicit re-link/consent flow requesting the elevated profile. A mount
that asks to create a missing repo with an `existing-repo` token fails before agent start with a
typed "re-link with repo-create permission" error.

Exact GitHub App permissions are a binding spike before production implementation. The spike must
prove the minimum permissions for:

- access verification
- clone/fetch
- push to an existing repo
- create a private user-owned repo
- refresh token rotation

Current external docs record the expected shape: GitHub's [refreshing user access tokens](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/refreshing-user-access-tokens)
page says expiring GitHub App user tokens have an eight-hour access token and six-month refresh
token, and refresh uses the GitHub App `client_secret`; GitHub's [create repository for the
authenticated user](https://docs.github.com/en/rest/repos/repos#create-a-repository-for-the-authenticated-user)
endpoint says GitHub App user access tokens work for that endpoint and require repository
`Administration` write permission. Implementation must verify these facts against the actual App
configuration.

## 6. Mount Binding And Repo Identity

GitHub mount binding uses structured fields, not security-sensitive URI query strings. Repo
identity is always fully qualified:

```text
{ host: "github.com", owner: "<owner-or-org>", repo: "<repo>" }
```

MVP accepts only `host == "github.com"`; other hosts return `unsupported_host`. This keeps the
contract Enterprise-ready without opening GitHub Enterprise API/config scope.

Binding fields:

- repo identity `{host, owner, repo}`
- `credential_secret_id`
- `create_if_missing`
- manifest/binding-controlled seed/initialize behavior
- durability class from the existing manifest model

Repo owner does not need to match the credential login. Existing org repos and other-account repos
are allowed when GitHub access verification succeeds. Repo creation is MVP-conservative: private
user-owned repos only, `create_if_missing=true`, and `repo-create` profile required. Org repo
creation is deferred.

Missing repo behavior:

- missing + `create_if_missing=false`: fail before agent start
- missing + `create_if_missing=true`: create private user-owned repo, then initialize according to
  the app manifest/binding seed behavior
- existing empty repo: leave empty unless the app manifest/binding says to seed or initialize

## 7. Delivery Model And Node Lifecycle

`github-token` attachments declare two consumers for one sealed tuple:

- `node-storage`: eligible for targeted pre-pod unseal for `github:` mount preparation and
  suspend backstop.
- `agent-render`: rendered into the agent secrets tmpfs for normal `git`/`gh` use after startup.

Storage credentials are not `spawn_artifacts` and are not `ArtifactTarget` container payloads.
`spawn_artifacts` remains the container materialization substrate for agent/sidecar artifacts.
GitHub storage credentials are carried by the A4-folded secret delivery path and mount binding
metadata.

`StartSpawn.secrets` is self-contained for the node. Each entry carries sealed bytes plus
non-secret routing metadata:

- secret id
- type (`github-token`)
- version
- delivery id
- usages (`node-storage`, `agent-render`)
- consuming mount names
- render profile/paths
- clear metadata needed for host/profile validation

Lifecycle:

```text
StartSpawn(secrets, mounts)
  -> pre-pod unseal only declared node-storage github-token secrets referenced by github mounts
  -> GitHub backend verify/clone/create/prepare using tmpfs credential provider
  -> StartPod(sidecar)
  -> normal secret injection/rendering, including agent GitHub config
  -> StartAgent
```

Pre-pod unseal is narrow. Generic app secrets, MCP secrets, and BYOK sidecar keys remain in the
normal pre-agent injection path. If pre-pod prepare fails, node wipes the per-spawn secrets tmpfs
before returning an error.

Replay state for delivered secrets is in-memory per `(spawn_id, generation, secret_id)` for MVP:
highest accepted version and seen delivery ids. Generation fencing requires fresh delivery on
resume/recreate and prevents cross-generation opens.

## 8. Backend Mechanics

The GitHub backend uses direct GitHub REST for API actions and plain `git` for clone/fetch/push.
It does not depend on `gh` for backend mechanics. `gh` config is rendered for the agent's
convenience only.

The node-side credential path is explicit: a credential provider rooted at
`SecretInjector.DirFor(spawnID)`. No plaintext token field is added to `spawnlet.Spawn`. The
provider reads only from the per-spawn secrets tmpfs and can be atomically refreshed when a live
token re-delivery rewrites credential material.

Backend prepare:

1. Validate host support and token-host match.
2. Validate permission profile for requested action.
3. Verify repo access via GitHub REST and/or credentialed git fetch before `StartAgent`.
4. If repo exists, clone/fetch into the mount host dir.
5. If repo is missing and `create_if_missing=true`, create a private user-owned repo and initialize
   according to manifest/binding seed behavior.
6. If repo is missing without create consent, fail typed before agent start.

Node-daemon GitHub egress is allowed only for declared `github:` mounts using bound
`node-storage` credentials. This is a deliberate node-trusted action: the node is the legitimate
unsealer and storage backend executor.

Agent-driven Git is not intercepted. Spawnery provisions credentials and a working tree; after
`StartAgent`, native Git semantics apply. The agent/user is responsible for real branch pushes.
Spawnery does not force-update real branches, create safety refs for agent pushes, or wrap git in
push rails.

## 9. Credential Hardening

Spawnery-delivered GitHub credential material lives only in secrets tmpfs and node memory during
unseal. Spawnery must not write token plaintext to env vars, durable disk, user home directories,
data mounts, or Kopia snapshots.

Rendered config:

- `GH_CONFIG_DIR` points into secrets tmpfs.
- Git credential config points into secrets tmpfs.
- `credential.useHttpPath=true`.
- The helper returns a token only for exact `{host, owner, repo}` matches.
- No `~/.git-credentials`.
- Node-side Git commands ignore or override repo-controlled credential helpers/config includes
  where feasible.

This hardening protects Spawnery-driven backend operations. It is not a guarantee that a
user-authorized agent cannot exfiltrate the GitHub token it was given. Agent token availability is
intentional capability and is documented as residual risk.

## 10. Suspend Backstop

GitHub backstop is a spawnlet suspend mechanism. It preserves already-committed local Git work that
has not reached any remote ref. It does not synthesize WIP commits for dirty working-tree state;
dirty state is the Kopia journal's responsibility. Reconsidering WIP dirty-state backstop is
tracked by backlog epic `sp-lqld`.

Backstop detection is reachability-based across all local branches/refs, not tracking-status-based:

- fetch/list remote refs
- for each local branch/ref, compute commits reachable locally but not remotely
- branches without upstreams are still considered
- detached HEAD commits are considered when not remote-reachable

Backstop refs are non-branch machine refs:

```text
refs/spawnery/backstop/<yyyy-mm-dd>/<spawn-id>/<generation>/<branch-name>/<branch-hash>
```

`<branch-name>` is sanitized and length-bounded for quick human identification. `<branch-hash>` is
still present to avoid collisions and preserve exact identity when names are truncated or
sanitized. Full original branch name, source commit, produced ref, and recovery metadata are
persisted separately; the ref path is helpful but not the source of truth.

GC is conservative and date-prefix based:

- keep backstop refs while the spawn exists
- after spawn deletion, default 30-day retention
- active-spawn refs are pruned only when superseded by a newer successful durability point for the
  same generation/branch
- GC is best-effort; inability to authenticate to GitHub during GC is warning-only

## 11. Suspend Failure And Recovery

Suspend order is:

1. final Kopia snapshot
2. GitHub backstop

If the final snapshot fails, suspend fails normally. If final snapshot succeeds and GitHub backstop
fails, suspend fails closed unless owner-sealed journal durability is already complete for that
mount. The exception requires:

- successful owner-sealed final snapshot
- durable CP-held owner-sealed journal-key ciphertext

When the exception applies, suspend may complete with structured degraded warnings: GitHub
committed-work backstop failed, but owner-sealed journal recovery is available.

`SuspendComplete` carries structured warning records. The CP persists them and UI/CLI surfaces
them. Logs alone are not sufficient.

On resume, owner-sealed journal restore is authoritative. If journal restore fails, Spawnery
presents:

- the failed journal generation/mount
- error details
- likely transient/permanent classification when possible
- available GitHub backstop refs

The user chooses retry journal restore, recover from backstop, start from repo, or abort/keep
suspended. Spawnery does not auto-merge, auto-checkout, or clobber anything.

All Garage/Kopia data, journal metadata, recovery records, and UI references are keyed by
`(spawn_id, generation, mount)`. Starting a later generation may be allowed by user choice, but it
must never overwrite or hide a failed generation's snapshot lineage.

## 12. Testing

Hermetic tests:

- `github-token` tuple CAS and metadata mirroring
- refresh race handling
- mount binding validation
- unsupported host rejection
- permission-profile gating
- `node-storage` pre-pod routing constraints
- credential provider rendering and atomic refresh rewrite
- replay high-water/delivery-id guards
- reachability-based backstop detection, including no-upstream branches and detached HEAD
- structured suspend warnings
- generation-keyed recovery metadata

Local/static-token Git server tests cover backend mechanics without the production OAuth path:

- existing repo clone/fetch/push
- missing repo fail-closed
- create-if-missing through a fake/local provider seam
- suspend backstop ref production
- GC behavior

`github_e2e` lane covers real GitHub semantics and fails loudly when opted into without required
environment:

- GitHub App user token handoff
- permission profiles
- repo-create gate
- actual clone/fetch/push/create behavior
- refresh API behavior

Token expiry tests use fake/forced-expiry paths for MVP. Do not add an 8-hour sleep test.

## 13. Beads Update Plan

After this spec lands:

- keep `sp-v40s` and `sp-u53.1`
- update both epics' notes/descriptions to point to this unified spec
- update `sp-u53.1` children for structured binding, node-storage credential path, backstop ref
  namespace, reachability detection, and recovery semantics
- update `sp-v40s` children for response-wrap handoff, memoryless refresh, permission profiles,
  metadata/CAS, web and spawnctl surfaces
- add production-token dependencies from `sp-u53.1` to the `sp-7h6.1` gates named in Section 2
- record `sp-vd5w` as immediate prerequisite to merge first

Roast is intentionally not run in this session; run a separate narrow adversarial review focused on
custody, replay, storage durability, GitHub permissions, and credential exfiltration surfaces before
implementation.

## 14. Decision Log

1. One unified spec for GitHub credentials plus GitHub storage; existing implementation epics remain.
2. Strict owner-online GitHub custody; configurable custody deferred to `sp-6pqt`.
3. Initial OAuth token handoff uses single-use in-memory response wrapping, not redirect plaintext
   or AS DB persistence.
4. Refresh is memoryless: owner client supplies refresh token per call; AS returns rotated tuple and
   forgets it.
5. `github-token` is one sealed, CAS-versioned tuple; clear metadata is non-secret and updated
   atomically with the sealed payload.
6. Repo-create permission is a separate elevated profile requiring explicit re-link/consent.
7. Mount bindings use structured repo identity and options, not URI query strings.
8. Repo references are fully qualified by host/owner/repo; MVP supports only `github.com`.
9. Existing org/other-owner repos are allowed after access verification; MVP repo creation is
   private user-owned only.
10. `node-storage` GitHub credentials may be unsealed pre-pod, but only when declared by a
    `github:` mount binding.
11. The agent also receives the same GitHub token via explicit `agent-render` route.
12. Storage credentials are not `spawn_artifacts`; `StartSpawn.secrets` carries self-contained
    routing metadata and sealed bytes.
13. Node backend uses REST + plain git; `gh` is agent convenience only.
14. No push rails for agent-driven Git; Spawnery only performs suspend backstop.
15. Backstop preserves committed local work only; no WIP commit floor in MVP (`sp-lqld`).
16. Backstop refs live under `refs/spawnery/backstop/<yyyy-mm-dd>/<spawn-id>/<generation>/<branch-name>/<branch-hash>`.
17. GitHub backstop failure can be downgraded only when owner-sealed journal final snapshot and
    owner-sealed key custody succeeded.
18. Journal restore failure is user-directed; all journal data/metadata is generation-keyed and
    must not be overwritten by later generations.

## Post-Implementation Notes

*As this design is implemented and iterated on -- bug fixes, adjustments, anything that diverged from the assumptions above -- append a dated note here, whether or not a formal debugging skill was used.*
