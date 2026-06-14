# User Secrets Store — Owner-Online Auto-Injection (sp-7h6.1)

> **Status:** design approved 2026-06-14. Epic `sp-7h6.1` (children `.1`–`.7`).
> Builds on: Owner-Sealed Secrets (`2026-06-10-owner-sealed-secrets-design.md`),
> E4 Identity & Secrets (`2026-05-28-spawnery-e4-identity-secrets-design.md`),
> Artifact-Injection + Cross-Agent Installer (`2026-06-14-cross-agent-installer-design.md`),
> Auth & Identity (`2026-06-11-auth-identity-design.md`).

## 1. Problem

There is no general user-secrets mechanism. Today exactly one inference key is plumbed,
and it is **node-global plaintext** (`OPENROUTER_API_KEY` read from the node's env at
startup and set as sidecar env at `internal/spawnlet/manager.go:~744`). There is no
GitHub token injection, no per-user key custody, no durable store. We need a user-scoped
secrets store whose typed entries (GitHub tokens, BYOK inference keys, arbitrary app
secrets) are **durably persisted CP-blind** and **auto-injected into the right container**
at spawn startup — surviving CP restarts, never exposing plaintext to the control plane.

## 2. Scope & Posture

### Reused as-is — NOT re-litigated here

- **Envelope crypto** (`internal/secrets/seal`): `Seal`/`Open`, a fresh per-write DEK,
  HPKE-sealed to each enrolled device pubkey + recovery key, at-rest AAD
  `(account_id, secret_id, version)`. `Envelope` is the opaque, CP-stored object;
  any tamper / version-downgrade breaks authentication on `Open`.
- **E2E delivery channel** (fully plumbed): `GetSpawnNodeKey` + `DeliverSecrets` RPCs
  (`internal/cp/secrets.go`), CP relays opaque ciphertext, node unseals against its
  retained HPKE sub-keys with in-flight AAD `(spawnId, generation, nodeId, notAfter,
  version)` + one-time `deliveryId`, then `manager.InjectSecret` writes plaintext to the
  per-spawn tmpfs at `/run/spawnery/secrets/`.
- **Artifact substrate** (sp-l5sx): sensitive values land at
  `/run/spawnery/secrets/<envVarName>` (`0600`); the in-pod `manifest.json` carries a
  `secretRefs` array; emitters inject each value into only the consuming server's native
  env (honors the never-persist M10 invariant — never PodSpec env).
- **Web crypto** (`web/src/keys/`): device keys, HPKE seal/open, and the revocation
  re-seal sweep (`sweep.ts`), which already targets a `SecretsCPClient` whose
  `getEnvelope`/`putEnvelope` are **stubs waiting on this epic's CP RPCs**.

### New in this epic

Durable CP-blind envelope store + CRUD RPCs (§4); the typed data model (§4); GitHub OAuth
capture + refresh ceremony (§5); spawn-start scoping + owner-online delivery orchestration
+ secrets-ready gate (§6); injection routing (§7); web management UI (§8).

### Custody invariant (locked)

**Owner-online only.** The CP holds ciphertext sealed to the owner's device/recovery keys
and **cannot decrypt**. Auto-injection therefore requires an authenticated owner **device
to be online** at spawn start/resume to unseal-from-vault and re-seal to the node sub-key.
A spawn that cannot get an owner device online **waits (suspended)** — identical to the
owner-sealed re-placement model. **No node/server custody is designed in this epic;**
unattended/headless secret delivery is explicitly out of scope.

Consequence for inference: the **node-global managed OpenRouter key stays** as the default
inference path (managed tier — a spawn with no BYOK still runs without an owner present).
BYOK inference keys are an owner-sealed **override**, not a wholesale replacement of the
existing injection. (The bead's "replace the OpenRouterKey injection" is implemented as
"override when a BYOK inference secret is attached," not "remove the managed default.")

## 3. Main Challenges

The hard part is reconciling **durable, CP-blind storage** with **auto-injection** when the
CP can never decrypt: the owner's device is the only entity that can move a secret from
at-rest custody onto a node, so spawn startup must be gated on owner presence rather than
assumed server-side. Secondary challenges: keeping a GitHub OAuth token's brief
server-exchange window honest (transient, never persisted) while supporting refresh; and
enforcing least-privilege so untrusted marketplace apps never see secrets the owner didn't
explicitly attach.

## 4. Durable Data Model + CRUD RPCs (sp-7h6.1.1, sp-7h6.1.2)

A CP-blind `secrets` table, mirroring the `OwnerRepo` / Bun / goose pattern
(`internal/cp/store`), keyed `(account_id, secret_id)`:

| column | notes |
|---|---|
| `account_id` | PK part; FK → owners |
| `secret_id` | PK part |
| `type` | `inference-key` \| `github-token` \| `generic-kv` |
| `name` | display name |
| `provider` | for `inference-key` (e.g. `openrouter`); extensibility seam for multi-provider (sp-21b.1) |
| `target` | type-derived default (see §7); `generic-kv` carries an explicit env-name or file-path |
| `version` | uint64, matches the envelope's at-rest AAD `version` |
| `envelope` | the opaque `Envelope` JSON — **CP never decrypts, only stores/serves** |
| `created_at` / `updated_at` | bigint epoch |

Implementation follows the existing DAO pattern: entity in `store/types.go`,
`UserSecretRepo` interface in `store/store.go`, impl in `store/user_secrets.go`, factory in
`store/bun.go`, paired goose migrations under `migrations/{pg,sqlite}/`.

**CRUD RPCs** (CP-side, owner-authenticated, ciphertext in/out — the CP validates auth +
AAD shape but never the plaintext): `PutSecret` (upsert envelope; `version++` enforced
monotonic), `GetSecret`, `ListSecrets` (metadata + envelope), `DeleteSecret`.

These RPCs are the concrete realization of the `SecretsCPClient` seam stubbed in
`web/src/keys/sweep.ts` (`getEnvelope` → `GetSecret`, `putEnvelope` → `PutSecret`). Wiring
them **unblocks the W3 revocation re-seal sweep** (sp-2ckv.7), which today runs against a
stub that rejects `getEnvelope`.

Durability: because envelopes live in the CP DB (SQLite/Postgres), CP restarts lose
nothing. The store is the source of truth for what secrets exist; `issues.jsonl`-style
passive exports are not involved.

## 5. GitHub Token — OAuth Capture + Refresh (sp-7h6.1.5)

`github-token` is one typed secret in the store (§4) whose value is a **structured sealed
payload** `(access, refresh, expiry)`. Only its provisioning UX differs from a pasted
secret.

**Capture.** The GitHub App is configured for expiring user tokens. On "Link GitHub"
(or login), the AS performs the server-side code exchange — `client_secret` is required and
GitHub does not support PKCE-only public-client exchange, so a brief AS window is
unavoidable. The AS **transiently** holds `(access, refresh, expiry)`, hands them to the
authenticated client over TLS, and **persists nothing**. The client seals the structured
payload and `PutSecret`s it. CP only ever sees the ciphertext. Paste remains a fallback
path to the same `github-token` secret.

**Refresh ceremony (owner-online, at spawn start).** If the access token is near expiry:
client unseals the refresh token from vault → AS `/refresh` exchange (same transient,
never-persist window, `client_secret` server-side) → client re-seals `(access', refresh')`
→ `PutSecret` (`version++`) → proceed to delivery. No background/headless refresh — it only
happens when an owner device is online to drive a spawn start.

**Custody boundary (documented explicitly):** the AS is a transient pass-through at code
exchange and at `/refresh`; it never persists tokens; the CP stores only the sealed
envelope. This is weaker than user-paste (which touches no server) but is the chosen
trade-off for no-manual-paste UX.

## 6. Spawn-Start Orchestration (sp-7h6.1.3, sp-7h6.1.4)

**Scoping — least-privilege (locked).** At `CreateSpawn` the owner selects which stored
secrets to attach. When the app manifest declares secret *requests* (by type/name), those
pre-select the toggles as a hint; absent a manifest request, nothing is pre-selected.
Untrusted apps receive **only** what the owner explicitly attaches. The attached
secret-ID set is persisted on the spawn row, so resume/recreate/migrate re-delivers the
same set (mirrors the artifact-persistence amendment from sp-l5sx).

**Delivery.** The owner device (online) calls `GetSpawnNodeKey`, unseals each attached
secret from vault, re-seals to the node sub-key (in-flight AAD + one-time `deliveryId`),
and calls `DeliverSecrets`; the CP relays opaque ciphertext; the node unseals and
`InjectSecret`s to tmpfs. This reuses the existing channel verbatim — generalized from one
secret to the attached N.

**Secrets-ready gate.** The in-pod launcher blocks agent exec until every attached secret
has landed under `/run/spawnery/secrets/` (resolves the artifact-substrate S4 pre-exec
concern — secrets must exist before the agent and its MCP children exec). If no owner
device delivers within the timeout, the spawn stays/returns **suspended** — never a
partial-secret start.

**Resume / recreate / migrate.** Same persisted attachment set, same owner-online delivery
and gate. No owner online → stays suspended (consistent with the custody invariant and
owner-sealed re-placement).

## 7. Injection Routing (Type-Derived)

All routing goes through the artifact substrate's secrets path (tmpfs file +
per-consumer env injection; never PodSpec env, honoring M10):

- `inference-key{provider}` → **sidecar** env (BYOK override of the managed default key).
- `github-token` → **agent** env `GITHUB_TOKEN` **and** the `~/.config/gh/hosts.yml` file
  (so both `git` and `gh` work).
- `generic-kv` → **agent** env name or file path per its explicit `target` hint.

## 8. Web UI + Dependency Caveat (sp-7h6.1.6)

Add/edit/remove typed secrets, sealing client-side in the browser via the existing
`web/src/keys/` crypto; a view of which spawns consume which secrets; a "Link GitHub"
button driving the OAuth capture of §5.

**Caveat (recorded as dependency/risk, not a backend blocker):** the strong custody
guarantee for the *web* path depends on the SPA-delivery slice (web-epic, roast finding
C2 — a CP-served SPA without a pinned origin/CSP/SRI can have malicious JS exfiltrate
plaintext). Until that slice lands, the strong guarantee is `spawnctl`/CLI-only and the
web path is best-effort. The backend slices (§4–§7) do not wait on it.

## 9. Testing (sp-7h6.1.7)

**Hermetic (`-race`, in-memory store):** CRUD round-trips; CP-blindness (assert the CP
path only ever holds ciphertext); version/AAD downgrade rejection on `Open`;
refresh-ceremony re-seal produces a higher-version envelope; scoping (an unattached secret
is never delivered).

**Build-tagged e2e:** a secret set via the API is delivered E2E and injected into the
**correct** container at startup — `GITHUB_TOKEN` visible to the agent, an inference key
reaching the sidecar — with the CP holding only ciphertext throughout; the secrets-ready
gate blocks agent exec until delivery; resume re-delivers the persisted attachment set.
Per repo convention these FAIL (never `t.Skip`) when their dep is down.

## 10. Decision Log (forks resolved during design)

1. **Custody model** → owner-online only (MVP). Strongest guarantee, CP fully blind, no
   node/server custody to build or revoke. Rejected: node/fleet-key fallback and
   per-type custody — both add a weaker custody tier + revocation surface not needed for MVP.
2. **GitHub token source** → capture the login OAuth token (with transient-never-persist
   AS boundary). Rejected: user-paste-only (worse UX) and server-minted App installation
   tokens (server-custodied — contradicts the custody invariant).
3. **Token refresh** → seal refresh token + on-demand owner-online refresh ceremony.
   Rejected: non-expiring-token + manual re-link (relies on App config, coarse revocation).
4. **Scoping** → owner selects per spawn, app manifest requests pre-select as hints.
   Rejected: strict app-declared-only (needs E5 manifest work first) and all-by-type
   (wide blast radius for untrusted apps).
5. **Routing** → type-derived targets, `generic-kv` carries an explicit hint. Matches the
   bead's `target` field and the substrate's `secretRefs`.

## 11. Child-Bead Mapping

| bead | section |
|---|---|
| sp-7h6.1.1 — data model + CRUD API | §4 |
| sp-7h6.1.2 — encrypt at rest CP-blind | §4 (reuses §2 envelope) |
| sp-7h6.1.3 — deliver over E2E channel | §6 |
| sp-7h6.1.4 — inject into right container | §6, §7 |
| sp-7h6.1.5 — GitHub token injection | §5 |
| sp-7h6.1.6 — web UI | §8 |
| sp-7h6.1.7 — tests + e2e | §9 |
