# User Secrets Store — Owner-Online Auto-Injection (sp-7h6.1)

> **Status:** design approved 2026-06-14; **comprehensively revised 2026-06-14 after roast**
> (verdict REVISE, 22 confirmed findings — see §12 changelog). Epic `sp-7h6.1`.
> Builds on: Owner-Sealed Secrets (`2026-06-10-owner-sealed-secrets-design.md`),
> E4 Identity & Secrets (`2026-05-28-spawnery-e4-identity-secrets-design.md`),
> Artifact-Injection + Cross-Agent Installer (`2026-06-14-cross-agent-installer-design.md`),
> Auth & Identity (`2026-06-11-auth-identity-design.md`).

## 1. Problem

There is no general user-secrets mechanism. Today exactly one inference key is plumbed,
and it is **node-global plaintext** (`OPENROUTER_API_KEY` read from the node's env at
startup and set as sidecar env at `internal/spawnlet/manager.go:~772`). There is no
per-user key custody and no durable store. We need a user-scoped secrets store whose typed
entries (BYOK inference keys, arbitrary app secrets) are **durably persisted CP-blind** and
**auto-injected into the right container** at spawn startup — surviving CP restarts, never
exposing plaintext to the control plane.

> **Scope change (roast):** first-class **GitHub-token provisioning** (OAuth capture,
> refresh, scope, git-credential wiring) is **split into its own epic** (§5) tied to the
> GitHub storage backend (`sp-u53.1`). This epic keeps a *generic* `github-token` secret
> type but does not own the GitHub-App integration.

## 2. Scope & Posture

This section is deliberately precise about **what already works** vs **what this epic must
build or complete** — the prior draft overstated the reuse, which the roast caught.

### 2a. Genuinely reused, working today

- **Envelope crypto at rest** (`internal/secrets/seal`): `Seal`/`Open`, a fresh per-write
  DEK, HPKE-sealed to each enrolled device pubkey + recovery key, at-rest AAD
  `(account_id, secret_id, version)`. The device-key `Open` over `AtRestAAD` **does reject
  a version downgrade** (`seal/deviceset.go` monotonic check) — verified. This is the
  storage primitive the data model sits on.
- **The relay + node-unseal + tmpfs-inject plumbing**: `GetSpawnNodeKey` /
  `DeliverSecrets` RPCs (`internal/cp/secrets.go`), node `handleSecretDelivery`
  (`internal/node/secrets.go`) with **generation fencing** (`attach.go staleGen` drops
  deliveries with `gen < live`), and `manager.InjectSecret` writing plaintext to the
  per-spawn tmpfs at `/run/spawnery/secrets/` (`manager.go:~1400`, mounted into the **agent**
  container only at `~722`).
- **Sidecar control endpoint** (`internal/sidecar/override.go`): a token-gated HTTP
  handler bound to the **pod IP** (`SIDECAR_CONTROL_ADDR`, `SIDECAR_CONTROL_TOKEN`),
  established after `StartPod` returns (PodIP known) and **before `StartAgent`**. Today it
  serves `/control/model`; this epic extends it (§7).
- **Web crypto** (`web/src/keys/`): device keys, HPKE seal/open, and the revocation
  re-seal sweep (`sweep.ts`), whose `SecretsCPClient.getEnvelope`/`putEnvelope` are
  **stubs waiting on this epic's CP RPCs**.

### 2b. Reused but **must be completed** (roast-confirmed gaps)

- **In-flight replay guards are stubbed.** `node/secrets.go:84-89` builds `InFlightAAD`
  with `Version=0, DeliveryID=""` (comment: "version-monotonic and deliveryId-once stateful
  checks are documented follow-up hooks"). `seal/delivery.go OpenFromOwner` and
  `subkey.OpenDelivered` delegate both checks to the caller; no caller implements them.
  **Cross-rotation replay is already blocked by generation fencing**, but the residual
  same-generation / same-sub-key window is real, and the **clients seal non-zero**
  `Version`/`DeliveryID` (`spawnctl/move.go`, `web/api/migration.ts`) while the node builds
  zero — an AAD-construction mismatch. Wiring real enforcement (high-water mark +
  seen-`deliveryId` set) needs **proto fields** (per-secret `version` + a `delivery_id` on
  `SealedSecret`). → owned by **sp-7h6.1.8** (prerequisite for secure delivery), which must
  also **verify the journal-key delivery path is not silently broken by the same mismatch**.
- **The artifact substrate (sp-l5sx) is a concurrent, largely-unbuilt spec** (no
  `spawn_artifacts` table, no `agentinstall`, no manifest materializer). Therefore **this
  epic does NOT depend on the sp-l5sx manifest/agentinstall path.** It uses only the
  *built* tmpfs `InjectSecret` path + the sidecar control endpoint + the new gate (§6).
  Secrets that must be *embedded into agent config files* (e.g. MCP-server env) are
  **deferred** to when sp-l5sx lands; MVP targets are sidecar (inference) and agent
  files/env (generic). This removes the hidden inter-epic ordering trap.

### 2c. Custody invariant (locked)

**Owner-online only.** The CP holds ciphertext sealed to the owner's device/recovery keys
and **cannot decrypt**. Auto-injection requires an authenticated owner **device online** at
spawn start/resume to unseal-from-vault and re-seal to the node sub-key. A spawn that cannot
get an owner device online **stays/returns suspended** (§6). No node/server custody is
designed here; unattended/headless secret delivery is out of scope.

The node-global **managed** OpenRouter key stays as the default inference path, **still
injected via `SidecarEnv`**. This is a **documented, deliberate exception** to the M10
never-persist invariant: the managed key is node-trusted and node-scoped, not owner-sealed.
**M10 binds the BYOK/user-secret paths, not the managed key.** Implementers must not route
user secrets through `SidecarEnv`.

## 3. Main Challenges

Reconciling **durable, CP-blind storage** with **auto-injection** when the CP can never
decrypt: the owner's device is the only entity that can move a secret onto a node, so spawn
startup must be **explicitly gated on owner-driven delivery** rather than assuming a
server-side push. Secondary: completing the in-flight replay guards so the rotation story is
real; routing a BYOK key to the sidecar (whose tmpfs the agent owns, not the sidecar); and
enforcing least-privilege so untrusted apps see only what the owner attached.

## 4. Durable Data Model + CRUD RPCs (sp-7h6.1.1, sp-7h6.1.2)

A CP-blind `secrets` table mirroring the `OwnerRepo` / Bun / goose pattern
(`internal/cp/store`), keyed `(account_id, secret_id)`:

| column | notes |
|---|---|
| `account_id`, `secret_id` | PK; `account_id` FK → owners |
| `type` | `inference-key` \| `github-token` \| `generic-kv` |
| `name` | display name |
| `provider` | for `inference-key` (e.g. `openrouter`); extensibility seam for sp-21b.1 |
| `target_kind` | `sidecar` \| `agent-file` \| `agent-env` (default per type, §7) |
| `target_ref` | env-var name or file path (for `agent-*` / `generic-kv`) |
| `version` | uint64; matches the envelope's at-rest AAD `version` (**per-secret**, not per-batch — resolves the versioning ambiguity) |
| `envelope` | opaque `Envelope` JSON — **CP never decrypts** |
| `created_at` / `updated_at` | bigint epoch |

Plus an attachment record so resume re-delivers the same set (§6): the **declared attached
secret-ID set persists on the spawn** (a `secret_attachments` column/table keyed by
`spawn_id` → `secret_id[]`; non-sensitive metadata only — the sealed payloads are *not*
stored, they are re-provided at create/resume).

**CRUD RPCs** (owner-authenticated; ciphertext in/out — the CP validates auth + AAD shape,
never plaintext): `PutSecret`, `GetSecret`, `ListSecrets`, `DeleteSecret`.

- **`PutSecret` concurrency (roast #14):** strict **optimistic CAS** — the request carries
  `expected_version`; the CP `UPDATE … WHERE version = expected_version` in a transaction
  and returns the new `version = expected_version + 1` (gaps not allowed — matches
  `sweep.ts`'s "exactly old+1"). On CAS conflict the RPC returns a typed `Conflict`; the
  caller (incl. the multi-device revocation sweep) **re-reads, re-seals, retries**.
- **`ListSecrets`** returns metadata + envelopes. It is the call the **W3 revocation sweep**
  needs: the enrollment/revocation ceremony **calls `ListSecrets` to populate
  `progress.secretIds` before `executeSweep`** (roast #20) — `sweep.ts` itself does not
  enumerate. This wiring is part of sp-7h6.1.1's web seam.

Wiring `GetSecret`/`PutSecret` realizes the `SecretsCPClient` seam stubbed in
`web/src/keys/sweep.ts` and **unblocks the W3 revocation re-seal sweep** (sp-2ckv.7).
Durability: envelopes live in the CP DB (SQLite/Postgres) — CP restarts lose nothing.

## 5. `github-token` Type — Generic Here, GitHub-App Integration Split Out

This epic supports a `github-token` **typed secret** the same way as any other: a value the
owner seals client-side, stores, attaches, and has delivered to an agent file/env (§7). A
user can paste a PAT today and it works as a generic secret.

**Split out (roast #4/#7/#8/#9/#10/#21/#22) → new epic `GitHub token provisioning for git
ops`, linked to `sp-u53.1` (GitHub storage backend / gh-backed volumes):** the first-class
GitHub integration — confirming the registration is a GitHub App with *Expiring user
authorization tokens*; capturing `(access, refresh, expiry)` **and the login/username**;
the transient-never-persist hand-off mechanism; the **net-new AS `/github/refresh`** endpoint
(distinct from the existing Spawnery session `/refresh`); **OAuth scope expansion** beyond
`read:user` (current hard-coded scope cannot clone/push); the `~/.config/gh/hosts.yml`
render **plus a git credential helper** (hosts.yml alone does not authenticate `git push`);
and the ~6-month refresh-token-expiry re-link path. None of that is in this epic; it depends
on this store + the GitHub storage backend.

## 6. Spawn-Start Orchestration (sp-7h6.1.3, sp-7h6.1.4, sp-7h6.1.8, sp-7h6.1.9)

The requirement that a spawn must not run without its secrets is made **explicit in the
Create/Resume Spawn API** (the chosen fork), eliminating the "device must somehow learn the
spawn is waiting" gap (roast #15) and the "attachment has no home" gap (roast #3).

**Declared set in the request.** `CreateSpawnRequest` / `ResumeSpawnRequest` (cp.proto)
carry the **declared attached secret-ID set**. The set is persisted on the spawn row
(§4), so resume/recreate/migrate re-declare the same set without the client re-choosing.

**Node sub-key folded into the response.** Placement assigns a node; the
Create/Resume response returns that node's HPKE sub-key (cert chain + signed sub-key +
generation) — the same payload as `GetSpawnNodeKey`, folded in to resolve the
placement-before-seal ordering (roast #15). `GetSpawnNodeKey` remains for re-fetch.

**Delivery is a required step.** The owner device (online — it just made the call) seals
each declared secret to the node sub-key with in-flight AAD
`(spawn_id, generation, node_id, not_after, per-secret version, delivery_id)` and calls
`DeliverSecrets`. The CP relays opaque ciphertext; the node verifies AAD equality +
`not_after` + **version-monotonic high-water mark + deliveryId-once** (sp-7h6.1.8) and
injects (§7).

**Secrets-ready gate (kept — roast #6/#16).** The spawn enters an explicit
`WaitingForSecrets` sub-state of Starting. The node **sequences startup**:
`StartPod` (sidecar up, control endpoint live) → **deliver + inject every declared secret**
(agent-targeted → tmpfs; `inference-key` → sidecar control POST, §7) → **gate clears** →
`StartAgent`. The agent never execs with a partial secret set. **This epic explicitly
builds this pre-exec path**; it does **not** adopt sp-l5sx's "start-without + diagnostic"
policy (the prior draft contradicted sp-l5sx; this is the deliberate divergence).

**Timeout / owner absent.** If the declared set is not fully delivered within a configured
timeout, the node reports the timeout to the CP, the spawn transitions to **Suspended**, and
the **pod is torn down** (no live container left waiting). Consistent with the owner-online
invariant and owner-sealed re-placement.

**Deleted-but-attached secret at resume (roast #19).** Fail-closed: if a declared secret no
longer exists (`GetSecret` → NotFound) the resume fails with a typed error naming the secret;
the owner must detach it or re-create the secret. No silent skip.

## 7. Injection Routing (Corrected for M10)

- **`inference-key{provider}` → sidecar via the control endpoint (roast #12).** The secrets
  tmpfs is not mounted into the sidecar and `SidecarEnv` is frozen at `StartPod`, so BYOK
  cannot go through either. Instead the node **POSTs the unsealed key (+ provider/base-url)
  to the sidecar's token-gated control endpoint** (extend `override.go` with a key/inference
  override alongside the model override), **after `StartPod`, before `StartAgent`** (the
  startup ordering the user specified). M10-clean: in sidecar memory only, over the
  pod-IP-bound, bearer-gated control channel; never env, never disk. This is the BYOK
  *override* of the managed default.
- **`github-token` / `generic-kv` → agent file by default (roast #11).** Routing
  `GITHUB_TOKEN` to agent **env** violates M10 (Docker persists env to `config.v2.json`;
  inherited via `/proc/<pid>/environ`). Default target is therefore a **tmpfs file**
  (`/run/spawnery/secrets/<ref>`, `0600`). An `agent-env` target is supported only via a
  **launcher `export`-before-exec** step, with the residual `/proc/environ` exposure
  **documented** at the secret/UX level so the owner opts in knowingly.

## 8. Surfaces: CLI (strong path) + Web (sp-7h6.1.6, sp-7h6.1.10)

The strong custody guarantee is **CLI-first** (owner-sealed phase ①), so the `spawnctl`
surface is first-class, not an afterthought (roast #18):

- **`spawnctl secret`**: `create` (type/name/target + seal client-side), `list`, `delete`.
- **Attachment**: `--secret <id>` (repeatable) on `spawnctl create` / `resume`; the CLI
  fetches the node sub-key from the create/resume response, seals, and delivers.
- **Empty-store / headless:** if no secrets are declared, the spawn starts normally (gate is
  a no-op). If an app *requests* secrets none are provided for, it starts without them
  (secret-dependent features may fail) — the gate only blocks on the **declared** set.

**Web UI**: add/edit/remove typed secrets sealing client-side via `web/src/keys/`; a view of
which spawns consume which secrets; attach toggles at create (app-manifest requests
pre-select as hints). **Caveat:** the strong custody guarantee for the *web* path depends on
the SPA-delivery slice (web-epic, roast C2); until it lands the strong guarantee is
CLI-only and the web path is best-effort. Recorded as a dependency/risk, not a backend
blocker.

## 9. Testing (sp-7h6.1.7)

**Hermetic (`-race`, in-memory store):**
- CRUD round-trips; CP-blindness (CP path only ever holds ciphertext); `PutSecret` CAS
  conflict + retry.
- **Two distinct replay legs, tested separately (roast #1 judge-raised):** (a) **at-rest**
  downgrade rejection via device-key `Open` over `AtRestAAD`; (b) **in-flight** delivery
  replay — version-monotonic + deliveryId-once rejection at the node (this is the new
  sp-7h6.1.8 guard, a different leg the prior "version/AAD downgrade" test did *not* cover).
- Scoping: an unattached/undeclared secret is never delivered.

**Build-tagged e2e:** a declared secret set is delivered E2E and injected into the **correct**
target — a BYOK inference key reaching the **sidecar via the control endpoint**, a
`github-token`/`generic-kv` value landing in the **agent tmpfs file** — with the CP holding
only ciphertext throughout; the **secrets-ready gate** blocks agent exec until delivery and
**times out to Suspended with the pod torn down**; resume re-delivers the persisted declared
set; deleted-attached-secret resume fails closed. Per repo convention these FAIL (never
`t.Skip`) when their dep is down.

## 10. Prerequisites & Inter-Epic Ordering

- **sp-7h6.1.8 (in-flight replay guards + proto fields)** is a prerequisite for secure
  delivery (sp-7h6.1.3/.4/.9) and must verify the journal-key path.
- **No hard dependency on sp-l5sx** (artifact substrate). Config-file-embedded secrets
  (MCP-server env) are deferred to when sp-l5sx lands; MVP uses sidecar-control + agent-tmpfs
  only.
- **GitHub-App integration** lives in the new GitHub-token epic, which depends on this store
  and on `sp-u53.1` (GitHub storage backend).

## 11. Decision Log (forks resolved)

1. **Custody** → owner-online only; CP fully blind; spawn waits suspended if no owner online.
   Managed OpenRouter key stays (documented M10 exception); BYOK is an owner-sealed override.
2. **Secrets-ready gate** → **kept**, made explicit in the Create/Resume API (declared set
   in request, node sub-key in response, delivery required before agent exec, timeout →
   Suspended + pod torn down). Deliberately diverges from sp-l5sx's start-without policy.
3. **GitHub push/token** → **split to a separate epic** linked to `sp-u53.1`; this epic keeps
   only a generic `github-token` type.
4. **BYOK inference** → **sidecar control endpoint**, initialized after `StartPod` and before
   `StartAgent` (startup-sequence ordering).
5. **Scoping** → owner-declares-per-spawn least-privilege; app-manifest requests pre-select.
6. **Routing** → `inference-key`→sidecar-control; `github-token`/`generic-kv`→agent **file**
   by default (env only via opt-in launcher export, M10 residual documented).
7. **Versioning** → per-secret `version`; `PutSecret` strict CAS (`expected_version`).

## 12. Changelog — roast revision (2026-06-14)

Roast verdict **REVISE** (22 confirmed). Folded in: corrected the overstated "fully plumbed
/ reused verbatim" claims (§2a/§2b); scoped the in-flight replay guards + proto fields as
sp-7h6.1.8 (#1/#2/#5); decoupled from the unbuilt sp-l5sx substrate (#17); made the
secrets-ready gate explicit in the Create/Resume API with a durable attachment home + node
sub-key in the response (#3/#6/#15), kept the strict gate and noted the sp-l5sx divergence
(#16); routed BYOK to the sidecar control endpoint (#12) and `github-token`/`generic-kv` to
agent files for M10 (#11), flagged the managed-key SidecarEnv M10 exception (#13); defined
`PutSecret` CAS (#14) and `ListSecrets`-feeds-the-sweep (#20); split all GitHub-App
machinery to a new epic (#4/#7/#8/#9/#10/#21/#22); added the spawnctl CLI surface (#18) and
the deleted-attached-secret resume policy (#19); split the at-rest vs in-flight replay tests
(#1 judge-raised).

## 13. Child-Bead Mapping

| bead | section |
|---|---|
| sp-7h6.1.1 — data model + CRUD API (+ CAS, ListSecrets-for-sweep, attachment persistence) | §4 |
| sp-7h6.1.2 — encrypt at rest CP-blind | §4 (reuses §2a envelope) |
| sp-7h6.1.3 — deliver over E2E channel | §6 |
| sp-7h6.1.4 — inject into right container (sidecar control + agent file) | §6, §7 |
| sp-7h6.1.7 — tests + e2e | §9 |
| **sp-7h6.1.8 — in-flight replay guards + proto fields (NEW)** | §2b, §6 |
| **sp-7h6.1.9 — secrets-ready gate + Create/Resume required-secrets API + state machine (NEW)** | §6 |
| **sp-7h6.1.10 — spawnctl secrets CLI surface (NEW)** | §8 |
| sp-7h6.1.6 — web UI | §8 |
| sp-7h6.1.5 — GitHub token → **MOVED to new epic** (GitHub token provisioning for git ops) | §5 |
