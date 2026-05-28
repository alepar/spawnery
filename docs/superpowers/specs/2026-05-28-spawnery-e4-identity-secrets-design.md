# Spawnery E4 — Identity & Secrets (Design)

**Bead:** `sp-7h6`
**Status:** Draft v1 (interview complete; pending user review)
**Date:** 2026-05-28
**Depends on:** [E0](2026-05-26-spawnery-e0-contracts-design.md),
[E1](2026-05-27-spawnery-e1-runtime-core-design.md),
[E2](2026-05-27-spawnery-e2-model-layer-design.md),
[E3](2026-05-28-spawnery-e3-storage-design.md)

Owns: user auth, the account model, the **server-blind vault passphrase** + BYO-secret
encryption (the root of the BYO trust story), and **node enrollment** (the trust anchor for
E0 §10 / E2 §2 / E1 §6).

---

## 1. Identity model

- **One user type**, with an optional **`creator`** capability (any user can enable creator
  mode and publish Apps). Per [System design §1](2026-05-26-spawnery-system-design.md).
- **User record** at the CP: `{ userId (UUID), handle, oauthSubs: { google?, github? }, vault
  meta (§4), recoveryCodeWraps[], status, createdAt }`.

---

## 2. Auth — OAuth 2.0 code flow with PKCE

- **Providers:** **Google** + **GitHub**. CP is the OAuth client / auth server (no separate BFF).
- Standard auth-code + **PKCE** for the SPA; CP exchanges the code and issues a Spawnery session
  cookie/JWT for subsequent CP API calls.
- **GitHub serves dual duty:** the same Spawnery **GitHub App** powers (a) user-to-server login
  and (b) the fine-grained per-repo storage grant (E3). Linking accounts is by OAuth subject;
  multiple providers can attach to one user record.

---

## 3. Vault passphrase & BYO-secret encryption

**Envelope encryption with multiple wraps.** The CP is **server-blind**: it only ever sees
wraps + KDF metadata + BYO ciphertexts, never the passphrase, vault key, or plaintext secrets.

**Setup:**
1. Client generates a random **vault key `VK`** (32 bytes; never leaves the client unwrapped).
2. Client derives **`PDK = Argon2id(passphrase, salt, kdfParams)`** (per-user random `salt` +
   `kdfParams` stored at CP — they're public).
3. **`wrap_passphrase = AEAD_encrypt(PDK, VK)`** → uploaded to CP.
4. Each BYO secret `K` stored as **`AEAD_encrypt(VK, K)`** at the CP.

**Daily unlock (with passphrase):** fetch `{wrap_passphrase, salt, kdfParams}`; derive `PDK`;
`VK = AEAD_decrypt(PDK, wrap_passphrase)`; decrypt BYO secrets as needed.

**Rotation:** to change passphrase, decrypt `wrap_passphrase` with old `PDK`, re-wrap `VK` with
new `PDK`, upload. `VK` itself only rotates on suspected compromise (re-encrypt all BYOs +
re-wrap all wraps).

### 3a. Argon2id parameters
Tuned for browser perf (WebCrypto/wasm). Starting target: memory 64 MiB, iters 3, parallelism 1
(revisit on real benchmarks). Stored per-user so we can re-tune over time without breaking
existing wraps (the user's stored `kdfParams` is what's used).

---

## 4. Recovery codes (opt-in)

- Format: **BIP-39 24-word mnemonic** (~256 bits entropy; human-readable; well-tested).
- **Multi-use** for MVP. Multiple recovery codes per user allowed.
- Setup: client generates `RC`, computes **`wrap_rc = AEAD_encrypt(KDF(RC), VK)`**, uploads
  `wrap_rc` to CP; the code is shown **once** to the user, then forgotten by the client.
- **Recovery:** user enters `RC` → client fetches `wrap_rc` → `VK = AEAD_decrypt(KDF(RC),
  wrap_rc)` → prompt for **new** passphrase → derive new `PDK` → re-wrap `VK` → upload new
  `wrap_passphrase`. BYO secrets intact (same `VK`).
- **Revoke a code:** delete its `wrap_rc` from the CP.
- **Lose passphrase + all recovery codes** → BYO secrets unrecoverable by design (re-enter).

---

## 5. Vault unlock UX (browser)

- **Prompt once per browser session** on first BYO-secret access.
- **`VK` held in memory only** (no IndexedDB / localStorage). Cleared on tab close + on
  configurable **inactivity timeout** (default 30 min).
- Persisting `VK` under a device-bound key (WebAuthn-PRF) for cross-session unlock = post-MVP.

---

## 6. Node enrollment & identity (trust anchor)

The mTLS / pubkey foundation that E0 §10, E2 §2, and E1 §6 all sit on.

1. **Owner creates a node entry** in the CP UI → CP issues a **one-time, short-lived enrollment
   token** (single-use, owner-scoped, ~10 min `exp`).
2. Operator runs the node with the token (config / CLI flag).
3. On first connect, the **node generates its own keypair** (private key never leaves the box),
   presents the enrollment token, and submits its public key.
4. CP **verifies the token**, **binds the node identity to the owner account**, and issues a
   **long-term node certificate** signed by **Spawnery's internal CA** (claims: `nodeId`,
   `ownerId`, `notAfter`, `caps`).
5. The **node uses mTLS** (with that cert) for all subsequent gRPC connects to the CP.
6. **The node's public key is what the CP vends to clients** for the per-session E2E channel
   (E0 §10) and for BYO-secret encryption (E2 §2).

**Anti-spoofing:** enrollment tokens are single-use, short-lived, owner-bound; node identity is
immutably bound to the issuing account at enrollment; mTLS authenticates every connect; a node
cannot impersonate another. Node cert rotation/revocation is a CP-side admin action (revoked
certs are rejected; the node re-enrolls if needed).

**Spawnery-operated nodes** (home machine, cloud burst) follow the same flow internally with
Spawnery as the owner — provisioned with the enrollment token at deploy time.

---

## 7. Session tokens (CP-signed)

Per E0 §10 / §5a:

- Issued by the CP on `POST /spawns/{id}/session`.
- **Signed JWT** with claims `{ spawnId, owner, node, exp }`; **CP signing pubkey** is published
  to nodes during enrollment so the node verifies tokens offline (no per-session CP roundtrip).
- Short-lived (minutes); reissue on rotation.

---

## 8. Threat-model summary

| Attack | Mitigation |
|---|---|
| CP fully compromised | Attacker gets wraps + BYO ciphertexts; offline brute-force on `wrap_passphrase` mitigated by Argon2id with tuned params; `wrap_rc` infeasible at ~256-bit entropy. No passphrase or `VK` ever stored. |
| Recovery code leak | Treated as cash — attacker w/ code + CP access can recover `VK`. User mitigates by storing codes offline; revoke leaked codes. |
| Node spoofing | One-time enrollment token + mTLS bound to owner; CA-signed cert; no shared secrets. |
| MITM on node-key vending | Client trusts the CP's vending; self-hoster can pin their node key out-of-band → **post-MVP**. |
| Browser XSS | `VK` lives only in JS memory; never persisted; clearing on tab close limits exposure. Hardening (CSP, etc.) is web-client engineering hygiene. |

---

## 9. Deferred (post-MVP)

WebAuthn-PRF persistent unlock · single-use recovery codes · out-of-band node-key pinning UX ·
hardware-key-backed vaults · enterprise SSO (SAML/OIDC) · multi-account linking beyond Google/GitHub.

---

## Appendix — E4 decision log

| # | Decision | Choice |
|---|---|---|
| E4.1 | Vault KDF + recovery | **Argon2id** + opt-in **BIP-39 24-word multi-use recovery codes** (envelope wrap of `VK`); CP server-blind |
| E4.2 | Node enrollment | **One-time owner-scoped enrollment token** → node-generated keypair → CP issues long-term mTLS cert (CA-signed, owner-bound); pubkey vended to clients |
| E4.3 | Vault unlock UX | **Once per browser session**, `VK` in-memory only, inactivity timeout |
| E4.4 | OAuth (asserted) | Google + GitHub via OAuth 2.0 code-flow + **PKCE**; CP is auth server; the Spawnery GitHub App does double duty (login + storage) |
| E4.5 | Account model (asserted) | Single user type + `creator` capability; UUID + handle + OAuth subject mappings |
| E4.6 | Session tokens (asserted) | CP-signed JWT (`spawnId, owner, node, exp`); nodes verify offline with the CP signing pubkey distributed at enrollment |
