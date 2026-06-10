# Owner-Sealed Secrets — E2E Secret Custody & Delivery (Design)

**Bead:** `sp-2ckv` (epic; brainstorm task `sp-2ckv.1`)
**Status:** Approved in brainstorming (Mode A — sections reviewed with user)
**Date:** 2026-06-10
**Research basis:** [brief](2026-06-10-owner-sealed-secrets-research-brief.md) ·
[results + merged synthesis](2026-06-10-owner-sealed-secrets-research-results.md) ·
[cloud run](2026-06-10-owner-sealed-secrets-research-results-cloud.md)
**Builds on:** [Node Auth & Unified Identity](2026-06-05-node-auth-unified-identity-design.md)
(`sp-ova`; partially implemented on `worktree-node-auth-sp-ova` — PKI, AS, mTLS, AS-signed
sessions, client pin/verify) · [E4 Identity & Secrets](2026-05-28-spawnery-e4-identity-secrets-design.md)
**Amends:** sp-ova §3.1 (enrollment-token scoping, §5 below)
**Consumers:** transient-tier journal keys (`sp-u53.5.4`,
[design §4](2026-06-10-transient-tier-kopia-journal-design.md)), user secrets store (`sp-7h6.1`),
BYOK inference keys. Resolves `sp-gtm` (CP key-vending MITM) for secret delivery.

The primitive: **small user secrets stored at the CP as ciphertext it cannot read, unsealed only
by the owner's clients, delivered re-sealed to cryptographically verified nodes — plaintext
exists only at the owner's client and in the target node's memory.** Threat model: a fully
compromised CP yields ciphertext, metadata, and DoS — never plaintext. The AS is an identity
authority, not a key escrow.

---

## 1. Trust & key inventory

**Owner side — per-device keypairs are the root of trust:**

- Each device (browser, phone, workstation CLI) generates a **non-extractable X25519 device
  keypair**. Web: WebCrypto `CryptoKey` (`extractable: false`) in IndexedDB — a browser-enforced
  boundary WASM cannot replicate; XSS can *use* a live key but not exfiltrate it (mitigations:
  strict CSP + SRI + the SPA's existing independent-of-CP delivery). CLI/daemon: `crypto/ecdh`
  X25519, keyfile `0600` under `~/.config/spawnctl`.
- A **BIP-39 recovery code** (generated at account setup, printed/stored by the owner) derives a
  keypair treated as an **always-enrolled "virtual device"** — recovery without server escrow.
- An **Argon2id passphrase fallback** KEK (client-side derivation only; parameters pinned in
  signed client code — the Bitwarden lesson: the server must never influence KDF cost; start at
  RFC 9106 second profile / OWASP-plus, tuned per device).
- **Passkey-PRF is deferred Tier-2** (convenience unlock wrapping a device key): fabric-scoped
  sync, no Firefox, device-bound Windows Hello, and lose-credential = lose-data make it unfit as
  the root. Never PRF-only.

**Node side — identity exists (sp-ova); add an encryption sub-key:**

- A node's identity is its AS-anchored mTLS cert (SAN
  `<nodeId>.<accountId>.<class>.nodes.spawnery.internal`, name-constrained chain, pinned roots).
- New: each node generates an **X25519 HPKE sub-keypair** and publishes the pubkey in a small
  structure **signed by its cert key** with an expiry — the RFC 9345 delegated-credential /
  Signal signed-prekey pattern. **Validity 72 h, rotate at half-life** (re-sign; no AS, no PKI
  change). Revocation latency is bounded by validity. Rejected alternatives: RFC 8410
  second-cert (couples rotation to AS issuance), CSR-time dual-key (couples encryption rotation
  to re-enrollment).

**Verification chain (this closes `sp-gtm` for secrets):** client pins Root CA + AS pubkeys
(shipped, `sp-9wd`) → verifies node cert chain + SAN against expected `(accountId | cloud,
class)` → verifies sub-key signature + expiry → only then seals. A compromised CP can relay keys
but cannot mint trust (the Tailnet Lock property).

**Device-set registry:** device pubkeys (and the recovery pubkey) are registered **with the AS**
(not the CP), signed into the account's device set — the owner-side analogue of "the relay never
holds trust." The CP sees envelopes only.

## 2. Secret store & envelope format

- The CP DB stores **opaque envelopes**. Construction: payload encrypted once under a random
  **DEK** (AEAD); the DEK is sealed per-recipient with **HPKE Base mode**
  (DHKEM-X25519-HKDF-SHA256; AES-GCM on web, ChaCha20-Poly1305 acceptable in Go) to every
  enrolled device pubkey + the recovery pubkey — the age-stanza pattern, implemented as a small
  wrapper over HPKE (decision: **HPKE everywhere**, one primitive; Go: CIRCL or stdlib
  `crypto/hpke`; web: noble-based polyfill — no native WebCrypto HPKE; X25519 itself is
  WebCrypto-native across engines).
- **AAD at rest:** `(accountId, secretId, version)` — the CP cannot splice seals across
  envelopes or replay an old version as current.
- Re-wrap on device add/remove touches only the DEK seals, never the payload. Secrets are
  KB-sized and writes are rare — re-seal-everything rotation is cheap by construction.
- FIPS note (recorded): X25519 is non-approved under Go `fips140=only`; strict-FIPS nodes would
  use P-256 DHKEM. Not a v1 concern.

## 3. Delivery flow (create / resume)

1. Owner's client fetches the target node's cert + signed HPKE sub-key (relayed by the
   untrusted CP).
2. Client verifies: pinned chain → SAN matches the expected `(accountId, class)` → sub-key
   signature → sub-key unexpired.
3. Client unseals the DEK with its device key and **re-seals the secret to the node sub-key via
   single-shot HPKE `Seal` with AAD `(spawnId, generation, nodeId, notAfter)`** plus a fresh
   nonce.
4. CP relays; the node `Open`s — any AAD mismatch (wrong spawn, stale generation, different
   node, expired) rejects. A relayed/replayed ciphertext is useless to any other node (different
   KEM key) or context (AAD).
5. The node holds plaintext **in memory only** for the active episode (§6).

No streaming nonce scheme is needed — writes are rare, reads happen at create/resume; single-shot
HPKE per delivery suffices.

## 4. Device & recovery lifecycle

- **First device:** enrolled at account setup — generate keypair, register pubkey with the AS,
  generate + display the BIP-39 recovery code (sealed set = {device₁, recovery}).
- **Add device:** new device displays a QR/link (its pubkey + challenge); an **already-enrolled
  device** verifies and re-seals the DEKs to the expanded set (Signal device-linking /
  Tailnet-Lock node-signing shape). The AS records the updated, owner-signed device set.
- **Remove device:** re-seal to the reduced set; bump `version`. The removed key cannot open new
  versions; superseded ciphertexts are deleted (accepting the standard "a once-authorized device
  may have cached old plaintext" caveat).
- **Recovery:** the BIP-39 virtual device unseals → enroll a fresh device → re-seal → optionally
  rotate the recovery code.
- **Node re-enrollment:** unaffected at rest (owner ciphertexts are sealed to device keys, not
  node keys); the node leg is re-sealed on demand at next create/resume.

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

- In-memory handling on the node uses memguard-class **off-heap** allocation + zeroize-on-suspend
  as **defense-in-depth, not a guarantee** (Go's GC copies/moves heap objects; mlock-on-heap is
  near-theater). Do not market it as a hard boundary.
- **The enforceable, tested invariant is never-persist:** secrets never touch disk, logs, or the
  Kopia journal. Secrets are delivered to consumers via memory/env, never as mount files; the
  journal therefore never sees them by construction.
- **Test, don't assert:** the e2e harness plants canary secrets, then (a) greps every file the
  node wrote during the episode, (b) inspects a post-episode process memory dump for the canary,
  (c) verifies the zeroize-on-suspend hook ran. Zero hits required.

## 7. Consumers, phases & testing

**Consumers:** transient-tier Kopia repo passwords (replaces the interim CP-custodied
`KeyProvider`; closes `sp-u53.5.4`), the `sp-7h6.1` user secrets store (same envelope store +
delivery), BYOK inference keys (the sidecar's key rides the same node leg at pod start).

**Implementation phases (under `sp-2ckv`):**

| # | Slice | Notes |
|---|---|---|
| ① | Node HPKE sub-key (gen/sign/publish/rotate/verify) + HPKE envelope + delivery leg, **single-device owner key, CLI-first** | proves the whole chain end-to-end without web UX |
| ② | Web SPA device keys (WebCrypto) + multi-device QR re-seal + BIP-39 recovery + Argon2id fallback + AS device-set registry | full custody story |
| ③ | Swap consumers onto the primitive: transient-tier journal keys (`sp-u53.5.4`), `sp-7h6.1`, sidecar BYOK | deletes the interim CP-custodied path |
| ④ | Fingerprint-bound enrollment tokens (§5) | lands with/after the sp-ova worktree merges |

**Deliberately reserved seams (deferred, not designed):** signed sub-keys enable a future
"pre-seal to a node before the spawn exists" headless flow (Vault-style single-use wrap is the
v1.5 candidate); AAD context-scoping enables capability tokens; the wrappable DEK enables a
future 2-of-2 CP+AS Shamir split. PRF Tier-2 layers on a device key without structural change.

**Testing:** hermetic unit — seal/unseal vectors, AAD rejection matrix (wrong spawn/gen/node,
expired `notAfter`), stale/forged sub-key, wrong-SAN, version-splice rejection. E2E (build-tagged)
negative cases mirroring sp-ova's PKI-soundness suite: CP-substituted sub-key rejected,
replayed ciphertext rejected, canary never-persist (§6), device add/remove/recovery flows.

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
| S.1 | Custody root | **Per-device non-extractable X25519 keypairs**, multi-recipient sealing; BIP-39 recovery as virtual device; Argon2id fallback; **PRF = deferred Tier-2, never PRF-only** |
| S.2 | Sealing primitive | **HPKE everywhere** (RFC 9180, DHKEM-X25519); small custom multi-recipient DEK wrapper (age-stanza pattern); no age/libsodium/COSE deps |
| S.3 | Node encryption key | **Cert-signed expiring HPKE sub-key** (72 h, rotate at half-life) over the sp-ova identity; rejected RFC 8410 second-cert + CSR dual-key |
| S.4 | Context binding | HPKE AAD: `(spawnId, generation, nodeId, notAfter)` in flight; `(accountId, secretId, version)` at rest |
| S.5 | Trust registries | Device set at the **AS** (owner-signed); CP stores envelopes only; verification always against pinned roots (sp-9wd) |
| S.6 | Enrollment | Fingerprint-bound `(accountId, class, fingerprint, expiry, single-use)` tokens, direct node→AS — amends sp-ova §3.1 |
| S.7 | Hygiene | **Never-persist is the tested invariant** (canary harness); memguard = defense-in-depth only |
| S.8 | Escrow | **None in v1** — AS holds pubkeys only; seams reserved for pre-seal wrap and 2-of-2 split |
| S.9 | Delivery | Owner-client unseal → re-seal to verified node sub-key → CP relays ciphertext; single-shot HPKE per delivery |
| S.10 | Phasing | CLI-first single-device → web multi-device + recovery → consumer swap (closes sp-u53.5.4) → enrollment hardening |
