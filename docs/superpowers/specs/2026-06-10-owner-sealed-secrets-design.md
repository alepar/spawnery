# Owner-Sealed Secrets — E2E Secret Custody & Delivery (Design)

**Bead:** `sp-2ckv` (epic; brainstorm task `sp-2ckv.1`)
**Status:** Approved + adversarially reviewed; amended 2026-06-10
**Date:** 2026-06-10
**Research basis:** [brief](2026-06-10-owner-sealed-secrets-research-brief.md) ·
[results + merged synthesis](2026-06-10-owner-sealed-secrets-research-results.md) ·
[cloud run](2026-06-10-owner-sealed-secrets-research-results-cloud.md)
**Adversarial review:** [storage+secrets roast](2026-06-10-storage-secrets-adversarial-review.md)
(amendments folded in — change markers `[roast Cn/Mn]`).
**Builds on:** [Node Auth & Unified Identity](2026-06-05-node-auth-unified-identity-design.md)
(`sp-ova`; partially implemented on `worktree-node-auth-sp-ova` — PKI, AS, mTLS, AS-signed
sessions, client pin/verify) · [E4 Identity & Secrets](2026-05-28-spawnery-e4-identity-secrets-design.md)
**Amends:** sp-ova §3.1 (enrollment-token scoping, §5), sp-ova §9 (AS-compromise row, §1)
**Blocking dependency:** an **independently-delivered web SPA** slice (pinned static origin /
AS-served, signed releases, SRI, strict CSP) gates phase ② — see §1 + §7 `[roast C2]`.
**Consumers:** transient-tier journal keys (`sp-u53.5.4`,
[design §4](2026-06-10-transient-tier-kopia-journal-design.md)), user secrets store (`sp-7h6.1`),
BYOK inference keys. Resolves `sp-gtm` (CP key-vending MITM) for secret delivery.

The primitive: **small user secrets stored at the CP as ciphertext it cannot read, unsealed only
by the owner's clients, delivered re-sealed to cryptographically verified nodes — plaintext
exists only at the owner's client and in the target node's memory.** Threat model: a fully
compromised CP yields ciphertext, metadata, and DoS — never plaintext. The AS is an identity
authority, not a key escrow.

> **Scope caveat `[roast C2]`:** this guarantee holds for **native clients (spawnctl/CLI) today**.
> For the **web SPA** it holds only once the SPA is delivered independently of the CP (a designed
> slice that blocks phase ②); a CP-served SPA can serve malicious JS that uses the live device key
> to exfiltrate plaintext, so until that slice ships the web path is explicitly not yet trusted.

---

## 1. Trust & key inventory

**Owner side — per-device keypairs are the root of trust:**

- Each device (browser, phone, workstation CLI) holds **two non-extractable keypairs `[roast M4]`**:
  an **X25519** keypair (HPKE sealing/unsealing) and an **ECDSA P-256 signing** keypair
  (device-set authorization — X25519 cannot sign, and Ed25519-in-WebCrypto isn't broadly reliable
  until ~2027). Both derive from one BIP-39 seed.
  - Web: WebCrypto `CryptoKey` (`extractable: false`) in IndexedDB — a browser-enforced boundary
    WASM cannot replicate; XSS can *use* a live key but not exfiltrate it. CLI/daemon:
    `crypto/ecdh` X25519 + `crypto/ecdsa` P-256, keyfiles `0600` under `~/.config/spawnctl`.
  - **The XSS boundary only holds if the SPA is delivered independently of the CP `[roast C2]`**
    — that delivery channel **does not exist yet** (no CSP/SRI/static origin in `web/`; sp-ova
    §7/§9 carry it as an open residual: "if CP-served, web verification is theater"). It is a
    **blocking dependency of phase ②** (§7). **Until it ships, the custody guarantee below holds
    for native clients (spawnctl/CLI) only**; the web path is explicitly not yet trusted.
- A **BIP-39 recovery code** derives an **always-enrolled "virtual device"** (X25519 + signing) —
  recovery without server escrow.
- An **Argon2id passphrase fallback** KEK (client-side derivation only; parameters pinned in
  signed client code — the Bitwarden lesson: the server must never influence KDF cost).
- **Passkey-PRF is deferred Tier-2** (convenience unlock wrapping a device key): fabric-scoped
  sync, no Firefox, device-bound Windows Hello, lose-credential = lose-data. Never PRF-only.

**Node side — identity exists (sp-ova); add an encryption sub-key:**

- A node's identity is its AS-anchored mTLS cert (SAN
  `<nodeId>.<accountId>.<class>.nodes.spawnery.internal`, name-constrained chain, pinned roots).
- New: each node generates an **X25519 HPKE sub-keypair** and publishes the pubkey in a small
  structure **signed by its cert key** with an expiry — the RFC 9345 delegated-credential /
  Signal signed-prekey pattern. **Validity 72 h, rotate at half-life.** The node **retains all
  unexpired sub-key private halves (max 2 concurrent)** and selects by key-ID / trial-`Open` so a
  rotation mid-delivery doesn't fail opaquely `[roast m2]`.
- **Revocation ≠ expiry `[roast M12]`:** validity alone does not revoke — a compromised node
  re-signs fresh sub-keys with its own cert key indefinitely. So sealing clients additionally
  consult an **AS-published, client-checked node revocation/deny-list** (or short-lived signed
  allow-list) at delivery step 2; an owner can mark a node revoked in the AS registry and clients
  **refuse to seal** past it. Specify the node leaf-cert lifetime; document that **secret rotation
  must be paired with node revocation** to be effective.

**Verification chain (closes `sp-gtm` for secrets):** client pins Root CA + AS pubkeys (shipped,
`sp-9wd`) → verifies node cert chain + SAN against expected `(accountId | cloud, class)` →
**checks the node is not on the AS revocation list** → verifies sub-key signature + expiry → only
then seals. A compromised CP can relay keys but cannot mint trust (Tailnet Lock property).

**Device-set registry — hash-chained, owner-signed `[roast M4]`:** device pubkeys (X25519 +
signing) live **at the AS**, but the AS **stores, never authors**. The set is a monotonic,
hash-chained log: a **genesis statement co-signed by device₁ + the recovery key** at first
enrollment; **every mutation signed by an existing member's signing key**. Clients verify the full
chain against the owner-held root and **pin the head locally before every re-wrap/seal**, refusing
unsigned or version-regressed sets. Consequence: a **stolen AS session cannot inject a device** (it
can't forge a member signature) — without this, the next routine re-wrap would seal every DEK to an
attacker. (Amend sp-ova §9's AS-compromise row accordingly.)

## 2. Secret store & envelope format

- The CP DB stores **opaque envelopes**. Construction: payload encrypted under a random **DEK**
  (AEAD); the DEK is sealed per-recipient with **HPKE Base mode** (DHKEM-X25519-HKDF-SHA256) to
  every enrolled device pubkey + the recovery pubkey — the age-stanza pattern, a small wrapper
  over HPKE (decision: **HPKE everywhere**, one primitive).
- **Every content write mints a fresh DEK `[roast M2]`** (forecloses key-reuse ambiguity and is
  what makes removal-rotation below sound).
- **Recipient-side DHKEM must use the non-extractable key `[roast M15]`:** the web Open leg
  computes DH via `crypto.subtle.deriveBits({name:'X25519', public: enc}, deviceKey, 256)` on the
  non-extractable `CryptoKey` (a **WebCrypto-native HPKE** path, e.g. `panva/hpke`, or a custom
  RFC 9180 KDF leg validated against the A.1 test vectors). A pure-JS noble polyfill would force
  **extractable** device keys and silently void §1's XSS boundary — **noble is permitted only for
  ephemeral/sender ops.** Enrollment **feature-detects native X25519 and refuses** rather than
  falling back to extractable keys. Go: CIRCL or stdlib `crypto/hpke`.
- **AAD at rest:** `(accountId, secretId, version)` — the CP cannot splice seals across envelopes
  or replay an old version as current.
- FIPS note: X25519 is non-approved under Go `fips140=only`; strict-FIPS nodes would use P-256
  DHKEM. Not a v1 concern.

## 3. Delivery flow (create / resume)

1. Owner's client fetches the target node's cert + signed HPKE sub-key (relayed by the
   untrusted CP).
2. Client verifies: pinned chain → SAN matches expected `(accountId, class)` → **node not on the
   AS revocation list** (§1, M12) → sub-key signature → sub-key unexpired.
3. Client unseals the DEK and **re-seals the secret to the node sub-key via single-shot HPKE
   `Seal` with AAD `(spawnId, generation, nodeId, notAfter, version)`** `[roast M11]` plus a
   node-issued **one-time `deliveryId`** in the AAD.
4. CP relays; the node `Open`s and enforces, explicitly: **AAD equality** (HPKE) + **`notAfter` ≥
   now** (a node clock check with bounded skew — HPKE does *not* check expiry) + **`version` not
   older than the highest seen for that secret** (defeats a CP replaying a pre-rotation ciphertext
   within the sub-key window) + **`deliveryId` accepted exactly once** (defeats same-context
   replay, which AAD alone cannot). A ciphertext is useless to any other node (different KEM key)
   or context.
5. The node holds plaintext **in memory only** for the active episode (§6).

**Re-placement / mid-flow / timeout `[roast M8]`:** the resume is an **interactive session**; if
the CP's first-choice node fails to start, the CP may request up to **K re-seals** to alternative
nodes within that same session (pull the reserved pre-seal single-use-wrap seam forward). A
`starting` episode whose key delivery **times out** (e.g. owner closed the laptop mid-"Move to
cloud") transitions to a **defined** state — back to `suspended`, target restore artifacts wiped —
**never a silent hang**. **Post-cutover, lifecycle §3 "attach auto-resumes" requires an enrolled
device**: an authenticated-but-unenrolled machine gets "resume requires an enrolled device," never
a hang; a hotel-browser user sees a read-only list + "approve from your phone / enter recovery code
only on a trusted device" banner, with an **ephemeral auto-expiring web-device class** and an
explicit warning before web recovery-code entry.

## 4. Device & recovery lifecycle

- **Lazy ceremony — not at signup `[roast M14]`:** account creation stays **OAuth-only**. The key
  ceremony (device keypairs + BIP-39 recovery) triggers **on the first secret-bearing action**
  (adding a BYOK key, or opting a spawn into owner-sealed durability / invoking migration) — never
  as an onboarding gate (the sp-73q "lazy vault" lesson: a curious marketplace user must not face
  a 24-word ceremony before doing anything). At the ceremony, mandatory user-facing copy:
  **"without this recovery code and your devices, suspended spawn contents cannot be recovered by
  anyone, including Spawnery."**
- **First device:** generate X25519 + signing keypairs, write the **genesis device-set statement
  co-signed by device₁ + the recovery key**, register at the AS; display the BIP-39 code.
- **Add device:** new device shows a QR/link (its pubkeys + challenge); an **already-enrolled
  device** verifies the challenge, appends a **member-signed** device-set mutation, and re-seals
  DEKs to the expanded set. The AS records (does not author) the signed log entry.
- **Remove device (and recovery-code rotation):** **mint a fresh DEK and re-encrypt the payload**
  `[roast M2]`, then seal to the reduced set; append a member-signed mutation; bump `version`.
  Document explicitly: pre-removal envelope versions remain decryptable by the removed key wherever
  old envelopes survive (CP backups, prior breach) — so **removing a device should prompt rotation
  of high-value underlying secrets** (e.g. the leaked API key itself), since re-keying the envelope
  does not un-leak a value the removed device already saw.
- **Recovery:** the BIP-39 virtual device unseals → enroll a fresh device → re-seal → rotate the
  recovery code.
- **Node re-enrollment:** unaffected at rest (owner ciphertexts are sealed to *device* keys); the
  node leg is re-sealed on demand at next create/resume.

## 5. Enrollment hardening (amends sp-ova §3.1)

Adopt the fingerprint-bound token flow: enrollment tokens are scoped to
**`(accountId, class, node-pubkey-fingerprint, expiry, single-use)`**, issued by the AS to the
owner's client over the **direct pinned AS connection**, redeemed **node→AS directly** with the
CSR (AS checks the CSR key matches the bound fingerprint). Properties (ACME
external-account-binding / Vault response-wrapping precedents): a CP-observed token cannot be
redeemed with a substituted key; single-use makes interception detectable; TTL bounds exposure;
scoping prevents cross-account/class escalation. The CP's residual power over enrollment is
denial of service — matching the threat model.

## 6. Plaintext hygiene — never-persist is the invariant

- **Delivery channel is NOT env `[roast M10]`.** PodSpec env vars are persisted to disk by the
  runtime (Docker: `/var/lib/docker/containers/<id>/config.v2.json` — empirically reproduced;
  containerd: its metadata DB), so the existing `SidecarEnv` path (manager.go:304 →
  docker_pod.go / cri/backend.go) **would write plaintext to disk on every spawn**. Instead the
  sidecar/agent fetches its secret at startup over a **pod-local socket / exec-stdin handshake**
  from the spawnlet (or an **unlinked-after-read tmpfs** file, with a swap caveat noted) — never
  via env.
- In-memory handling uses memguard-class **off-heap** allocation + zeroize-on-suspend as
  **defense-in-depth, not a guarantee** (Go's GC copies/moves heap objects; mlock-on-heap is
  near-theater; plaintext also transits the heap inside HPKE `Open`). Not a hard boundary.
- **The enforceable, tested invariant is never-persist** for **Spawnery-delivered** secrets: they
  never touch disk, logs, or the journal.
- **Agent-written secret files ARE journaled (documented residual):** if the agent writes a
  credential into a mount file, the journal captures it. Under **node-local** custody this is no
  worse than the node already holding it; under **owner-sealed** it is encrypted to the owner. The
  interim secrets-glob default-exclude (transient-tier §2) reduces incidental capture. Stated, not
  silently assumed.
- **Test, don't assert `[roast M10]`:** the e2e harness plants canary secrets, then greps every
  file the node wrote during the episode **including the runtime state dirs** (`/var/lib/docker/
  containers/*`, the containerd root) — zero hits required. Whole-process memory-dump zero-hits is
  **not** asserted (unsound under Go's GC); the memory assertion is **scoped to the memguard
  region + the zeroize-on-suspend hook having run**.

## 7. Consumers, phases & testing

**Consumers:** transient-tier Kopia repo passwords for the **owner-sealed** durability class —
enables cross-node migration / node-death survival (transient-tier §4; there is **no interim
CP-custodied tier to replace** — node-local custody handles the no-ceremony case); the `sp-7h6.1`
user secrets store; BYOK inference keys (sidecar key over the node leg at pod start).

**Implementation phases (under `sp-2ckv`):**

| # | Slice | Notes |
|---|---|---|
| ① | Node HPKE sub-key (gen/sign/publish/rotate/retain-2/verify) + revocation-list check + HPKE envelope + delivery leg, **single-device owner key, CLI-first** | proves the chain end-to-end; native-client only, no web trust dependency |
| ② | Web SPA device keys (native-WebCrypto DHKEM) + multi-device QR re-seal + signed hash-chained device set + BIP-39 recovery + Argon2id fallback | **blocked on the independent-SPA-delivery slice `[roast C2]`** |
| ③ | Wire consumers: owner-sealed transient-tier journal keys (unblocks transient-tier ③ migration), `sp-7h6.1`, sidecar BYOK over the socket/stdin channel | secret delivery via socket/stdin, not env (§6) |
| ④ | Fingerprint-bound enrollment tokens (§5) | lands with/after the sp-ova worktree merges |

**Independent-SPA-delivery slice (new, blocks phase ②) `[roast C2]`:** a pinned static origin /
AS-served SPA with signed releases + SRI + strict CSP — its own designed spec. Until it lands, the
custody guarantee is scoped to native clients; the headline threat-model paragraph and §1 say so,
and the consumer bead notes it.

**Deliberately reserved seams (deferred):** signed sub-keys enable a future "pre-seal to a node
before the spawn exists" headless flow (the **K-re-seal-in-session** path of §3 is the first user);
AAD context-scoping enables capability tokens; the wrappable DEK enables a future 2-of-2 CP+AS
Shamir split. PRF Tier-2 layers on a device key without structural change.

**Testing:** hermetic unit — seal/unseal vectors; **AAD rejection matrix** (wrong
spawn/gen/node/version, expired `notAfter` via clock, replayed `deliveryId`); stale/forged
sub-key; wrong-SAN; **revoked-node refusal**; **version-splice + post-removal-key rejection**
`[M2]`; **poisoned device-set** (unsigned/regressed/AS-injected member) refusal `[M4]`; CI
assertion `deviceKey.extractable === false` end-to-end + no raw-device-key-bytes import path
`[M15]`. E2E (build-tagged), mirroring sp-ova's PKI-soundness suite: CP-substituted sub-key
rejected; replayed/pre-rotation ciphertext rejected; **canary never-persist with runtime state
dirs in scope** (§6, M10); device add/remove/recovery flows.

## 8. Deferred

PRF Tier-2 unlock · headless delegation (pre-seal single-use wrap first; 2-of-2 CP+AS Shamir if
unattended spawns become core) · ciphertext padding (metadata: an honest-but-curious CP sees
sizes/timing/access patterns — accepted and documented for v1) · P-256 DHKEM FIPS variant ·
server-assisted rate-limited recovery (Signal-SVR/iCloud-escrow style) · team/multi-owner
sharing · Ed25519-in-WebCrypto reliance (revisit ~2027).

---

## Appendix — decision log

| # | Decision | Choice |
|---|---|---|
| S.1 | Custody root | Per-device **non-extractable X25519 + ECDSA-P256 signing** keypairs (`[M4]`); BIP-39 recovery virtual device; Argon2id fallback; **PRF deferred Tier-2** |
| S.2 | Sealing primitive | **HPKE everywhere** (RFC 9180, DHKEM-X25519); **fresh DEK per write** (`[M2]`); recipient DHKEM on the **non-extractable** key via native WebCrypto, refuse extractable (`[M15]`) |
| S.3 | Node encryption key | Cert-signed HPKE sub-key (72 h, **retain 2 concurrent** `[m2]`); **+ AS revocation-list check** (revocation ≠ expiry, `[M12]`); rejected RFC 8410 / CSR dual-key |
| S.4 | Context binding | In-flight AAD `(spawnId, generation, nodeId, notAfter, **version**)` **+ one-time `deliveryId`**; node enforces clock + version-monotonic + delivery-once (`[M11]`); at-rest `(accountId, secretId, version)` |
| S.5 | Trust registries | Device set at the AS = **hash-chained, member-signed log** (genesis co-signed device₁+recovery); AS stores≠authors; clients verify+pin before every seal (`[M4]`) |
| S.6 | Enrollment | Fingerprint-bound `(accountId, class, fingerprint, expiry, single-use)` tokens, direct node→AS — amends sp-ova §3.1 |
| S.7 | Hygiene | Delivery via **socket/stdin, not env** (`[M10]`); never-persist tested incl. runtime state dirs; agent-written secret files = documented journaled residual; memguard = defense-in-depth |
| S.8 | Escrow | **None in v1** — AS holds pubkeys only; seams reserved for pre-seal wrap and 2-of-2 split |
| S.9 | Delivery | Owner-client unseal → re-seal to verified, **non-revoked** node sub-key → CP relays ciphertext; **K-re-seal-in-session** for re-placement, defined timeout state (`[M8]`) |
| S.10 | Phasing | CLI-first → web (**blocked on independent-SPA-delivery slice `[C2]`**) → consumer wiring (unblocks owner-sealed migration) → enrollment hardening; **ceremony is lazy, not at signup** (`[M14]`) |
| S.11 | Web trust scope | Custody guarantee holds for **native clients until the independent-SPA-delivery slice ships** (`[C2]`) |
