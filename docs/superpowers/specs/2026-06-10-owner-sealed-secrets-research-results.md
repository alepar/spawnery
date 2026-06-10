# Owner-Sealed Secrets — Deep-Research Results (in-session run)

**Date:** 2026-06-10 · **Brief:** [research brief](2026-06-10-owner-sealed-secrets-research-brief.md)
· **Bead:** `sp-2ckv.1`
**Run:** deep-research harness, 106 agents, 24 sources fetched, 120 claims extracted, 25
adversarially verified (3-vote): **24 confirmed, 1 refuted**. A parallel cloud-session run of the
same brief is expected to fill the coverage gaps listed at the end — merge before designing.

---

## Headline synthesis

The verified evidence strongly supports the ciphertext-only-CP design:

- **The untrusted-middle model is proven production practice** — 1Password, Bitwarden, and
  Tailscale Tailnet Lock all ship systems where the cloud middle stores/relays only ciphertext
  (or unsigned key announcements) and plaintext/trust decisions exist only at owner clients and
  verified endpoints.
- **Owner key custody:** the **WebAuthn PRF passkey path is web-implementable today** (Bitwarden
  ships it; PRF rides on CTAP2 hmac-secret; Apple platform passkeys support it since iOS 18 /
  macOS 15) — but it **must be paired with a fallback** (password-derived per
  Bitwarden/1Password precedent) because PRF requires authenticator + OS + browser to all
  support it, and platform gaps remain.
- **Sealing format: HPKE (RFC 9180)** with DHKEM(X25519, HKDF-SHA256) is a direct fit —
  single-shot `Seal` to a recipient pubkey with **native AAD** means binding
  `spawnId/generation/nodeId` to each sealed secret falls out of the spec for free.
- **Trust distribution: Tailnet Lock is the template** for "CP relays keys but cannot mint
  trust" — client-verified signatures over announced keys + a hash-chained, owner-signed trust
  log. 1Password's recovery flow is the template for **owner-client re-wrap** on
  rotation/recovery.

## Verified findings

### 1. Ciphertext-only cloud storage is proven prior art — `high` (2-1 ×2)
1Password and Bitwarden are E2E with an "ignorant server": all keys generated client-side, all
encryption local; the server stores only ciphertext (1Password: vault data encrypted to
client-held keys; Bitwarden: a Protected Symmetric Key + auth hash) and can never learn the
account password or keys.
*Sources:* [1Password white paper](https://1passwordstatic.com/files/security/1password-white-paper.pdf),
[Bitwarden security white paper](https://bitwarden.com/help/bitwarden-security-white-paper/).
*Scoping from the sources themselves:* "ciphertext only" holds for secret **payloads**, not all
metadata (emails, device info, pubkeys are cleartext); and 1Password's own appendix concedes a
**vendor-served web client can be subverted to defeat E2EE** — directly our web-SPA finding
(sp-ova §7: SPA must be delivered independently of the CP; the Go CLI is the stronger anchor).

### 2. Password-derived custody is the incumbent, web-feasible pattern — `high` (3-0 ×2)
Bitwarden: client-side PBKDF2 @600k default, **Argon2id opt-in via WASM** (so 1Password's
JS-speed rationale for avoiding modern KDFs is no longer a hard blocker). 1Password:
**two-secret key derivation** — PBKDF2 @650k XORed with HKDF of a locally-held high-entropy
**Secret Key**, making server-side data useless for offline cracking. Takeaway: pairing the
password with a second high-entropy secret (2SKD) neutralizes pure-password custody's main
weakness.
*Sources:* same two white papers, verified verbatim.

### 3. Passkey/WebAuthn-PRF custody is web-implementable in production today — `high` (3-0 ×6)
The PRF extension yields a per-credential 32-byte secret per assertion (evaluated at a fixed
input like "end-to-end encryption key"), HKDF-stretched (WebCrypto-native) into key material.
**Bitwarden ships this in production**: PRF-derived key → decrypts a PRF-encrypted private key →
unwraps the User Symmetric Key; the PRF key never leaves the client; server stores only
ciphertext. PRF is implemented atop CTAP2 `hmac-secret`, so hmac-secret hardware keys give wide
authenticator support.
*Sources:* [Bitwarden PRF deep-dive](https://contributing.bitwarden.com/architecture/deep-dives/passkeys/implementations/relying-party/prf/),
[W3C PRF explainer](https://github.com/w3c/webauthn/wiki/Explainer:-PRF-extension),
[Yubico PRF developer guide](https://developers.yubico.com/WebAuthn/Concepts/PRF_Extension/Developers_Guide_to_PRF.html),
Bitwarden white paper.

### 4. The 2026 PRF support matrix is real but gated — fallback is mandatory — `medium`
PRF-based decryption works only when **authenticator + OS + browser all** support PRF in the
ceremony (Bitwarden falls back to master password otherwise). Apple platform passkeys support
PRF since iOS 18/macOS 15 (Safari 18+), but iOS/iPadOS **doesn't pass PRF to external security
keys** (WebKit bugs partially outdate the exact wording by iPadOS 26.4); some macOS paths need
Chromium. ~~"Windows Hello lacks PRF"~~ — **refuted 0-3**: verifier evidence says Windows Hello
supports PRF after KB5077181 (2026-02), but that positive came from secondary sources —
**re-test before relying on it** (and check bitwarden/clients#19858).
**Net design rule: PRF as the preferred path, never the only path.**

### 5. Wrap-to-recipient + owner-client re-wrap = direct 1Password prior art — `high` (3-0 ×3)
Each recipient's copy of a vault key is encrypted to that recipient's pubkey (keypair generated
on-device, never capturable by the server). Recovery: a recovery-group member's **client**
decrypts the vault key and **re-encrypts it to the locked-out user's new pubkey**, the server
relaying only ciphertext throughout — the exact template for an owner client re-sealing secrets
to a new node/device key. Caveat from the source: recovery mechanisms are "inherently weak
points"; for our single-owner model the analog is multi-device sync or an explicit owner
recovery key, not third-party recovery groups.
*Sources:* 1Password white paper, [restore design chapter](https://agilebits.github.io/security-design/restore.html).

### 6. Tailnet Lock is the proven "CP can't mint trust" + key-binding pattern — `high` (3-0 ×5)
Threat model explicitly assumes a compromised coordination server distributing
attacker-generated keys. With Tailnet Lock, nodes accept CP-announced peer pubkeys **only with a
valid signature from an owner-held trusted key (TLK)**; the server never generates/stores/sees
TLK material; trust-set changes travel as a **hash-chained log of owner-signed AUMs**. Mapped to
us: the owner client verifies a node's HPKE pubkey against an owner-/AS-signed statement before
sealing; key-set changes are signed + hash-chained, never CP-asserted. Documented limits that
transfer: **enablement is trust-on-first-use** (our pinned AS/Root-CA layer closes exactly that
gap) and the CP can still deny service.
*Sources:* [Tailnet Lock whitepaper](https://tailscale.com/kb/1230/tailnet-lock-whitepaper),
[blog](https://tailscale.com/blog/tailnet-lock). GA since June 2025, implementation open source.

### 7. HPKE (RFC 9180) is a direct fit as the sealing format — `high` (3-0 ×3)
KEM + KDF + AEAD composition; stateless single-shot `Seal(pkR, info, aad, pt)`; **AAD is
native** — `Open` fails on mismatch, so binding `(spawnId, generation, nodeId)` is free
(note: AAD values travel cleartext alongside the ciphertext; that's metadata, acceptable here).
Registered DHKEM(X25519, HKDF-SHA256) + AES-128-GCM/ChaCha20Poly1305; Go has stdlib/community
HPKE; WebCrypto X25519 (Secure Curves) is shipping in modern browsers (ChaCha20Poly1305 is not
in WebCrypto — use AES-GCM on web). RFC 9180 is IRTF Informational but widely deployed
(ECH/MLS/OHTTP).
*Source:* [RFC 9180](https://www.rfc-editor.org/rfc/rfc9180.html).

---

## Refuted claim (do not assert)

- ~~"Windows Hello lacks CTAP hmac-secret so PRF on Windows 11 needs roaming authenticators"~~
  — **0-3**; Windows Hello reportedly PRF-capable post-KB5077181 (2026-02), pending first-party
  re-verification.

---

## Coverage gaps — filled by the parallel cloud run

> **Resolved 2026-06-10:** the [cloud-run report](2026-06-10-owner-sealed-secrets-research-results-cloud.md)
> covered every gap below (and added a recommended node-enrollment flow). See **Merged
> synthesis** at the end of this doc. Original gap list kept for the record:

1. **The identity-binding core question:** node HPKE pubkey ↔ existing X.509/mTLS signing
   identity — signed sub-key statement vs RFC 8410 X25519-in-X.509 vs TLS delegated-credential
   (RFC 9345) pattern vs Matrix cross-signing (MSC1756) precedent. Tailnet Lock is the closest
   verified analog but uses its own trust log, not X.509.
2. **Headless delegation survey** — Vault response-wrapping, macaroons/biscuits, KMS grants,
   TPM/enclave escrow, 2-of-2 CP+AS threshold: the entire section produced no surviving claims.
3. **Sealing-format alternatives** — age, libsodium sealed boxes, JOSE ECDH-ES / COSE-HPKE vs
   HPKE-direct; multi-recipient envelope support specifics.
4. **WebCrypto non-extractable X25519 reality** — Secure Curves status per browser; do
   non-extractable CryptoKeys in IndexedDB suffice for the SPA's owner key, or is WASM-HPKE with
   extractable material the honest answer (and what does that change in the XSS threat model)?
5. **Remaining prior art** — Signal/WhatsApp device-add + encrypted-backup HSM vaults, iCloud
   Keychain escrow (SEP guess-limits), Kubernetes sealed-secrets, sops+age workflows.
6. **Go memory hygiene** — mlock/GC-copy realities, memguard worth-it-or-theater.
7. **Windows Hello PRF re-test** (post-KB5077181) — first-party verification.

---

## Source quality

24 sources; the surviving findings rest on **primary vendor security white papers + RFCs**
(1Password, Bitwarden, Tailscale, W3C, Yubico, RFC 9180), verified verbatim — but white papers
describe *intended* design, not audited behavior. Fetched-but-unverified: Vault
response-wrapping docs, AWS KMS grants + EncryptionContext, RFC 8410, RFC 9345 (delegated
credentials), Matrix MSC1756, age spec (C2SP), hpke-js, Igalia Secure-Curves survey,
spacetime.dev Go memory security — all good leads for the gap sections.

---

## Merged synthesis (in-session + cloud runs, 2026-06-10)

The runs **agree on every overlapping conclusion** (ciphertext-only CP proven; HPKE+AAD; Tailnet
Lock as the trust template; 1Password re-wrap; PRF real-but-gated; KDF client-side with pinned
params). The cloud run resolves all gaps and sharpens three things:

1. **Identity binding (the core question): cert-signed HPKE sub-key.** The node generates an
   X25519 HPKE keypair and publishes the pubkey in a small, expiring structure **signed by its
   mTLS cert key** — the RFC 9345 delegated-credential / Signal signed-prekey pattern. Client
   verifies cert chain + SAN against pinned roots → verifies sub-key signature → seals to the
   sub-key. No PKI change, no AS involvement, rotation = re-sign (keep validity short,
   hours–days; revocation latency = sub-key validity). Rejected: RFC 8410 second-cert (couples
   to AS issuance) and CSR-time dual-key (couples encryption rotation to re-enrollment).
2. **Owner custody recommendation (refines the in-session lean): per-device keypairs are the
   root of trust, not PRF.** (c) per-device non-extractable X25519 keypairs (WebCrypto
   CryptoKey in IndexedDB; `crypto/ecdh` in Go), secrets sealed multi-recipient to all enrolled
   devices; device-add = QR/link re-seal from an enrolled device (Signal/Tailnet-Lock style);
   recovery = BIP-39 code as an always-enrolled "virtual device". (b) Argon2id passphrase as
   fallback (client-side, params pinned in signed client code — the Bitwarden
   server-influenced-KDF criticism is the cautionary tale). (a) **PRF demoted to Tier-2
   convenience unlock**: cross-device PRF works only within one passkey sync fabric, Firefox
   has no PRF, Windows Hello PRF (KB5077181, 2026-02) is device-bound, and lose-credential =
   lose-data. Never PRF-only.
3. **Node enrollment hardening (cloud-run addition):** enrollment tokens scoped to
   `(accountId, class, node-pubkey-fingerprint, expiry, single-use)`, issued by the AS to the
   owner's client over a direct pinned connection, redeemed node→AS directly with the CSR —
   fingerprint binding makes a CP-observed token unredeemable with a substituted key (ACME
   external-account-binding / Vault response-wrapping properties). Extends sp-ova §3.1 (which
   had account-scoped single-use tokens but not fingerprint binding). CP's residual power: DoS
   only.

Other resolved gaps:
- **Sealing formats:** HPKE for the node leg (native AAD → bind
  `(spawnId, generation, nodeId, notAfter)`; reject replay/cross-context). age
  (`filippo.io/age`) is the low-risk Go default for the owner-at-rest multi-recipient envelope
  (age has **no AAD** natively — fine at rest, wrong for the node leg). libsodium sealed boxes:
  anonymous-sender only. **COSE-HPKE/JOSE-HPKE are still drafts (-25/-15) — do not build on.**
  FIPS note: X25519 is non-approved under Go's `fips140=only` — P-256 DHKEM if strict-FIPS
  nodes ever matter.
- **WebCrypto reality:** X25519 available across engines **today**; Ed25519 only since Chrome
  137 (2025-05) — rely broadly ~2027. No native HPKE (noble polyfill or libsodium WASM).
  **Non-extractable CryptoKeys are a real browser-enforced boundary WASM can't replicate** —
  XSS can *use* the key while the page is open but can't exfiltrate it; keep device private
  keys in WebCrypto, use WASM only for envelope ops with ephemeral keys.
- **Go memory hygiene: best-effort, not a guarantee.** GC copies/moves objects, so
  mlock-on-heap is near-theater; memguard helps only via off-heap mmap allocations. **The
  enforceable invariant is never-persist** — test with canary secrets grepped from memory
  dumps + all written files after episode end; zeroize on suspend.
- **Headless delegation seams (survey only, design later):** most plausible v1.5 =
  Vault-style "owner pre-seals + single-use TTL interception-detectable token"; biscuits fit
  authorization-not-key-delivery; KMS-grant analogue pulls the AS into escrow (defer); 2-of-2
  CP+AS Shamir split is the only defensible escrow shape (threshold-HPKE itself is
  research-grade). V1 must leave: signed sub-keys (enables pre-seal-to-future-node), AAD
  context scoping, and a wrappable owner data key (envelope structure).
- **Remaining prior art:** Signal SVR / iCloud Keychain escrow = enclave-rate-limited recovery
  — adopt only if server-assisted recovery is ever wanted; sops `updatekeys` = the
  re-seal-on-recipient-change operation; Vault cubbyhole is not E2E by itself.

### Design-spec inputs (net)

Owner side: per-device X25519 keypairs (root) + BIP-39 virtual device + Argon2id fallback; PRF
Tier-2 later. Node side: cert-signed expiring HPKE sub-key over the existing sp-ova identity;
fingerprint-bound enrollment tokens. Wire: secrets stored CP-side as multi-recipient envelopes
to device keys; delivery = owner client verifies (pinned roots → SAN → sub-key sig), unseals,
re-seals via HPKE with `(spawnId, generation, nodeId, notAfter)` AAD; node holds plaintext
memory-only (memguard as defense-in-depth, never-persist as the tested invariant). Staged: ①
foundations (sub-key + sealing + device keys) → ② multi-device + recovery → ③ PRF convenience
→ ④ headless via pre-seal tokens. The dual sealing target (device keys at rest, node sub-keys
in flight) is the one structural addition over standard designs.
