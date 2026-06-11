# Auth & Identity: GitHub Login, AS-Signed Sessions, Node-Verified Intents

**Date:** 2026-06-11
**Builds on:** [E4 Identity & Secrets](2026-05-28-spawnery-e4-identity-secrets-design.md) (§1
identity model, §2 OAuth — vault sections superseded by owner-sealed device keys),
[Node Auth & Unified Identity](2026-06-05-node-auth-unified-identity-design.md) (§7a AS-signed
session authority), [Web Epic](2026-06-11-web-epic-spa-delivery-device-keys-migration-design.md)
(WM17 sequencing gate; device-set log keying)

## Problem

The entire stack authenticates users with a compiled-in shared dev token mapping every caller to
one principal — fine for `just dev`, fatal for the hosted CP+AS behind a public origin (web-epic
roast WM17). This epic ships real identity: GitHub login at the AS, AS-signed sessions verified
offline by the CP **and by nodes** (sp-ova §7a, both layers), client proof-of-possession on
node-verified operations, and the answer to the web epic's open question: **device-set logs are
keyed on `accountId`**.

## Decisions

| # | Decision | Alternatives rejected |
|---|----------|----------------------|
| A-D1 | **Real OAuth in one epic, GitHub-only at launch** (Google later via E4 link-by-subject) | Interim invite-token scheme (throwaway auth on the security path); multi-provider launch |
| A-D2 | **AS is the IdP** (per sp-ova): OAuth at the AS, AS-signed tokens, CP/nodes verify offline against pinned pubkeys | CP as OAuth client (E4 v1 — superseded; CP must not be able to mint sessions) |
| A-D3 | **Hybrid session custody**: short-lived bearer access token in SPA memory; HttpOnly refresh cookie scoped to the AS origin only | All-cookie (ambient auth re-opens WM18 CSWSH + credentialed CORS); localStorage refresh (XSS = durable takeover) |
| A-D4 | **Open registration with a kill switch** (`REGISTRATION_ENABLED`); capacity protection lives at the cloud-spawn scheduler layer, not identity | Invite-gated signup; manual allowlist |
| A-D5 | **Both verification layers now**: CP *and* node verify sessions; node additionally verifies StartSpawn-causing intents | CP-layer only (left "compromised CP forges sessions/spawns" open until sp-gtm) |
| A-D6 | **Bespoke signed-protobuf tokens, not JWT**: one algorithm (Ed25519) hardcoded both ends, signed raw bytes (WM9 discipline), zero third-party interop need | JWT (algorithm-agility surface, JOSE canonicalization layering, library baggage; revisit as an OIDC facade only if third parties ever verify our tokens) |
| A-D7 | **cnf-bound tokens + intent signatures on node-verified ops only**; plain bearer for CP-local API calls | Full DPoP on every request (signing CP-bound requests defends nothing — the compromised CP *is* the verifier); pure bearer (compromised CP forges StartSpawn/attach with harvested tokens) |

## §1 Identity model

The **AS owns identity**. New AS user store (sqlite, same store-driver pattern as the CP):
`{ accountId (UUID), githubSub, handle (from GitHub login, editable later), status, createdAt }`.
E4 §1 minus the vault fields.

**`accountId` is the universal key:** CP `owner_id` = accountId (only `internal/cp/auth`
changes — the CP is owner-id-agnostic by design); **device-set logs at the AS are keyed on
accountId** (closes the web epic's open question); node tenancy (`NodeOwner`) and enrollment
tokens already speak accountId. GitHub is a linked credential, not the key — additional
providers attach to the same account by OAuth subject, no migration.

## §2 Login flows

- **Web SPA:** OAuth 2.0 auth-code + PKCE against the AS (`/authorize` → GitHub → AS callback →
  token mint). The AS registers a **GitHub App** (E4 dual-duty: the same app later powers
  per-repo storage grants without re-consent).
- **spawnctl:** `spawnctl login` — native-app PKCE with loopback redirect
  (browser → AS → `127.0.0.1:<random>` callback → token exchange); headless fallback: paste a
  one-time code from the AS device page. Refresh token in the spawnctl state dir (0600).
- **Dev mode:** `CP_DEV_TOKENS` retained, valid only on dev-mode CP instances (also closes
  web-roast WL5: localhost CORS is dev-only). `just dev` needs no GitHub App or AS.

## §3 Tokens

**Access token** — compact signed protobuf (`proto/auth/v1`), **15-minute TTL**, verified
offline by CP and nodes against the pinned **AS session-token signing key** (a dedicated
Ed25519 keypair — deliberately separate from the AS's PKI intermediate; compromising one role
cannot mint the other's artifacts):

```protobuf
message SessionTokenBody {
  string account_id       = 1;
  string handle           = 2;
  string token_id         = 3;  // random per mint; log correlation
  string audience         = 4;  // "api" (CP + nodes)
  int64  issued_at        = 5;  // unix seconds
  int64  expires_at       = 6;  // issued_at + 15min
  bytes  session_key_hash = 7;  // "cnf": SHA-256 of the client session pubkey SPKI (§5)
}
// wire: base64url(body_bytes) + "." + base64url(ed25519_sig(body_bytes))
```

Verifiers check the signature over the **exact received body bytes** (WM9 raw-bytes
discipline), then parse.

**Refresh token** — pure capability handle: `base64url(32 random bytes)`, **no structure, no
signature, not offline-validatable** (its whole purpose — revocability — forces a DB lookup at
its single consumer, the AS; signing it would only create a second self-authorizing credential).
Server-side session row, keyed by hash:

```
refresh_sessions(token_hash PK, account_id, family_id, client_kind web|cli,
                 created_at, last_used_at, expires_at,  -- 30d sliding window
                 superseded_by NULL, revoked)
```

**`/refresh` semantics** — both artifacts replaced, never extended:
1. hash → row; reject if revoked/expired.
2. **`superseded_by` set = reuse detected** → revoke the entire family, reject, log. A stolen
   refresh token is a *detected event*, not a silent 30-day compromise.
3. Mint a new access token (fresh 15-min window) **and** a new refresh token (same
   `family_id`); stamp the old row `superseded_by`; slide the window.

One login = one family. Logout / "sign out everywhere" / admin = revoke family/families.
Web: refresh cookie is HttpOnly, AS-origin-scoped, used by exactly one endpoint.

**Threat shape (recorded):** the access token is the widely-exposed credential (every CP RPC,
in-band WS, relayed node-ward — including through a potentially compromised CP) and is
worthless in ≤15 min; the refresh token is the sensitive credential and is shown to exactly one
party, hashed at rest, one-time-use, theft-self-announcing.

**AS-offline behavior (recorded):** running spawns + node↔CP mTLS unaffected; valid access
tokens verify fine (pinned pubkey, no AS liveness dependency); open sessions survive (token
authorizes session *open*, SSH-style — not every frame). Interactive access decays to zero over
the 15 min after outage start (`/refresh` down); login/signup/device-set ops/fail-closed
revocation checks (M12) are down. Refresh tokens are not invalidated by an outage — recovery is
silent re-auth. The AS is on the interactive-availability path with a 15-minute fuse, never on
the running-workload path; it stays the smallest, cheapest-to-make-redundant component.

## §4 CP integration

`internal/cp/auth` swaps the token-map for offline verification (signature, expiry, audience) →
`WithOwner(ctx, accountId)`. Everything downstream (store, scheduler, registry, lifecycle, ws)
is untouched. WS keeps the in-band token bind, verified the same way — non-ambient, so WM18
stays closed. The AS session pubkey reaches the CP via pinned config (file/env), **never fetched
from the AS at boot** — verification must not depend on AS liveness. Prod CP accepts only
AS-signed tokens; `CP_DEV_TOKENS` only in dev mode.

## §5 Node-verified sessions + proof-of-possession intents

**What it closes** (sp-ova §7a): "compromised CP forges a session to my spawn / starts forged
workloads under my identity." The CP relays; it never vouches.

**Client session keypair (the "cnf" key).** At login, the client mints an ephemeral session
keypair — web: non-extractable ECDSA P-256 (WebCrypto, IndexedDB); spawnctl: state-dir file —
registers the pubkey with the AS, and every access token in that family carries
`session_key_hash`. The refresh family binds to the same key: a stolen refresh cookie alone is
useless. This key is **separate from device keys** — auth works for users who never run the
custody ceremony (M14 lazy); zero-ceremony, per-login-family.

**Access tokens are bearer; intents are not.** A bearer artifact authenticates its *holder*,
not its owner — a compromised CP harvesting tokens from the request stream can replay them for
≤15 min. Therefore the operations a **node** verifies require a live signature by the session
key (proof-of-possession), over the semantic intent bytes:

- **`SessionOpen`**: (spawnId, generation, issued_at, jti) — signed.
- **The four StartSpawn-causing intents** — `CreateSpawn`, `ResumeSpawn`, `RecreateSpawn`,
  `MigrateSpawn` — parameters signed by the client; the CP threads token + intent signature to
  the node leg.

Node verification, fully offline: AS signature on the token → `exp`/`aud` →
`token.account_id == spawn owner` → intent signature against the pubkey hashing to
`session_key_hash` → freshness window + per-node jti uniqueness (replay made inert by
spawnId/generation binding). Self-hosted nodes keep `owner == NodeOwner` on top (single-tenant
invariant — a lying CP is irrelevant). Cloud nodes bind the CP-asserted owner at StartSpawn
(§7a's split model: AS proves identity, CP routes, node enforces the match).

**CP-local API calls stay bearer-only** (A-D7): a compromised CP is the verifier there —
signatures on CP-bound requests defend nothing and tax everything.

**Long operations:** a migration's resume leg can land ~40 min after the originating RPC. The
client is already required live for the journal-key re-seal leg, so the CP requests a **fresh
token + intent signature from the connected client for the resume leg** (invisible via normal
refresh). Client gone → resume can't proceed → the web-epic WM3 revert-to-`suspended` leg —
correct: nothing starts without the owner.

**Key distribution:** the AS session pubkey joins the node enrollment bundle (next to the
pinned Root CA), config-overridable for rotation. Anchored at enrollment, never fetched from
the CP.

**Modes:** `NODE_AUTH_MODE=insecure` (dev) skips; `enforced` requires all of it.

**Recorded residual:** headless/scheduled flows (sp-3rtm delegation) have no live client to
sign intents — that delegation story remains its own deferred design. sp-gtm **narrows**:
intent signing moved here; what remains there is E2E relay *payload* encryption.

## §6 Gating & rollout

- `REGISTRATION_ENABLED` (AS, default on): off → existing accounts fine, new GitHub subjects
  get "registration closed."
- **WM17 gate**: formally satisfied when A1+A2+A5 are live on the hosted AS+CP; the web epic's
  public-DNS flip unblocks then.
- **Cloud-capacity limit/queue is out of scope** → scheduler bead (exists as
  `ResourceExhausted` today; queue is an enhancement on that seam).
- AS persistence: sqlite via the CP's store-driver pattern (users, refresh families, device-set
  chains).

## §7 Slices & testing

| Slice | Scope |
|---|---|
| A1 — AS identity core | user store; GitHub App OAuth (PKCE); session-token signing keypair; access mint; refresh families (rotation + reuse-kill); `/refresh`; registration switch |
| A2 — CP verification | offline verify in `internal/cp/auth`; WS in-band; prod/dev split; CORS finalization |
| A3 — spawnctl login | loopback PKCE + headless paste; state-dir tokens; transparent refresh |
| A4 — node layer | session pubkey in enrollment bundle; `SessionOpen` + four intents carry token + cnf signatures; node offline verification; resume-leg re-supply; `NODE_AUTH_MODE` gating; proto changes |
| A5 — web client auth | login screen + redirect handling; in-memory access token + silent refresh; session keypair; intent signing |

Order: A1 → {A2, A3} → A4; A5 after A2.

**Testing.** Hermetic Go: token mint/verify vectors; rotation + reuse-detection family-kill;
registration switch; CP interceptor; node verification negatives (forged AS sig / expired /
wrong owner / wrong cnf / replayed jti / stale freshness — all refused). An in-process **fake
OAuth provider** so login flows test without GitHub. Cross-language vectors (WM9 discipline):
intent-signing bytes + session-key SPKI hashing, Go ↔ TS. Playwright: login → spawn → attach
against the fake provider. Host-gated e2e: enforced node refuses an unsigned StartSpawn.

## Out of scope (tracked)

- Google provider (E4 link-by-subject, later); account linking UI.
- Cloud-capacity queue (scheduler bead).
- Headless/scheduled delegation (sp-3rtm).
- E2E relay payload encryption (sp-gtm, narrowed).
- OIDC facade for third-party token verification (only if ever needed).
