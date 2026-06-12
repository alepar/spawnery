# Auth & Identity: GitHub Login, AS-Signed Sessions, Node-Verified Intents

**Date:** 2026-06-11 (amended 2026-06-12 with the
[adversarial review](2026-06-12-auth-identity-adversarial-review.md)'s 1 critical + 13 majors +
3 panel-minors — markers `[AC1]`/`[AMx]`/`[MCx]`; unverified minors folded inline)
**Beads:** sp-ussy (epic), sp-ussy.1–.5 (A1–A5)
**Builds on:** [E4 Identity & Secrets](2026-05-28-spawnery-e4-identity-secrets-design.md) (§1/§2 —
vault superseded by owner-sealed device keys), [Node Auth & Unified
Identity](2026-06-05-node-auth-unified-identity-design.md) (§7a session authority),
[Web Epic](2026-06-11-web-epic-spa-delivery-device-keys-migration-design.md) (WM17 gate)

## Problem

The stack authenticates users with a compiled-in shared dev token mapping every caller to one
principal. This epic ships real identity: GitHub login at the AS, AS-signed sessions verified
offline by the CP **and by nodes**, client proof-of-possession on node-verified operations, and
the answer to the web epic's open question: **device-set logs are keyed on `accountId`** (as a
storage/lookup key only — trust remains the OwnerRoot chain `[MC3]`).

## Decisions

| # | Decision | Alternatives rejected |
|---|----------|----------------------|
| A-D1 | **Real OAuth in one epic, GitHub-only at launch** | Interim invite tokens; multi-provider launch |
| A-D2 | **AS is the IdP**: OAuth at the AS, AS-signed tokens, CP/nodes verify offline against pinned key-sets | CP as OAuth client (CP must not mint sessions) |
| A-D3 | **Hybrid session custody**: bearer access token in SPA memory; HttpOnly refresh cookie on the AS, **same-site with the SPA** `[AM2]`, with `/refresh` requiring **session-key PoP** `[AM5]`. Residual acknowledged: `/refresh` is the design's one credentialed-CORS endpoint, strictly origin-allowlisted | All-cookie (ambient CP auth re-opens WM18); localStorage refresh |
| A-D4 | **Open registration with a kill switch**; capacity at the scheduler layer (sp-z6us) | Invite gating; allowlist |
| A-D5 | **Both verification layers now**: CP + node sessions, node-verified **SignedIntents** `[AC1]` | CP-layer only |
| A-D6 | **Bespoke signed-protobuf tokens, not JWT** — one signing algorithm hardcoded; a **`key_id` is key *selection*, not algorithm agility, and is included** `[AM4]`. Exactly two client-visible algorithms system-wide: Ed25519 (AS signing), ECDSA P-256 (client cnf keys, ALL clients incl. spawnctl) `[AM11]` | JWT (alg agility, JOSE layering); kid-less single-pin (fleet-breaking rotation) |
| A-D7 | **cnf-bound tokens + SignedIntents on node-verified ops only**; CP-local calls bearer | Full DPoP everywhere (the CP is the verifier there); pure bearer |

## §1 Identity model

AS-owned user store (sqlite, store-driver pattern):
`{ accountId (UUID), githubSub, handle, status, createdAt }`.

- **`githubSub` := GitHub's immutable numeric user id** (int64) — **never `login`**, which is
  mutable and re-registrable (recycled-username takeover) `[AM9]`. `handle` (from `login`) is
  **display-only**: nothing keys, links, or authorizes on it; collisions/renames are cosmetic.
- `accountId` is the universal key: CP `owner_id`, device-set log key, node tenancy, enrollment
  scoping. Providers are linked credentials (Google later, by subject).

## §2 Login flows

**Web SPA:** auth-code + PKCE against the AS; the AS↔GitHub leg is a **confidential client**
(client_secret + `state`; GitHub also supports PKCE — use it, but the secret is the load-bearing
credential) `[AM9]`.
- **CSRF/fixation `[AM8]`:** per-request `state` on the client↔AS leg with an explicit SPA-side
  check on return (carrying the original SPA route for post-login restore); the AS binds its
  GitHub callback to the browser session that initiated it (server-side state row) — a forged or
  injected callback cannot graft an attacker's code onto a victim's flow. `redirect_uri` is
  **exact-match registered**, with the RFC 8252 §7.3 loopback variable-port allowance as the
  *only* relaxation.
- **Callback-time failures** (registration closed, `access_denied`, PKCE mismatch) return
  structured error redirects to the SPA, which renders them — never a bare AS error page.
- **GitHub App** registration (E4 dual-duty), with the recorded caveat that user-to-server
  tokens expire (~8h + refresh) — relevant to E3, not to login (we discard GitHub's token after
  `GET /user`).

**spawnctl:** `spawnctl login` — loopback PKCE (browser → AS → `127.0.0.1:<random>`).
**Headless = a real RFC 8628 device grant `[AM7]`**, not a paste-a-code OOB handoff: spawnctl
POSTs its **session pubkey** to the AS device-authorization endpoint → receives
`{device_code, user_code, verification_uri}` → the **user enters the code AT the AS** in an
authenticated browser that displays full account context → spawnctl polls the token endpoint.
`user_code` short-TTL, rate-limited. (The direction matters: the user confirms at the AS;
nothing transferable is ever pasted *into* the requesting machine's channel.)

**Automation:** one interactive bootstrap + the sliding refresh family sustains indefinite
single-host automation (the gh/gcloud model). Do not seed rotating refresh tokens into
ephemeral CI; broader delegation is sp-3rtm.

**Dev mode:** `CP_DEV_TOKENS` on dev-mode CP only — but see §5 dev-parity `[AM12]`: dev clients
still mint session keys and sign intents.

## §3 Tokens

**Access token** — signed protobuf, **15-minute TTL**:

```protobuf
message SessionTokenBody {
  string account_id       = 1;
  string handle           = 2;
  string token_id         = 3;
  string audience         = 4;  // "cp" | "node" [MC2] — split: node-relayed tokens are aud=node,
                                // REJECTED by the CP interceptor; aud=cp never leaves CP-bound use
  int64  issued_at        = 5;
  int64  expires_at       = 6;
  bytes  session_key_hash = 7;  // SHA-256 over the DER SPKI exactly as exportKey('spki') /
                                // x509.MarshalPKIXPublicKey emit [AM11]
  string key_id           = 8;  // AS signing-key selector [AM4]
}
// wire: RawURLEncoding (unpadded) base64url(body) "." base64url(sig)  [MC1]
// sig = ed25519( "spawnery/session-token/v1" || body_bytes )  — every AS-signed artifact class
// carries a mandatory domain-separation prefix [MC1]
```

Verifiers check the signature over the exact received body bytes, then parse.
`internal/sessiontoken` (the legacy CP-signed prototype) is **superseded and deleted in A4**
`[MC1]`.

**Key rotation `[AM4]`.** Verifiers (CP + nodes) hold a small **ordered set** of pinned AS
session pubkeys keyed by `key_id` (current + next). Routine rotation: pre-publish next key to
configs → overlap window (both valid) → AS switches signing → retire old. **Emergency
(compromise) path:** a replacement statement signed via the enrollment-pinned **PKI chain** —
this is the one deliberate carve-out from the role-separation rule, verified against the pinned
root, deliverable through the untrusted CP. Rotation overlap is hermetically tested.

**Refresh token** — opaque 32 random bytes; session row keyed by hash:

```
refresh_sessions(token_hash PK, account_id, family_id, client_kind web|cli,
                 session_pubkey_spki,            -- [AM5] PoP verification material
                 created_at, last_used_at, expires_at,   -- 30d sliding
                 family_created_at,               -- absolute family max age: 90d [AM6]
                 superseded_by NULL, successor_cache NULL, revoked)
```

**`/refresh` semantics:**
1. **PoP required `[AM5]`:** the request carries a fresh session-key signature over
   (refresh-token-hash, timestamp, nonce), verified against `session_pubkey_spki` *before*
   rotation. A stolen cookie alone is now actually useless — and CSRF-triggered rotation dies
   with the same stroke. (Threat shape corrected: without this, a cookie thief held silent
   CP-plane control for 30 days.)
2. Reject revoked / past sliding expiry / past **90-day family max age** `[AM6]`.
3. **Bounded grace `[AM3]`:** presenting the *most recently* superseded token within ~45s
   returns the **same cached successor pair** (idempotent replay, keyed off `successor_cache`) —
   honest races (two tabs, lost response, parallel spawnctl) no longer kill the family. Reuse
   outside the window or ≥2 generations old → revoke family, log (the theft signal stays
   meaningful because false positives are gone).
4. Mint new access + refresh; stamp `superseded_by`; slide the window.

**Client single-flight `[AM3]`:** SPA serializes refresh via Web Locks (BroadcastChannel
fallback); spawnctl takes an advisory file lock + atomic-rename on the state file; both add
±jitter to proactive refresh. **CLI residual (recorded):** the state dir holds both the cnf key
and the refresh token — one directory read is full compromise until family expiry; OS-keychain
storage is a tracked improvement, not MVP.

**Logout `[AM10]`:** AS `/logout` = revoke family + expire the cookie (Set-Cookie). "Sign out
everywhere" = revoke all families — **and propagates** (§5).

**Cookie & CORS contract `[AM2]`:** AS and SPA MUST share a registrable domain not on the PSL
(e.g. `auth.spawnery.dev` / `app.spawnery.dev`) — cross-*site* placement breaks silent refresh
under Safari ITP / Firefox TCP, a browser-selective perpetual login loop. Cookie:
`Secure; HttpOnly; SameSite=Strict; Path=/refresh` (host-only). `/refresh` enforces a strict
Origin allowlist; AS serves credentialed CORS (exact-origin ACAO + ACAC:true) for exactly the
canonical SPA origin on `/refresh` + token endpoints — an explicit **A1 deliverable** (the web
epic's WL6 covered bearer RPCs only). Playwright covers cross-origin `/refresh` including the
cold-reload path.

**AS-offline behavior** (unchanged from v1, recorded): running spawns + mTLS unaffected; valid
access tokens verify; open sessions survive; interactive access decays over 15 min; recovery is
silent re-auth. **Abuse controls:** per-IP/account rate limits on `/authorize`, `/refresh`,
registration, device-grant `user_code` attempts; cap concurrent families per account. Recorded:
new-device login is GitHub-gated (their outage = no new logins) while existing-session refresh
and revocation are not.

## §4 CP integration

`internal/cp/auth` swaps the token map for offline verification: signature (key-set by
`key_id`), expiry, **`aud == "cp"`** `[MC2]` → `WithOwner(ctx, accountId)`. WS keeps the
in-band bind through the same `auth.Owner` seam. The AS session pubkey set arrives via pinned
config, never fetched from the AS. Bookkeeping corrections: **WM18 (WS-upgrade Origin
allowlist) is owned by web-epic W1**, not this epic; CP CORS finalization (A2) covers bearer
RPCs — the credentialed `/refresh` contract is A1's (§3).

**Revocation propagation `[AM10]`:** family/account revocation emits an AS→CP signed revocation
event (short-poll fallback); the CP terminates open WS sessions and in-flight relays bound to
revoked `token_id`s. Live sessions additionally **re-present a current token in-band every
~15 min** (aligned with refresh); failure to re-present closes the session. "Sign out
everywhere" now actually severs the attacker's open terminal — the stolen-laptop response is
real, not cosmetic.

## §5 Node-verified sessions + SignedIntents

**Client session keypair.** **ECDSA P-256 for ALL clients** (web: non-extractable WebCrypto in
IndexedDB; spawnctl: state-dir file) `[AM11]` — no per-client algorithm dispatch. Registered
with the AS at login; `session_key_hash` in every token; refresh family PoP-bound to it (§3).
- **Key-loss lifecycle `[AM6]`** (WM11's lesson applied to session keys): request
  `navigator.storage.persist()` at creation; on every session restore the SPA **positively
  verifies the key can sign (test signature) BEFORE calling `/refresh`**; key missing → revoke
  the family server-side and route to clean re-login (cheap by design — re-login is one GitHub
  redirect). Safari ≥7-day-absence re-login is the recorded accepted residual. Optional
  in-family key rotation (new pubkey signed by old) is specced for A5-later; **cnf-mismatch is a
  distinct error code** so clients drive recovery instead of presenting flaky-product bugs.
- **Multi-account:** single account per browser profile (second login supersedes the family);
  recorded, not engineered around.

**SignedIntent — the artifact `[AC1]`.** Per-operation signed protobuf:

```
SignedIntent {
  domain   = "spawnery/intent/<op>/v1"   // create-spawn | resume-spawn | recreate-spawn |
                                         // migrate-spawn | session-open ; domain-separated
  body     = { jti, issued_at,
               spawn_id, generation,     // CP-RESOLVED values echoed back (two-phase, below)
               target_node_id,           // explicit target binding — no cross-node fan-out
               op-specific params:       // create: app_ref + image DIGEST + model + mounts;
                                         // resume/recreate/migrate: data_ref/placement;
                                         // session-open: session_id }
  sig      = P-256( domain || body_bytes )
}
```

- **Two-phase sign-after-resolve:** the client submits the operation → the CP resolves and
  **echoes back the committed tuple** `{spawn_id, generation, target_node_id, image digest}` →
  the client validates it against its pended operation, signs, returns. (CreateSpawn's
  spawn_id/generation/placement don't exist before the CP acts — v1's "replay made inert by
  spawnId/generation binding" was **false as written**; this makes it true.)
- **Carriage + correspondence:** the exact signed bytes travel **verbatim** in a new auth
  envelope on `StartSpawn`/`SessionOpen` (token + SignedIntent + **full DER SPKI** — the cnf is
  a hash; the node needs the key `[AM11]`). The node verifies: AS sig on token → exp/aud=node →
  owner match → SPKI hashes to `session_key_hash` → intent sig over the received bytes →
  **field-by-field equality between the signed body and what it is about to execute**. Any
  CP-substituted parameter fails the correspondence check.
- **Freshness:** window = **90s**, skew budget = ±30s, both named constants; a node rejecting
  on skew returns its own time. jti cache covers the window **across restarts** (refuse intents
  predating process start). Machine-readable NACK codes thread back through Connect errors / WS
  close reasons; clients refetch the echoed tuple and retry once. Queued/rescheduled provisioning
  (detached goroutine today, capacity queue sp-z6us later) uses the **re-supply leg as a
  reusable mechanism**, not a migration one-off.
- **Recorded residual:** node auth-refusal signals flow through the untrusted CP (it can
  swallow/fabricate errors); nodes keep a local owner-readable refusal log surfaced via the
  direct verified channel later (follow-up, not MVP).

**Resume-leg re-sign — client-driven `[AM1]`.** No CP-solicited signing, ever (a client that
signs CP-supplied bytes is a signing oracle): the initiating client **pends** the operation
locally (spawnId, op, target, nonce), **polls** a pending-intent endpoint for the CP-committed
tuple, validates it against its pended record, signs single-use, submits. Clients MUST refuse
any solicitation not matching a locally pended operation. `MigrateSpawn` stays one blocking RPC
for the CLI (spawnctl runs the poll-and-sign loop inside it); the web modal polls naturally.

**Dev/prod parity `[AM12]`** (the sp-gzvo lesson — same Provision seam, two days prior): dev
clients **always** mint session keys and sign intents; the dev stack's fake provider/CP-boot dev
AS keypair mints cnf-bearing tokens so the full A4/A5 data path runs in `just dev`.
`NODE_AUTH_MODE=insecure` is redefined **verify-and-log-don't-enforce** — missing/garbage
signatures are loud in dev logs instead of skipped. A hermetic CP test asserts **all four
lifecycle handlers thread token + SignedIntent** into the node-bound StartSpawn.

**Proto additions enumerated `[AM11]`:** auth envelope (token, intent bytes, SPKI) on
`node/v1 StartSpawn` + `SessionOpen`; `session_id` inside the signed open (a compromised CP
must not rebind one signed open within the spawn); `generation` exposed on the client surface
(SpawnSummary) for tuple validation.

**Modes & self-hosted:** unchanged — self-hosted nodes keep `owner == NodeOwner`; cloud binds
the CP-asserted owner at StartSpawn (sp-ova §7a split model). Headless delegation: sp-3rtm.

## §6 Gating, rollout, durability

- `REGISTRATION_ENABLED` (AS, default on).
- **WM17 gate** satisfied when A1+A2+A5 are live.
- **Pre-A1 data is disposable `[MC3]`:** staging device-set chains/owners under dev-token string
  identities are wiped; owners re-run genesis after first real login; the hosted CP launches
  empty. No re-key procedure is built.
- **AS durability `[AM13]`** — the missing story, now specced: the AS sqlite is **tier-0 data**
  (users + device-set tables: losing githubSub→accountId orphans every accountId-keyed artifact
  platform-wide — CP rows, node-cert SANs, sealed-secret AADs; that's destruction, not a
  15-minute fuse). Continuous replication (litestream-class) + snapshot backups, **stated RPO
  ≤ 5 min**, and a rehearsed restore drill. Restore semantics vs the WM6 monotonic chain-head
  pin: a stale restore hard-fails pinned clients by design — the drill includes re-pin-from-
  enrolled-device guidance. **Signing-key custody:** session + PKI keys on disk 0600 under the
  AS user, offline escrow copies (same ceremony as the sp-ova root); the shipped
  generate-in-memory default is dev-only. Recovery matrix: {DB lost → restore from replication;
  key lost → AM4 emergency rotation; both → restore + rotation}. **Re-binding a fresh accountId
  to surviving artifacts is an explicit non-goal for MVP** — backups are the answer.

## §7 Slices & testing

Slices A1–A5 as before (A1 → {A2, A3} → A4; A5 after A2), with the roast deltas folded in:
A1 gains AS credentialed-CORS + `/refresh` PoP + grace + rate limits + durability/backup setup +
OAuth state/fixation defenses + RFC 8628 device grant; A2 gains aud=cp + key-set verification +
revocation-event intake; A3 gains single-flight file locking + device-grant polling; A4 gains
the SignedIntent envelope/correspondence/jti machinery + revocation-driven WS termination +
in-band re-presentation + the four-handler hermetic threading test + `internal/sessiontoken`
deletion; A5 gains key-presence self-check + Web Locks single-flight + pending-intent
poll-and-sign + cnf-mismatch recovery UX + first-run UX (hard login wall — **no anonymous RPCs,
stated**; `state` carries the original route).

**Testing additions** (on top of v1's): forged-callback + code-injection negatives `[AM8]`;
rotation-overlap (two valid keys) hermetic `[AM4]`; refresh-race suite — two concurrent
refreshes, lost-response retry, parallel CLI — all surviving via grace `[AM3]`; `/refresh`
without PoP refused `[AM5]`; family max-age + key-loss → clean re-login Playwright `[AM6]`;
SignedIntent correspondence negatives — CP-substituted image/target/generation each refused
`[AC1]`; cross-node + cross-restart jti replay refused `[AC1]`; revocation severs an open WS
`[AM10]`; cross-origin `/refresh` incl. cold reload `[AM2]`; Go↔TS vectors for intent bytes,
DER-SPKI hashing, P1363↔DER conversion `[AM11]`.

## Out of scope (tracked)

Google provider; account linking UI; cloud-capacity queue (sp-z6us); headless delegation
(sp-3rtm); relay payload encryption (sp-gtm); OIDC facade; OS-keychain CLI storage; in-family
key rotation (specced, deferred); node refusal-log surfacing.

## Roast disposition

AC1 + AM1–AM13 + MC1–MC3 amended above; 11 unverified minors folded inline (CLI state-dir
residual, queued-provisioning re-supply, first-run wall, handle semantics, callback errors,
multi-account, WM18/WL5 ownership corrections, refusal-signal residual, NACK codes + skew,
abuse controls). 3 findings refuted with reasoning — see the
[review](2026-06-12-auth-identity-adversarial-review.md); do not re-litigate (notably: GitHub
**does** validate PKCE since 2025-07 — the "GitHub ignores PKCE" claim is stale and must not be
re-introduced). Owner decisions: AC1 → two-phase sign-after-resolve; AM5 → PoP at `/refresh`;
AM10 → in-epic revocation propagation + re-presentation; AM13 → backups tier-0, re-bind
non-goal.
