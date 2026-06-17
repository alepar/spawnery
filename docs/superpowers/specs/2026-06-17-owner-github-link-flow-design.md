# Owner-Facing GitHub Link Flow (web + spawnctl)

**Bead:** `sp-v40s.20` (epic `sp-dl62` — End-user GitHub mounts) · **Date:** 2026-06-17 · **Status:** draft

**Builds on / amends:**
[`2026-06-14-github-credentials-and-storage-unified-design.md`](2026-06-14-github-credentials-and-storage-unified-design.md)
(§3 Custody, §16 round-3 AS-custodial) and round-2 finding **F14**
([`2026-06-16-github-storage-backend-adversarial-review-r2.md`](2026-06-16-github-storage-backend-adversarial-review-r2.md)).
**This spec amends §16.2** (see Decision L1).

---

## 1. Problem

The AS-side GitHub-link machinery is **fully built and merged** on master (`6180cf6`,
`sp-v40s.10-.15`): `internal/authsvc/github_link.go` exchanges the GitHub App OAuth code,
holds the tuple, drives `GitHubLinks.Upsert` into AS-custodial storage, AS-custodial mint/refresh
runs with node-identity authZ, and the CP fans the access token out to nodes. But **nothing drives
it from the user side** — there is no web button and no `spawnctl` command, so an owner cannot
*establish* a `github-token` at all. This is the #1 blocker for end-user GitHub mounts.

This spec designs the owner-facing driver — the web SPA and `spawnctl` flow that lets an owner
**create / relink / revoke** a GitHub link — and reconciles the now-contradictory custody semantics
(§3 predates §16.2) so the flow fits the AS-custodial reality.

## 2. Main challenges

1. **Custody contradiction.** §3 has the owner seal the tuple as disaster-recovery (DR). §16.2 moved
   durable custody to the AS as *sole rotation authority*, and spike 3 proved rotation **invalidates
   the predecessor** refresh token. So any owner-held copy goes stale after the AS's first refresh
   (~8h) and — being CP-blind (sealed client-side) — neither CP nor AS can keep it fresh. The owner's
   "DR copy" therefore has no coherent operational job.
2. **Nonce / possession handle (F14, unresolved).** §3 step 4 hand-waved how the owner client learns
   to redeem the freshly-exchanged tuple "without the nonce landing in the URL / history / Referer /
   AS logs," and what binds that redemption to the right exchange.
3. **Web vs CLI mechanics.** A browser can do a top-level OAuth redirect; a CLI cannot. The built leg
   assumes an *ambient browser session* (302-redirect `authorize` + a `SameSite=Strict` cookie), which
   does not fit the SPA's real **Bearer** auth, and does not fit a CLI at all.

## 3. Key decisions made

**The AS is the sole custodian of the durable credential; the owner keeps nothing.** Because the AS
can always mint a fresh access token from the refresh token + `client_secret` (both AS-side), there is
no operational reason for the owner or CP to hold any token material. So: **no CP-side GitHub secret,
no owner DR copy.** Recovery from a lost/broken chain is **relink** (re-run OAuth); a lost AS keystore
means every link relinks. This dissolves challenge 1 and shrinks blast radius (the tuple never leaves
the AS, not even to the browser).

**A single in-memory `link_session` possession handle, unified across web + CLI.** An authenticated
`start` call returns `{authorize_url, link_session}`; the client holds `link_session` only in memory
(SPA: `sessionStorage`; CLI: process memory) — **never** in a URL, cookie, history, or Referer. Redeem
requires `link_session` **and** fresh owner auth (session/Bearer). This resolves F14 *by elimination*
(the handle never enters the browser) and replaces the built cookie/302 leg, fixing the
Bearer-vs-top-level-navigation mismatch.

**CLI mirrors `cmd/spawnctl/login.go`:** loopback (desktop) + device-poll (headless/SSH), auto-selected
exactly as login already does.

**Single default link per account for MVP** (the store stays `secret_id`-keyed, so multiple named links
is a later additive change).

## 4. Custody model (amends §16.2)

| Artifact | Where it lives | Durable? |
|---|---|---|
| Refresh token (the credential) | **AS only**, encrypted at rest | Yes — sole copy |
| Access token | Ephemeral; AS mints on demand, seals per-node, CP fans out (§16.4) | No |
| Owner / CP copy | **None** | — |
| Link metadata (login, github_user_id, host, version, …) | AS store; served read-only via `GET /github/links` | Yes |

Consequences:
- **No DR.** A lost AS keystore forces every user to relink. Explicitly accepted (relink is cheap; the
  refresh chain is intentionally single-homed). This **supersedes §16.2's "CP stores an owner-sealed
  copy of the credential for owner disaster-recovery / cross-device relink"** clause — there is no
  sealed copy; cross-device relink is served by account-bound AS endpoints instead (§7).
- **No tuple handoff to the client.** `redeem` returns **metadata only**. The access/refresh tokens
  never transit to the browser or CLI — strictly safer than the built endpoint, which returns the full
  tuple JSON (`github_link.go:317-330`).
- **Fanout unaffected.** §16.4's per-node sealed fanout (AS→node, CP relays opaque bytes) is a runtime
  delivery path, independent of the (now-removed) owner DR copy. No conflict.

## 5. Security spine — the `link_session` possession handle (resolves F14)

The two-step "commit only on a fresh authenticated request that proves possession of *this* exchange"
shape is kept; only the possession channel changes from a browser cookie to an in-memory handle.

```
1. start   (authenticated)  → AS mints account-bound `state` + `link_session`,
                              records client-kind {web | loopback:<port> | device},
                              returns {authorize_url, link_session}.    link_session: client memory only
2. (client opens authorize_url in a browser; user approves on GitHub)
3. callback (GitHub→AS, correlated by `state`) → AS exchanges code, stashes the pending tuple
                              keyed by `link_session`, bounces the browser to the client-kind landing.
4. redeem  (authenticated, body {link_session}) → AS checks the pending entry's account == caller,
                              single-use pops it, Upserts into AS custody, returns metadata only.
```

Properties:
- **`link_session` never appears in any URL, cookie, history, or Referer.** It is returned over the
  authenticated `start` response body and presented over the authenticated `redeem` request body.
  This is strictly stronger than the built `SameSite=Strict` cookie (which at least sits in browser
  storage). **F14 is resolved by elimination.**
- **Replay needs both factors.** A leaked `link_session` is inert without the owner's session/Bearer;
  a stolen `state` (used to drive a forged callback) yields only a *pending* entry bound to the
  owner's account that the attacker cannot redeem (no owner auth) and that the owner will not redeem
  (wrong/unknown `link_session`). The entry expires.
- `state`: high-entropy, single-use, account-bound, short TTL (existing `githubLinkStateTTL`).
- `link_session`: high-entropy, single-use, short TTL (replaces `githubLinkNonceTTL`).

## 6. Decision points, by section

### 6.1 AS HTTP surface (`internal/authsvc`)

Recommended: reshape the merged endpoints; retire the cookie/nonce machinery.

- **`POST /github/link/start`** (was `GET …/authorize`, 302-redirect). Authenticated
  (`githubLinkAccountFromReq`). Body/query: `secret_id` (defaulted for MVP), optional `host`,
  `client_kind` ∈ {`web`, `loopback`, `device`} (+ loopback `port`). Creates the account-bound `state`
  (storing PKCE verifier, secret_id, host, client_kind/landing) and a `link_session`; returns JSON
  `{authorize_url, link_session}`. *Why not keep the 302?* A top-level browser navigation cannot carry
  the SPA's `Authorization: Bearer` header; returning the URL lets the client navigate itself and works
  identically for the CLI.
- **`GET /github/link/callback`** (unchanged route; logic adjusted). Correlate by `state`, exchange
  the code, `FetchUser`, stash the pending tuple **keyed by `link_session`** (not a cookie nonce), then
  redirect the browser to the recorded landing: SPA settings page / loopback `http://127.0.0.1:<port>/done`
  / a device "you may close this tab" AS page. **No `Set-Cookie`.** `?error=` propagation unchanged.
- **`POST /github/link/redeem`** (unchanged route; input changed). Authenticated. Body `{link_session}`.
  Single-use pop the pending entry; verify `pending.accountID == caller`; `GitHubLinks.Upsert`; return
  **metadata only** `{secret_id, host, login, github_user_id, version, updated_at}`. Returns **`202`
  / `{status:"pending"}`** when the OAuth has not completed yet, so the device path can poll.
- **`GET /github/links`** (new). Authenticated, account-bound. Returns the account's link metadata
  (MVP: 0 or 1 row). Backs the web panel and `spawnctl gh status`. Needs a store
  `GitHubLinks().List(ctx, accountID)` (today only `Get(secretID)` exists).
- **`POST /github/link/revoke`** (unchanged). Account-bound kill switch (`DELETE /grant` + local
  fail-closed flip), already built.

Considered & discarded: **keep the web cookie, bolt CLI on separately** — two parallel possession
mechanisms to maintain, and the web leg keeps the latent ambient-auth assumption that breaks under
Bearer. Rejected for the unified handle.

Considered & discarded: **let `callback` finalize the Upsert directly** (drop client redeem) — removes
the fresh-auth + possession check, re-exposing the stolen-`state` attack (an attacker binds *their*
GitHub identity into the owner's link slot). Rejected; the redeem step is the security spine.

### 6.2 Web driver (`web/`)

New "Settings → GitHub" panel. Reads `GET /github/links` → renders "Linked as @login (vN)" with
**Relink** / **Revoke**, or "Link GitHub". Link/Relink:
`POST start` (Bearer, `client_kind=web`) → write `link_session` to **`sessionStorage`** → navigate
top-level to `authorize_url`. On return to the settings page → read `link_session` from `sessionStorage`
→ `POST redeem` (Bearer) → clear `sessionStorage` → re-fetch the panel. Surface `?error=`.

*Why `sessionStorage`:* a top-level navigation to GitHub wipes SPA JS memory, but `sessionStorage` is
per-origin and survives the round-trip, and never lands in a URL. It is script-readable (unlike the
HttpOnly cookie), but it is inert without the Bearer, and an XSS that could read it already holds the
Bearer — no net new exposure. No `secret_id`-keyed CP secret is created (Decision L1); the panel is
metadata-only.

### 6.3 spawnctl driver (`cmd/spawnctl/`)

Mirrors the loopback/device structure already proven in `login.go`.
- **`spawnctl gh link`** (default `secret_id`): `POST start` (Bearer) →
  - **loopback** (default when a browser is reachable): bind `127.0.0.1:0`, `client_kind=loopback`+port,
    open the browser to `authorize_url`; on the `/done` hit, `POST redeem` (Bearer, in-memory
    `link_session`); print "Linked as @login".
  - **device** (`--device`, or auto when headless / `--no-browser`): `client_kind=device`, print
    `authorize_url`; poll `POST redeem` (Bearer) with backoff until `200` (success) or terminal error.
  `link_session` lives only in process memory; it is never printed and never put in a URL.
- **`spawnctl gh status`**: `GET /github/links` → print.
- **`spawnctl gh revoke`**: `POST revoke`.

### 6.4 Multi-device (open question d)

Falls out of account-binding. Every endpoint authorizes by account; the AS `version` (monotonic in
`Upsert`) is the authority. Any owner-authenticated device may link / relink / revoke. No per-device
copies, no deviceset sealing, no CAS — there is nothing client-side to keep in sync.

### 6.5 Relink semantics

Relink = a fresh OAuth that `Upsert`s `version+1`, replacing the AS chain. The superseded chain is not
explicitly revoked (consistent with §16.5 — revoke is the compromise kill switch only). **Documented
MVP rough edge:** spawns already running against the prior version keep their minted access token until
it expires (~8h), then fail-closed on refresh (version superseded) and surface relink-required; they
recover on rebind/restart. Not solved here (a make-before-break relink is out of MVP).

## 7. Scope

**In:** the AS surface reshape (§6.1) incl. retiring the cookie/nonce and updating
`internal/authsvc/github_link_test.go`; the new `List` store query; the web panel (§6.2); the spawnctl
`gh link|status|revoke` commands (§6.3).

**Out (tracked elsewhere):** egress floor for the AS↔GitHub + node legs (`sp-u53.1.6`); AS-custodial
mint/refresh + CP fanout (built, `sp-v40s.10-.15`); spawn-start initial access-token delivery (runtime,
built/elsewhere); suspend backstop (deferred, §16.7); multiple named links per account (additive
later); make-before-break relink.

## 8. Testing

- **AS handlers** (hermetic, table-driven like `github_link_test.go`): `start` mints account-bound
  `state`+`link_session`; `callback` keys pending by `link_session` and sets **no cookie**; `redeem`
  enforces account-match + single-use + returns metadata-only (asserts **no token material** in the
  body) + `202` pending path; `List` is account-scoped; `revoke` unchanged. Assert `link_session`
  never appears in any redirect `Location`.
- **Web** (vitest): panel renders linked/unlinked from `GET /github/links`; link writes/reads
  `sessionStorage` and clears it; `?error=` surfaced.
- **spawnctl**: loopback `/done`→redeem and device poll→redeem, mirroring the `login.go` test shape;
  assert `link_session` is never logged/printed.

## 9. Decision Log

- **L1 — AS is sole custodian; no owner/CP copy (amends §16.2).** The durable credential (refresh
  token) lives only at the AS; no CP-side GitHub secret and no owner-sealed DR copy. Recovery = relink;
  lost AS keystore = relink-all. Supersedes §16.2's owner-sealed-DR-copy clause.
- **L2 — `redeem` returns metadata only.** Token material never transits to client; edit the merged
  endpoint to drop the tuple from its response.
- **L3 — Unified in-memory `link_session` possession handle** replaces the `SameSite` cookie + nonce;
  `authorize` 302 → authenticated `start` that returns `{authorize_url, link_session}`. Resolves F14 by
  elimination and fixes the Bearer-vs-top-level-nav mismatch.
- **L4 — Keep the two-step (start→…→redeem) security spine.** `callback` does not finalize; redeem
  requires fresh owner auth + the handle. Defeats the stolen-`state` link-hijack.
- **L5 — CLI = loopback + device**, auto-selected, mirroring `cmd/spawnctl/login.go`.
- **L6 — New account-bound `GET /github/links`** is the single source of truth for the UI/CLI; no
  CP-side metadata mirror.
- **L7 — Single default link per account for MVP**; store stays `secret_id`-keyed for later
  multiplicity.
- **L8 — Multi-device falls out of account-binding**; AS `version` is authority; no client-side sync.
- **L9 — Relink does not revoke the prior chain**; running-spawn staleness on relink is a documented
  MVP rough edge.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
