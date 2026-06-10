# Owner-Sealed Secrets — Deep-Research Results (cloud run)

**Date:** 2026-06-10 · **Brief:** [research brief](2026-06-10-owner-sealed-secrets-research-brief.md)
· **Bead:** `sp-2ckv.1` · **Companion:** [in-session run results](2026-06-10-owner-sealed-secrets-research-results.md)
**Provenance:** parallel cloud deep-research session over the same brief (amended revision);
imported verbatim below (only this header added). Fills the in-session run's coverage gaps —
incl. the identity-binding core question, headless-delegation survey, format comparison, and a
recommended node-enrollment flow. Merged synthesis lives in the companion doc.

---

# End-to-End-Encrypted Secret Storage & Delivery for Spawnery: Architecture, Custody, and Sealing

## TL;DR
- **Node enrollment (recommended): nodes generate keypairs locally; only the CSR leaves the machine. Enrollment is direct node→AS, authenticated by a single-use, fingerprint-bound enrollment token the owner's client obtains from the AS over a direct, pinned connection. The CP is never in the cert-issuance loop and can never mint an AS-signed node cert.** Token-leak proofing: scope the token to `(accountId, class, node-pubkey-fingerprint, expiry, single-use)` so even a token observed or relayed by the CP is useless — the CP cannot substitute its own CSR (ACME external-account-binding / Tailscale pre-auth-key-with-lock-signature pattern). The CP's only residual power is denial of service.
- **Build owner custody on per-device X25519 keypairs sealed multi-recipient with HPKE/age-style envelopes, and bind each node's HPKE encryption key to its existing mTLS cert via a cert-signed "device sub-key" (delegated-credential pattern).** This is the only family that (a) works in the web SPA *today* without waiting on PRF maturity, (b) gives CLI parity in Go, and (c) closes the CP key-vending MITM gap using the pinned-roots layer you already shipped.
- **Use passkey-PRF as a Tier-2 convenience unlock, not the root of trust.** As of mid-2026, synced passkeys (iCloud Keychain, Google Password Manager, 1Password, Bitwarden) *do* return the same PRF output across devices in one sync fabric, but Firefox and roaming-key-on-Safari/iOS gaps, plus "lose the credential = lose the data" semantics, make PRF unsafe as the sole custodian. Keep a password/Argon2id + recovery-code path as fallback.
- **Keep the AS out of escrow in v1.** Plaintext lives only in the owner's client and the verified node's memory. Leave three explicit seams for headless delegation later (Vault-style single-use wrap minted while owner present; capability tokens à la biscuit; 2-of-2 threshold escrow CP+AS) but design none of them now.

---

## Node enrollment: CP-proof cert issuance (recommended flow)

The Tailnet-Lock-style guarantee ("the relay never holds signing keys; clients verify signatures over keys before using them") is only as strong as the enrollment path. The recommended flow:

1. **Node generates its keypair locally.** The private key never leaves the node; the AS never sees it (an AS that knows node private keys could hand out node identities wholesale on compromise).
2. **Owner's client requests a one-time enrollment token from the AS** over a *direct* connection (pinned AS pubkey, not relayed via CP), authenticated by the owner's session.
3. **Token reaches the node out-of-band of the CP** (paste, QR), or — if any path might transit the CP — the token is **bound to the node's pubkey fingerprint**: the client fetches the node's pubkey fingerprint first; the AS issues a token scoped to `(accountId, class, pubkey-fingerprint, expiry, single-use)`.
4. **Node connects directly to the AS** (AS pubkey baked into the node daemon), presents token + CSR; the AS verifies the token (including that the CSR's key matches the bound fingerprint), signs, and returns the cert.

**Token-leak proofing properties:**
- *Fingerprint binding* means a stolen/observed token cannot be redeemed with a different keypair — the CP cannot substitute its own CSR.
- *Single-use* makes interception detectable (legitimate enrollment fails if the token was already redeemed — the Vault response-wrapping property).
- *Short TTL* bounds the exposure window.
- *`(accountId, class)` scoping* prevents a leaked token from enrolling a node into another account or escalating self-hosted→cloud class.
- With all four, **the CP's only remaining capability is denial of service**, which matches the threat model (CP fully compromised, ciphertext/relay only).

For self-hosted v1, the direct-only variant (paste/QR + direct node→AS) is sufficient and simplest; fingerprint binding is cheap enough to do from day one and removes the "never transits the CP" operational assumption entirely.

Precedents: ACME external account binding (RFC 8555 §7.3.4), Tailscale pre-auth keys (with Tailnet Lock, pre-signed via `tailscale lock sign`), Kubernetes bootstrap tokens + CSR approval, EST (RFC 7030).

---

## Key Findings

1. **The closest structural precedent to your AS+pin model is Tailscale Tailnet Lock**, which solves exactly your "compromised control plane must not be able to insert keys" problem with a TOFU-then-locked signing chain. Its node-key-signing design transfers directly; its disablement-secret and signing-node mechanics (max 20 signing nodes, rotate Tailnet Lock keys at most once per year to bound TKA growth) are tailnet-specific and don't.
2. **Every mature "server stores ciphertext, client unseals, reseal to recipient" system uses an envelope/recipient model** (1Password vault keys, sops+age recipients, Vault cubbyhole, sealed-secrets). For small KB-sized blobs sealed to N device keys + M node keys, **HPKE (RFC 9180) and age are the two production-grade choices**; age is the lower-risk default in Go, HPKE is the better foundation for the future E2E relay channel.
3. **WebCrypto curve reality (2026): X25519 is broadly available; Ed25519 only shipped in Chrome 137 in May 2025** (after Firefox 129 in August 2024 and Safari 17.0), so per the IPFS/Igalia analysis developers can only "confidently start relying on simple and stable support for Ed25519 in most users' browsers" around **2027**. Native HPKE is *not* in WebCrypto; you'll polyfill (noble) or use WASM (libsodium.js). This argues for X25519-based sealing (libsodium sealed boxes / age stanza / HPKE-DHKEM-X25519) where the KEM primitive is natively available.
4. **Binding a node's *encryption* key to its *signing/TLS* identity is a solved pattern**: publish an HPKE/X25519 public key signed by the node's cert key (delegated-credential / signed-prekey precedent), rather than reusing the TLS key or minting a second X.509. RFC 9345 (delegated credentials) and Signal's signed prekeys are the precedents.
5. **Go memory-hygiene "guarantees" are weak by construction.** `mlock` on Go-managed memory is close to theater because the GC copies/moves objects; `memguard` only helps if it owns the allocation off-heap. Treat node-side plaintext hygiene as best-effort defense-in-depth, not a hard guarantee, and make never-persist the real invariant.

---

## Details

### Section 1 — Prior-art architectures

#### 1Password (2SKD + SRP)
- **Stored server-side:** fully E2E-encrypted vault items *and metadata* (URLs, vault names). The server holds SRP verifiers and encrypted key material, never plaintext keys. Per the published Security Design white paper, key generation is client-side (web or CLI).
- **Key hierarchy:** Two-secret key derivation (2SKD) combines the user's **account password** and a 128-bit machine-generated **Secret Key** via PBKDF2 (the white paper documents 650,000 PBKDF2 rounds per guess for an attacker holding the Secret Key, with the defender doing more) into the **Account Unlock Key (AUK)**; a separate salt derives **SRP-x** for authentication. The AUK unwraps the user's private key, which unwraps vault keys.
- **Device add:** a new device authenticates via SRP and obtains the encrypted private key, decryptable only with AUK (password + Secret Key, the latter transferred via Emergency Kit / QR).
- **Recovery:** there is no password-reset for personal accounts; recovery depends on the Secret Key (Emergency Kit). Business/Teams have a recovery-group / admin model.
- **Transfer to Spawnery:** the 2SKD idea — *combine a memorized secret with a high-entropy machine secret so the server can't brute-force* — is directly relevant to the password-derived fallback. The SRP layer is orthogonal to your problem (you already authenticate nodes via mTLS and AS-signed tokens).

#### Bitwarden (master-password KDF; org/collection keys)
- **Stored server-side:** encrypted vault (protected symmetric key wrapped by master key), plus a master-password hash for auth.
- **KDF:** Bitwarden's help docs state "By default, Bitwarden is set to iterate 600,000 times, as recommended by OWASP" client-side **PBKDF2-HMAC-SHA256**, and "The master password hash has a total default of 700,000 iterations" once server-side iterations are added; as of the 2026.2.1 release Bitwarden raised the *minimum* PBKDF2 iterations to 600,000. Argon2id is offered as an alternative (implemented per OWASP).
- **2024–25 KDF criticism:** Daniel "Palant" documented that Bitwarden's **server-side iterations add no security benefit** (an attacker who steals the vault only redoes client iterations), and that historically some accounts had dangerously low iteration counts (as low as 100,000, and a server could in principle downgrade to 5,000). The lesson for Spawnery: **do KDF entirely client-side, pin the parameters in signed client code, and never let the CP influence KDF cost.**
- **PRF unlock:** Bitwarden ships passkey-PRF vault unlock; its docs confirm PRF is "a deterministic operation where the output will always be the same for a certain input." As of Nov 2025 this extended to Chromium browser extensions.

#### Tailscale Tailnet Lock (the structural twin)
- **Mechanism:** WireGuard node keys (X25519) are distributed by the coordination server, but with Tailnet Lock each node verifies that a peer's node key carries a signature from a **trusted Tailnet Lock Key (TLK)**. The set of trusted TLKs is tracked in a per-node append-only, hash-chained **Tailnet Key Authority (TKA)** ("lockchain") updated via signed Authority Update Messages.
- **Trust model:** TOFU — you trust the coordination server once at setup, then "move the center of trust into your network." Per the Tailnet Lock white paper, "Signing keys are verified by the locally-managed key authority, and are never seen or stored by the control plane." Recovery from a compromised signing key uses `tailscale lock recover-compromised-key` (fork the log, remove devices signed by the bad key).
- **Limits/incidents:** max 20 signing nodes; recommended yearly TLK rotation to bound TKA growth; the white paper itself flags the residual genesis-AUM risk (a malicious control plane at *initial* setup could seed attacker TLKs). No public breach.
- **Transfer:** **This is your model.** Your AS-held name-constrained intermediate plays the role of the trusted signer; your pinned Root CA + AS pubkeys play the role of the TKA genesis state distributed independently of the CP. The key insight you can borrow: *clients verify signatures over keys before using them, and the relay never holds signing keys* — which is exactly your stated design. The enrollment flow at the top of this report (fingerprint-bound single-use tokens, direct node→AS) is the missing piece that keeps the CP out of cert issuance end-to-end.

#### HashiCorp Vault response wrapping (untrusted-relay delivery)
- **Mechanism:** Vault places a response into the **cubbyhole** of a **single-use wrapping token** with a short TTL; the recipient performs one `unwrap`. The secret "never appears in logs or transit," and **malfeasance is detectable** — if an interceptor unwraps first, the legitimate recipient's unwrap fails. Introduced in Vault 0.8; documented through v1.21.x.
- **Transfer:** the *single-use, interception-detectable, TTL-bounded* delivery pattern is the right shape for your **headless "mint a capability while the owner is present"** seam (Section 5) — and the same properties are reused in the enrollment-token design above. But note Vault's cubbyhole relies on Vault holding the encryption key — *not* end-to-end; for Spawnery the wrapped payload must itself be sealed to the node key so the CP-as-relay never sees plaintext.

#### Kubernetes sealed-secrets / Mozilla sops + age (recipient-key envelopes)
- **sops + age:** secrets are encrypted to one or more **age X25519 recipients** (`age1...`); the ciphertext stanza lists each recipient. Multi-recipient and **Shamir threshold key-groups** are first-class (sops `key_groups` + `shamir_threshold`). `sops updatekeys` re-encrypts to a changed recipient set without changing the data key — exactly the **owner-key-rotation / add-a-device** operation you need.
- **Bitnami sealed-secrets:** a cluster-side controller holds the private key; you encrypt to its public key. This is *recipient-key envelope* but with a single controller key — closer to your *node* side (seal to the node's key) than your *owner* side.
- **Transfer:** the age recipient model is essentially a turnkey version of your seal-to-N-recipients requirement. Its weakness for you: age's standard recipients are *anonymous* (no sender authentication) — fine for confidentiality, but you'll add sender/context binding via AAD (Section 4).

#### Signal / WhatsApp device-add + encrypted backups; Apple iCloud Keychain escrow
- **Signal device linking** uses a QR-code link flow; each device has its own identity keypair and the sender encrypts per-device (Sesame/sender-key). **Secure Value Recovery (SVR)** stores an encrypted master key behind a PIN, with an **SGX enclave enforcing a small max-guess count** (so a weak PIN can't be brute-forced server-side); restore sends only an "access key," not the PIN, and uses Argon2 over the PIN with a backupId salt. Signal's newer **Secure Backups** use a 64-character recovery key never shared with Signal.
- **Apple iCloud Keychain escrow** wraps the keychain under the user's iCloud Security Code and an **HSM cluster public key**; recovery requires SRP proof of the security code to the HSM. Per Apple Platform Security: "After the 10th failed attempt, the HSM cluster destroys the escrow record and the keychain is lost forever… These policies are coded in the HSM firmware. The administrative access cards that permit the firmware to be changed have been destroyed."
- **Transfer:** these are the **gold standard for the recovery-with-abuse-resistance problem**. The pattern — *escrow a strong key behind a memorable secret, but rate-limit guesses in hardware/enclave* — is what you'd adopt **only if** you decide to offer server-assisted recovery. In v1 (no escrow), your analogue is a printed high-entropy recovery code acting as a "virtual device," which is simpler and keeps the AS out of escrow.

#### Pattern transfer summary
| Pattern | Transfers to Spawnery? | Why |
|---|---|---|
| Tailnet Lock signed-key chain | **Yes, directly** | Same "untrusted relay must not vend keys" threat; you already have the signer (AS) + pinned roots |
| age/sops recipient envelopes | **Yes** | Turnkey multi-recipient seal; reuse for device+node sealing |
| Vault response-wrapping | **Partially** | Right shape for headless single-use delivery and enrollment tokens, but not E2E by itself |
| 1Password 2SKD | **Yes, for fallback** | Combine memorized + high-entropy secret, client-side only |
| Bitwarden KDF | **As a cautionary tale** | Do KDF client-side, pin params, never let server influence cost |
| iCloud/Signal enclave escrow | **Only if you add recovery** | Gold standard but pulls AS/HSM into trust; defer |

### Section 2 — Owner key custody

#### (a) Passkey-derived (WebAuthn PRF)
- **What it is:** the PRF extension (WebAuthn L3, built on CTAP2 `hmac-secret`) lets the RP evaluate a per-credential PRF at authentication time, yielding a deterministic 32-byte secret you run through HKDF into an encryption key.
- **2026 support matrix (honest):**
  - **Browsers:** Chromium (Chrome/Edge/Brave) and Safari 18 support PRF; **Firefox** has only a prototype and historically deprioritized it (Bugzilla 1863819). So PRF is **not universal** in your web SPA.
  - **Platforms / sync determinism (the load-bearing fact):** synced passkeys return the **same PRF output across all devices in one sync fabric**, because the `hmac-secret` seed travels inside the E2EE-replicated credential. This holds for **iCloud Keychain, Google Password Manager, 1Password, and Bitwarden** (corroborated by the neutral w3c/webauthn PRF explainer and provider docs; per-provider specifics rest partly on Corbado/MojoAuth vendor analyses). **Windows Hello is device-bound and does NOT roam its PRF secret.** Sync is **fabric-scoped, not device-scoped** — an iCloud passkey and a separate Google passkey are different credentials with different PRF outputs, so a laptop and phone must use the *same* provider.
  - **Verified caveat:** early iOS 18.0–18.3 had a bug where the *same* iCloud-synced passkey returned **different** PRF outputs over hybrid (QR) vs platform transport (Apple Developer Forums #774111); fixed in 18.4+. WebKit bugs 311099/314934 still affect *roaming security keys* on Safari macOS/iPadOS 26.4 (not platform passkeys).
  - **Windows Hello timeline:** no `hmac-secret`/PRF until the **Feb 2026 cumulative update KB5077181** on Windows 11 25H2 (build 26200.7840+).
  - **Hardware keys + CLI:** YubiKey 5 (CTAP2 `hmac-secret`) PRF **is reachable from a Go CLI** via `keys-pub/go-libfido2` (CGO binding to libfido2; exposes `HMACSecretExtension`, `HMACSalt`, returns `HMACSecret`, with a fixed-test-vector example confirming determinism on hardware) or by shelling out to libfido2's `fido2-cred`/`fido2-assert` (the approach gocryptfs uses). Browser PRF on roaming USB/NFC keys works in **Chrome 132+/Firefox 139+ on macOS but NOT Safari and NOT iOS/iPadOS** (Apple doesn't pass extension data to roaming authenticators).
- **Failure/lockout:** PRF keys are **bound to the credential**. If the credential is lost or re-created (new credential ID → new seed), the PRF output changes and PRF-encrypted data is **permanently unrecoverable** — Corbado: "If that passkey is lost, the encrypted data becomes permanently inaccessible." Provider account-recovery only helps if it restores the *same synced credential*.
- **Real E2E deployments using PRF:** Bitwarden (vault unlock), 1Password (encrypt data with saved passkeys), and various vault projects (e.g., the Hoddor browser-vault feature request).

#### (b) Password-derived (Argon2id; existing in-house passphrase + BIP-39)
- **Parameters:** RFC 9106 gives two recommended Argon2id configs — **first choice t=1, p=4, m=2 GiB**; **second (memory-constrained) t=3, p=4, m=64 MiB**, both 128-bit salt / 256-bit tag. OWASP's pragmatic web profile is **m=19 MiB, t=2, p=1** (minimum) or **m=46 MiB, t=1, p=1**. A 2025 arXiv study found OWASP's 46 MiB config cuts compromise rates ~42.5% vs SHA-256 at $1/account, while the jump to RFC 9106's 2 GiB adds only ~17–23% more despite 44.5× the memory — so for a *browser* deriving a key, a mid setting (e.g., 256–512 MiB if the device tolerates it, else 64 MiB) is defensible.
- **In-house direction (vault passphrase + BIP-39 recovery codes):** sound and standard. BIP-39 gives a high-entropy recovery "virtual device." Keep KDF client-side; pin params in signed client code.
- **OPAQUE (PAKE):** OPAQUE buys you *protection of the password against a malicious server during authentication* (no password-equivalent verifier server-side, offline-dictionary resistance). But in your model the **CP is already untrusted and never sees the vault key** — the vault key is derived client-side and only ciphertext reaches the CP. So OPAQUE mainly helps if you also use the passphrase to *authenticate to the AS*; for *encryption-key derivation* it adds little over a client-side Argon2id KDF. Verdict: **not worth it for v1.**

#### (c) Per-device keypairs + cross-device authorization (recommended)
- **Mechanism:** each device (laptop browser, phone, workstation CLI) generates a **non-extractable X25519 keypair**; the secret is sealed to *all enrolled device public keys* (multi-recipient). Adding a device = an already-enrolled device re-seals the secret to the new device's key (or vouches for it), via a QR/link flow — structurally identical to Signal device-linking and Tailnet Lock node signing. Recovery = a printed recovery code whose key is treated as an always-enrolled "virtual device."
- **Why it fits:** web-feasible *today* (X25519 is in WebCrypto across browsers; non-extractable `CryptoKey` in IndexedDB), CLI parity is trivial in Go (`crypto/ecdh` X25519 or age), and it directly mirrors the verification model you already built.

#### Owner-custody comparison
| Option | Multi-device UX | Recovery UX + abuse resistance | Web feasible *today* | CLI parity | Impl complexity | Lockout risk |
|---|---|---|---|---|---|---|
| **(a) Passkey-PRF** | Excellent *if* same sync fabric; broken across fabrics; Firefox gap | Poor unless provider restores same credential; no native rate-limited escrow | Partial (no Firefox; Safari/iOS roaming-key gaps) | Workable via libfido2 (CGO) | Medium (matrix handling, fallbacks) | **High** — lose credential = lose data |
| **(b) Password+Argon2id+BIP-39** | Same passphrase everywhere; manual entry | Good (BIP-39 code); abuse-resistance only if you add enclave/rate-limit | **Yes** | **Yes** | Low–Medium | Medium (forget passphrase + lose code) |
| **(c) Per-device keypairs + cross-device auth** | Very good (enroll once per device) | Good (recovery code = virtual device); no server escrow needed | **Yes** | **Yes** | Medium (enrollment/reseal flows) | Low–Medium (need ≥1 enrolled device or recovery code) |

**Recommendation: (c) as the root of trust, (b) as the recovery/fallback path, (a) as an optional Tier-2 convenience unlock layered on top of (c).** This keeps the AS out of escrow, works in the SPA today, and degrades gracefully.

### Section 3 — Sealing format & key-agreement groundwork

#### Envelope-format comparison
| Format | Multi-recipient | Web support 2026 | Go support | Audit/maturity | Relay-channel reuse |
|---|---|---|---|---|---|
| **HPKE (RFC 9180)** | Per-recipient encapsulation (seal to each pubkey) | No native WebCrypto; noble polyfill or libsodium WASM | CIRCL (`cloudflare/circl/hpke`); Go std `crypto/hpke` emerging | High — formal analyses cited in RFC; used in TLS ECH, MLS, ODoH | **Best** — Auth/PSK modes + key schedule are the natural base for the future relay |
| **age (X25519 recipients)** | **Native** (`-r` repeated; stanza per recipient) | via WASM/JS ports; X25519 native in WebCrypto | `filippo.io/age` (mature, stable v1 API) | High — widely deployed, simple, Filippo-authored | Good for store-and-forward; less suited to interactive channel |
| **libsodium sealed boxes** | One box per recipient (anonymous sender) | libsodium.js (WASM) | via cgo or pure-Go ports | High — NaCl lineage | Anonymous-only (X25519 + XSalsa20-Poly1305); no sender auth |
| **JOSE ECDH-ES / COSE-HPKE** | Multi-recipient JWE; COSE-HPKE draft | JOSE libs broad; COSE-HPKE immature | jose libs; COSE-HPKE WIP | JOSE mature; **COSE-HPKE still a draft** | Standardized headers, but draft churn |

- **HPKE modes/suites:** Base (anonymous sender), Auth (sender-authenticated via sender KEM key), PSK, AuthPSK. Standard suites include DHKEM(X25519, HKDF-SHA256)+HKDF-SHA256+AES-128-GCM or +ChaCha20Poly1305, and P-256/P-521 variants. The CFRG is standardizing PQ KEMs (ML-KEM hybrids) under the new HPKE WG.
- **age authentication nuance:** Filippo's "age and Authenticated Encryption" notes X25519 age recipients provide *recipient* authentication ("the sender knew the recipient") but **not multiple sender identities** — to distinguish "sealed by device A vs device B" you need separate keypairs or an explicit signature/AAD.
- **COSE-HPKE status (fast-moving, flag explicitly):** `draft-ietf-cose-hpke` was at **-25 (April 2026)**; the JOSE counterpart `draft-ietf-jose-hpke-encrypt` at **-15 (Nov 2025)**; PQ/T hybrid KEM drafts (`draft-reddy-cose-jose-pqc-hybrid-hpke-11`, Feb 2026) reference RFC 9864 (Oct 2025, fully-specified JOSE/COSE algorithms). **These are not yet RFCs** — do not build on them as stable.

#### WebCrypto reality check (2026)
- **X25519/Ed25519:** Igalia drove Curve25519 into the W3C WebCrypto spec. **X25519 (key agreement) is available across the three engines**; **Ed25519 shipped in Chrome 137 (May 2025)**, after Firefox 129 (August 2024) and Safari 17.0. The IPFS/Igalia analysis projects developers can confidently rely on Ed25519 "in most users' browsers" only around **2027**. Practical implication: prefer X25519 for the KEM and avoid hard Ed25519 dependence in the SPA (or polyfill via noble) until then.
- **HPKE:** **not natively in WebCrypto.** Options: `@noble`-backed JS HPKE (`hpke` npm + `@panva/hpke-noble`) or libsodium WASM. Both are reasonable; noble is auditable TS, libsodium.js is battle-tested NaCl.
- **Non-extractable keys + XSS threat model:** a `CryptoKey` created `extractable:false` and stored in IndexedDB **cannot have its raw bytes exfiltrated even by injected JS** — but XSS can still *use* the key (call `deriveBits`/`decrypt`) while the page is open, and can phish the unlock. So non-extractability protects against *key theft / offline persistence*, not against *live abuse during an XSS window*. Mitigations: strict CSP, Subresource Integrity, the SPA being delivered independently of the CP (you already do this), short in-memory unlock windows, and binding decrypt operations to a user-gesture/WebAuthn step where possible. **WASM vs WebCrypto:** WebCrypto's non-extractable keys are a real, browser-enforced boundary that WASM/libsodium.js cannot replicate (WASM keys live in linear memory readable by JS), so **prefer native WebCrypto for the device private keys** and use WASM only for the envelope wrapping where keys are ephemeral.

#### Go side
- **`crypto/ecdh`:** stdlib X25519/NIST ECDH, clean and recommended for the node daemon and CLI. **FIPS posture:** Go 1.24+ ships the **FIPS 140-3-validated Go Cryptographic Module v1.0.0** (CMVP cert #5247, CAVP A6650); `GODEBUG=fips140=on` enables it. **Caveat: X25519 is *not* FIPS-approved** — under `fips140=only`, `crypto/ecdh` X25519 returns `errors.New("crypto/ecdh: use of X25519 is not allowed in FIPS 140-only mode")`. If you ever need strict-FIPS nodes, seal with P-256 DHKEM instead; otherwise X25519 in default mode is fine.
- **CIRCL (`cloudflare/circl/hpke`):** mature HPKE with all RFC 9180 modes/KEMs; good if you choose HPKE over age.
- **age (`filippo.io/age`):** stable v1 API, X25519 recipients, multi-recipient native; lowest-risk Go default.

#### Identity-binding (node encryption key ↔ mTLS identity) — recommendation
The node cert key is a TLS/signing key; reusing it for decryption is poor hygiene (key-reuse across algorithms, no agreement EKU). Three patterns:
1. **(Recommended) Cert-signed HPKE sub-key ("device-bound sub-key" / delegated-credential style):** the node generates an X25519 HPKE keypair and publishes the pubkey in a small structure **signed by its mTLS cert key**, with an expiry. The client verifies the cert chain + SAN against pinned roots, then verifies the sub-key signature, then seals to the sub-key. This is exactly the **RFC 9345 delegated-credential** precedent — per RFC 9345 §3, "In the absence of an application profile standard specifying otherwise, the maximum validity period is set to 7 days," and an operator self-signs a short-lived credential under its CA-issued cert — and **Signal's signed prekey** precedent (long-term identity key signs a rotating prekey). It needs **no PKI change**, supports easy rotation (re-sign a new sub-key), and keeps the AS uninvolved.
2. **Second cert / X.509 extension carrying a KEM key (RFC 8410 X25519-in-X.509, keyAgreement EKU):** cleaner PKI-wise but requires the AS to issue a *second* cert per node and adds enrollment/rotation cost; heavier than needed for KB secrets.
3. **CSR-time dual-key enrollment at the AS:** the node submits both a TLS key and a KEM key in one CSR; the AS binds them. Strongest binding, but couples encryption-key rotation to AS re-enrollment — undesirable since you want to rotate encryption sub-keys independently of identity.

**Pick #1.** Rotation: node mints a new signed sub-key anytime (short validity, e.g., days); clients always fetch the current sub-key and verify the signature chains to the pinned roots. This mirrors what TLS 1.3 delegated credentials and Signal signed prekeys teach: *let the long-term identity key vouch for a rotating, purpose-specific key.*

### Section 4 — Delivery & lifecycle mechanics

#### The unseal→re-seal flow (concrete)
1. Client requests target node's cert + signed HPKE sub-key (relayed by the untrusted CP).
2. Client verifies cert chain + SAN `<nodeId>.<accountId>.<class>.nodes.spawnery.internal` against **pinned Root CA + AS pubkeys** (independent of CP), confirming the key belongs to the expected `(accountId, class)`. **This is the step that closes the CP key-vending MITM gap.**
3. Client verifies the sub-key signature (Section 3 #1).
4. Client unseals the secret with its device key, re-seals to the node's HPKE sub-key.
5. CP relays the ciphertext; node decrypts in memory.

#### Replay / AAD / freshness
- **Bind ciphertexts to `(spawnId, generation, nodeId)` as AAD** (HPKE `info`/AAD; age has no AAD natively — another reason to prefer HPKE for the node leg, or wrap age output with an AEAD that takes AAD). This prevents the CP from **replaying** a ciphertext sealed for spawn A/generation 1 into spawn B or a re-used node generation.
- **Freshness:** include a per-reseal nonce and a short validity/`notAfter` in the AAD; the node rejects stale or wrong-context ciphertexts. Because writes are rare and reads happen at create/resume, you don't need a streaming nonce scheme — single-shot HPKE seal per delivery is sufficient.
- **What can't be replayed:** a ciphertext bound to `(spawnId, generation, nodeId)` is useless to any other node (different KEM key) or any other spawn/generation (AAD mismatch).

#### Node-side plaintext hygiene (Go)
- **Reality:** Go's GC copies and moves heap objects, so `mlock`/`madvise` on a Go slice address gives **little guarantee** — the runtime may have already copied the bytes elsewhere (documented limitation of `memguard`; "the address you call mlock on isn't even guaranteed to be the address passed in"). `memguard`/`mlocker` help **only** because they allocate **off-heap via mmap/VirtualAlloc**, lock those pages, add guard pages/canaries, and zeroize on `Destroy()`.
- **Verdict:** `memguard`-class libraries are **worth it as defense-in-depth** for the brief window a secret is in node memory (they genuinely reduce swap exposure and core-dump leakage), but they are **not a hard guarantee** against a root attacker or full RAM capture. Don't market them as such.
- **The real invariant is never-persist:** secrets exist only in the spawn's active-episode memory, never written to disk, logs, or the kopia journal. Test it: run the node under a harness that greps process memory dumps and all written files for known canary secrets; assert zero hits after episode end; verify zeroization on suspend via a `Destroy()`-on-suspend hook.

#### Rotation & revocation
- **Owner key rotation / add-remove device:** re-seal every stored ciphertext to the new recipient set (the sops `updatekeys` pattern). Cheap because secrets are KB-sized and writes are rare.
- **Node re-enrollment (new node key):** only secrets *currently sealed to that node for an active/about-to-resume spawn* must be re-sealed; at-rest owner ciphertexts are sealed to *owner device keys*, not node keys, so they're unaffected. The node leg is re-sealed on demand at next create/resume. Re-enrollment itself uses the fingerprint-bound token flow (top of report): new keypair, new fingerprint, new single-use token from the owner's client.
- **Secret rotation:** generate new secret in client, seal, store; old ciphertext is superseded.
- **Revocation latency:** because clients verify against **pinned roots independent of the CP**, revoking a node = the AS stops signing its sub-key / its cert expires; latency is bounded by sub-key validity (keep it short, e.g., hours–days). A compromised CP **cannot** extend trust because it holds no signing keys.

#### Audit / metadata leakage
An honest-but-curious CP can log **ciphertext sizes, timing, and access patterns** (which account, which node, when a spawn reads a secret). For your secret types (repo passwords, API keys, GitHub tokens) the **content is protected**; the metadata leak (that account X ran a spawn on node Y at time T, secret blob ~N bytes) is low-sensitivity but non-zero. Mitigations if needed: pad ciphertexts to fixed buckets (HPKE/AEAD don't hide length), and avoid encoding secret semantics in object names. For v1 this metadata exposure is acceptable; document it.

### Section 5 — Headless delegation (seam survey only)
- **Capability attenuation (macaroons / biscuits):** biscuit tokens (public-key signed, **offline attenuation**, Datalog policies, Protobuf-serialized, sealed variant prevents further attenuation) could express "unseal for spawn X until T." *Trust implication:* a capability lets a holder *trigger* an unseal but must not itself reveal plaintext — so the capability authorizes the *node* to receive a reseal, it doesn't carry the key. Good fit for *authorization*, not *key delivery*.
- **Vault-style response wrapping / single-use tokens minted while owner present:** owner, while online, pre-seals the secret to the node's future sub-key (or to a one-time KEM key) and hands the CP a single-use, TTL-bounded, interception-detectable token. *Trust implication:* matches the "no owner at spawn time" case best and keeps E2E if the wrapped payload is sealed to the node, not to Vault. **Most plausible v1.5 option.**
- **KMS grants (AWS KMS grant model):** time/condition-scoped delegations to use a key, with encryption-context constraints. *Self-hosted analogue:* the AS (or a dedicated KMS-like service) issues scoped grants — but this **pulls a key authority into the trust boundary**, contradicting "AS is not escrow." Defer unless you accept that tradeoff.
- **SSH-agent-style forwarding from an online owner device:** the owner's device stays the only holder of the unseal key and answers reseal challenges live. *Trust implication:* strongest (no new escrow) but requires an owner device to be reachable — not truly "headless."
- **Enclave/TPM escrow (Nitro/SEV/TPM2 sealing):** seal an owner sub-key to a specific node's TPM/enclave measurement so only that attested node can unseal. *Trust implication:* good never-leaves-hardware story, hardware-/attestation-dependent, and ties recovery to specific silicon.
- **Threshold escrow (2-of-2 CP+AS; Shamir / threshold-HPKE):** split the unseal key so **neither CP nor AS alone** can decrypt. *Trust implication:* this is the *one* escrow that's defensible against your threat model (CP compromise alone yields nothing; AS compromise alone yields nothing), at the cost of both parties' liveness and more crypto complexity. Threshold-HPKE is **research-grade**, not turnkey; Shamir splitting of a wrapping key is mature (sops key-groups demonstrate it).

**Seams to leave open in v1:** (1) make the node's encryption key a *signed sub-key* so a future "pre-seal to node before spawn exists" works (mint an ephemeral node KEM key, sign it, seal to it); (2) carry `(spawnId, generation, nodeId, notAfter)` in AAD so a capability/wrap can be scoped to exactly one delivery; (3) keep the owner-side data key **wrappable** (envelope structure) so you can later add a 2-of-2 CP+AS Shamir split *or* an enclave-sealed copy without re-architecting. Design none of these now.

---

## Recommendations (staged adoption path for Spawnery's constraints)

**Stage 0 — Foundations (do first).**
- **Node enrollment: fingerprint-bound single-use token flow** (top of report). Node generates keypair locally; owner's client gets a `(accountId, class, pubkey-fingerprint, expiry, single-use)` token from the AS over a direct pinned connection; node enrolls directly with the AS via token + CSR. CP can never mint a cert; a leaked token is unredeemable by anyone but the bound keypair.
- Node side: each node generates an **X25519 HPKE keypair**, publishes the pubkey in a **cert-key-signed sub-key structure** (Section 3 #1), short validity (hours–days). Reuse your pinned-roots verification to bind it to `(accountId, class)`.
- Sealing: **age (`filippo.io/age`) for the owner-at-rest envelope** (native multi-recipient to device keys) and **HPKE (CIRCL or Go stdlib) for the node leg** so you get AAD binding to `(spawnId, generation, nodeId, notAfter)`. If you want one library, use HPKE for both and add an age-style multi-recipient wrapper.
- Owner custody: **per-device X25519 keypairs**, non-extractable `CryptoKey` in IndexedDB on web, `crypto/ecdh` in Go CLI/daemon. Seal each secret to all enrolled device keys.

**Stage 1 — Multi-device + recovery (single-owner v1).**
- Device enrollment via QR/link reseal flow (Signal/Tailnet-Lock style): an enrolled device re-seals the owner data key to the new device's pubkey.
- Recovery: a printed **BIP-39 high-entropy recovery code** treated as an always-enrolled virtual device. Fallback unlock: **Argon2id** (client-side, params pinned in signed client code; start ~64–256 MiB, t≥2, p=4 tuned to device, per RFC 9106 second profile / OWASP). No server escrow; AS stays out of the key path.

**Stage 2 — Convenience (gated on platform maturity).**
- Add **passkey-PRF as a Tier-2 unlock** wrapping the device key, *only* for users whose provider is in a single sync fabric with PRF (iCloud Keychain, Google Password Manager, 1Password, Bitwarden). Detect support at runtime; **always keep Stage-1 recovery as the floor.** Do not ship PRF-only.
- Revisit Ed25519-in-WebCrypto reliance around **2027** when Chrome 137+ has propagated.

**Stage 3 — Headless (only when needed).**
- Implement the **Vault-style "owner pre-seals + single-use TTL token"** seam first (least new trust). Evaluate **2-of-2 CP+AS Shamir escrow** only if customers demand fully unattended spawn creation and accept the AS entering a *split* (never sole) escrow role.

**Benchmarks/thresholds that would change the recommendation:**
- If **Firefox ships PRF** *and* all your users' providers expose cross-fabric-consistent PRF, you could promote passkey-PRF from Tier-2 to co-primary.
- If you need **strict FIPS nodes**, switch node sealing from X25519 DHKEM to **P-256 DHKEM** (X25519 is non-approved under `fips140=only`).
- If metadata leakage to the CP becomes a stated concern, add **fixed-size ciphertext padding**.
- If unattended/scheduled spawns become a core feature, prioritize Stage 3.

---

## Caveats (thin / vendor-asserted / fast-moving evidence)
- **PRF cross-device determinism** is the linchpin and rests heavily on vendor/commercial sources (Corbado, MojoAuth) plus the neutral w3c explainer; the *design intent* is clear and corroborated, but per-provider behavior has shifted (the verified iOS 18.0–18.3 PRF-mismatch bug, fixed 18.4+) and could shift again. Treat any single provider's guarantee as version-specific.
- **WebCrypto Ed25519** only reached Chrome in May 2025 (Chrome 137), after Firefox 129 (Aug 2024) and Safari 17.0; broad reliance is premature until ~2027. X25519 is the safer near-term primitive.
- **COSE-HPKE / JOSE-HPKE are drafts** (`cose-hpke-25` Apr 2026; `jose-hpke-encrypt-15` Nov 2025), not RFCs — do not treat as stable.
- **Windows Hello PRF** is brand-new (KB5077181, Feb 2026) and device-bound (won't roam) — exclude from your synced-fabric assumptions.
- **Go memory hygiene** guarantees are weak; `memguard` is defense-in-depth, not a hard boundary. The enforceable property is never-persist, which you must test, not assert.
- **Threshold-HPKE** is research-grade; only Shamir-split wrapping keys are production-ready today.
- Your simplifying assumptions materially help: **pinned roots independent of the CP** + **AS-held name-constrained intermediate** give you Tailnet-Lock-grade key-vending protection *for free* relative to systems that must bootstrap trust; **small secrets + rare writes** make re-seal-everything rotation cheap; **single-owner v1** removes the org/collection-key complexity that dominates 1Password/Bitwarden designs. The main complication your model adds over standard designs is the **dual sealing target** (owner device keys at rest, node sub-keys in flight), which the signed-sub-key + AAD-binding design handles cleanly.
