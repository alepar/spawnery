# Web Epic: Independent SPA Delivery, Web Device Keys, and "Move to…" Migration UI

**Date:** 2026-06-11 (amended same day with the [adversarial review](2026-06-11-web-epic-adversarial-review.md)'s
21 confirmed majors + 8 minors — markers `[WMx]`/`[WLx]` below)
**Beads:** sp-2ckv.6 (W1), sp-2ckv.3 (W2), sp-2ckv.7 (W3), sp-8dkp (W4)
**Builds on:** [Owner-Sealed Secrets](2026-06-10-owner-sealed-secrets-design.md),
[Transient-Tier Kopia Journal](2026-06-10-transient-tier-kopia-journal-design.md),
[Storage/Secrets Adversarial Review](2026-06-10-storage-secrets-adversarial-review.md) (prior roast),
[Node Auth & Unified Identity](2026-06-05-node-auth-unified-identity-design.md)

## Problem

The owner-sealed secrets epic's custody guarantee currently holds for native clients only. For the
web it requires (roast C2) that the SPA be delivered **independently of the CP** — a CP-served SPA
can ship malicious JS that uses the live device key, defeating E2E. Once that delivery channel
exists, the web needs device keys (ceremony, enrollment, recovery, management) so browsers become
peers in the device set. Finally, MigrateSpawn (shipped server-side in sp-u53.5.3) needs its web
face — including the in-browser journal-key re-seal leg that owner-sealed migration requires.

**MVP deployment assumption:** the CP and AS are project-hosted only (self-hostable *spawnlets*,
not CPs). This lets the SPA hard-pin its API origins. **Sequencing gate `[WM17]`:** W1–W4 build
and deploy to a private/staging origin on the current dev-token mechanism, but the **public DNS
flip of the canonical origin is gated on minimal per-account token issuance** (at least an
invite-token scheme) — a single shared principal must never be exposed on a public origin with
per-account AS/CP state behind it. Real login is its own epic (being brainstormed next); the AS
must also state what identity it keys device-set logs on as part of that work.

## Decisions (with alternatives considered)

| # | Decision | Alternatives rejected |
|---|----------|----------------------|
| D1 | **Separate pinned static origin** for the SPA, not AS-served, not CP-served | AS-served (couples JS delivery to AS ops); installable PWA/pinned SW (fragile, deferred) |
| D2 | **One canonical origin** (e.g. `app.spawnery.dev`) since the CP is project-hosted for MVP | Per-deployment static origins (no self-hosted CPs to serve); CP-agnostic endpoint config (unneeded; would force loose `connect-src`) |
| D3 | **Pipeline-gated signing only**: CI signs `dist/`, deploy verifies-or-refuses; browser trusts origin TLS + SRI. Signing attests **publication provenance, not build integrity** `[WM20]` | Public transparency manifest (cheap but deferred); browser-side verification à la Code Verify (separate product, deferred) |
| D4 | **Web-first symmetric enrollment**: ceremony can start in web; any enrolled device (web or CLI) approves new devices — enrollment is **bidirectional** (approval response anchors the new device) `[WM5]` | CLI-first bootstrap (web-only users can never enroll); CLI-only approval (pointless asymmetry) |
| D5 | **Scope cut (amended):** core ceremony + enrollment + device-management UI + a **minimal web recovery-phrase flow** `[WM12]` in MVP; **defer** Argon2id passphrase fallback (weakens posture) and the ephemeral web-device class | Including Argon2id/ephemeral-class in MVP; recovery-flow-less MVP (left web-only users with no in-product loss path) |
| D6 | **Auth is out of scope** for the custody design, with two honesty caveats: the public-origin **sequencing gate** `[WM17]`, and custody being CP-auth-independent **only after owner-root pinning** on each device `[WM5]` | Folding a login design into this epic |
| D7 | Move-to = **spawn-scoped steps modal** with **preflight size/ETA, cancel-before-suspend, and minimize-to-badge** `[WM14]` (honors prior-roast m1 / transient-tier T.11) | Global modal; fire-and-forget toast + badge; uncancellable blocking modal |

## §1 Independent SPA delivery (slice W1, sp-2ckv.6)

**Origin model.** The SPA is published to a canonical static origin operated independently of the
CP boxes. Host requirement: custom response headers (Cloudflare Pages / Netlify class; GitHub
Pages excluded — no header support). The CP serves **APIs only**; its CORS allowlist is exactly
the canonical origin (localhost origins are allowed **only on dev-mode CP instances**, never baked
into the production allowlist `[WL5]`). The **AS gets the same CORS pinning** — every §2
device-set RPC is a browser→AS call; the AS RPCs are Connect-JSON like the CP's, with the same
bearer mechanism `[WL6]`.

**WebSocket origin enforcement `[WM18]`.** CORS does not govern WS upgrades. The CP MUST validate
the WS-upgrade `Origin` header against the same allowlist (replacing today's
`OriginPatterns: ["*"]` in `internal/cp/ws.go`). WS auth stays the in-band token bind (browsers
cannot attach headers to WS). The web client dials the **configured CP origin**, not
`location.host` (which breaks the moment the SPA leaves the CP origin).

**Build & integrity.**
- `vite build` with content-hashed assets; the build stamps **SRI `integrity`** attributes on
  every asset reference — **including dynamic `import()` chunks** (import-map integrity or
  `modulepreload` entries; if a chunk cannot be covered, the build fails rather than shipping it
  unhashed) `[WL4]`. The header/CSP config (`_headers` or equivalent) ships **inside the signed
  `dist/`**, not hand-edited at the host `[WL4]`. SRI does not protect against host compromise —
  the host is residual trust; SRI pins assets to the `index.html` that shipped with them.
- No inline scripts: the theme-bootstrap script moves to an external hashed file `[WM19]`.
- **CSP is enumerated against the real bundle, not aspirationally `[WM19]`:**
  `default-src 'none'`; `script-src 'self'`; `style-src 'self'` **plus whatever the audited
  bundle actually needs** — known offenders today: webfonts need `font-src 'self'`; sonner
  injects a runtime `<style>` (bundle its CSS or swap the dep); xterm.js injects dynamic styles
  (cover with hashes/nonces or a **documented, deliberate** scoped relaxation — never a silent
  `style-src 'unsafe-inline'`); `connect-src` hard-pinned to the hosted CP + AS origins
  (https + wss); `img-src 'self' data:`; `frame-ancestors 'none'`; `base-uri 'none'`;
  `object-src 'none'`. No `unsafe-eval`.
- CI runs the Playwright suite against the **prod-built, CSP-enforced** bundle, and the suite
  must **exercise fonts, toasts, the terminal, and one highlight/diagram block** `[WM19]` so a
  dep that regresses to eval/inline fails CI, not production.

**Release gating.** CI builds `dist/` **once** with `npm ci` from the committed lockfile (never
`npm install`) `[WM20]`, signs it with **cosign keyless** (the CI workflow's OIDC identity); the
deploy job **verifies against the pinned exact `--certificate-identity` and
`--certificate-oidc-issuer` (no regexp)** and refuses to publish otherwise `[WM21]`. The same
digest flows test → sign → deploy (the tested bundle is the deployed bundle), with a pre-sign
scan rejecting forbidden values (localhost origins, `unsafe-*` CSP, dev tokens) `[WL3]`.
- **The gate is falsifiable `[WM21]`:** every release runs a pipeline self-test asserting verify
  FAILS for (a) the artifact with its signature stripped and (b) a fixture signed by a different
  identity.
- **Anti-rollback `[WL2]`:** the deploy job enforces a monotonic release counter (or digest
  allowlist); host-dashboard rollbacks and preview deployments are disabled/governed.
  `index.html` ships `Cache-Control: no-cache`; hashed assets ship `immutable`.
- The deploy credential is environment-scoped (deploy env separate from CI build env).

**Trust anchors in the bundle `[WM8]`.** The signed SPA build compiles in the pinned sp-ova Root
CA and the AS pubkeys — this is how the browser later verifies node sub-key chains (§3) without
trusting the CP relay.

**Endpoint config.** CP/AS base URLs are build-time constants in the canonical build. `just web`
dev mode keeps the localhost override and permissive dev behavior, unchanged.

**Residuals (stated honestly `[WM20]`).** Trusted: the static host, the CI signing path, **and
the entire npm dependency tree at build time** — a compromised transitive dep produces a signed,
CSP-clean bundle that can exfiltrate unsealed plaintext over the allowed `connect-src`. Lockfile
discipline + a dependency-update/provenance policy (SLSA attestation as a follow-up) reduce but
do not eliminate this. Untrusted: the CP, per the whole point.

## §2 Web device keys + multi-device (slices W2+W3, sp-2ckv.3 + sp-2ckv.7)

**Crypto substrate `[M15]`.**
- Device key = native WebCrypto **X25519, `extractable: false`**, stored as a CryptoKey in
  IndexedDB; plus a non-extractable ECDSA-P256 signing key for device-set log entries.
- HPKE envelope ops (same envelope format as `internal/secrets/seal`) with recipient-side DHKEM
  via `deriveBits`. The **`extractable === false` invariant applies to persistent device keys**;
  the ceremony/recovery flows are the specced exception — mnemonic-derived key material passes
  through zeroable `ArrayBuffer`s, is imported non-extractable immediately, and is best-effort
  wiped; mnemonic inputs disable autocomplete; the spec states plainly that mnemonic-derived keys
  are extractable-by-construction while in use `[WL1]`.
- **Signature interop `[WM9]`:** WebCrypto ECDSA emits raw IEEE-P1363; Go uses ASN.1-DER. The web
  client converts DER↔P1363 at the WebCrypto boundary.
- **Canonical bytes `[WM9]`:** chain signatures and hashes are computed over the **stored raw
  entry bytes** — clients parse entries for semantics but verify signatures/hashes against the
  original bytes as fetched (no re-serialization on either side). This removes the cross-language
  canonical-JSON problem entirely. (`internal/secrets/seal/deviceset.go` is refactored to match:
  store + transmit the signed byte form.)
- **u64 timestamp precision `[WM10]`:** `InFlightAAD` and sub-key signatures bind
  `u64(UnixNano)`, which exceeds `Number.MAX_SAFE_INTEGER`. The web client parses RFC3339-nano
  with BigInt precision (never via `Date`) and encodes AAD u64s as BigInt.
- Feature-detect X25519 WebCrypto up front; unsupported browsers get "use a supported browser or
  spawnctl" — never an extractable polyfill key.
- **Key persistence `[WM11]`:** the ceremony calls `navigator.storage.persist()` and surfaces the
  result; on startup the client detects key-loss-while-enrolled (IndexedDB evicted — e.g.
  Safari's 7-day ITP rule) and drives re-enroll + revoke-the-stale-member; the loss-disclosure
  copy names browser-data clearing and Safari's eviction explicitly, and recommends enrolling a
  second device before the first seal. Safari-heavy users remain a recorded residual (Argon2id
  stays deferred, D5).

**Lazy ceremony `[M14]`.** Nothing at signup. The first secret-bearing action opens the ceremony:
generate device #1's keys + a **BIP-39 recovery phrase** (the always-enrolled virtual device; its
X25519+ECDSA keys are derived in-page, co-sign the genesis entry, then are wiped from memory).
Mandatory loss-disclosure copy **plus a phrase re-entry confirmation step** `[WM11]`. The genesis
entry (co-signed device #1 + recovery `[M4]`) publishes atomically to the AS; abandoning
mid-ceremony persists nothing anywhere. A **ceremony-time round-trip check** asserts the
re-derived recovery pubkeys equal the enrolled recovery DeviceRef before publishing `[WM10]`.
The action that triggered the ceremony is **parked and auto-resumed** on completion; where a
node-local durability choice is valid, the user can decline the ceremony into node-local instead
`[WL7]`.

**Device-set registry `[M4]`.** The hash-chained member-signed log is **stored at the AS** (the
AS stores, members author — the AS cannot forge entries).
- **Append is a CAS `[WM1]`:** the append RPC rejects any entry whose `PrevHash` ≠ stored head
  (or `Version` ≠ head+1) and returns the new head; clients retry-rebase on conflict. The CAS
  requires no AS-side signature validation — it is pure head comparison.
- **AS-compromise statement `[WM6]`** (amends sp-ova §9): the AS **cannot forge** entries
  (member signatures), but **can withhold or serve a stale prefix** — omission defeats
  revocation for clients that re-seal against the stale view. Mitigations: the current
  chain-head hash is bound into every seal's AAD; clients require a fresh-head check before any
  full-corpus re-seal; the head version is monotonic per pin (head regression = hard fail).
- Clients verify the full chain and pin its head before **every** seal operation; verification
  failure fails closed (no seal, no unseal).
- **Trust anchor `[WM5]`:** a device's `OwnerRoot` pin comes **from the enrollment approval, not
  from the AS**. Only device #1 derives the root itself (genesis authorship). Every later device
  receives `OwnerRoot` + current head in the approval response, pins them locally, and hard-fails
  any chain whose genesis differs. A web client never TOFUs its anchor from the first AS fetch.

**Enrollment (D4, bidirectional `[WM5]`, authenticated `[WM4]`).** A new device generates its
keys and shows an enrollment link (QR is just a rendering) that is **short-lived and single-use**,
carrying the new device's pubkeys. Approval requires a **sound SAS**: both sides independently
derive the verification code from (genesis hash ‖ current head ‖ new-device pubkeys) — the
approver from the pubkeys it *received*, the enrollee from its *own* keys; the code is **never
parsed from the link** (a code carried in the link authenticates nothing). Code entropy/encoding
is pinned (chunked words/digits, ≥48 bits for a user-compared code backed by the commit
structure; the exact SAS construction is fixed at implementation time against the substitution
test). The approver — any enrolled device, web or `spawnctl key approve` — signs the append
entry, **returns `OwnerRoot` + head to the enrollee** `[WM5]`, and **re-seals every existing
secret** (fetch each ciphertext from the CP → unseal locally → re-seal to the new member set →
put back). Entry bodies carry an **authenticated label** (self-asserted device name + enrolled-at,
inside the signed bytes, confirmed by the approver) `[WM15]`.

**Re-seal epochs `[WM2]`.** Enrollment/revocation re-seals are not atomic across N secrets, so:
each ciphertext is stamped with the device-set version it was sealed under (the re-seal epoch,
already implied by the head-hash AAD binding `[WM6]`); a removal is **incomplete until every
secret's epoch ≥ the removal entry's version**; the client persists progress and **resumes**
interrupted sweeps; the UI shows "revocation in progress — N secrets still openable by the
removed device" until complete; the CP flags (and may reject) ciphertext uploads sealed to a
non-member set.

**Revocation cascade `[WM7]`.** Entry authorship is already recorded (`Sigs[].SignerPub`). On
`Remove(A)`, the client surfaces every device that A enrolled (transitively) and offers to remove
them in the same sweep — a stolen device must not survive revocation through a pre-planted
second enrollment.

**Recovery flow (MVP, D5 amended `[WM12]`).** A web user whose only device is lost recovers
in-product: enter the recovery phrase (preceded by the M8 trusted-device warning, verbatim from
the parent design) → derive the recovery keys in-page → sign an add-entry for the fresh device →
re-seal → **force recovery-code rotation** (fresh phrase, fresh virtual device, retire the old
one). This is the one flow where seed material exists in page memory; the copy says so.

**Device management (W3, sp-2ckv.7).** A Settings section lists devices from the verified log
with their authenticated labels + enrolled-at `[WM15]`; the recovery virtual device renders
distinctly and is **not removable via normal revoke**; revoking the current or last
non-recovery device requires explicit recovery-phrase confirmation `[WM15]`. Revoke = removal
entry + epoch-tracked re-seal to survivors `[WM2]` + the cascade prompt `[WM7]`.

**Unenrolled-session UX (M8 minimal).** An unenrolled browser uses all non-secret features; a
secret-bearing action pivots into enroll-or-approve-from-another-device. The ephemeral
("approve this browser for 24h") device class is deferred (D5).

## §3 "Move to…" migration UI (slice W4, sp-8dkp)

**Placement (D7).** "Move to…" in the spawn's ⋯ actions (list row + spawn header). It opens a
**spawn-scoped modal** overlaying only that spawn's view; other spawns stay fully usable.

**Preflight — before any lifecycle call `[WM13]` `[WM14]`.** The modal's first screen does ALL
gating while the spawn is still running:
- **Enrollment check** for owner-sealed spawns: unenrolled browsers see the enroll/approve
  prompt here, with the spawn untouched — never after suspend. (The pivot is enrollment, not the
  genesis "ceremony": approving needs another enrolled device.)
- **Target picker**: a new owner-scoped node-enumeration RPC (`ListMigrationTargets(spawn_id)`)
  returning eligible nodes (id, class, yours/cloud, online). Eligibility enumeration is a **new
  registry method** (shipped `TargetEligible` is a single-node pre-suspend gate, which
  MigrateSpawn re-checks at placement — the picker is advisory) `[WL8]`.
- **Size/ETA estimate `[WM14]`** (honors prior-roast m1 / transient-tier T.11): journal size from
  node stats (surfaced through `ListMigrationTargets` or a small stats RPC) + an honest transfer
  estimate. Cancel here is free. *(Transfer-size follow-up: enable Kopia's client-side zstd
  compression policy in the journaler — separate server-side bead.)*
- **Durability pivot `[WM16]`** (restores transient-tier §4/§5): for a **node-local** spawn,
  Move-to offers the designed cheap upgrade — "moving requires upgrading this spawn's storage to
  owner-sealed" — which re-seals the existing repo password to the owner's device set
  (`journalkey.SealToOwner`; ceremony/enrollment first if needed), then proceeds. No journal data
  is re-encrypted. **Server-side guard:** MigrateSpawn gains a durability-class check rejecting
  cross-node moves whose mounts cannot restore on the target (today it checks tenancy only —
  the UI disable alone enforces nothing). **Ephemeral** mounts: movable with a plain warning that
  ephemeral data does not travel (correct vocabulary: ephemeral / node-local / owner-sealed).

**Modal phases.**
1. Suspending on origin node
2. *(owner-sealed spawns)* "Unlocking journal key on this device": fetch journal-key ciphertext
   (`GetJournalKeyCiphertext`) → unseal with the device key → **verify the target node's sub-key
   chain in-browser `[WM8]`**: pinned Root CA (compiled into the signed bundle, §1) → leaf cert
   (narrowed X.509 profile: P-256 leaf signed by the pinned chain; Go↔TS test vectors) → SAN/
   tenancy/name constraints → **AS revocation check, fail-closed** (prior-roast M12) → sub-key
   signature + validity → node-issued `deliveryId` (prior-roast M11; not client-minted) →
   re-seal → `DeliverSecrets`.
3. Restoring on target
4. Active

After phase 1 the modal can **minimize to the row badge** `[WM14]` and be reopened; a tab reload
reconstructs the modal state from spawn status + delivery state.

**Errors — split by leg `[WM3]`** (the blanket "revert, data intact" claim is deleted; each leg
states what the system actually does):
- **Suspend-leg failure** → spawn lands in `error`; UI offers Recreate ("data as of the last
  journal snapshot").
- **Resume-leg failure** → CP `RevertSuspended` → spawn lands in `suspended` (not running); UI
  says so and offers Resume-on-origin.
- **Delivery-leg failure** (browser closed mid-re-seal, node never gets the key) → the spawn is
  active-but-keyless on the target; this is a **persistent, reload-derivable state**: the UI
  shows "journal key not yet delivered — retry from an enrolled device" until delivery succeeds.
  The CP-side delivery deadline sub-protocol (prior-roast M8: deadline → auto-revert to
  suspended + wipe target artifacts) is specced as the server-side companion and tracked as W4
  CP work.
- **CP unreachable mid-poll** → "connection lost, retrying" with backoff `[WM14]` — never an
  infinite spinner, never a false terminal state.

## §4 Phasing & testing

**Slices:** W1 (SPA delivery) → W2 (device-key core) → W3 (device management) → W4 (Move-to).
W1 gates W2 (roast C2). W4's owner-sealed leg depends on W2. **Public-origin DNS flip is gated
on minimal per-account auth (sequencing gate, `[WM17]`)** — W1–W4 deploy to a private/staging
origin until then.

**Testing.**
- **Cross-language interop is the load-bearing suite**: shared test vectors checked by both Go
  (`spawnctl`) and TS (web) for: HPKE envelopes; device-set chain entries **as raw stored
  bytes** (sign/verify + hash-chain) `[WM9]`; **SignedSubKey verification and
  InFlightAAD/NodeSealed open-at-the-node vectors with non-zero sub-millisecond nanos**
  `[WM10]`; **mnemonic → (X25519 pub, P-256 pub) derivation vectors** `[WM10]`; DER↔P1363
  signature conversion edges.
- Vitest hermetic: ceremony state machine; chain verification (tamper / fork / stale-head /
  **head-regression** fail closed `[WM6]`); CAS retry-rebase `[WM1]`; re-seal epoch
  resume `[WM2]`; BigInt timestamp parsing `[WM10]`.
- Playwright: two-browser-context enrollment (device A approves device B) **plus a
  substituted-pubkey failure test** (MITM'd link must fail the SAS) `[WM4]`;
  abandon-mid-ceremony leaves no trace; key-eviction recovery prompt `[WM11]`; recovery-phrase
  flow with forced rotation `[WM12]`; the suite runs against the CSP-enforced prod build and
  exercises fonts, toasts, terminal, and a highlight block `[WM19]`.
- Go hermetic: AS device-set registry RPCs (CAS conflicts, stale-prefix serving detected by
  client logic); `ListMigrationTargets` enumeration `[WL8]`; MigrateSpawn durability-class
  guard `[WM16]`.
- **Release-gate self-test `[WM21]`:** every release verifies that verification FAILS on a
  signature-stripped artifact and a wrong-identity fixture.
- W4: revoked-node-refusal test (delivery to a node with a revoked cert must fail closed,
  prior-roast M12) `[WM8]`; migration modal Vitest with mocked RPCs; full two-node move stays a
  host-gated e2e.

## Out of scope (deferred, tracked)

- Browser-side release verification (Code Verify-style) and public transparency manifest (D3);
  SLSA provenance attestation `[WM20]` rides with these.
- Argon2id passphrase fallback KEK; ephemeral web-device class (D5).
- Real login/identity (D6) — **own brainstorm next**; the §1 sequencing gate `[WM17]` is this
  epic's only auth obligation.
- Self-hosted CP origins / CP-agnostic endpoint config (revisit when self-hosted CPs exist).

## Roast disposition

All 21 confirmed majors (WM1–WM21) and 4 panel minors (WL1–WL4) are amended into the sections
above; 4 unverified minors (WL5–WL8) likewise. 5 findings were refuted by the panel — see the
[review doc](2026-06-11-web-epic-adversarial-review.md) for the refutation reasoning (do not
re-litigate). Owner decisions on the contested items: WM16 → restore the transient-tier upgrade
pivot; WM12 → minimal recovery flow in MVP (Argon2id stays deferred); WM17 → sequencing gate
(auth brainstormed separately); WM14 → honor m1 (preflight ETA + cancel + minimize), plus the
Kopia client-side-compression follow-up.
