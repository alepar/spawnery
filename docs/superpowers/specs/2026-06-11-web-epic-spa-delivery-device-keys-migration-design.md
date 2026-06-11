# Web Epic: Independent SPA Delivery, Web Device Keys, and "Move to…" Migration UI

**Date:** 2026-06-11
**Beads:** sp-2ckv.6 (SPA delivery), sp-2ckv.3 (web device keys), + new tasks created from this spec
**Builds on:** [Owner-Sealed Secrets](2026-06-10-owner-sealed-secrets-design.md),
[Transient-Tier Kopia Journal](2026-06-10-transient-tier-kopia-journal-design.md),
[Storage/Secrets Adversarial Review](2026-06-10-storage-secrets-adversarial-review.md) (roast C2/M4/M8/M14/M15)

## Problem

The owner-sealed secrets epic's custody guarantee currently holds for native clients only. For the
web it requires (roast C2) that the SPA be delivered **independently of the CP** — a CP-served SPA
can ship malicious JS that uses the live device key, defeating E2E. Once that delivery channel
exists, the web needs device keys (ceremony, enrollment, recovery, management) so browsers become
peers in the device set. Finally, MigrateSpawn (shipped server-side in sp-u53.5.3) needs its web
face — including the in-browser journal-key re-seal leg that owner-sealed migration requires.

**MVP deployment assumption:** the CP and AS are project-hosted only (self-hostable *spawnlets*,
not CPs). This lets the SPA hard-pin its API origins.

## Decisions (with alternatives considered)

| # | Decision | Alternatives rejected |
|---|----------|----------------------|
| D1 | **Separate pinned static origin** for the SPA, not AS-served, not CP-served | AS-served (couples JS delivery to AS ops); installable PWA/pinned SW (fragile, deferred) |
| D2 | **One canonical origin** (e.g. `app.spawnery.dev`) since the CP is project-hosted for MVP | Per-deployment static origins (no self-hosted CPs to serve); CP-agnostic endpoint config (unneeded; would force loose `connect-src`) |
| D3 | **Pipeline-gated signing only**: CI signs `dist/`, deploy verifies-or-refuses; browser trusts origin TLS + SRI | Public transparency manifest (cheap but deferred); browser-side verification à la Code Verify (separate product, deferred) |
| D4 | **Web-first symmetric enrollment**: ceremony can start in web; any enrolled device (web or CLI) approves new devices | CLI-first bootstrap (web-only users can never enroll); CLI-only approval (pointless asymmetry) |
| D5 | **Scope cut:** core ceremony + enrollment + device-management UI in MVP; **defer** Argon2id passphrase fallback (weakens posture; recovery phrase covers loss) and the ephemeral web-device class (M8 minimal flow falls out of lazy ceremony for free) | Including either in MVP |
| D6 | **Auth is out of scope.** The epic keeps the existing bearer-token mechanism; real login is its own epic. Custody never depends on CP auth (the CP is untrusted for custody) | Folding a login design into this epic |
| D7 | Move-to = **spawn-scoped steps modal** (overlays only that spawn's view until a terminal state) | Global modal; fire-and-forget toast + badge |

## §1 Independent SPA delivery (slice W1, sp-2ckv.6)

**Origin model.** The SPA is published to a canonical static origin operated independently of the
CP boxes. Host requirement: custom response headers (Cloudflare Pages / Netlify class; GitHub
Pages excluded — no header support). The CP serves **APIs only**; its CORS allowlist is exactly
the canonical origin plus localhost-dev.

**Build & integrity.**
- `vite build` with content-hashed assets; a build step stamps **SRI `integrity`** attributes on
  every script/style reference in `index.html`. No inline scripts anywhere in the bundle.
- The origin serves a **strict CSP header**: `default-src 'none'`; `script-src 'self'`;
  `style-src 'self'` (hashes if any inline style is unavoidable); `connect-src` **hard-pinned to
  the hosted CP + AS origins** (https + wss for the terminal); `img-src 'self' data:`;
  `frame-ancestors 'none'`; `base-uri 'none'`; `object-src 'none'`. No
  `unsafe-inline`/`unsafe-eval`.
- CI runs the Playwright suite against the **prod-built, CSP-enforced** bundle so a dep that
  regresses to eval/inline fails CI, not production.

**Release gating.** CI builds `dist/` and signs the artifact with **cosign keyless** (the CI
workflow's OIDC identity); the deploy job **verifies the signature against the pinned workflow
identity and refuses to publish otherwise**. The origin is only ever written by
that gated job. Residual trust (stated): the static host and the CI signing path. The CP remains
untrusted — that is the point.

**Endpoint config.** CP/AS base URLs are build-time constants in the canonical build. `just web`
dev mode keeps the localhost override and permissive dev behavior, unchanged.

## §2 Web device keys + multi-device (slices W2+W3, sp-2ckv.3 + device management)

**Crypto substrate `[M15]`.**
- Device key = native WebCrypto **X25519, `extractable: false`**, stored as a CryptoKey in
  IndexedDB; plus a non-extractable ECDSA-P256 signing key for device-set log entries.
- HPKE envelope ops (same envelope format as `internal/secrets/seal`) with recipient-side DHKEM
  via `deriveBits` on the non-extractable key. The client **refuses to operate** on any
  extractable key (runtime assertion + CI assertion `extractable === false`).
- Feature-detect X25519 WebCrypto up front; unsupported browsers get "use a supported browser or
  spawnctl" — never an extractable polyfill key.

**Lazy ceremony `[M14]`.** Nothing at signup. The first secret-bearing action opens the ceremony:
generate device #1's keys + a **BIP-39 recovery phrase** (the always-enrolled virtual device; its
X25519+ECDSA keys are derived in-page, co-sign the genesis entry, then are wiped from memory).
Mandatory loss-disclosure copy. The genesis entry (co-signed device #1 + recovery `[M4]`)
publishes atomically to the AS; abandoning mid-ceremony persists nothing anywhere.

**Device-set registry `[M4]`.** The hash-chained member-signed log is **stored at the AS** (new
append/fetch RPCs; the AS stores, members author — the AS/CP cannot forge entries). Clients verify
the full chain and pin its head before **every** seal operation; verification failure fails
closed (no seal, no unseal).

**Enrollment (D4).** A new device generates its keys and shows an enrollment link (QR is just a
rendering of the same link) carrying its pubkeys + a short fingerprint code. Any enrolled device —
web or `spawnctl key approve` — opens it; the user visually matches the fingerprint code on both
screens; the approver signs the append entry and **re-seals every existing secret** (fetch each
ciphertext from the CP → unseal locally → re-seal to the new member set → put back).

**Device management (W3).** A Settings section lists devices from the verified log; revoke =
member-signed removal entry + re-seal everything to the survivors.

**Unenrolled-session UX (M8 minimal).** An unenrolled browser uses all non-secret features; a
secret-bearing action pivots into enroll-or-approve-from-another-device. The ephemeral
("approve this browser for 24h") device class is deferred (D5).

## §3 "Move to…" migration UI (slice W4)

**Placement (D7).** "Move to…" in the spawn's ⋯ actions (list row + spawn header). It opens a
**spawn-scoped modal** overlaying only that spawn's view (tabs/terminal/chat hidden behind it
until a terminal state); other spawns stay fully usable.

**Target picker — new RPC.** `ListMigrationTargets(spawn_id)` → eligible nodes (id, class,
yours/cloud, online), with eligibility computed server-side by the registry's `TargetEligible`
tenancy/class logic. `spawnctl move` can adopt it later for a `--list` flag.

**Modal phases.**
1. Suspending on origin node
2. *(owner-sealed spawns only)* "Unlocking journal key on this device": fetch journal-key
   ciphertext (`GetJournalKeyCiphertext`) → unseal with the device key → verify the target node's
   cert-signed sub-key (`GetSpawnNodeKey`) → re-seal → `DeliverSecrets`. If the browser is not
   enrolled, the modal pivots into the §2 ceremony/enrollment prompt — this is the W2→W4
   dependency.
3. Restoring on target
4. Active

**Data-class behavior.** Node-local spawns cannot move cross-node by design (key never leaves the
node): entry disabled with an explanation. Scratch-only spawns are movable, with a plain warning
that scratch data does not travel.

**Errors.** Failure → CP reverts to the origin node; the modal shows "Reverted — your data is
intact on *<origin>*" and unblocks the view. Slow migrations are fine: the modal also tracks
`ListSpawns` status polling and dismisses only on a terminal state (active / reverted / failed).

## §4 Phasing & testing

**Slices:** W1 (SPA delivery) → W2 (device-key core) → W3 (device management) → W4 (Move-to).
W1 gates W2 (roast C2: no web custody before independent delivery). W4's owner-sealed leg
depends on W2; its RPC + scratch-only path could land alongside W2 but ships as one slice after,
for simplicity.

**Testing.**
- **Cross-language interop is the load-bearing suite**: shared test vectors (HPKE envelopes +
  device-set chain entries) checked by both Go (`spawnctl`) and TS (web). A web-authored chain
  entry must verify in Go and vice versa — CLI and browser are peers in the same log.
- Vitest hermetic: ceremony state machine; chain verification (tamper / fork / stale-head fail
  closed); HPKE ops via Node's webcrypto (X25519 supported; RFC 9180 A.1 vector keys imported
  non-extractable).
- Playwright: two-browser-context enrollment (device A approves device B); abandon-mid-ceremony
  leaves no trace; the suite runs against the CSP-enforced prod build (also W1's regression gate).
- Go hermetic: AS device-set registry RPCs; `ListMigrationTargets` eligibility.
- Migration modal: Vitest with mocked RPCs; a full two-node move stays a host-gated e2e.

## Out of scope (deferred, tracked)

- Browser-side release verification (Code Verify-style) and public transparency manifest (D3).
- Argon2id passphrase fallback KEK; ephemeral web-device class (D5).
- Real login/identity (D6).
- Self-hosted CP origins / CP-agnostic endpoint config (revisit when self-hosted CPs exist).
