# Adversarial Review ("Roast") — 2026-06-11 Web Epic: SPA Delivery, Device Keys, Migration

**Spec:** `/var/home/alepar/AleCode/spawnery/docs/superpowers/specs/2026-06-11-web-epic-spa-delivery-device-keys-migration-design.md`

## Verdict

The epic's headline custody-*confidentiality* guarantee largely holds (non-extractable device keys, sealed envelopes, CP untrusted for custody), but the spec ships with a dense cluster of confirmed *integrity, availability, interop, and UX-failure* gaps that a panel verified against both the spec text and shipped code. The most consequential are concurrency and freshness holes in the device-set registry (no append CAS, no anti-rollback, no per-enrollee trust anchor), a re-seal/revocation flow that is non-atomic and non-cascading, a migration error story that promises a revert the system cannot produce, an enrollment SAS that authenticates nothing as written, and a web crypto-substrate that mandates cross-language interop while leaving canonical encoding, signature formats, timestamp precision, and node-PKI verification unspecified. A recurring pattern is that the spec silently fails to carry forward agreed prior-roast amendments (M4 AS-compromise statement, M8 delivery sub-protocol, m1 preflight/cancel, M11/M12 node-cert revocation) and frames a public, multi-user deployment on an auth mechanism (`dev-token`) that makes every visitor the same principal. None rise to a custody-breaking critical under the stated MVP threat model, but 21 are must-fix majors before W1/W2 freeze the wire and UX. Five candidate findings were refuted (mostly by reading the actual data flow or by the requirement already living in the parent design).

## Confirmed findings

### WM1 — Device-set append has no compare-and-swap; concurrent appends fork the chain
**Sections:** S2 (device-set registry, D4); parent S.5
**Claim:** The log is a strictly linear chain, yet the AS append RPC is specified only as "the AS stores, members author" with no optimistic concurrency control. Two devices (or tabs) that both fetch head N and author N+1 fork the set.
**Evidence that survived:** `deviceset.go:234-242` rejects any non-+1 version / prev-hash mismatch; `AddDevice`/`RemoveDevice` build off local `head()` with no head token returned to the caller; spec lines 77-80 give no CAS/head-pin rule; the test plan even requires forks to "fail closed."
**Impact:** A stored fork makes `VerifyDeviceSet` fail closed for every client → no seal, no unseal (owner locked out of all secrets); if the AS keeps one entry, the other device's enroll/revoke is silently lost. Recoverable via the project-hosted AS → major.
**Amendment:** Specify the append RPC as a CAS on the current head (reject `PrevHash ≠ stored head` / `Version ≠ head+1`), returning the new head; clients retry-rebase. State that CAS does not require the AS to author or validate signatures.

### WM2 — Re-seal across N secrets is non-atomic with no epoch or resume; partial revocation leaves secrets sealed to the revoked device
**Sections:** S2 (enrollment re-seal; revoke = removal entry + re-seal to survivors)
**Claim:** Revoke/enroll require the browser to fetch→unseal→re-seal→put back *every* secret. There is no transaction, re-seal epoch, or partial-completion detection.
**Evidence that survived:** Spec lines 84-90; `journalkey_ciphertext.go:80` `PutJournalKeyCiphertext` is per-(spawn,mount) with no version; `assertOpenableByOwner` (91-104) passes any envelope openable by *some* current device and does not reject one still sealed to a removed device, so a partially re-sealed corpus passes every guard.
**Impact:** A crash/closed tab mid-revocation leaves un-swept secrets openable by the revoked device indefinitely while the UI reports success. Degrades an already best-effort guarantee → major.
**Amendment:** Define a re-seal epoch stamped on each ciphertext and on the device-set head; revocation incomplete until every secret's epoch ≥ the removal entry's version; surface "revocation in progress / N secrets still exposed" and make it resumable; add a CP-side guard that flags/rejects ciphertext sealed to a non-member.

### WM3 — The blanket "revert to origin, data intact" promise is false for every shipped failure leg
**Sections:** S3 (errors), D7; prior roast M8
**Claim:** §3's "Failure → CP reverts to the origin node" is wrong for suspend-leg timeouts (terminal `error`), resume-leg reverts (land in `suspended`, not running), and the browser-driven post-resume journal-key delivery leg (no CP timeout/waiter, no revert possible).
**Evidence that survived:** `lifecycle.go:230-239` suspend timeout → `SetError`; `:312-352` `failResume`/`RevertSuspended` only inside `resumeLocked`, rolls to `suspended`; `secrets.go:93-117` `DeliverSecrets` is fire-and-relay with no waiter/timeout; `move.go:87,111-113` CLI copy "migrated, but journal key not yet delivered — retry the move." M8's delivery-timeout sub-protocol exists in `owner-sealed-secrets-design.md §3` but is implemented nowhere.
**Impact:** Owner closes the laptop mid-"Move to cloud" → spawn active-but-empty on target indefinitely, success-looking UI, no recovery hook; suspend-timeout users told "data intact" while in terminal error. Data recoverable by retried delivery → major.
**Amendment:** Split the error table by leg (suspend-fail → error+Recreate; resume-fail → suspended+Resume; delivery-fail → persistent "key not delivered / retry" state derivable after reload). Either scope the revert claim to MigrateSpawn-internal failures or spec the M8 CP-side delivery deadline (→ suspended + wipe target artifacts). Delete the blanket revert claim.

### WM4 — Enrollment authentication is one unspecified "short fingerprint code" carried inside the link it is meant to authenticate
**Sections:** S2 (enrollment), D4
**Claim:** Approval (sign + re-seal every secret) is gated solely on a human matching a short fingerprint code with no specified length/entropy/derivation side, no expiry/single-use, and the code carried in the same link as the pubkeys. Three breaks: brute-forceable short truncations, vacuous check if parsed-from-link, and phishable attacker-originated links.
**Evidence that survived:** Spec lines 84-88; `key.go:96-101` the only existing fingerprint is 128-bit/32-hex (far beyond practical visual comparison); parent §5's fingerprint-bound tokens are node→AS, not device→device. Crypto fact: matching a k-bit truncated fingerprint costs ~2^k cheap keygen trials.
**Impact:** A substituted or phished enrollment link enrolls an attacker device; the approving device hands it every sealed secret in the same action. Requires a link-channel/phishing adversary → major.
**Amendment:** Specify a sound SAS derived independently on each side from received/own pubkeys (never parsed from the link), pinned encoding and ≥80-bit entropy or a commit-based short-auth-string protocol; chunked entry; short-lived single-use link bound to an approver session; add a substituted-pubkey Playwright failure test.

### WM5 — Newly enrolled devices (≥2) have no trust anchor; first chain fetch is TOFU against the untrusted AS
**Sections:** S2 (device-set registry, lazy ceremony, enrollment), D4, D6
**Claim:** `VerifyDeviceSet` needs an `OwnerRoot` the CLI persists at genesis, but enrollment is strictly one-directional (new→approver) so a newly enrolled browser's first fetch is pure TOFU; a malicious AS serves a forged parallel chain rooted in attacker keys + the victim's new pubkey. Genesis-to-account binding rides the deferred bearer-token auth and lazy ceremony keeps the squat window open.
**Evidence that survived:** `deviceset.go:108-113,209-282` verifies against a held root; `key.go:42-49` persists the root only for device #1; spec "verify the full chain and pin its head" never states what anchors the first verification; `connect.ts:2` `DEV_TOKEN`; prior roast M4 panel note flagged the genesis-TOFU leg.
**Impact:** AS compromise (or bearer-session squat) defeats the member-signed chain for every device except #1; secrets re-sealed from a split-viewed browser go to an attacker set with no detection → major.
**Amendment:** Make enrollment bidirectional — the matched fingerprint commits to (genesis hash ‖ head ‖ new-device pubkeys), computed independently on both screens; the approval response carries `OwnerRoot` + head, pinned by the new device (IndexedDB/keyfile) and hard-failing any chain whose genesis differs. Add genesis anti-squat and correct D6 (custody is CP-auth-independent only after root pinning).

### WM6 — AS withholding/stale-head rollback defeats revocation; the M4 AS-compromise statement was never written
**Sections:** S2 (device-set registry), D6; prior roast M4; parent S.5, sp-ova §9
**Claim:** The spec asserts only "the AS/CP cannot forge entries" and is silent on omission: a validly-signed chain prefix omitting the latest removal verifies cleanly, and a re-sealing client re-seals everything to a set still containing the revoked device. The M4-mandated AS-compromise can/cannot statement and sp-ova §9 amendment never happened.
**Evidence that survived:** Spec lines 78-81, line 18 (CP+AS project-hosted, single operator); `deviceset.go:209-261` verifies only internal consistency; M4 amendment text mandates the AS-compromise statement; sp-ova §9 untouched. Note: "every new tab is a clean TOFU client" overstated (IndexedDB persists), but the prefix attack works against pinned clients.
**Impact:** Revocation is defeatable by the AS operator via pure omission; the "cannot forge" paragraph reads stronger than it is. Requires AS misbehavior (the trusted anchor) → major.
**Amendment:** Add an explicit AS-compromise can/cannot table (forge: no; withhold/serve-stale: yes) and update sp-ova §9. Bind the current chain-head hash into every seal's AAD; add a signed monotonic head counter / fresh-head check before any full-corpus re-seal; web clients obtain their initial pin from an enrolled device, never the first AS response.

### WM7 — Revocation does not cascade: a compromised device enrolls a second attacker device first
**Sections:** S2 (enrollment, device management)
**Claim:** Any current member can append an Add entry. Compromised device A enrolls attacker A′ before removal; removing A re-seals everything to a set still containing A′.
**Evidence that survived:** `deviceset.go:173-177,284-300` `AddDevice` requires only a current-member signature; `RemoveDevice` removes one device with no cascade/quarantine; spec D4/§2 (any enrolled device approves, fingerprint match by the compromised device itself). The parent M3 fresh-DEK fix is bypassed because A′ is a legitimate survivor. (Authorship *is* recorded in `Sigs[].SignerPub`, so the amendment's "record the author" is partly satisfied at the data layer; the gap is spec/UI semantics.)
**Impact:** Stolen device bootstraps persistent access surviving the owner's revocation — false assurance in the exact incident-response flow → major.
**Amendment:** On `Remove(A)`, surface (and offer to also remove) every device A enrolled; or require quorum/owner-root co-sign for enrollment so a single compromised device cannot unilaterally add members.

### WM8 — In-browser node PKI verification is required by §3 but entirely unspecified; the reference client stubs it
**Sections:** S1 (endpoint config), S2 (crypto substrate), S3 (phase 2); prior roast M11/M12
**Claim:** §3 names only the sub-key step but the shipped chain is five steps; the spec omits the M12-mandated AS revocation check, never says how the SPA obtains the pinned Root CA / AS pubkeys, and specifies no X.509 approach (WebCrypto has no X.509). The reference client skips verification and mints `deliveryId` client-side contra M11.
**Evidence that survived:** `subkey/verify.go:50-110` (pinned roots + name constraints + SAN/tenancy, fail-closed `RevocationChecker`, nodeID match, sub-key sig, validity); `move.go:33,121-130` "the chain+sub-key are NOT yet PKI-verified here", client-minted uuid; `cp.proto:224` hands back raw PEM; spec §1 lists only CP/AS base URLs; M12 makes the revocation check mandatory at this point.
**Impact:** If W4 copies the existing client, a compromised CP relays a fabricated sub-key and receives the re-sealed journal key — voiding custody on the path this epic protects; revoked-node delivery (M12's attack). Spec omission with a clear fix → major.
**Amendment:** Add a §2 bullet: pinned Root CA + AS pubkeys compiled into the signed SPA build; the in-browser X.509 approach (named library or a narrowed P-256-leaf-signed-by-root profile) with Go↔TS vectors; AS revocation fetch (fail-closed) in §3 phase 2; state the web leg implements the M11 node-issued deliveryId; add a revoked-node-refusal W4 test.

### WM9 — Cross-language chain interop has no canonical encoding and an unmentioned DER↔P1363 ECDSA conversion
**Sections:** S2 (device-set registry, crypto substrate), S4 (interop suite)
**Claim:** Both the signed body and the hash chain are computed over Go `encoding/json` output re-serialized at verification, so a TS client must byte-replicate Go (field order, padded std-base64, nil→`null`, uint64-as-number, HTML escaping). "Deterministic per-language" ≠ cross-language canonical, and the spec pins none. Separately, WebCrypto ECDSA is raw IEEE-P1363 while Go uses `SignASN1`/`VerifyASN1`.
**Evidence that survived:** `deviceset.go:62-76` `signedBody()`, `:80-87` `hash()` over the full entry incl. Sigs, `:45,95,307` ASN.1-DER; spec §4 lists vectors with no encoding definition; verification fails closed → lockout. (Today's fields contain no HTML-escapable chars, so the escape hazard is latent until a future free-form field; interop CI would catch gross divergence.)
**Impact:** Any divergence (key order, base64 variant, null-vs-empty, r||s) makes honest web clients fail closed → owner locked out; the path of least resistance is weakening verification. A future label field diverges silently after vectors pass → major.
**Amendment:** Either define signatures/hashes over the STORED raw entry bytes (parse for semantics, verify original bytes), or pin an explicit canonical encoding (RFC 8785 JCS or the exact Go output) and ship a TS serializer with edge-case vectors. Add one sentence mandating DER↔P1363 conversion at the WebCrypto boundary.

### WM10 — §4 interop suite omits the structures W4 and recovery depend on: u64-nanosecond timestamps and the mnemonic→keypair derivation
**Sections:** S2 (lazy ceremony), S3 (phase 2), S4 (testing)
**Claim:** (1) `InFlightAAD` and the sub-key signature bind `u64(UnixNano)` (~1.78e18, > `Number.MAX_SAFE_INTEGER`); `SignedSubKey` reaches the browser as Go RFC3339-nano JSON. Default TS `Date`/`Number` truncates → wrong `signedBytes` (bad sig) and wrong AAD (node `OpenFromOwner` mismatch). (2) Recovery re-derivation (BIP-39 → HKDF-Expand → circl `DeriveKeyPair` → bespoke P-256 scalar) has no cross-language vectors.
**Evidence that survived:** `delivery.go:30-40`, `subkey.go:73-105`, `cp/secrets.go:27` + `move.go:89-92,131-137`, `device.go:26-135`; sub-keys minted from ns-resolution `time.Now()`; spec §4 vector list omits SignedSubKey/AAD/derivation. Prior roast M15 covers HPKE vectors only.
**Impact:** Every web-driven owner-sealed migration fails closed at the node, debuggable only in the host-gated e2e; a TS derivation divergence yields a recovery pubkey that isn't the enrolled one — permanent loss discovered at all-devices-lost time → major.
**Amendment:** Mandate BigInt-precision RFC3339-nano parsing (never `Date`) and BigInt u64 AAD encoding; add SignedSubKey-verify and InFlightAAD/NodeSealed open-at-node vectors with non-zero sub-ms nanos; add mnemonic→(X25519 pub, P-256 SEC1 pub) vectors; add a ceremony-time round-trip check asserting re-derived pubkeys equal the enrolled recovery DeviceRef before publishing.

### WM11 — Web device keys live in evictable IndexedDB with no persistence request, eviction detection, or phrase-save verification
**Sections:** S2 (crypto substrate, lazy ceremony, device management), D4, D5
**Claim:** The device key exists only as a non-extractable CryptoKey in IndexedDB; the spec never mentions `navigator.storage.persist()`, eviction, or key-loss-while-enrolled. Safari ITP deletes script-writable storage after 7 idle days (D1 defers the PWA exemption); the ceremony shows the phrase once with no re-entry quiz; the device set has no liveness concept.
**Evidence that survived:** Spec §2 (sole persistence statement), no `persist`/`eviction` anywhere; D1 defers PWA; D5 "recovery phrase covers loss"; `deviceset.go` has no dead-member concept; platform facts (ITP 7-day cap, non-extractable = unbackupable). Prior roast does not cover eviction.
**Impact:** An occasional Safari user loses web custody every idle week, silently; between eviction and recovery every owner-sealed action is bricked; if the phrase was never saved, permanent loss. Recoverable with a saved phrase → major.
**Amendment:** Mandate `navigator.storage.persist()` during the ceremony and surface the result; detect key-loss-while-enrolled on startup and drive re-enroll + revoke-stale-member; add a phrase confirmation step; name browser-data clearing and Safari's 7-day rule in the loss copy; recommend a second device before the first seal; reconsider D5's passphrase deferral for Safari or record the residual.

### WM12 — Recovery when the only enrolled device is lost has no MVP flow, and web seed entry ships without the M8-mandated warning
**Sections:** S2, D5; parent §4, prior roast M8
**Claim:** A web-only user who loses their single browser cannot re-enroll (D4 needs an enrolled approver); the BIP-39 recovery device is the only path but the MVP scope contains no recovery-phrase-entry flow, no rotate-after-use, and no warning copy — so D5's "recovery phrase covers loss" rationale is unimplemented. Typing the master seed materializes the root of all keys in page memory; M8 required an explicit warning before web recovery-code entry.
**Evidence that survived:** Spec §2 scope list; D5 quote; parent §4 recovery lifecycle; parent §3 [M8] trusted-device banner; `key.go:209-219` `recoverDevice` exists but enroll-fresh-and-reseal not wired; `device.go:72-117` mnemonic derives the full seed in-page. (One panel vote refuted: the recovery virtual device is always enrolled and `spawnctl key recover` exists, so total lockout is overstated; net it remains a real scope/UX gap and an M8 warning omission.)
**Impact:** A web-only user with a perfect phrase may still hit no in-product recovery path; later web seed entry without warnings hands a hotel-browser the master seed → major (contested).
**Amendment:** Either pull a minimal recovery flow into MVP (enter phrase → derive in-page → sign add-entry → re-seal → force recovery-code rotation, with the M8 warning verbatim) or un-defer Argon2id and rewrite D5's rationale. State that recovery entry is the one flow where the seed is extractable in page memory.

### WM13 — Unenrolled-browser enrollment pivot fires after the spawn is suspended, stranding it with no resume path
**Sections:** S3 (phase 2), S2 (enrollment), D4
**Claim:** §3 sequences the enrollment check inside phase 2, after "Suspending on origin node." Enrollment needs a second already-enrolled device; if the user abandons, revert lands in `suspended` and resuming an owner-sealed spawn itself needs the missing key leg. The check is local and could gate the modal before phase 1; the pivot can never be the full genesis "ceremony."
**Evidence that survived:** Spec §3, §2 (D4); `lifecycle.go:344` `RevertSuspended` → `suspended`; owner-sealed §3 [M8] "resume requires an enrolled device." MigrateSpawn pre-validates target tenancy before suspending and `spawnctl move` loads the key before any RPC — the spec contradicts the codebase's own preflight convention.
**Impact:** "Move to…" on a hotel browser tears down the active spawn, then the user discovers they need their phone; worst case the spawn sits suspended until they get home with no path back → major.
**Amendment:** Make enrollment a preflight: the Move-to entrypoint checks local enrollment (and, for owner-sealed spawns, unseal ability) before any lifecycle call; unenrolled browsers see the enroll prompt with the spawn still running. Strike "ceremony/" from the pivot copy.

### WM14 — Move-to modal has no cancel, no preflight ETA, no offline/reload behavior — silently reverting the accepted m1 amendment
**Sections:** S3 (errors), D7; prior roast m1, transient-tier §5/T.11
**Claim:** D7's modal blocks the spawn view until terminal with no cancel/minimize/preflight, contradicting the m1 amendment ("preflight ETA + cancel + honest size/transfer estimate") carried into transient-tier §5/T.11; the spec asserts "slow migrations are fine" against m1's 25-45+ min residential-uplink scenario. No behavior is defined for CP-unreachable mid-poll or tab reload.
**Evidence that survived:** Spec §3/D7 (no preflight/estimate/cancel; fire-and-forget rejected); transient-tier `§5 lines 186-188` + T.11; roast m1 amendment text; the new spec's header cites C2/M4/M8/M14/M15 but not m1, and does not defer/amend T.11. (One vote rated minor, noting m1 was itself a downgraded UX item and the modal blocks only one spawn's view.)
**Impact:** Users start large moves blind and cannot abort; a tens-of-minutes uncancellable modal holds the spawn view; a CP blip leaves it spinning. A confirmed prior fix silently reverted → major (contested).
**Amendment:** Add preflight size/ETA (spec the journal-size source), a cancel-before-suspend affordance mapping to a defined CP state, minimize-to-badge after phase 1, and an offline/retry branch for poll failures — or explicitly amend transient-tier §5/T.11 and the m1 record with rationale.

### WM15 — W3 device list renders raw pubkeys with no authenticated names, no recovery-device distinction, and no last-device/recovery guardrails
**Sections:** S2 (device management W3, device-set registry)
**Claim:** "Lists devices from the verified log; revoke = removal entry + re-seal" — but `DeviceRef` is two raw pubkeys, `Entry` carries no label/timestamp, the recovery virtual device is an ordinary removable member, and no guard blocks revoking your own/last/recovery device. Any naming outside the signed entry is AS/CP-mutable.
**Evidence that survived:** `deviceset.go:32-35,52-59,126,153-171`; `key.go:96-101` (only identity is a 32-hex fingerprint); spec W3 is one sentence. Recovery member is in principle distinguishable client-side via `OwnerRoot.RecoverySignPub`, but neither spec nor CLI does it.
**Impact:** The headline scenario (revoke the stolen laptop from your phone) presents N unlabeled fingerprints; mis-revoke leaves the thief enrolled and re-seals to the thief; accidentally revoking the recovery member deletes the only loss-recovery path D5 relies on → major.
**Amendment:** Put an authenticated label (name + enrolled-at) inside the signed entry body, self-asserted at enroll and confirmed by the approver. Render the recovery device distinctly and make it non-removable through normal revoke. Block revoking the current/last non-recovery device without explicit recovery-phrase confirmation.

### WM16 — "Node-local spawns cannot move" contradicts the transient-tier migrate-triggers-upgrade decision, uses stale vocabulary, and is UI-only
**Sections:** S3 (data-class behavior); transient-tier §1/§4/§5
**Claim:** Transient-tier specifies migration as the trigger for the cheap node-local→owner-sealed upgrade; the new spec permanently disables the entry instead, silently re-litigating a recorded decision. The disable is cosmetic — MigrateSpawn checks tenancy only, so any CLI caller can still migrate a node-local spawn into unrestorable mounts. "Scratch-only" uses retired taxonomy and the ephemeral warning mis-attributes the loss.
**Evidence that survived:** Spec §3 lines 115-117; transient-tier §5 `182-185`, §4 `168-171`, §1 `45-49`, phase ④ `240`; `journalkeys.go:13-15` and `journalkey_ciphertext.go:50-55` exist for the upgrade; `lifecycle.go:361-414` tenancy-check-only; `ParseDurability` unset→Ephemeral while transient-tier defaults prior-scratch to node-local.
**Impact:** Forecloses the designed upgrade path without recording the reversal; default node-local users hit a dead end; the "cannot move by design" claim is enforced nowhere server-side → major.
**Amendment:** Replace the disabled entry with the designed pivot (Move-to on node-local opens the lazy ceremony + upgrade, then proceeds). If the cut is intentional for MVP, say so as an explicit amendment to transient-tier §5/phase ④ with rationale, and add a server-side durability-class guard to MigrateSpawn. Restate the table in ephemeral/node-local/owner-sealed vocabulary and fix the ephemeral warning.

### WM17 — D6 "keep the existing bearer-token mechanism" is a hardcoded shared dev-token; W1's public origin and W2's per-account AS registry cannot ship on it
**Sections:** S1, S2, D2, D6, S4 (sequencing)
**Claim:** The "existing mechanism" is the literal constant `Bearer dev-token`; the CP seeds dev-token owners; the AS has no user auth (its enroll API delegates user authentication to a caller that doesn't exist). Yet the epic publishes a public SPA origin and adds per-account AS device-set RPCs and CP-stored ciphertexts while deferring login — so every visitor is the same principal.
**Evidence that survived:** Spec D6 (30-31), line 18, D2; `connect.ts:2` `DEV_TOKEN`; `auth/auth.go` stub; `seed.go:22`; `authsvc/enroll.go:41` (caller must authenticate accountID owner — none exists); `cp.proto:32-37` owner-gated RPCs. (`PutJournalKeyCiphertext` partially guards blind overwrite but not replay/rollback, delete, migrate, or AS-log pollution. One vote rated minor, noting E4/login is tracked and registry integrity rests on the signed chain.)
**Impact:** Either W1-W3 silently block on the unscheduled login epic, or they ship against the stub: a public origin where any user enumerates/destroys others' spawns, ciphertexts, and device-set state. Confidentiality intact; availability/integrity/tenancy break → major (contested).
**Amendment:** Add an explicit §4 sequencing gate: public-origin deployment requires at minimum per-account token issuance (even an invite-token scheme), and specify what identity the AS keys the device-set log on — or re-scope the MVP deployment assumption to private/trusted-network deployments.

### WM18 — CORS does not govern the wss terminal endpoint; WS Origin validation is unspecified and the shipped CP wildcard-accepts any Origin
**Sections:** S1 (origin model)
**Claim:** The only CP-side origin control is the CORS allowlist, but WebSocket upgrades are not subject to CORS; the spec says nothing about Origin validation on the upgrade path, the shipped CP wildcard-accepts all origins, and the browser WS API cannot attach an Authorization header (token rides in-band). The current client dials `ws://${location.host}`, breaking once the SPA leaves the CP origin.
**Evidence that survived:** Spec lines 37-39, 44-45; `ws.go:19` `OriginPatterns: []string{"*"} // dev only`, `:39-44` in-band bind auth; `TerminalView.tsx:92`, `AcpSessionPanel.tsx:42` `ws://${location.host}/ws/session`. (Auth is a non-ambient bearer token, so a cross-site upgrade fails closed today; the CSWSH escalation is conditional on future cookie auth. One vote rated minor.)
**Impact:** Cross-site WebSocket hijacking surface on the production CP; if WS auth ever becomes cookie/ambient (login epic), CSWSH becomes full terminal access; the origin-pinning story is incomplete for the endpoint carrying live terminal I/O → major (contested).
**Amendment:** §1: the CP MUST validate the WS-upgrade Origin against the same allowlist as CORS (replacing `*`); state the WS auth mechanism (in-band token bind) since headers are unavailable; fix the client to dial the configured CP origin.

### WM19 — The specced strict CSP breaks the actual bundle (no font-src, runtime <style> injection, inline theme script), inviting a silent style-src 'unsafe-inline' regression
**Sections:** S1 (build & integrity, CSP), D3
**Claim:** `default-src 'none'; script-src 'self'; style-src 'self'` with no `unsafe-inline` breaks the SPA three ways: no `font-src` blocks every webfont; sonner (and per-panel diagram/xterm) inject `<style>` at runtime; `index.html` ships an inline theme-bootstrap script.
**Evidence that survived:** Spec lines 43-49; `fonts.ts:13-17` + vendored Hack `@font-face`; built `index.html` inline IIFE; built CSS has zero `data-sonner` rules (injected via `createElement("style")`); xterm.js dynamic style injection sites (terminal core) cannot be hash-allowlisted. (Mermaid is not actually reachable in the current bundle — that sub-claim is wrong — but xterm strengthens the overall point; the CI Playwright gate catches breakage pre-prod, which reinforces the unsafe-inline pressure.)
**Impact:** Shipping the CSP as written breaks terminal fonts and toasts; the quiet fix (`style-src 'unsafe-inline'`) reopens a CSS-injection channel inside the origin holding the live device key → major.
**Amendment:** Enumerate the final CSP after running it against the real bundle: add `font-src 'self'`; bundle sonner's CSS (or swap the dep); cover terminal/dynamic styles with nonces/hashes or document a scoped, deliberate `style-src` relaxation; move the theme script to an external file or pin a script hash. Require the CI gate to exercise fonts, toasts, and one highlight/diagram block.

### WM20 — Signing gates publication, not the build: a compromised npm dependency is signed, deployed, CSP-compliant, and exfiltrates plaintext to the CP
**Sections:** S1 (build & integrity, release gating), D3, D6
**Claim:** cosign signs whatever `vite build` emitted; a compromised package injects ordinary `'self'` bundle code (no eval/inline) that the CSP gate and signature both pass. `connect-src` is pinned to CP + AS — exactly the declared custody adversary (D6) — so malicious code unseals with the live device key and POSTs plaintext inside normal RPC traffic. The residual-trust sentence names only "the static host and the CI signing path," not the transitive npm tree; no lockfile-frozen install is required.
**Evidence that survived:** Spec lines 41-55 (no build-input controls), line 54, 43-47, D6; `package-lock.json` exists but `npm ci` is never required in the release workflow; prior roast C2 (malicious SPA JS uses the live key — non-extractability stops key theft, not plaintext theft). xz/event-stream succeed against honest CI, so this is inside the stated trust set's blind spot. (One vote rated minor: the proposed `npm ci`/provenance fixes don't prevent a lockfile-recorded version-bump compromise; value is documentation + hygiene.)
**Impact:** One malicious transitive dependency voids web custody for every enrolled user with all W1 mechanisms green; the exfil channel is explicitly allowed by the spec's own CSP → major (contested).
**Amendment:** §1: `npm ci` from a committed lockfile only; a dependency-update/provenance policy; rewrite the residual-trust sentence to state the full dependency tree is trusted at build time and that signing attests publication provenance, not build integrity. Consider SLSA provenance attestation.

### WM21 — W1's central security mechanism is unfalsifiable: no test that the deploy gate refuses an unsigned/wrongly-signed artifact, and keyless verification is easy to misconfigure silently
**Sections:** S1 (release gating), S4 (testing)
**Claim:** §4's W1 coverage is only "the suite runs against the CSP-enforced prod build" — no test exercises the refuse branch (unsigned dist/, wrong workflow identity, wrong issuer). cosign keyless verification is easy to misconfigure silently (unanchored `--certificate-identity-regexp`, swallowed exit codes).
**Evidence that survived:** Spec §4 lines 130-140 (no release-gate test) versus its own explicit device-set tamper/fork/stale-head tests; §1 does not pin the verify invocation's exact identity/issuer. (The "omitting `--certificate-oidc-issuer` passes everything" claim is dated for cosign ≥2.0, which errors loudly; but unanchored-regexp, verify-wrong-artifact, and swallowed-error modes remain.)
**Impact:** The load-bearing W1 mechanism gating all web custody can be a silent no-op from day one with no signal → major.
**Amendment:** Add a pipeline self-test on every release: verify MUST fail for (1) the artifact with its signature stripped and (2) a fixture signed by a different identity; assert the verify invocation pins exact `--certificate-identity` and `--certificate-oidc-issuer` (no regexp).

## Minor notes

Panel-confirmed but downgraded to minor:

- **WL1 — "Refuses to operate on any extractable key" contradicts the in-page recovery-device derivation; "wiped from memory" is unimplementable in JS** (S2, D5). The HKDF-Expand-only + bespoke P-256 derivation forces raw key bytes through userland JS, and the mnemonic transits immutable GC'd strings. *Fix:* scope `extractable===false` to persistent device keys; spec the ceremony/recovery exception (userland derivation in zeroable ArrayBuffers, imported non-extractable immediately, best-effort wipe); disable autocomplete on mnemonic inputs; state honestly that mnemonic-derived keys are extractable-by-construction while in use.
- **WL2 — No anti-rollback or cache policy** (S1, D3). cosign signatures verify forever; host dashboard rollback/preview deployments bypass the gate; `index.html` cache lifetime unspecified. All attack actors sit inside the stated residual trust. *Fix:* monotonic release counter / digest allowlist at deploy; govern dashboard rollback and preview deployments; mandate `Cache-Control: no-cache` on `index.html` and `immutable` on hashed assets.
- **WL3 — Nothing binds the signed artifact to the prod configuration** (S1, D2/D6). Keyless OIDC identity captures neither build mode nor inputs, and the tested bundle is not digest-bound to the deployed one. The strict CSP is an origin header (not bundle-carried) and `DEV_TOKEN` is a D6 deferral, so the headline impacts are overstated. *Fix:* build once, digest-bind test→sign→deploy; add a pre-sign forbidden-value scan (localhost origins, unsafe-* CSP).
- **WL4 — The SRI/integrity story is overstated** (S1, D3). `index.html` (the hash root) is unverified and host-mutable, dynamic `import()` chunks carry no integrity, module subresources are not transitively checked, and the CSP/headers storage location is unspecified. The host is already a stated residual trust, so this is precision/hygiene. *Fix:* cover all emitted chunks (import-map integrity or `inlineDynamicImports`/`modulepreload`), or delete the SRI claim from D3 and state browser-side integrity = TLS + trusted host + CI gate; mandate `_headers` inside the signed `dist/`; state SRI does not protect against host compromise.

Unverified (not run through the full panel):

- **WL5 — localhost-dev origins baked into the production CP CORS allowlist** (S1): latent credentialed-CORS hole that activates when the login epic moves off pure bearer tokens. *Fix:* allow localhost only on dev-mode CP instances; note the constraint for the login epic.
- **WL6 — AS-side CORS and browser-facing wire shape unspecified** (S1, S2): every §2 device-set RPC is a browser→AS call but only CP CORS is pinned, so the entire W2 slice is blocked by the browser as specced. *Fix:* mirror the CP rule for the AS; specify the AS RPCs' wire shape (Connect vs HTTP) and cross-origin auth.
- **WL7 — Lazy ceremony interrupts the first secret-bearing action with no defined resumption and no decline-to-node-local path** (S2, D5). *Fix:* park and auto-resume the triggering action on ceremony completion; offer an explicit "use node-local instead" decline where valid.
- **WL8 — `ListMigrationTargets` attributes "tenancy/class" eligibility to `TargetEligible`, which is tenancy-only and single-node in shipped code** (S3). *Fix:* spec a new registry enumeration method (owner-eligible + cloud nodes with class/online), keep `TargetEligible` as the single-node pre-suspend gate, define the picker as advisory with MigrateSpawn re-checking at placement.

## Refuted by panel

- **Move-to modal phase ordering is unimplementable against shipped MigrateSpawn** — refuted (2-1). In the shipped owner-sealed flow the target's journal *restore* happens after key delivery, so the spec's phase order (suspend → re-seal → restore → active) matches the real data flow when phase 2 runs after the blocking RPC returns; `GetJournalKeyCiphertext` has no liveness dependency; ciphertext persists at the CP so a closed tab strands nothing. Only the §3 "terminal state" wording conflates modal phases with wire statuses — a clarification, not a contradiction.
- **Recovery-phrase virtual device is an unrevocable/unrotatable standing skeleton key** — refuted (3-0). OwnerRoot anchors only the immutable genesis co-signers, not ongoing membership; `RemoveDevice`/`AddDevice` rotate the recovery device within the same chain with no new genesis, and parent §4 explicitly specifies recovery-code rotation (fresh DEK + re-seal). The surviving residual (old envelopes openable by a removed key) is the already-amended prior-roast M2.
- **W3 revoke wording regresses M2/M10 (permits same-DEK re-wrap)** — refuted (2-1). The envelope format has no same-DEK re-wrap operation; `seal.go` mints a fresh DEK on every `Seal`, so "unseal → re-seal to the new set" is necessarily fresh-DEK re-encryption. Version monotonicity is AAD-bound (not a CP duty), and the parent design's normative removal procedure remains in force; the only residue is restating disclosure copy.
- **Device-set registry "stored at the AS" is unreconciled with a CP-side mirror that bricks revocation re-seals** — refuted (2-1). The `PutJournalKeyCiphertext` guard self-skips for an owner with no CP-registry devices, `MemDeviceRegistry.Enroll` has zero production callers, and `SealToOwner` has no production call site — the bricking and injection paths are unreachable in shipped code; "stored at the AS" is the parent design's already-agreed M4 decision.
- **cosign-keyless deploy gate is circular (repo-write compromise ships signed malicious JS)** — refuted (2-1). The spec explicitly names "the static host and the CI signing path" as residual trust, so repo-write compromise is a compromise of a *declared-trusted* component (true of spawnctl too); W1 implements roast C2 (remove the untrusted CP from JS delivery), which it does. Stronger non-circular remedies (transparency manifest, browser-side verification) are consciously deferred in D3. Worth a one-line implementation note (Environment-scoped deploy credential), not a design flaw.

## Amendment checklist

| Finding | Spec section to amend |
|---|---|
| WM1 | S2 (device-set registry append RPC, D4) |
| WM2 | S2 (enrollment/revoke re-seal) + CP non-member guard |
| WM3 | S3 (errors) / D7; spec the M8 delivery sub-protocol |
| WM4 | S2 (enrollment), D4 |
| WM5 | S2 (enrollment anchor), D4, D6 |
| WM6 | S2 (device-set registry), D6; sp-ova §9 |
| WM7 | S2 (enrollment, device management) |
| WM8 | S1 (endpoint config), S2 (crypto substrate), S3 (phase 2) |
| WM9 | S2 (crypto substrate, registry), S4 (interop suite) |
| WM10 | S2 (lazy ceremony), S3 (phase 2), S4 (testing) |
| WM11 | S2 (crypto substrate, lazy ceremony, device management), D4, D5 |
| WM12 | S2, D5 (recovery flow scope) |
| WM13 | S3 (phase 2 sequencing), S2, D4 |
| WM14 | S3 (errors), D7; transient-tier §5/T.11 |
| WM15 | S2 (device management W3, signed entry body) |
| WM16 | S3 (data-class behavior); transient-tier §5/phase ④ + MigrateSpawn guard |
| WM17 | S1, S2, D2, D6, S4 (sequencing) |
| WM18 | S1 (origin model, WS upgrade) |
| WM19 | S1 (CSP), D3 |
| WM20 | S1 (build & integrity, release gating), D3, D6 |
| WM21 | S1 (release gating), S4 (testing) |
| WL1 | S2 (crypto substrate), D5 |
| WL2 | S1 (release gating), D3 |
| WL3 | S1 (build & integrity, release gating), D2/D6 |
| WL4 | S1 (build & integrity), D3 |
| WL5 | S1 (origin model) |
| WL6 | S1, S2 (AS CORS + wire shape) |
| WL7 | S2 (lazy ceremony), D5 |
| WL8 | S3 (target picker) |