# Deep-Research Brief — Owner-Sealed Secrets & E2E Key Delivery

**Date:** 2026-06-10 · **For:** designing Spawnery's owner-sealed secret primitive (`sp-2ckv`;
brainstorm task `sp-2ckv.1`). Consumers: transient-tier journal keys
([design §4](2026-06-10-transient-tier-kopia-journal-design.md)), user secrets store (`sp-7h6.1`),
BYOK inference keys. Builds on [node-auth / unified identity](2026-06-05-node-auth-unified-identity-design.md)
(`sp-ova`) and the [E4 identity & secrets design](2026-05-28-spawnery-e4-identity-secrets-design.md).

Copy the prompt below into a deep-research agent. It is self-contained.

---

## PROMPT

You are an applied-cryptography / security-architecture researcher. Produce a rigorous,
citation-backed report on **end-to-end-encrypted user-secret storage and delivery** for a
platform where secrets must pass through cloud infrastructure as ciphertext and reach plaintext
**only on verified end nodes**. Be concrete, version-specific, and skeptical of marketing —
prefer primary sources (RFCs, vendor security whitepapers, audited designs, post-incident
writeups, browser/platform compatibility data). Note dates and versions; flag thin or contested
evidence. Prefer designs with public audits or formal analyses.

### Context (the system being designed)

- **Spawnery** runs sandboxed coding-agent "spawns" on **nodes**: user-self-hosted machines
  (macOS/Linux, rootless) and Spawnery-operated cloud hosts (Linux). A **control plane (CP)**
  orchestrates spawns and relays traffic; an **Auth Service (AS)** is the identity authority.
- **The node-identity layer EXISTS — build on it, do not redesign it.** Per the implemented
  design: a pinned offline **Root CA**; a name-constrained **self-hosted intermediate held by
  the AS**; an **offline cloud intermediate**. A node's identity is its mTLS certificate; the
  SAN encodes `<nodeId>.<accountId>.<class>.nodes.spawnery.internal`. Clients (web SPA, CLI)
  **pin the Root CA + AS pubkeys independently of the CP** and can verify which account/class a
  node cert belongs to. Session tokens are AS-signed, offline-verified by nodes. The CP holds
  **no signing keys**.
- **Threat model:** assume the **CP is fully compromised** — it stores, schedules, and relays,
  but must never see secret plaintext. The CP may store/relay **ciphertext only**. Plaintext
  exists in exactly two places: the **owner's client** (during seal/unseal) and the **verified
  target node** (in memory, for the spawn's active episode). An **AS compromise** should also
  not yield secret plaintext (the AS is an identity authority, not a key escrow) — unless the
  report argues a deliberate, explicit escrow split is worth it.
- **Secrets are small blobs** (KBs): per-spawn backup-repo passwords, BYO inference API keys,
  GitHub tokens. No streaming, no large payloads. Writes are rare; reads happen at spawn
  create/resume.
- **Clients:** a **web SPA** (React; delivered independently of the CP — an established
  constraint) and **Go native clients** (`spawnctl` CLI, the node daemon) with pins baked in.
  **Multi-device owners are the norm** (laptop browser + phone + workstation CLI). v1 is
  single-owner (no team sharing).
- **The flow to design for:** at spawn create, the owner's client generates a secret (or accepts
  user input), seals it so the CP stores only ciphertext; at create/resume, the owner's client
  **unseals and re-seals to the verified target node's key**; the CP relays; the node decrypts
  in memory. The client must be able to verify (via the pinned roots) that the key it seals to
  belongs to the expected `(accountId, class)` node — this is what closes the known
  "CP key-vending MITM" gap.

### Research questions (address each as its own section)

**1. Prior-art architectures — "server stores ciphertext; owner unseals; re-seals to a target".**
For each, document: what exactly is stored server-side, the key hierarchy, how a new
device/target is added, recovery story, and any public incidents/audit findings:
- **1Password** (SRP + Secret Key two-factor KDF; vault key sharing to devices),
- **Bitwarden** (master-password KDF; org/collection keys; 2024–25 KDF criticisms),
- **Tailscale tailnet lock** (signing-node chains vouching for node keys — structurally close to
  our AS+pin model) and Tailscale's general key distribution,
- **HashiCorp Vault response-wrapping** (single-use wrapped delivery through an untrusted relay),
- **Kubernetes sealed-secrets / Mozilla sops + age** (recipient-key envelope workflows),
- **Signal / WhatsApp device-add + encrypted backups** (device-link QR flows; HSM-backed
  recovery vaults), **Apple iCloud Keychain escrow** (SEP/HSM escrow with guess limits),
- any other platform with a genuinely comparable "untrusted middle" design (e.g. Keybase,
  Excalidraw E2E rooms, Firefox Sync's old vs new design).
State which architectural patterns transfer to our constraints and which don't, and why.

**2. Owner key custody — evaluate all three families, recommend one (+ fallback).**
- **(a) Passkey-derived:** WebAuthn **PRF extension** deriving a stable symmetric/asymmetric key
  from a passkey. Give the 2026 support matrix honestly: browsers (Chrome/Safari/Firefox),
  platforms (iCloud Keychain, Google Password Manager, Windows Hello), hardware keys (CTAP2
  hmac-secret), **passkey-sync semantics** (does a synced passkey yield the same PRF output
  across devices? per provider!), CLI feasibility (FIDO2 from a Go CLI), and the failure/lockout
  story. Cite real deployments using PRF for E2E encryption (e.g. Bitwarden's PRF unlock, hush,
  others).
- **(b) Password-derived:** Argon2id (current OWASP/RFC 9106 parameters), the existing in-house
  direction (vault passphrase + BIP-39 recovery codes); OPAQUE as a PAKE upgrade — what does it
  buy over client-side KDF for this use?
- **(c) Per-device keypairs + cross-device authorization:** Signal/Tailscale-style device
  chains: each device holds a non-exported keypair; secrets are sealed to all enrolled device
  keys (multi-recipient); adding a device = an existing device re-seals or vouches (QR/link
  flow). Recovery = escrow or recovery code as a "virtual device".
- Judge on: multi-device UX, recovery UX + abuse resistance, web-SPA feasibility *today*,
  CLI parity, implementation complexity, and what happens when the user loses everything.

**3. Sealing format & key-agreement groundwork** *(this section is shared foundation: a future
E2E relay channel will reuse the same node-key verification + agreement story).*
- Compare envelope formats for seal-to-recipient of small secrets: **HPKE (RFC 9180)** (modes,
  suites, library maturity), **age** (X25519 recipients; `filippo.io/age` as a Go library),
  **libsodium sealed boxes**, **JOSE ECDH-ES / COSE-HPKE**. Multi-recipient support (sealing one
  secret to N device keys + M node keys), authenticated vs anonymous sender modes, and
  deniability/replay considerations.
- **WebCrypto reality check (2026):** X25519/Ed25519 availability across browsers (Secure
  Curves status), HPKE availability (native? polyfill quality?), **non-extractable key storage**
  (CryptoKey in IndexedDB — what does non-extractability actually protect against; XSS threat
  model for an SPA doing E2E crypto; mitigations), WASM-crypto (libsodium.js) vs WebCrypto
  trade-offs.
- **Go side:** `crypto/ecdh`, CIRCL (HPKE), age — maturity and FIPS posture.
- **The identity-binding question (key groundwork):** the node's certificate key is a
  *signing/TLS* key — reusing it for decryption is poor practice. Survey the patterns for
  **binding a node ENCRYPTION pubkey to an existing X.509/mTLS identity**: (i) node publishes an
  HPKE pubkey **signed by its cert key** (a "device-bound sub-key" / delegated-credential-style
  attestation), (ii) a second cert/extension carrying a KEM key (keyEncipherment/agreement EKUs,
  X25519-in-X.509 RFC 8410), (iii) CSR-time dual-key enrollment at the AS. Cover rotation,
  what TLS 1.3 delegated credentials / Signal's signed-prekey precedent teach here, and
  recommend one.

**4. Delivery & lifecycle mechanics.**
- The unseal→re-seal flow concretely: client fetches node cert (relayed by untrusted CP),
  verifies chain + SAN against pinned roots, checks the encryption-key binding (§3), seals,
  CP relays, node decrypts. What can/can't be replayed; whether to bind ciphertexts to
  `(spawnId, generation, nodeId)` as AAD; freshness/nonce strategies.
- **Node-side plaintext hygiene:** memory-only handling in Go (mlock/madvise limits, GC copies —
  what's actually achievable in a Go daemon; `memguard`-class libraries — worth it or theater?),
  zeroization on suspend, never-persist guarantees and how to test them.
- **Rotation & revocation:** owner key rotation (re-seal all ciphertexts), node re-enrollment
  (new node key — what must be re-sealed), secret rotation; revocation latency windows.
- **Audit:** what an honest-but-curious CP can log (sizes, timing, access patterns) and whether
  metadata leakage matters for these secrets.

**5. Headless delegation — pattern survey only (leave the right seam, don't design it).**
Flows with no owner client present (agent-initiated spawn creation, scheduled runs, CP-restart
auto-recovery). Survey, with one paragraph each on trust implications:
- capability attenuation (macaroons, **biscuits**) — delegate "unseal for spawn X until T",
- Vault-style **response wrapping** / single-use tokens minted while the owner IS present,
- KMS **grants** (AWS KMS grant model) and what a self-hosted analogue looks like,
- SSH-agent-style forwarding from an online owner device,
- enclave/TPM escrow (node-bound sealed keys; Nitro/SEV/TPM2 sealing),
- **threshold escrow** (e.g. 2-of-2 split between CP and AS — neither alone can unseal;
  Shamir/threshold-HPKE maturity).
Conclude with which seams the v1 design should leave open to retrofit the most plausible option.

### Deliverable

- A structured report with the five sections above, plus:
  - a **comparison table** for owner-key custody options (multi-device UX, recovery,
    web-feasibility-today, CLI parity, complexity, lockout risk);
  - a **comparison table** for sealing formats (multi-recipient, web support, Go support,
    audit/maturity, relay-channel reuse);
  - a **prior-art table** (what's server-side, device-add, recovery, incidents);
  - a **recommendation + staged adoption path** for Spawnery's exact constraints (pinned
    AS/Root-CA identity layer already implemented; CP ciphertext-only; web SPA + Go CLI;
    multi-device single-owner; small secrets; headless deferred) — including what is
    implementable in the web SPA **today** vs gated on platform maturity;
  - explicit callouts where evidence is thin, vendor-asserted, or fast-moving (PRF support,
    WebCrypto curves, COSE-HPKE drafts).
- Cite versions, dates, and sources throughout. Call out where our assumptions (pinned roots
  independent of the CP, AS-held name-constrained intermediate, small secrets only,
  single-owner) materially simplify or complicate the standard designs.
