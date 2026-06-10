# Adversarial Review — Transient Tier + Owner-Sealed Secrets (Roast Results)

**Date:** 2026-06-10 · **Beads:** `sp-u53.5`, `sp-2ckv` · **Method:** 6-lens adversarial critique
(security / distsys / ops / ux / spec-consistency / feasibility) → dedup → 3-judge refutation
panel per finding (refuter, scope-judge, web fact-checker; ≥2 real votes to survive) →
synthesis. 80 agents; 36 raw findings → 24 deduped → **23 confirmed, 1 rejected**.

Reviews: [transient-tier design](2026-06-10-transient-tier-kopia-journal-design.md) ·
[owner-sealed-secrets design](2026-06-10-owner-sealed-secrets-design.md)

---

# Adversarial Review Report — SPEC A (Transient Tier / Kopia Journal) + SPEC B (Owner-Sealed Secrets)

**Date:** 2026-06-10 · **Status of specs:** approved, pre-implementation · **Panel:** 3-judge refutation per finding; severities below are **panel-adjusted** (several headline criticals were downgraded where a refutation leg landed).

---

## 1. Verdict

**Both designs are sound to implement — with mandatory amendments.** No finding requires an architectural rewrite. The HPKE-everywhere envelope, generation-tagged journal, bucket-per-spawn Garage, and cert-signed node sub-keys all survived adversarial review as constructions. What did *not* survive is a set of recurring patterns the amendments must fix before the relevant phase:

1. **Restore-source selection must be pinned, not selected** (SPEC A §3 contradicts itself and lifecycle §3; same-generation zombies pollute the manifest set). *Blocking for phase ①/②.*
2. **SPEC A's "durability floor = git tier" claim is falsified by SPEC A's own deletions** (WIP-branch persist dropped; scratch has no git tier; per-spawn delete-capable keys outlive episodes). *Fix in-spec before phase ①.*
3. **The phase-④ owner-sealed posture is not reconciled with CP duties and lifecycle guarantees** (marker probing, attach auto-resume, re-placement re-seal, retroactive sealing of pre-swap Kopia repos). *Must be amended before phase ② locks formats.*
4. **SPEC B asserts security mechanisms that don't exist or can't be built as written** (independent SPA delivery, "owner-signed" device set with no signing key, env delivery vs never-persist, polyfill vs non-extractable keys). *All cheap to fix now, expensive after phase ②.*
5. **Operational envelopes are unbudgeted** (Kopia scan cost, maintenance cadence, Garage outage semantics, node cache disk). *Gate phase ② on the benchmarks in §4 below.*

Two findings remain **critical** after refutation; sixteen are **major**; three **minor**. Every amendment below names the exact section to change.

---

## 2. Confirmed Findings (ranked)

### CRITICAL

---

#### C1. Restore selection is unanchored and self-contradictory; a same-generation zombie pollutes the manifest set Recreate restores from — SPEC A §3 (vs lifecycle §3, §6.1, §6.5)

**Scenario.** Lifecycle bumps generation *before* provisioning, so §3's rule "restore the latest manifest of the current generation" selects from an empty set and necessarily means latest-of-prior-gen — which conflicts with §3's other rule (restore the `persist_marker` manifest). The hole bites on Recreate-from-`unreachable`: no suspend marker exists, and the partitioned gen-N node keeps journaling well-formed gen-N manifests (it holds the Garage key — revoked only at spawn delete — and the in-memory repo password). Node B restores gen-5 manifest M1, dies inside the 1–5 s debounce window before any gen-6 snapshot lands; second Recreate falls back to latest-of-gen-5 = the zombie's torn M2. Reader-side generation filtering structurally cannot fence the generation being restored FROM. This is exactly the two-writer corruption lifecycle §6.1's server-side compare-and-set existed to kill — and §3 explicitly withdraws that CAS.

**Evidence.** A §3: "Resume restores only the latest manifest of the current generation" AND "persist_marker = the suspend snapshot's manifest ID"; "(This replaces lifecycle §6.1's server-side compare-and-set)"; "Zombie writers are harmless by construction." Lifecycle §3: resume claims `starting` (bump generation) before provisioning.

**Panel.** 3/3 real, critical sustained. All quotes verified verbatim; Kopia multi-writer mechanics confirmed; the spec's own zombie-writer e2e only tests *cross*-gen filtering and cannot catch this.

**Amendment — SPEC A §3:** Replace both restore rules with: *clean resume restores the persist_marker manifest IDs; Recreate samples latest-of-prior-gen exactly once inside the lifecycle §6.3 claim transaction and records the per-mount manifest IDs on the new container row before provisioning; all restores of that episode use the pin idempotently; any prior-gen manifest newer than the recorded cutoff is fenced garbage.* Add to §6 testing: zombie keeps writing through Recreate + a second Recreate; assert restored state == pinned manifest both times. Combine with per-generation Garage key rotation (M1 below).

---

#### C2. "Independent-of-CP" SPA delivery is asserted as existing but is an unbuilt, explicitly-flagged-open sp-ova residual — a CP-positioned SPA defeats the entire E2E guarantee for web users — SPEC B §1, §7 phase ② (A §4 inherits)

**Scenario.** Phase ② puts the root of trust (non-extractable device keys + all cert-chain/SAN/sub-key verification) into the web SPA, citing "strict CSP + SRI + the SPA's existing independent-of-CP delivery." Verified in-repo: no CSP, no SRI, no production SPA-delivery path or design exists anywhere; sp-ova §7/§9 carry "web-SPA delivery origin (must be independent of the CP)" as an unresolved gating residual ("If CP-served, web verification is theater"). Malicious JS in the spec's own threat model silently *uses* the live key to unseal every DEK and exfiltrate plaintext (non-extractability stops key theft, not plaintext theft), and forges all node verification. "Never plaintext" is false for every web-enrolled account — the default post-② account shape. SPEC A §4's journal-key sealing inherits this.

**Evidence.** B §1 (verbatim); sp-ova §7/§9 residual (2); grep of `web/` confirms no CSP/SRI; owner-sealed research §1 quotes 1Password's concession that a vendor-served web client defeats E2EE.

**Panel.** 2/3 real, critical sustained by both real votes. One refuter noted the CP does not currently serve the SPA at all — which both real votes turned around: *no* production delivery channel exists, so the "existing" mitigation denotes nothing. SPEC B converts a flagged-open residual into an asserted fact underpinning its core guarantee.

**Amendment — SPEC B §1, §7, §8:** Delete "existing" from §1's mitigation list. Add to §7 a **blocking dependency of phase ②**: an independently-delivered, integrity-verified SPA distribution slice (pinned static origin or AS-served, signed releases, SRI, strict CSP) — its own designed spec. Until it lands, scope the custody guarantee in §1 and the headline threat-model paragraph to native clients (spawnctl/CLI) only, and say so in the sp-2ckv.3 bead. Amend sp-ova's residual list to point at the new slice.

---

### MAJOR

---

#### M1. Durability floor for journal-only state is false: superseded/zombie nodes keep delete-capable Garage keys forever, while the spec deletes the git WIP floor and scratch's honesty notice — SPEC A §1/§2/§3/§6 *(downgraded from critical, 3/3 real)*

**Scenario.** Keys are "revoked on spawn delete (not per-generation)," so every node a spawn ever ran on retains a live rw+delete key (Garage has no object-lock/versioning — verified). The headline use case — "Move to cloud" because the laptop is suspect — leaves the suspect laptop able to `DeleteObject` the whole bucket while the spawn is suspended. The §3 acceptance ("durability floor = the git persistent tier") is falsified by the spec's own changes: scratch has **no** git tier (and the reset notice is removed on the promise scratch "survives via the journal"), and §6 deletes lifecycle §5's WIP-branch persist, so uncommitted work's sole copy is the journal. "Zombie writers are harmless by construction" is contradicted two sentences later by the delete residual.

**Panel.** 3/3 real; 2/3 downgraded to major (attacker needs node-daemon compromise that already saw plaintext; blast radius one spawn; loud failure in-episode). The compromised-CP rollback-then-prune leg was **refuted** (CP DoS is explicitly in-model and the CP holds the Garage admin API anyway).

**Amendment — SPEC A §3:** Mint a **fresh Garage access key per generation** on the same bucket; revoke the superseded key as part of suspend-complete/recreate/migrate (Garage's per-key-per-bucket admin API supports this cheaply — update the fencing paragraph and the e2e list). Scope "harmless by construction" to additive corruption only, and explicitly carve out delete/GC as a loss vector for scratch-only spawns. **SPEC A §6:** reinstate the WIP-commit-at-suspend floor for git-backed mounts (or reinstate the scratch/uncommitted-work honesty notice) — see also M3.

---

#### M2. Device removal never rotates the DEK: a stolen device key + any retained pre-removal envelope decrypts the CURRENT secret — SPEC B §2/§4 *(downgraded from critical, 2/3 real)*

**Scenario.** Workstation stolen; user removes the device. Removal "re-seals the SAME DEK to the reduced set" — payload ciphertext and DEK unchanged. Attacker with the stolen 0600 keyfile plus any pre-removal envelope (compromised CP that "deleted" nothing, DB backup) unseals the old DEK seal and decrypts the current payload. The §4 claim "the removed key cannot open new versions" is false for the removal-created version; the only mitigation ("superseded ciphertexts are deleted") is performed by the CP — the component the threat model declares compromised. The accepted "cached old plaintext" caveat is strictly weaker than indefinite decryptability of the at-rest current secret. Bites hardest on the never-rewritten Kopia repo password (§7).

**Panel.** 2/3 real, major. Refuter's valid point (absorbed): no re-keying revokes knowledge of an already-distributed *value* — proper remediation includes rotating the underlying secret — and §2's construction arguably implies fresh DEKs per content write. Both real votes: the revocation semantics as written are internally inconsistent and the fix is the spec's own "cheap by construction" rotation.

**Amendment — SPEC B §4:** On device removal (and recovery-code rotation), **mint a fresh DEK and re-encrypt the payload**, then seal to the reduced set. Add one sentence to §2: every content write mints a fresh DEK (forecloses the future-writes ambiguity). Document explicitly that pre-removal versions remain readable by the removed key wherever old envelopes survive, and that removal of a device should prompt rotation of high-value underlying secrets. Add the §7 test vector: removed key + old envelope must not decrypt the post-removal payload.

---

#### M3. Total key loss = every suspended spawn permanently gone, while SPEC A deletes the key-independent git WIP floor — cross-spec, A §1/§6 + B §8/S.8 *(downgraded from critical, 2/3 real)*

**Scenario.** Post-phase-④, lose all devices + recovery code (+ Argon2id passphrase) → every suspended spawn unresumable forever: all scratch, all uncommitted work. The compound-loss mode is the standard E2E tradeoff and B discloses no-escrow — but the deferral interacts with A's simultaneous deletion of the WIP-branch floor (uncommitted work previously landed on the user's own GitHub, recoverable with zero Spawnery keys) and removal of the scratch notice, and **that interaction is stated nowhere**. The same total loss applies to the single-Garage-box death (replication deferred).

**Panel.** 2/3 real, major. Refuter's corrections (absorbed): A §6 keeps git push at suspend (floor = committed work as of last suspend, not last manual push); WIP machinery was designed, never implemented; Argon2id passphrase is a third recovery root; B's phasing ships full custody before journal-key cutover. The surviving defect: undisclosed compound-loss mode + deleted/weakened floors for uncommitted work and scratch.

**Amendment — SPEC A §6:** Keep WIP-commit-at-suspend for git-backed mounts as a key-independent floor (opt-out allowed); if not, restore the honesty notice for uncommitted/scratch data. **SPEC B §4 (first-device ceremony) + §7:** add mandatory user-facing copy: "without this code and your devices, suspended spawn contents cannot be recovered by anyone, including Spawnery." **SPEC B §7:** gate the phase-④/③ journal-key cutover on shipping either server-assisted rate-limited recovery or the reserved CP+AS 2-of-2 split.

---

#### M4. "Owner-signed device set" is unimplementable as inventoried (X25519 cannot sign), has no genesis anchor, and no specified client-side chain verification — SPEC B §1/§4, S.1/S.5 *(3/3 real)*

**Scenario.** The only owner-side keys are X25519 (no signature op; Ed25519 explicitly deferred in §8), so no key can produce the "owner-signed" set. The inevitable degradation — AS records whatever an authenticated session uploads — makes a stolen AS session sufficient to append an attacker pubkey; the next routine re-wrap on any honest device seals **every DEK** (journal keys, BYOK, user secrets) to the attacker. §4's "an already-enrolled device verifies and re-seals" never says what "verifies" checks. The project's own research (§6, Tailnet Lock) prescribed the hash-chained owner-signed log the design dropped for the device set while adopting it for the node leg.

**Panel.** 3/3 real, major (not critical: CP-compromise headline guarantee untouched; AS is the hardened anchor; genesis-TOFU leg is weak since Tailnet Lock enablement is also TOFU). Must land before phase ② freezes the key inventory and registry format.

**Amendment — SPEC B §1/S.1:** Add a per-device **non-extractable signing keypair (WebCrypto ECDSA P-256; Go crypto/ecdsa)**; BIP-39 seed derives both X25519 and signing keys. **§4:** specify the chain — genesis statement co-signed by device₁ + recovery key at setup; every mutation signed by an existing member; monotonic version + hash chain; clients verify the full chain against the owner-held root and pin the head locally **before every re-wrap/seal**, refusing unsigned/regressed sets (AS stores, never authors). Add a poisoned-device-set case to §7's test matrix. State explicitly what an AS compromise can/cannot do, and amend sp-ova §9's AS-compromise row in the same change.

---

#### M5. Maintenance-only-at-suspend × seconds-cadence snapshots = unbounded index-blob growth; quick maintenance is not the deletion vector the fencing rule guards — SPEC A §2/§3, T.7 *(3/3 real, downgraded from critical)*

**Scenario.** Kopia relies on ~hourly quick maintenance to compact n/q index blobs (verified: docs + field reports of 28k–90k index blobs wedging maintenance). A multi-day busy episode under the 1–5 s debounce accumulates thousands of uncompacted index blobs: snapshot latency silently exceeds the debounce (widening the advertised loss window), resume materialization stalls, and suspend inherits a deferred full-maintenance backlog while lifecycle §4 makes attaches wait and "Persist failure → error." Crucially, quick maintenance **never deletes metadata without another copy** — so allowing it does not reopen the zombie-deletion hole motivating §3.

**Panel.** 3/3 real, major (degradation is gradual; §5 telemetry includes scan duration; "typically at suspend" doesn't *forbid* CP-commanded mid-episode maintenance — but no cadence is designed, and the wedged-suspend chain is plausible).

**Amendment — SPEC A §2 + T.7:** Split the policy by safety class: **quick (index-compacting) maintenance runs on a regular cadence** (node-local on Kopia defaults, or CP-commanded hourly via heartbeat to preserve §3's letter); only deleting full maintenance stays CP-commanded. Add CP-scheduled periodic full maintenance for long-running spawns; run suspend-time maintenance **after** the spawn is marked suspended, decoupled from the persist-failure path; add a per-repo index-blob-count alarm to §5; gate phase ② on the 48 h churn soak (see §4).

---

#### M6. Lifecycle §6.6 marker probing requires reading Kopia manifests — impossible for the phase-④ key-less CP; SPEC A promises a probe its own §4 removes — A §3/§4 + B §3 *(2/3 real)*

**Scenario.** CP restarts mid-suspend with the spawn in `suspending`; §6.6 requires probing per-mount persist_markers ("backend confirms intact"). The marker is a Kopia manifest ID inside encrypted q packs — listing snapshots requires the repo password the phase-④ CP can never hold, and B's delivery flow has no CP-probe or owner-assisted leg. The CP must either trust stale DB rows (false `suspended` on torn persist → silent stale restore without the `recovered` flag) or flip every CP-crash-during-suspend to `error`.

**Panel.** 2/3 real, major. Refuter's reading (DB marker rows written incrementally are the real probe) is itself an unwritten amendment — i.e., the finding's own alternative fix; the spec as written promises a backend-intactness probe the CP cannot perform.

**Amendment — SPEC A §3:** Add a **key-free durability witness**: after the final snapshot flush the node writes a tiny plaintext sentinel per (spawn, generation, mount) — e.g. `s3://<bucket>/markers/gen-<N>/<mount>` containing the manifest ID — via its Garage key; CP marker-probing becomes S3 HEAD/GET (content integrity remains Kopia-AEAD-verified at restore). Alternatively, explicitly amend lifecycle §6.6 for journaled spawns: DB marker row = clean-suspend criterion, integrity verification moves node-side to restore time — recorded as a deliberate weakening. Either way, stop promising a probe the phase-④ CP can't do.

---

#### M7. Manifest pruning "after a successful resume" has a crash window leaving a generation with ZERO restorable manifests; retention clause contradicts prune clause; Kopia safety parameters unpinned — SPEC A §2/§3 *(2/3 real)*

**Scenario.** Resume to gen N+1 succeeds; CP prunes gen-N manifests; node B dies before its first gen-N+1 snapshot lands (1–5 s window) → Recreate finds nothing to restore: scratch gone entirely, git back to last push — the precise loss the tier exists to prevent. "Successful resume" is undefined; "last-N + per-generation heads" vs "prune superseded generations' manifests" contradict. Separately, §3's zombie-sweep argument silently depends on Kopia SafetyFull (24 h blob-delete min age, 4 h GC margin — verified) which the spec never pins.

**Panel.** 2/3 real, major. Refuter's heads-retained reading is plausible but unwritten — exactly the ambiguity to close now.

**Amendment — SPEC A §2:** Define the prune precondition as a happens-before anchor: *prune gen N only after at least one durable gen-N+1 snapshot manifest exists for EVERY mount* (CP gates on the node reporting its first post-resume snapshot); per-generation heads are retained until that anchor. **§3:** pin maintenance to **SafetyFull** explicitly, and add: CP commands maintenance only when no live container row exists for the spawn AND the prior generation's node is confirmed stopped or its 24 h margin has elapsed.

---

#### M8. Owner-in-the-loop sealing has no protocol for re-placement, mid-flow failure, or unenrolled clients — resumes strand, "attach auto-resumes" silently regresses, web-from-anywhere can't resume — B §3/§4 + A §4/§5 vs lifecycle §3/§6.3/§6.5 *(2/3 real)*

**Scenario.** AAD binds one placement; a failed start on node X forces a fresh owner-client verify+seal for node Y — unsequenced in lifecycle §6.3, with no timeout state if the user closed the laptop mid-"Move to cloud" (promised "atomic from the user's view"). Post-④, lifecycle §3's "attach auto-resumes" silently fails from any authenticated-but-unenrolled machine (A's deferral covers only headless/scheduled runs, not interactive attach); a traveling user on a hotel browser cannot resume anything, and the recovery-code escape hatch plants a permanent device key in an untrusted IndexedDB. Recreate-after-node-failure needs the same delivery.

**Panel.** 2/3 real, major. Refuter correctly notes generic `error`/`unreachable` catch-alls exist — but `unreachable`'s user-acked Recreate is the wrong terminal for a closed-laptop migration, and the delivery-failure semantics are genuinely unspecified.

**Amendment — SPEC B §3:** Specify the delivery sub-protocol inside the resume transition: client holds an interactive session for the whole resume; CP may request up to K re-seals for re-placements within it; a `starting` episode whose key delivery times out transitions to a **defined** state (back to `suspended`, target restore artifacts wiped). Pull the reserved pre-seal single-use-wrap seam forward for same-resume re-placement. **Lifecycle §3 (ripple, via A §6):** post-④, auto-resume-on-attach requires an enrolled device; unenrolled attach returns "resume requires an enrolled device," never a hang. **SPEC B §4:** define the unenrolled web session (read-only list + "approve from your phone / recovery code only on a trusted device" banner), an ephemeral auto-expiring web-device class, and an explicit warning before web recovery-code entry.

---

#### M9. Kopia has no dirty-path API — full-tree rescan per snapshot makes the 1–5 s cadence scan-bound on node_modules-scale trees; the include-default's revisit metric (upload bytes) can never fire — SPEC A §2, T.2/T.3/T.12 *(2/3 real)*

**Scenario.** Every `kopia snapshot` re-walks the entire tree (verified: only upload is incremental; the changeset feature request is open). A monorepo with node_modules included = 100k–1M entries → seconds-to-tens-of-seconds per scan, fired every 1–5 s during builds: permanent stat storm, laptop CPU/battery burn, effective loss window = scan + upload (minutes) exactly during high-churn windows. The T.2 revisit trigger measures upload amplification — CDC dedup keeps that quiet while scan latency blows up; the watcher's dirty paths are discarded at the Kopia boundary.

**Panel.** 2/3 real, major. Refuter's points absorbed: §5 already lists scan duration as telemetry and §8 defers overlayfs harvesting "if scan cost demands" — detection exists, but no gate, adaptive cadence, or correct revisit metric.

**Amendment — SPEC A §2:** (a) adaptive debounce — never schedule the next snapshot sooner than k× the last scan duration, per mount; (b) change T.2's revisit trigger from upload-amplification to **scan-duration-based**; (c) note the embedded-library lever the CLI lacks: evaluate a custom `fs.Directory` serving cached entries for watcher-untouched subtrees. **§7:** phase-② gate: scan p95 < debounce on a 500k-file fixture (see §4).

---

#### M10. "Delivered via memory/env" violates never-persist by construction: Docker/containerd persist env to disk; the §6 canary grep fails or is silently unenforced; whole-process memory-dump zero-hits is unsound under Go — SPEC B §6/§7 *(3/3 real)*

**Scenario.** Phase ③ rides the existing env path (verified in-repo: `manager.go:304` SidecarEnv → `docker_pod.go:68` / `cri/backend.go:88`); Docker persists Env in `/var/lib/docker/containers/<id>/config.v2.json` (empirically reproduced by a judge), containerd in its metadata DB — so the spec's own prescribed channel writes plaintext to disk on every spawn. §6(b)'s zero-hits memory dump contradicts the spec's own Go-GC concession: plaintext transits the heap inside HPKE Open.

**Panel.** 3/3 real, major.

**Amendment — SPEC B §6:** Specify the delivery channel as part of the design: sidecar/agent fetches the secret at startup over a **pod-local socket or exec-stdin handshake** from the spawnlet (or unlinked-after-read tmpfs file with a swap note) — never via PodSpec env. Put `/var/lib/docker` and the containerd root **explicitly in scope** of the §6(a) file grep. Scope §6(b)'s memory assertion to the memguard region + zeroize-hook verification; keep zero-hits only for the file-grep leg.

---

#### M11. Delivery-leg AAD omits secret version and has no freshness mechanism: a compromised CP replays a pre-rotation ciphertext; the promised "replayed rejected" e2e is unimplementable for same-context replay — SPEC B §2/§3/§7, S.4 *(2/3 real)*

**Scenario.** Owner rotates a leaked API key (at-rest version bumped), but in-flight AAD is (spawnId, generation, nodeId, notAfter) — no version. Within the 72 h sub-key window, a CP that cached the old delivery re-feeds the pre-rotation secret; rotation silently fails to take effect node-side. Generation is no defense: the node learns its generation FROM the CP. RFC 9180 single-shot Open verifies AAD equality only — "expired" needs an unstated node clock check; same-context replay rejection needs a uniqueness mechanism HPKE doesn't have, so §7's test will be quietly watered down.

**Panel.** 2/3 real, major (no new plaintext goes to the attacker — the stale secret goes to a legitimate node — hence not critical).

**Amendment — SPEC B §3:** In-flight AAD becomes **(spawnId, generation, nodeId, notAfter, version)**; node rejects versions older than the highest seen per secret. Specify node-side checks explicitly: reject notAfter < now (bounded skew); include a node-issued one-time deliveryId in the AAD, accepted exactly once. **§7:** rewrite the rejection test list to name the mechanism behind each case (AAD mismatch vs clock vs one-time challenge).

---

#### M12. 72 h sub-key validity ≠ revocation: no revocation check exists on the sealing path, and a compromised node re-signs fresh sub-keys with its cert key, so the real bound is the unspecified leaf-cert lifetime — SPEC B §1/§3, S.3 *(2/3 real)*

**Scenario.** Node stolen; owner rotates the leaked secrets — but sealing clients verify only chain → SAN → sub-key sig → unexpired (no CRL/OCSP/deny-list anywhere), and since sub-key rotation is a purely local re-sign, the attacker mints valid sub-keys until cert expiry. With a compromised CP steering placement, every create/resume keeps delivering freshly rotated secrets to the attacker. The §1 claim "revocation latency is bounded by validity" rests on a research-doc premise (AS stops signing sub-keys) the final design dropped.

**Panel.** 2/3 real, major (incident-response gap, not steady-state leak; honest CP lets the owner stop placement).

**Amendment — SPEC B §1/§3:** Add an owner/AS-driven kill switch independent of expiry: an AS-published, client-checked **node revocation list** (or short-lived signed allow-list) consulted at §3 step 2, and/or owner-marked node revocation in the AS registry that clients refuse to seal past. Specify the node leaf-cert lifetime, correct the "bounded by validity" sentence, and document that secret rotation must be paired with node revocation/deregistration to be effective.

---

#### M13. Garage failure semantics undefined on two hot paths: an outage turns fleet idle-suspends into user-visible `error` and blocks creates; journaling halts with no spool/bound — SPEC A §1/§3/§8 vs lifecycle §4 *(3/3 real)*

**Scenario.** Single-binary prod Garage reboots for 10 minutes: idle-suspend persists fail → "Persist failure → error" across the fleet (user-driven recovery for healthy spawns); create-time bucket/key mint 500s; async journaling silently stops with no spool, lag bound, or alert threshold — the seconds-level loss window is unboundedly void. (Bucket-count headroom at 10k spawns is unverified — flagged low-confidence; Garage docs publish no limits.)

**Panel.** 3/3 real, major. §5's journal-lag metric exists but no threshold/UI/degraded mode does; §8's deferral covers replication tuning, not outage semantics.

**Amendment — SPEC A §3/§6:** Define degraded modes: suspend with unreachable Garage completes as `suspended(journal-stale, last good snapshot T)` when the git tier persisted, not `error`; bounded local disk spool + lag alert threshold surfaced in UI/telemetry (note: a same-node spool does not survive node death); **lazy bucket/key mint on first snapshot** so creation never blocks on Garage; commit a minimal replication posture or documented RPO before phase ②; add a CP reconciler for orphaned buckets/keys.

---

#### M14. BIP-39 + device-key ceremony at account setup re-creates the sp-73q onboarding gauntlet; after phase ③ there is no escape hatch — SPEC B §4/§7 × A §1 *(3/3 real)*

**Scenario.** A curious marketplace user must sit through keygen + a 24-word recovery ceremony before doing anything. sp-73q's recorded P1 fix was explicitly "LAZY vault (no passphrase until first BYO key — free tier never needs it)"; SPEC B mandates the ceremony at setup, never cites sp-73q, and phase ③ deletes the CP-custodied path fleet-wide — making the ceremony a hard prerequisite for basic suspend/resume of every spawn.

**Panel.** 3/3 real, major. One paragraph to fix now; expensive after phase ② builds the signup ceremony.

**Amendment — SPEC B §4/§7:** Account creation stays OAuth-only; the key ceremony triggers **lazily on the first secret-bearing action** (BYOK, opting a spawn into owner-sealed journaling). Phase ③ becomes a **per-account opt-in cutover**, with the interim KeyProvider retained as a permanent, honestly-labeled "Spawnery-managed (we can technically read journal contents)" tier. Cite sp-73q in the decision log.

---

#### M15. Named web HPKE polyfill cannot consume non-extractable X25519 CryptoKeys — following §2 literally forces extractable device keys, silently voiding §1's XSS boundary — SPEC B §1/§2, S.1/S.2 *(2/3 real)*

**Scenario.** hpke-js's X25519 KEM is pure noble: `derive(sk: Uint8Array, …)` over a fake XCryptoKey with `extractable = true` (verified from source). The recipient Open leg uses the static device key, so the path of least resistance is an extractable key — XSS then exfiltrates the long-term scalar, and all round-trip tests pass. Refuter identified that a WebCrypto-native HPKE lib (panva/hpke) exists and resolves this — i.e., the fix is library selection plus a spec sentence, which is exactly why it must be written down.

**Panel.** 2/3 real, major (silent failure mode in the secondary boundary; primary CP-compromise model unaffected).

**Amendment — SPEC B §2:** Replace "noble-based polyfill" with: recipient-side DHKEM **must** compute DH via `crypto.subtle.deriveBits({name:'X25519', public: enc}, deviceKey, 256)` on the non-extractable CryptoKey (WebCrypto-native HPKE lib or custom KDF leg per RFC 9180, validated against A.1 test vectors); noble permitted only for ephemeral/sender operations. **§7:** acceptance test asserting `deviceKey.extractable === false` end-to-end and no code path imports raw device-key bytes; enrollment feature-detects native X25519 and **refuses** rather than falls back.

---

#### M16. Interim CP-custodied phases: gitignored/agent-written secret files journaled CP-readable, migration ships before sealing, and the phase-④ swap cannot retroactively seal pre-existing Kopia repos — A §2/§4/§7 + B §6/§7 *(3/3 real, with refuted legs)*

**Scenario.** The strongest surviving leg (all three judges): Kopia's master key is replicated in `kopia.repository` and **every pack blob**; a password/provider swap rewrites only the format blob (kopia#309 verified) — so phase ④'s "swaps the provider / deletes the interim path" leaves every pre-swap journal readable forever to anyone who captured the CP-held passwords (backups, past breach), falsifying §4's target guarantee and sp-u53.5.4's close criterion for the entire transition cohort. Secondary legs: the interim reverses lifecycle §5's gitignored-never-leaves-the-node posture for the file class where real credentials live (with phase-③ "Move to cloud" shipping before sealing), and B §6's "the journal never sees secrets by construction" is scoped only to Spawnery-delivered secrets while the canary harness can't catch agent-written ones.

**Panel.** 3/3 real, major. The interim CP-readability itself is an explicit, sound acceptance (refuted as a standalone finding); what's unsound is the cutover's false retroactive guarantee plus missing disclosure.

**Amendment — SPEC A §7 phase ④:** Specify the swap: **create a NEW Kopia repo per spawn (or force full re-encryption) and migrate snapshots; pre-swap repos are documented CP-readable and retired.** **§2:** add a default-exclude secrets glob (`.env*`, `*credentials*`, `id_*`, `*.pem`, token files) during the interim, or gate self-hosted→cloud MigrateSpawn on phase ④ — at minimum, required disclosure copy per the demo §5 messaging rules. **SPEC B §6:** scope never-persist to Spawnery-delivered secrets explicitly; add a documented residual that agent-written secret files ARE journaled (CP-readable until sealing); extend the canary harness with agent-written credential patterns.

---

#### M17. Suspend's final snapshot vs in-flight async watcher snapshots: no drain/barrier, and "latest" is undefined — A §2/§3 *(2/3 real, severity contested)*

**Scenario.** A pre-quiesce watcher scan (racing live writes → torn tree) can land its manifest after the suspend snapshot; resume-by-"latest" could pick it over the marker. Two judges noted Kopia orders by **StartTime** (torn W started before S, so sorts earlier), which defuses the headline data-loss path *if* the implementer uses Kopia's convention — but the spec neither defines "latest," binds clean resume to the marker, nor mandates a barrier, so nothing forces the safe implementation.

**Panel.** 2/3 real; both surviving votes converge on a clarification-to-major gap rather than a guaranteed loss.

**Amendment — SPEC A §2/§3:** Specify the journaler as a per-mount serialized snapshot queue with a suspend barrier: cancel pending debounce timers, drain/abort in-flight snapshots, take the final snapshot, send markers, only then maintenance/teardown. Define "latest" = latest StartTime among **complete** manifests; clean resume restores the marker manifest unconditionally (latest-of-gen is only the crash fallback per C1's pin). Add the e2e: suspend racing a deliberately slowed watcher snapshot; assert restore == marker.

---

#### M18. Scratch silently flips from "no residue" to "continuously copied to a CP-readable remote" with no user disclosure or per-mount opt-out — A §1 (T.8)/§4 vs data-mounts M.3/M.7 *(2/3 real, panel split on severity)*

**Scenario.** Scratch's contract was "always seed, never persist… no residue"; SPEC A journals it continuously to Garage, CP-readable through phases ①–③, removes the only user-facing scratch notice, and offers path excludes but no whole-mount ephemerality knob. Violates the project's own messaging rule ("Disclose plainly…"). Panel corrections: the amendment of data-mounts is formally declared (header "Amends:"), and the secret-app example was wrong in detail (its secret is seed content) — the surviving core is the missing disclosure + opt-out.

**Panel.** 2/3 real; one judge minor, one major.

**Amendment — SPEC A §1:** Add a per-mount `durability: ephemeral | journaled` knob (manifest- and user-settable); existing scratch defaults to journaled with a one-time notice. **§4:** required interim disclosure copy: "journals are encrypted; until <milestone>, Spawnery infrastructure can technically decrypt them." Audit seed/example apps for reset-on-suspend reliance before deleting the lifecycle notice.

---

### MINOR

---

#### m1. Migration UX/ops kernel: node-side Kopia cache disk unbudgeted; no preflight/cancel/failure copy; include-node_modules inflates cold cloud restores through a residential uplink — A §2/§5/§6 *(2/3 real, downgraded from major)*

Panel refuted "zero failure states" (lifecycle state machine covers it) and treated include-by-default as an explicit acceptance — but confirmed: per-spawn repos × ~5 GB + 5 GB default cache soft limits with no sizing/cleanup-on-delete anywhere; no preflight size/ETA or cancel; the T.2 revisit trigger watches the write path, not the restore path; an 8 GB cold pull over a 20–50 Mbps home uplink is 25–45+ min behind "instant" copy.
**Amendment — A §5/§6:** set embedded-Kopia cache limits per spawn; delete caches on spawn delete/migrate-away; add node disk headroom to scheduler inputs; preflight ETA + cancel + "move failed — your data is safe, resume here" copy in the move dialog; temper "instant"; make migrate-to-cloud restore artifact dirs lazily/in background; add the constrained-link benchmark (§4 below).

#### m2. Sub-key rotation overlap unspecified — B §1/§3 *(3/3 real)*

Nothing requires retaining the superseded private half until notAfter; a superseded key stays verification-valid up to 36 h, and §3 defines no re-fetch/re-seal retry, so deliveries fail opaquely.
**Amendment — B §1:** node retains all unexpired sub-key private halves (max 2 concurrent), selecting by key ID/trial-Open. **B §3:** on Open failure/staleness, client re-fetches, re-verifies, re-seals (bounded retries); exercised in §7 e2e.

#### m3. Decision log T.6 contradicts §3: "per-spawn prefix-scoped creds" is impossible in Garage — A T.6 *(3/3 real)*

**Amendment — A Appendix T.6:** one-line edit: "per-spawn bucket + per-bucket access key (Garage has no prefix policies)"; record the one-bucket-per-live-spawn assumption for capacity testing.

---

## 3. Rejected, but worth a sentence

- **"Lossless seconds-level" vs phase ①'s 60 s periodic trigger** — rejected 2:1: the staged rollout is disclosed in-document and phase ② carries the loss-window acceptance gate; no silent guarantee break.
- Worth carrying from refuted *legs* of confirmed findings: the compromised-CP **rollback-then-prune** path is in-model DoS, not a new guarantee break (CP holds the Garage admin API anyway); B §3.4's replay claim is **scoped to other nodes/contexts** — but should still say "expired" rejection is a clock check, not an AAD property (one line).

---

## 4. Spikes / benchmarks / e2e gates before or during phase ①–②

| Gate | Phase | What it proves |
|---|---|---|
| **Zombie double-Recreate e2e** — partitioned node keeps journaling through Recreate ×2; assert restore == pinned manifest both times | ① | C1 pin works; reader-side filtering hole closed |
| **Suspend-race e2e** — deliberately slowed watcher snapshot racing suspend; assert restore == marker | ① | M17 barrier + "latest" definition |
| **Per-generation Garage key mint/revoke spike** via admin API (CreateKey/AllowBucketKey/DeleteKey) incl. revocation timing | ① | M1 fix is as cheap as claimed |
| **Kopia scan benchmark** — 500k-file fixture; gate phase ② on scan p95 < debounce; verify adaptive-debounce behavior | ②-gate | M9 |
| **48 h churn soak** — busy spawn, no suspend; bound snapshot p95, repo-open time, index-blob count (alarm threshold) under the chosen quick-maintenance cadence | ②-gate | M5 |
| **Restore-latency benchmark over a constrained (50 Mbps) link**, not just LAN | ② | m1 — the LAN benchmark will pass while the real path fails |
| **Garage many-bucket load test** at target spawn count (≈10k buckets/keys) + orphan reconciliation | ①/② | M13 low-confidence headroom flag |
| **WebCrypto recipient-DHKEM spike** — deriveBits on non-extractable X25519 key, validated against RFC 9180 A.1 vectors; CI assertion `extractable === false` | ② | M15 |
| **Never-persist harness scope check** — reproduce env persistence in `/var/lib/docker/containers/*/config.v2.json` and containerd metadata; confirm the socket/stdin delivery leaves zero hits with runtime state dirs in-scope | ③-gate | M10 |
| **Pre-swap repo re-key spike** — confirm new-repo-per-spawn snapshot migration cost at phase ④ cutover (kopia#309 constraint) | before ④ design freeze | M16 |

**Bottom line:** amend first (C1, C2, M1, M4, M10, M15, M16 are the format/guarantee-shaping ones — cheapest now, costly after phase ② freezes envelope formats, key inventories, and the signup ceremony), then implement. Nothing here invalidates the journal-tier or owner-sealed architectures themselves.