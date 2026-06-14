# User Secrets Store — Owner-Online Auto-Injection (sp-7h6.1)

> **Status:** approved 2026-06-14; **revised twice after roast** (R1 REVISE, R2 BLOCK).
> R2 found that R1's "decouple from the unbuilt sp-l5sx substrate" was based on a **false
> premise** — the artifact/secret substrate is built — and that R1's Create/Resume delivery
> handshake deadlocks against the real lifecycle. This version is **code-grounded** and
> **builds on the existing substrate** rather than duplicating it. See §13 changelog.
> Builds on: Owner-Sealed Secrets (`2026-06-10-owner-sealed-secrets-design.md`),
> Artifact-Injection + Cross-Agent Installer (`2026-06-14-cross-agent-installer-design.md`),
> E4 Identity & Secrets, Auth & Identity (`2026-06-11-auth-identity-design.md`).

## 1. Problem

There is no general user-secrets mechanism. Today exactly one inference key is plumbed,
node-global plaintext (`OPENROUTER_API_KEY` → `SidecarEnv` at `manager.go:~772`). We need a
user-scoped store whose typed entries (BYOK inference keys, arbitrary app secrets) are
**durably persisted CP-blind** and **auto-injected into the right container** at spawn
startup — surviving CP restarts, never exposing plaintext to the control plane.

The central realization of this revision: **most of the delivery/injection machinery already
exists.** This epic is mostly a thin *catalog* + *orchestration* layer over the existing
artifact-injection substrate and the A4 intent round-trip. First-class GitHub-token
provisioning is split to its own epic (`sp-v40s`, §5).

## 2. The Existing Substrate (verified — what we build ON)

- **A user secret is a `sensitive` spawn_artifact.** `store.Artifact`
  (`internal/cp/store/types.go:135`, table `spawn_artifacts`, migration `0012`) already has
  exactly the routing fields a secret needs: `Sensitive bool`, `EnvVarName`,
  `TargetContainer` (`AGENT=1|SIDECAR=2`), `DestPath`, `Mode`, with `Inline` **nil for
  sensitive** (value rides the E2E channel). DAO: `AddArtifacts`/`GetArtifacts`
  (`store/spawn_artifacts.go`); persisted at create (`server.go:735`) and **re-threaded on
  resume/recreate/migrate** (`lifecycle.go:786 GetArtifacts → Provision`). `ArtifactSpec`
  on the wire (`cp.proto:91`, `node.proto:153`) flows CP→node→spawnlet.
- **Sealed-value delivery + tmpfs injection exist.** `DeliverSecrets` relays opaque
  ciphertext; node `handleSecretDelivery` (`node/secrets.go`) unseals via its HPKE sub-key;
  `SecretInjector.Write` / `manager.InjectSecret` writes `0600` to the per-spawn tmpfs
  `/run/spawnery/secrets/<ref>` (`spawnlet/secrets.go`), bind-mounted into the **agent**.
  `ArtifactStager.Materialize` (`spawnlet/artifacts.go:64`) already routes sensitive
  artifacts to the `SecretInjector`.
- **An in-container readiness gate exists.** `agentinstall/secretwait.go` polls the secrets
  dir for declared refs with a timeout (called from `dispatch.go:116` for MCP `SecretRefs`).
- **The A4 two-phase intent round-trip exists.** `provisionSpawn` (`server.go:752`):
  `PickNodeID` (concrete node, from `nodeKeyCache` populated on Register/Heartbeat) →
  `buildPendingIntent(target_node_id)` → `pendingIntents.await(...)` **blocks** for the
  client's `SubmitIntent` → `Provision` sends `StartSpawn` → node `CreateWithSelection`
  (`StartPod` → **gap** → `StartAgent`). `ResumeSpawn`/`resumeLocked` use the *same*
  round-trip. The client already polls `GetPendingIntent` and submits a `SignedIntent`.
- **Envelope crypto at rest** (`internal/secrets/seal`): `Seal`/`Open`, per-write DEK,
  HPKE-to-device-keys, at-rest AAD `(account_id, secret_id, version)`; downgrade rejected at
  `Open`. **Web crypto** (`web/src/keys/`) incl. the revocation sweep with the stubbed
  `SecretsCPClient.getEnvelope/putEnvelope` this epic wires.

### What is NOT done (and is this epic's real work)

1. The durable CP-blind **secrets catalog** (envelopes + CRUD + CAS) — §4.
2. **Attach** = mint a `sensitive` spawn_artifact from a catalog entry at create/resume — §6.
3. **Owner-online delivery folded into the A4 round-trip** (the deadlock fix) — §6.
4. **BYOK → sidecar** consumption (the sidecar can't read secrets today) — §7.
5. **In-flight replay guards + a live journal-key delivery bug fix** — §8.
6. **A real node-revocation checker** (currently a no-op) — §9.

### Custody invariant (locked)

Owner-online only; CP fully blind. The owner device must be online at start/resume to unseal
+ re-seal. The managed `OPENROUTER_API_KEY` stays via `SidecarEnv` — a **documented M10
exception** (node-trusted); user/BYOK secrets never use `SidecarEnv`.

## 3. Main Challenges

Folding the owner-online seal/deliver into the existing pre-provision window without a
deadlock; making the sidecar — whose key/upstream are baked into a construction-time closure
and whose control channel is agent-reachable — consume a BYOK key safely; and completing the
in-flight replay guards (which also fixes a live cross-node delivery bug).

## 4. Durable Secrets Catalog + CRUD RPCs (sp-7h6.1.1, sp-7h6.1.2)

A CP-blind `secrets` table (Bun/goose, mirroring `OwnerRepo`), keyed `(account_id, secret_id)`:

| column | notes |
|---|---|
| `account_id`, `secret_id` | PK |
| `type` | `inference-key` \| `github-token` \| `generic-kv` |
| `name` | display name |
| `provider` | for `inference-key` (e.g. `openrouter`); seam for sp-21b.1 |
| `target_container` | `AGENT` \| `SIDECAR` — copied to the spawn_artifact at attach |
| `env_var_name` / `dest_path` | routing ref — copied to the spawn_artifact at attach |
| `version` | uint64; equals the envelope's at-rest AAD `version` |
| `deviceset_epoch` | the device-set epoch this envelope was sealed under (§ sweep) |
| `envelope` | opaque `Envelope` JSON — **CP never decrypts** |
| `created_at` / `updated_at` | |

Note routing lives on the **catalog entry** and is *copied into a spawn_artifact at attach*;
there is **no new attachment table** — the existing `spawn_artifacts` rows are the per-spawn
binding, and `GetArtifacts` already re-threads them on resume/migrate.

**CRUD RPCs** (owner-authenticated; ciphertext in/out):

- **`CreateSecret`** — INSERT (fails if `secret_id` exists); sets `version=1`.
- **`PutSecret`** — optimistic CAS: `UPDATE … WHERE version = expected_version`,
  sets `version = expected_version + 1`; CAS miss → typed `Conflict` (caller re-reads,
  re-seals, retries). **The CP parses `envelope.at_rest` and rejects unless
  `account_id`/`secret_id` match and `version == new_version`** — so a client cannot store an
  envelope whose embedded AAD version diverges from the row, which would silently defeat
  at-rest downgrade rejection (roast R2 #10). The CP validates this *cleartext AAD metadata*;
  it still never sees the secret value.
- **`GetSecret`**, **`ListSecrets`**, **`DeleteSecret`**.

`Get/PutSecret` realize the `SecretsCPClient` seam in `web/src/keys/sweep.ts` → **unblock the
W3 revocation sweep** (sp-2ckv.7). The enrollment/revocation ceremony **calls `ListSecrets`
to populate `progress.secretIds` before `executeSweep`** (the sweep does not enumerate
itself). **Sweep creation-window** (roast R2 #12): each envelope carries `deviceset_epoch`;
the sweep re-seals only entries with `epoch < current`; secrets created during a sweep seal to
the *current* set and need no re-seal. Durable in the CP DB → CP restarts lose nothing.

## 5. `github-token` Type — Generic Here; GitHub-App Integration Split Out

`github-token` is supported as a **generic typed secret** (seal a pasted token → store →
attach → deliver to an agent file). The first-class GitHub integration — GitHub-App expiring
tokens, `(access, refresh, expiry)` + login capture, the transient hand-off, the net-new AS
`/github/refresh`, scope expansion beyond `read:user`, the `gh` credential helper, the
6-month re-link — is **epic `sp-v40s`**, which builds on this store and on `sp-u53.1` (GitHub
storage backend). Out of scope here.

## 6. Owner-Online Delivery, Folded Into the A4 Round-Trip (sp-7h6.1.3, .4, .9)

This is the corrected orchestration. It reuses the existing pre-provision window, so there is
**no new spawn state, no node→CP timeout message, and no post-ACTIVE delivery race.**

**Attach (declare).** `CreateSpawnRequest`/`ResumeSpawnRequest` carry the **attached
secret-ID set** (the owner's selection; app-manifest requests pre-select). At provision the CP
mints, for each attached secret, a **`sensitive` spawn_artifact** from the catalog entry's
routing (`target_container`, `env_var_name`/`dest_path`, `Sensitive=true`, `Inline=nil`) and
persists it via `AddArtifacts`. Resume/migrate re-thread these via the existing `GetArtifacts`
path — no new persistence.

**Seal + deliver in the existing client round-trip.** The fold-in (no deadlock — the CP is
*already blocked at `await`* waiting for the client):

1. Client `CreateSpawn`/`ResumeSpawn` → polls `GetPendingIntent`.
2. `GetPendingIntentResponse` is extended to return the assigned node's **HPKE sub-key**
   (cert chain + signed sub-key + generation) **from `nodeKeyCache`** (available
   pre-provision — *not* via `liveNode`, which needs a live container).
3. Client unseals each attached secret from vault and re-seals to the node sub-key with
   in-flight AAD `(spawn_id, generation, node_id, not_after, per-secret version, delivery_id)`.
4. `SubmitIntentRequest` is extended to carry the `repeated SealedSecret secrets` alongside
   the `SignedIntent`. The CP's `await` returns both.
5. `Provision` threads the sealed secrets into the **`StartSpawn`** message
   (`repeated SealedSecret secrets`), so the node has them in-hand atomically with start.

**Inject before agent exec (the gate is intrinsic).** In `CreateWithSelection`, between
`StartPod` and `StartAgent` (the verified gap), the node unseals each `SealedSecret` and
routes it (§7): agent-target → `InjectSecret` to tmpfs; sidecar-target → POST to the sidecar
control endpoint. `StartAgent` runs only after. No separate gate is built — sequencing **is**
the gate; the in-container `secretwait.go` (for MCP refs) then finds the files already
present. Journal-key delivery (already pre-`Restore`) is unchanged.

**Timeout / owner absent.** If the client never `SubmitIntent`s, the existing
`pendingIntents.await` TTL (~2 min) fires → provision fails → spawn → **Errored** via the
existing path (correct semantics for a never-started spawn — no bogus "resumable" Suspended
ghost, roast R2 #14). No new state or message.

**Deleted-but-attached secret** (roast R1 #19): at provision, a missing catalog entry
(`GetSecret` → NotFound) fails the create/resume with a typed error naming the secret.

## 7. Injection Routing

- **`inference-key{provider}` → sidecar (sp-7h6.1.4).** The sidecar cannot consume secrets
  today: key + upstream are captured in the Director closure at construction
  (`proxy.go:18`, `anthropic.go`), `Override` holds only the model
  (`atomic.Pointer[string]`). This epic: (a) add atomic `key` (+ `upstream` for
  multi-provider) to `Override`, read **per-request** in both the OpenAI and Anthropic
  Directors; (b) extend the control endpoint to set them; (c) the **node POSTs the unsealed
  BYOK key in the `StartPod`→`StartAgent` gap** — i.e. **before the agent container exists**,
  so the agent cannot sniff the in-transit key (the user's "initialize before agent
  processes start" ordering doubles as the security property). **Hardening** (roast R2 #9):
  bind the control listener to a **Unix socket in the sidecar's mount namespace** (not
  `0.0.0.0` TCP) so a netns-sharing agent has no reachable network endpoint; mid-spawn
  live key rotation (agent present) is **deferred** behind that hardening.
- **`github-token` / `generic-kv` → agent file (default).** Lands at
  `/run/spawnery/secrets/<env_var_name>` (`0600`) via the existing `InjectSecret`. Agent env
  violates M10 (Docker persists env); `agent-env` is supported only via an opt-in
  launcher-`export`-before-exec with the `/proc/environ` residual **documented** at the secret.
- **MCP-config-embedded secrets** (value rendered into an agent config file) require the
  `agentinstall` engine + a `manifest.json` (whose CP-side generation is an open sp-l5sx
  item) — **deferred**. MVP targets are sidecar + plain agent file only, which need neither.

## 8. In-Flight Replay Guards + Journal-Key Bug Fix (sp-7h6.1.8)

**Live bug (verified):** `handleSecretDelivery` builds `InFlightAAD{Version:0,DeliveryID:""}`
(`node/secrets.go:87`) while clients seal non-zero (`spawnctl/move.go:155`,
`web/api/migration.ts:440`). `InFlightAAD.bytes()` encodes all six fields → `OpenFromOwner`
returns `ErrAADMismatch` on **every real cross-node owner-sealed delivery** (journal-key
migration). Unit tests mask it (`secrets_test.go` seals with zeros; `move_test.go` pins
matching values). This is filed as a standalone bug (blocks migration today) and fixed here.

**Fix:** add per-secret `version` + `delivery_id` to the `SealedSecret` proto; the node
reconstructs the in-flight AAD from the **received** fields (not hardcoded zeros); then
enforces **version-monotonic** (per-`(account,secret)` high-water mark) + **deliveryId-once**
(seen-set). **Scope/durability** (roast R2 #13): the high-water mark + seen-set are scoped to
`(spawn_id, generation)` and held in node memory; generation fencing (`staleGen`) already
drops cross-generation replay, and start-time user-secret delivery is one batch per
generation, so the in-memory window is sound; an intra-generation spawnlet restart is
documented as the residual (re-delivery bumps generation on resume). Prerequisite for
secure delivery (.3/.4/.9).

## 9. Wire a Real Node-Revocation Checker (sp-7h6.1.11)

`subkey.VerifyNodeForSealing` defaults to `AllowAll` (`verify.go:42`, `IsRevoked`→`false`);
no real checker is injected (Go or web), so a **known-revoked node can currently receive
owner secrets** — a no-op in the "verified" chain this epic relies on. Implement a
`RevocationChecker` backed by an **AS-published node-revocation list** (new AS endpoint), wire
it into every `VerifyNodeForSealing` call site (Go) and `verifyNodeForSealing` (web). Shared
with the broader owner-sealed posture; owned here because user secrets are the first
production consumer.

## 10. Surfaces: CLI (strong path) + Web (sp-7h6.1.6, sp-7h6.1.10)

CLI is the strong-custody path: `spawnctl secret create|list|delete` (seal client-side,
typed, target kind+ref); `--secret <id>` (repeatable) on `spawnctl create`/`resume` →
`GetPendingIntent` (node sub-key) → seal → `SubmitIntent` with the sealed set. Empty
declared set → gate is a no-op. **Web UI**: typed CRUD sealing in-browser via `web/src/keys/`,
attach toggles at create; strong web guarantee rides the SPA-delivery slice (roast C2) —
CLI-strong until then.

## 11. Testing (sp-7h6.1.7)

Hermetic: CRUD + CAS (incl. `CreateSecret` first-write and `PutSecret` envelope-version
validation); **two replay legs tested separately** — at-rest downgrade (device-key `Open`)
and **in-flight** version-monotonic/deliveryId-once at the node (sp-7h6.1.8); scoping
(undeclared secret never delivered); deviceset-epoch sweep coverage.
Build-tagged e2e: attached secret delivered via the **SubmitIntent→StartSpawn** path and
injected before agent exec — BYOK key reaching the **sidecar** (control endpoint), a
generic secret in the **agent tmpfs file** — CP ciphertext-only throughout; missing
`SubmitIntent` → Errored via await-TTL; resume re-threads `spawn_artifacts` and re-delivers;
deleted-attached-secret fails closed; **a cross-node migration regression test that would have
caught the journal-key AAD bug.**

## 12. Decision Log

1. Custody → owner-online only; CP blind; managed key = documented M10 exception.
2. **Reuse `spawn_artifacts`** as the per-spawn binding (no new attachment table / routing
   columns / parallel gate) — reverses R1's mistaken decoupling.
3. **Fold delivery into the A4 intent round-trip** (node sub-key in `GetPendingIntent` from
   `nodeKeyCache`; sealed secrets in `SubmitIntent`→`StartSpawn`; node injects in the
   `StartPod`→`StartAgent` gap) — fixes the deadlock; no new CP state/timeout message.
4. Timeout via existing `pendingIntents.await` TTL → Errored (not a new Suspended ghost).
5. BYOK → sidecar control endpoint, delivered pre-`StartAgent` (agent absent → no sniff),
   over a **Unix socket** in the sidecar mount ns; live key/upstream atomics; mid-spawn
   rotation deferred.
6. `github-token` provisioning split to `sp-v40s`; here it is a generic type.
7. CAS: `CreateSecret` (insert) + `PutSecret` (strict `expected_version` CAS) + CP validates
   the envelope's at-rest `version`/ids.

## 13. Changelog

- **R1 (REVISE, 22 confirmed):** corrected overclaims; *(mistakenly)* decoupled from sp-l5sx;
  added a parallel gate + create/resume handshake; split GitHub.
- **R2 (BLOCK, 29 confirmed):** R1's sp-l5sx "unbuilt" premise was **false** (substrate
  exists); R1's "node key in Create/Resume response + deliver before exec" **deadlocks**
  (create is async; resume returns at ACTIVE). This rewrite: reuse `spawn_artifacts` +
  `DeliverSecrets` + `secretwait`; fold delivery into the A4 round-trip; drop the new
  WaitingForSecrets state/timeout message; correct PutSecret (CreateSecret + envelope-version
  validation + epoch); scope BYOK-sidecar reality + control-channel hardening; surface the
  **live journal-key AAD bug** and the **revocation no-op** as concrete tasks.

## 14. Child-Bead Mapping

| bead | section |
|---|---|
| sp-7h6.1.1 — catalog + CRUD (CreateSecret/PutSecret CAS, envelope-version validation, ListSecrets-for-sweep, epoch) | §4 |
| sp-7h6.1.2 — encrypt at rest CP-blind | §4 (reuses §2 envelope) |
| sp-7h6.1.3 — deliver folded into the A4 round-trip | §6 |
| sp-7h6.1.4 — inject: attach→spawn_artifact (agent file) + BYOK→sidecar control endpoint | §6, §7 |
| sp-7h6.1.7 — tests + e2e (incl. journal-key regression) | §11 |
| **sp-7h6.1.8 — in-flight replay guards + proto fields + journal-key bug fix** | §8 |
| **sp-7h6.1.9 — A4-folded delivery wiring (GetPendingIntent node key, SubmitIntent secrets, StartSpawn thread, gap inject)** | §6 |
| **sp-7h6.1.10 — spawnctl secrets CLI** | §10 |
| **sp-7h6.1.11 — real node-revocation checker + AS endpoint** | §9 |
| sp-7h6.1.6 — web UI | §10 |
| sp-7h6.1.5 — GitHub token → **MOVED to sp-v40s** | §5 |
