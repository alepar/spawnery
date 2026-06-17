# Owner-Facing GitHub Link Flow (web + spawnctl)

**Bead:** `sp-v40s.20` (epic `sp-dl62` — End-user GitHub mounts) · **Date:** 2026-06-17 · **Status:** draft

**Builds on / amends:**
[`2026-06-14-github-credentials-and-storage-unified-design.md`](2026-06-14-github-credentials-and-storage-unified-design.md)
(§3 Custody, §16 round-3 AS-custodial) and round-2 finding **F14**
([`2026-06-16-github-storage-backend-adversarial-review-r2.md`](2026-06-16-github-storage-backend-adversarial-review-r2.md)).
**This spec amends §16.2** (see Decision L1).

> **Revision r1 (2026-06-17), post-roast (BLOCK → revised).** A `superpowers:roast` pass found the
> original "unify on an initiator-held in-memory `link_session`, retire the SameSite cookie" spine
> **unsafe**: the retired cookie was *channel-bound to the OAuth-completing browser*, and moving the
> handle to the initiator opened credential-capture/identity-injection (§5 A1/A2). r1 **binds the
> redemption handle to the OAuth-completing context** (cookie for web, loopback `rc` for CLI) and keeps
> the AS-custodial custody decisions (L1/L2).

> **Revision r2 (2026-06-17), post re-roast (BLOCK → revised).** A focused re-roast confirmed the r1
> completer-binding spine is sound but found a dense set of **mechanization/precision gaps the reshape
> introduced** (no design reversal). r2 mechanizes them: `flow_id` is strictly device-only at redeem
> (web/loopback require the completer secret *in addition*); the OAuth `state` correlator is split from
> a `flow_id`-keyed flow record with an explicit reaper; the web `redeem` route uses `corsCredentialed`
> (not `corsBearerSimple`, which omits `Allow-Credentials`); the version bump + `deliveryID` are DB-side
> (`RETURNING`) and survive revoke→relink; identity-continuity is a peek-before-pop `confirm_switch`
> gate; the prefetch-DoS/ERROR interaction is resolved (only user-denial is terminal); `secret_id` is
> committed to account-derived; and the A1 device residual is re-characterized honestly (`client_kind`
> is attacker-chosen, so A1 = App phishing, not "closed for web/loopback"). This was the roast cap
> (iteration 2). The re-roast's "S2 (relink token invalidation)" escalation was **already answered by
> spike `sp-v40s.3`** (relink does not invalidate predecessors) and is folded into §6.5; the only
> genuinely open empirical items are spikes **S1** (web CORS/cookie) and **S3** (shared-host loopback).

---

## 1. Problem

The AS-side GitHub-link machinery is **built and merged** on master (`6180cf6`, `sp-v40s.10-.15`):
`internal/authsvc/github_link.go` exchanges the GitHub App OAuth code, holds the tuple, drives
`GitHubLinks.Upsert` into AS-custodial storage; AS-custodial mint/refresh runs with node-identity
authZ; the CP fans the access token out to nodes. But **nothing drives it from the user side** — no web
button, no `spawnctl` command — so an owner cannot *establish* a `github-token` at all. This is the #1
blocker for end-user GitHub mounts.

This spec designs the owner-facing driver — web SPA and `spawnctl` — that lets an owner **create /
relink / revoke** a GitHub link, and reconciles the now-contradictory custody semantics (§3 predates
§16.2) so the flow fits the AS-custodial reality.

## 2. Main challenges

1. **Custody contradiction.** §3 has the owner seal the tuple as DR. §16.2 made the AS the *sole*
   custodian + rotation authority, and rotation invalidates the predecessor (spike 3), so any owner-held
   copy goes stale within ~8h and cannot be refreshed by CP/AS. The owner's "DR copy" has no coherent
   operational job.
2. **Who may redeem (F14 + roast).** The handoff between the GitHub-initiated callback and the owner's
   commit must be bound so that (a) the redeemed credential belongs to the GitHub identity the owner
   *intended*, and (b) a third party can neither capture a victim's credential nor inject their own
   identity into the owner's link. The binding should not leak the handle to URL/history/Referer/logs
   (the web cookie meets this fully; the loopback `rc` rides a `127.0.0.1` URL and relies on the
   compensating controls in §6.3 — an explicit, bounded carve-out, not a violation).
3. **Web vs CLI mechanics.** A browser can do a top-level OAuth redirect; a CLI cannot. The merged leg
   assumes an *ambient browser session* (302 `authorize`) which does not fit the SPA's **Bearer** auth,
   and does not fit a CLI at all.

## 3. Custody model (amends §16.2)

| Artifact | Where it lives | Durable? |
|---|---|---|
| Refresh token (the credential) | **AS only**, encrypted at rest | Yes — sole copy |
| Access token | Ephemeral; AS mints on demand, seals per-node, CP fans out (§16.4) | No |
| Owner / CP copy | **None** | — |
| Link metadata (login, github_user_id, host, version, status) | AS store; served via `GET /github/links` | Yes |

- **No DR.** A lost AS keystore forces every user to relink. Explicitly accepted. **Supersedes §16.2's
  owner-sealed-DR-copy clause** — there is no sealed copy; cross-device relink is served by account-bound
  AS endpoints (§6.4).
- **No tuple handoff to the client.** `redeem` returns **metadata only**; the merged endpoint's full
  tuple emission (`github_link.go:313-330`) **must be removed in the same edit** (L2).
- **Fanout unaffected.** §16.4's per-node sealed fanout is independent of the (removed) owner DR copy.

## 4. Single default link & account-derived `secret_id`

MVP exposes **one GitHub link per account**. The link's `secret_id` is **server-derived from the
authenticated account** (`gh:<accountID>`); `start` **ignores any client-supplied `secret_id`** and
computes it from the caller. This makes a cross-account collision **structurally impossible** (the
durable store stays `secret_id`-addressable, so multiple named links per account remain an additive
change later, and the node mint path — which carries `secret_id` but no account identity,
`github_mint.go` — is unchanged; a composite `(account_id, secret_id)` key is **not** an additive
alternative and is rejected for MVP).

A redeem-time **ownership guard** (`Upsert` must reject when an existing link row's `AccountID != caller`)
is kept as **defense-in-depth** — it mirrors the guard `revoke` already has (`github_link.go:376`) and
covers the merged code's latent cross-account-overwrite path (file a fix bead). Under account-derived
ids the guard is normally unreachable; the relevant §8 test asserts **`start` derives the caller's own
id and ignores a supplied foreign id** (not a "reject B's id" path, which the derivation forecloses).

## 5. Security spine — handle bound to the OAuth-completing context

**Principle:** the secret that gates `redeem` is minted **at the callback** (post-OAuth) and delivered
**only to the context that completed the GitHub OAuth** — never to the initiator. This restores the
channel-binding the merged SameSite cookie provided and that the pre-roast initiator-held handle
destroyed.

### 5.1 Data model — two structures (resolves the lifecycle addressing gap)

- **OAuth `state` correlator** (keyed by `state`): `{flow_id, PKCE verifier, account, secret_id, host,
  client_kind(+loopback port)}`, short TTL. Used **only** by the callback to find the flow from GitHub's
  `state` param. Deleted on a **successful** exchange (§6.1 prefetch-DoS).
- **Flow record** (keyed by `flow_id`, created at `start`, TTL ≈15m): `{account, secret_id, host,
  client_kind, status ∈ ISSUED|READY|ERROR(code), completer_secret (nil until callback, web/loopback
  only), pending tuple (nil until READY)}`. This is the durable-through-flow record that device polling
  and `redeem` operate on. A **background reaper** evicts expired flow records (and their pending refresh
  tokens) and caps map size — TTL expiry is not lazy-only (closes the orphaned-credential-in-memory gap).

### 5.2 Flow

```
start  (authenticated)  → derive secret_id=gh:<account>; create state correlator + flow record
                          (status=ISSUED); return {authorize_url, flow_id}. NO redemption secret here.
(browser opens authorize_url; user approves on GitHub)
callback (GitHub→AS, by state) → exchange code, FetchUser; flow record → READY with the pending tuple;
                          mint the COMPLETER-bound secret + deliver to the completing context:
                            • web      → HttpOnly,Secure,SameSite=Strict cookie on the callback response
                                          (and record its value as flow.completer_secret)
                            • loopback → 302 the browser to http://127.0.0.1:<port>/done?rc=<one-time>
                                          (record rc as flow.completer_secret)
                            • device   → none (status READY only)
                          On GitHub user-DENIAL (?error=access_denied) → flow → ERROR(code). A forged/junk
                          code that merely fails the exchange is a NO-OP (flow stays ISSUED; see §6.1).
redeem (authenticated, body {flow_id, confirm_switch?}) → look up the flow record by flow_id; verify
                          flow.account == caller; enforce the CHANNEL rule:
                            • client_kind ∈ {web,loopback}: REQUIRE the completer secret (web cookie /
                              loopback rc) to match flow.completer_secret — flow_id alone is rejected.
                            • client_kind == device: flow_id + Bearer suffices (accepted A1 residual).
                          identity-continuity peek (§6.5); then atomic single-use pop + DB-side Upsert
                          (§6.6); return metadata only {secret_id, host, login, github_user_id, version,
                          updated_at, status}.
```

Status mapping: **ISSUED → `202 {pending}`**, **ERROR → terminal `4xx {error,code}`**, **identity-change
without `confirm_switch` → `409 {identity_change, old, new}` (flow stays READY, not popped)**,
**READY+ok → `200`+metadata**, **unknown/expired → `404`**. `202`/terminal-`4xx` via `redeem` are
**device-poll-reachable only** — web/loopback never poll (they redeem once post-callback) and learn OAuth
errors via the browser `?error=` redirect.

### 5.3 Threat model

- **A1 — attacker-initiates / victim-completes (credential capture).** Attacker calls `start` (own
  account), phishes the victim into opening `authorize_url`; GitHub silently re-consents a
  previously-authorized App, so the callback binds the **victim's** refresh chain. *web/loopback:* the
  completer secret (cookie / `rc`) is delivered to the **victim's** context, which the attacker does not
  control, so the attacker cannot redeem and the victim cannot (flow account ≠ victim) — entry reaped.
  **But `client_kind` is attacker-chosen, so a rational A1 attacker simply selects `device`**, where
  redeem needs only `flow_id`+Bearer. **Honest residual:** A1 is GitHub-App **phishing** — one-click for
  a previously-authorized victim (silent re-consent) — and the captured grant lands in the *attacker's*
  Spawnery account, giving the attacker the victim's installation-selection-scoped access. It is **not**
  "closed for web/loopback" in any way that constrains a determined attacker; the bounds are GitHub's
  consent UX (first-time only), the installation-selection blast radius (§16.2), and the grant-wide kill
  switch (§16.5). The consent/`@login` warnings (§6.3) protect the **victim==operator** self-phishing
  case only. Accepted per L10/user decision; documented, not claimed-closed.
- **A2 — attacker-completes / owner-redeems (identity injection).** Attacker approves as themselves into a
  flow the owner initiated; owner redeems and unknowingly operates as the attacker. *web/loopback:*
  closed — the completer secret lands in the attacker's context, and the owner's redeem (holding only
  `flow_id`) is rejected by the channel rule (§5.2). *all paths:* the **peek-before-pop
  identity-continuity gate** (§6.5) surfaces the resolved `@login` for relinks; a first link has no prior
  identity, so A2-on-first-link relies on the channel rule (web/loopback) or folds into the device
  residual.
- **PKCE caveat.** GitHub App PKCE (S256, 2025-07-14, optional/non-enforcing) protects code
  confidentiality only; the confidential-client AS holds verifier + `client_secret`, so **PKCE provides
  no A1/A2 defense** — the channel-binding does.
- **`authorize_url` is possession-sensitive** (carries `state`; lands in history / printed on device).

## 6. Component design

### 6.1 AS HTTP surface (`internal/authsvc`)

- **`POST /github/link/start`** (was `GET …/authorize` 302). Authenticated. Derives
  `secret_id=gh:<account>`; creates the `state` correlator + `flow_id` flow record (ISSUED); returns
  `{authorize_url, flow_id}`. *Why not the 302:* a top-level navigation cannot carry the SPA's Bearer.
  Loopback `port` MUST be the CLI's actually-bound ephemeral port; the AS rejects ports outside the
  ephemeral range and only ever appends the fixed `/done` path (bounds the CSRF-to-localhost surface).
- **`GET /github/link/callback`** (route unchanged; reworked). Correlate by `state`; **GitHub user-denial
  (`?error=`) → flow ERROR(code) + browser `?error=` redirect**; otherwise exchange + `FetchUser`, flow →
  READY, deliver the completer secret per `client_kind` (web cookie / loopback `/done?rc` / device none),
  delete `state` **only on successful exchange**. A junk/forged code that fails the exchange does **not**
  consume `state` and does **not** drive ERROR (so it cannot brick a legitimate later completion). Device
  callback redirect target: a generic AS "you may close this tab" page.
- **`POST /github/link/redeem`** (route unchanged; reworked). Authenticated. Body `{flow_id,
  confirm_switch?}` + the completer secret (web cookie auto-sent / loopback `rc`). Enforces flow.account
  == caller, the **channel rule** (§5.2: web/loopback require the completer secret; `flow_id`-only is
  rejected unless `client_kind==device`), the **ownership guard** (§4), the **identity-continuity peek**
  (§6.5), then **atomic single-use pop + DB-side Upsert** (§6.6); returns metadata only. Honors the §5.2
  status codes. **Remove the tuple emission** (L2).
- **`GET /github/links`** (new). Authenticated, account-bound. Returns link metadata with explicit
  **`status ∈ {linked, revoked, relink_required}`** (do not filter revoked rows to look never-linked);
  MVP 0/1 row. Needs `GitHubLinks().List(ctx, accountID)` (which must surface revoked/relink_required).
- **`POST /github/link/revoke`** (unchanged) — account-bound kill switch.
- **CORS (corrected).** `POST /github/link/redeem` carries the HttpOnly cookie cross-origin (SPA at
  `app.X`, AS at `auth.X`), so it MUST use **`corsCredentialed`** (sets `Access-Control-Allow-Credentials:
  true`, exact-origin) — `corsBearerSimple` deliberately omits `Allow-Credentials` and would silently
  break the web path. `POST /github/link/start` and `GET /github/links` are Bearer-only and use
  `corsBearerSimple`. Register the explicit `OPTIONS` routes for all three (Go 1.22 method-mux routes
  `OPTIONS` separately; without them preflights 405).

**Multi-instance AS constraint (`sp-v40s.4`).** The `state` correlator + `flow_id` flow records are
AS-side; `start`/`callback`/`redeem` are independently-routed and the **GitHub→AS callback cannot honor
browser sticky-session affinity**. A horizontally-scaled production AS MUST use the **shared volatile
store with atomic redeem-and-delete** (`sp-v40s.4`); single-instance/sticky is dev-only. Binding
deployment constraint.

### 6.2 Web driver (`web/`)

Settings → GitHub panel reads `GET /github/links` → renders `status` ("Linked as @login (vN)" /
"relink required" / "Link GitHub") with Relink / Revoke. Link/Relink: `POST start` (Bearer,
`client_kind=web`) → store `{flow_id}` in `sessionStorage` (non-secret marker) → top-level navigate to
`authorize_url`. On return: **after `bootstrap()`/silent-refresh completes** (the SPA Bearer is wiped by
the navigation), if the marker is present → `POST redeem` with `credentials:'include'` (the HttpOnly
callback cookie rides cross-origin under `corsCredentialed`; body carries `flow_id`) → **confirm the
returned `@login`** (on relink identity-change, redeem returns `409`; show the `@old→@new` modal, then
re-redeem with `confirm_switch=true`) → clear the marker → refresh the panel.

- **Recovery:** if bootstrap fails (key-lost / revoked / login-required), clear the stale marker and
  surface retry — do not strand a flow.
- **Topology (document):** SameSite=Strict callback cookie ⇒ SPA same-site with AS; redeem fetch uses
  `credentials:'include'`.
- **TTLs:** callback cookie ≈5m (must outlast a cold reload + bootstrap, spike S1), flow record ≈15m.
- **New-tab caveat:** if `authorize_url` is opened in a different tab, the return tab lacks the marker;
  surface "finish in the original tab / retry".

### 6.3 spawnctl driver (`cmd/spawnctl/`)

Structurally mirrors `login.go`'s loopback/device auto-selection (the device path is **not** RFC 8628 —
no short `user_code`; the user opens the full `authorize_url`).
- **`spawnctl gh link`** → `POST start` (Bearer):
  - **loopback** (default when a browser is reachable): bind `127.0.0.1:0`, send that OS-assigned port as
    `client_kind=loopback`; open the browser to `authorize_url`; on `/done?rc=…` read `rc`, `POST redeem`
    (Bearer + `flow_id` + `rc`), **confirm `@login`**, print result. The `/done` page is **self-contained
    (no external subresources), sets `Referrer-Policy: no-referrer`, and `history.replaceState`s the `rc`
    out of the address bar**; `rc` is single-use + short-TTL and useless without Bearer+`flow_id` — the
    §2 carve-out. **Shared-host residual:** loopback A1-closure assumes the victim's browser and the
    attacker's spawnctl are on different hosts; on a shared multi-user host a co-resident attacker could
    bind the port — document the single-user-host precondition (spike S3).
  - **device** (`--device`, or auto when headless / `--no-browser`): `client_kind=device`, print
    `authorize_url` + the §5.3 consent warning; poll `POST redeem` (Bearer + `flow_id`): `202` keep
    polling, **`401` refresh Bearer + retry**, **`409` retry**, terminal `4xx{error}` fail fast, `200`
    confirm `@login` then done.
- **`spawnctl gh status`** → `GET /github/links`. **`spawnctl gh revoke`** → `POST revoke`.

### 6.4 Multi-device

Falls out of account-binding: every endpoint authorizes by account; the AS `version` (DB-atomic, §6.6)
is the authority. Any owner-authenticated device may link / relink / revoke; no per-device copies, no
client-side sync.

### 6.5 Relink semantics + identity continuity

Relink = a fresh OAuth that bumps `version` and replaces the AS chain (no explicit revoke of the prior
chain — §16.5; but see S2). **Identity-continuity gate (peek-before-pop):** `redeem` first reads the
existing link's `github_user_id` *without* popping; if a row exists and the newly-authorized identity
differs and `confirm_switch` is not set → return `409 {identity_change, old:@a, new:@b}` leaving the flow
**READY** (not consumed); the client shows an `@old→@new` confirmation and re-redeems with
`confirm_switch=true`, which then atomically pops + Upserts. A concurrent redeem during the confirm window
is handled by the single-use pop (loser → `404`, restart). First link (no existing row) needs no confirm.

**Running-spawn behavior on relink — RESOLVED by spike `sp-v40s.3`.** That spike empirically established:
a fresh re-link (new device/web authorization) **does NOT invalidate predecessor tokens** — only
refresh-token *rotation* (kills the immediate predecessor access token) and the grant-wide `DELETE /grant`
do; a targeted `DELETE /applications/{client_id}/token` kills one access token. Consequences for this
design, both now **confirmed** rather than assumed:
- (a) "Graceful until ~8h" holds: a running spawn's already-minted access token survives a relink until
  its own ~8h expiry, then fails-closed on refresh (superseded version) and surfaces relink-required.
- (b) **Orphaned-chain residual (confirmed).** A relink leaves the prior refresh chain **alive at GitHub
  for up to ~6 months**; there is no targeted refresh-token revoke (only grant-wide `/grant`, which would
  also kill the new link). This widens the §16.2/F5 AS-compromise window. MVP mitigations: the **reaper**
  (§5.1) calls targeted `DELETE /token` on any exchanged-but-abandoned flow's access token so it does not
  linger 8h (the orphaned refresh token it discards is unreferenced — re-usable only via a new OAuth);
  the relink-orphaned prior refresh chain is **documented as grant-wide-revoke-only** and accepted for
  MVP. Make-before-break relink and per-chain revocation remain out of scope.

### 6.6 Atomic version + `deliveryID`, and revoke→relink

The version bump and `deliveryID` are **DB-side**, not app-computed. A new store method does
`INSERT … ON CONFLICT (secret_id) DO UPDATE SET version = github_links.version + 1, …` and **`RETURNING`
the version + `deliveryID`** (derived from the returned version). This:
- serializes concurrent redeems on the row lock → each gets a distinct sequential version + `deliveryID`
  (no app-level `Get→+1→Upsert` race, no colliding `deliveryID`);
- **survives revoke→relink:** the increment reads the existing row's version **regardless of `revoked`**
  (the merged `Get … WHERE revoked=0` reset version to 1 and reproduced a prior `deliveryID`, letting a
  surviving pre-revoke node mint on the new chain). The relink Upsert clears `revoked` and yields
  `version+1`, never a reset.

Define the new `GitHubLinkRepo` method + sentinel; verify the conditional-upsert+`RETURNING` against the
actual schema (Postgres/bun). The merged `Get` filtering revoked rows must not be the version source.

## 7. Scope

**In:** the AS surface rework (§6.1) incl. the two-structure lifecycle + reaper, completer-bound secret
delivery + channel rule, the corrected CORS wiring, account-derived `secret_id` + ownership guard,
DB-side atomic version/`deliveryID` + revoke→relink fix, retiring the tuple emission, and updating
`github_link_test.go`; the `List` store query + `status`; the web panel (§6.2); spawnctl
`gh link|status|revoke` (§6.3).

**Out / dependencies:** egress floor (`sp-u53.1.6`); AS-custodial mint/refresh + CP fanout (built);
spawn-start initial token delivery (runtime); suspend backstop (deferred §16.7); multiple named links
per account (additive later); make-before-break relink. **Escalation E1 — mount→link resolution:** how a
`github:owner/repo` mount resolves to the account-default `secret_id` (`storage/github.go`
`CredentialSecretID` empty; CP `spawn_mounts.credential_secret_id`) is the epic's load-bearing binding and
is **not** answered here — needs an owner under `sp-dl62`.

## 8. Testing

- **AS handlers** (hermetic, table-driven): `start` derives `gh:<account>` and ignores a client-supplied
  foreign id; `callback` ISSUED→READY/ERROR with per-`client_kind` secret delivery, user-denial→ERROR,
  junk-code→no-op (state survives, flow stays ISSUED); `redeem` enforces account==flow, the **channel
  rule** (web/loopback `flow_id`-only is rejected; only `device` accepts `flow_id`), ownership guard,
  **peek-before-pop identity-continuity** (`409` then `confirm_switch` commit), atomic single-use pop,
  metadata-only (**assert no token material**), and the §5.2 status codes incl. terminal ERROR. Assert no
  completer secret appears in a redirect `Location` **except** the loopback `/done?rc` carve-out.
  Concurrency: two redeems for the same `secret_id` → distinct sequential versions + `deliveryID`s.
  **revoke→relink → version=2, `deliveryID` ≠ v1.** Reaper evicts an orphaned READY flow within TTL.
- **Web** (vitest): panel renders `linked|revoked|relink_required`; redeem gated on bootstrap, uses
  `credentials:'include'`; `409` identity-change → confirm modal → `confirm_switch` re-redeem; marker
  cleared on success and on bootstrap failure; `?error=` surfaced.
- **spawnctl:** loopback `/done?rc`→redeem (assert `rc`/secrets never logged; `/done` self-contained,
  `no-referrer`, history-stripped) and device poll honoring 202/401/409/terminal/200; `@login`
  confirmation; consent warning printed.

## 9. Decision Log

- **L1** — AS sole custodian; no owner/CP copy (amends §16.2). Recovery = relink.
- **L2** — `redeem` returns metadata only; remove the merged tuple emission.
- **L3** — `authorize` 302 → authenticated `start` returning `{authorize_url, flow_id}`.
- **L4** — Redemption handle bound to the OAuth-completing context (web cookie / loopback `rc`), minted at
  callback. Reverses the pre-roast initiator-held handle. Closes A2 for web/loopback.
- **L5** — Two-structure lifecycle: single-use OAuth `state` correlator + `flow_id`-keyed flow record
  (ISSUED→READY/ERROR→popped) with a background **reaper**; status mapping `202/4xx/409/200/404`.
- **L6** — `secret_id` is **account-derived** (`gh:<account>`); composite key rejected (not additive
  through the mint path); ownership guard kept as defense-in-depth.
- **L7** — DB-side atomic version increment + `deliveryID` via `RETURNING`; survives revoke→relink
  (reads version across revoked rows). No app-level `Get→+1`.
- **L8** — Pinned TTLs: flow ≈15m, callback cookie ≈5m (spike S1).
- **L9** — Relink identity-continuity via **peek-before-pop `confirm_switch`** (`409` then confirm); first
  link needs none. Running-spawn invalidation **confirmed by `sp-v40s.3`** (relink does not invalidate
  predecessors → graceful-until-8h; orphaned prior refresh chain is grant-wide-revoke-only; reaper does
  targeted `DELETE /token` on abandoned flows).
- **L10** — Device flow kept; **A1 is an honest phishing residual** (not "closed for web/loopback" —
  `client_kind` is attacker-chosen), bounded by consent UX + installation-selection scope + kill switch;
  `@login` confirmation + consent warning protect the victim==operator case.
- **L11** — Callback records OAuth **user-denial** as terminal ERROR; a junk/forged code is a no-op (no
  state consumption, no ERROR) so it cannot brick a legitimate completion.
- **L12** — `redeem` uses **`corsCredentialed`** (cookie-bearing); `start`/`GET links` use
  `corsBearerSimple`; explicit `OPTIONS` routes. Web redeem sequenced after `bootstrap()` with
  stale-marker recovery; SameSite cookie ⇒ SPA same-site with AS.
- **L13** — Multi-instance AS: single-instance/dev or the `sp-v40s.4` shared volatile store (the
  GitHub-originated callback cannot bind sticky sessions).
- **L14** — Multi-device falls out of account-binding; AS `version` is authority.
- **L15** — Loopback `rc` URL carve-out (§2): single-use + short TTL + dual-factor (Bearer+`flow_id`+`rc`)
  + self-contained `no-referrer` `/done` + history strip + ephemeral-port bound; shared-host precondition
  documented (spike S3).

## Roast disposition (r2)

| Re-roast finding (clustered) | Disposition |
|---|---|
| CORS: redeem needs `corsCredentialed`, not `corsBearerSimple` (blocker) | Fixed — §6.1, L12 |
| `flow_id` not bound to `client_kind` → A2 web/loopback bypass (major) | Fixed — channel rule §5.2, L4 |
| A1 "defended for web/loopback" overclaimed (`client_kind` attacker-chosen) (major) | Re-characterized honestly — §5.3, L10 |
| Confirmation post-commit / "before Upsert" unimplementable (major) | Fixed — peek-before-pop `confirm_switch` §6.5, L9 |
| Lifecycle addressing: `flow_id`→record vs `state` correlator (major) | Fixed — two-structure model §5.1, L5 |
| No reaper / orphaned READY holds live refresh token (major) | Fixed — reaper §5.1, L5 |
| state-retention + flow→ERROR self-defeat (major) | Fixed — only user-denial terminal §6.1, L11 |
| Atomic version not mechanized + `deliveryID` coupling (major) | Fixed — DB-side `RETURNING` §6.6, L7 |
| revoke→relink resets version → `deliveryID` collision (major) | Fixed — increment across revoked rows §6.6, L7 |
| Device poll 401/409 mis-treated as terminal (major) | Fixed — poll rules §6.3 |
| composite-key ≠ additive; commit to account-derived (major) | Fixed — §4, L6 |
| loopback `rc`-in-URL / Referer / self-contained `/done` / port bound / shared-host (minors) | Fixed — §6.3, L15, S3 |
| OPTIONS registration; device callback redirect target; status codes device-only (minors) | Folded — §6.1/§5.2 |
| Escalation S2 — relink/fresh-auth token invalidation | **Already resolved by `sp-v40s.3`** — folded into §6.5 (graceful-until-8h confirmed; orphaned-chain residual documented + reaper `DELETE /token`) |
| Escalation E1 — mount→link resolution | §7 (separate `sp-dl62` bead) |

## 10. Spikes

- **S1 — Web CORS + cookie round-trip. RESOLVED (2026-06-17, by code inspection).** `corsBearerSimple`
  (`internal/authsvc/deviceset_http.go:22-46`) sets `Access-Control-Allow-Origin`+`Vary` but
  **deliberately omits `Access-Control-Allow-Credentials`** (own doc comment); `corsCredentialed`
  (`internal/authsvc/cors.go:14-34`) sets **`Access-Control-Allow-Credentials: true`** + exact-origin
  ACAO (hard 403 for other origins) and passes Origin-less CLI callers through. Per the Fetch spec a
  cross-origin `credentials:'include'` request (required to attach the HttpOnly SameSite=Strict callback
  cookie) is blocked unless the response carries ACAC:true — so **`redeem`→`corsCredentialed` is correct
  and necessary; `corsBearerSimple` would silently break web redeem** (the re-roast blocker). Cookie TTL
  (≈5m) ≫ cold `bootstrap()` (seconds) — ample headroom; no TTL change needed. **Impl note for `.20.1`:**
  `corsCredentialed` is a method on `*IdP`; the GitHub-link handlers hang off `*Service`, so the
  credentialed wrapper (or an equivalent that also accepts `Authorization: Bearer`) must be shared/exposed
  to them. The live browser round-trip is wired as a `.20.1`/`.20.2` integration test (cheapest live
  confirmation; the rule itself is deterministic).
- ~~**S2 — Relink / fresh-auth token invalidation.**~~ **Resolved by `sp-v40s.3`** (verdict 2026-06-16):
  a fresh re-link does NOT invalidate predecessor tokens; only rotation (predecessor access token) and
  grant-wide `/grant` do. Folded into §6.5. No new spike needed.
- **S3 — Shared-host loopback isolation. RESOLVED (2026-06-17, by local demonstration).** A separate
  process connected to another's `127.0.0.1` listener and delivered a fake `rc`; a second `bind()` on the
  held port returns `EADDRINUSE` (first binder owns it). So `127.0.0.1` is **host-global with no
  per-process/per-user isolation** (absent network namespaces) — RFC 8252 §8.3 loopback interception. On a
  shared multi-user host, a co-resident attacker who picks `client_kind=loopback:<port>` and binds first
  receives the victim's `rc`, so **loopback does not close A1 there** — it degrades to the device residual.
  The single-user-host precondition (L15/§6.3) is therefore **necessary, not optional**; `.20.3` should
  additionally prefer device flow on a detected shared/headless host (no worse than the documented device
  residual). Per-user loopback isolation (network namespaces) is out of MVP scope.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from the
assumptions above — append a dated note here, whether or not a formal debugging skill was used.*

- 2026-06-17 (IMPLEMENTED, `feat/sp-dl62-integration`): the owner-facing link flow (r2 spine) was
  implemented and integrated as part of epic `sp-dl62`. Gates green on the combined branch
  (`go test -race ./...`, no `gen/` drift, `golangci-lint` 0); final whole-epic review PASS,
  containment invariant (a) (refresh token AS-only) re-confirmed across the combined change. Landed:
  - **sp-v40s.20.1 (AS surface)** — `start` (302→authenticated JSON `{authorize_url, flow_id}`,
    account-derived `secret_id = gh:<account>`), the two-structure lifecycle (single-use OAuth `state`
    correlator + `flow_id`-keyed flow record `ISSUED/READY/ERROR`) with a background reaper that
    `DELETE /token`s exchanged-but-abandoned access tokens, per-`client_kind` completer-bound delivery
    (web HttpOnly/Secure/SameSite=Strict cookie · loopback `rc` · device none), `redeem` with the
    channel rule + peek-before-pop `confirm_switch` identity continuity (409 leaves flow READY) +
    DB-side atomic version/`deliveryID` via `RETURNING` (survives revoke→relink), account-bound
    `GET /github/links`, and per-route CORS (`corsCredentialed` for the cookie-bearing redeem,
    `corsBearerSimple` for start/list). **The merged sealed-tuple emission was removed — `redeem`/`List`
    return metadata only** (`{secret_id, host, login, github_user_id, version, updated_at, status}`);
    the token columns are never decrypted on these paths (invariant a).
  - **sp-v40s.20.2 (web driver)** — Settings → GitHub panel: reads `GET /github/links`, drives
    start → top-level OAuth navigate → redeem gated on `bootstrap()`/silent-refresh (so the in-memory
    Bearer is restored), cookie via `credentials:'include'`, 409 `identity_change` confirm modal,
    `?error=` surfacing, and a bootstrap-failure stranded-marker reaper. The only client-held secrets
    are the non-secret `flow_id` marker and the browser-auto-attached HttpOnly completer cookie (never
    read by JS). S1 (cross-origin CORS + cookie round-trip) is exercised here.
  - **sp-v40s.20.3 (spawnctl driver)** — `gh link` (loopback default: binds `127.0.0.1:0`, serves a
    self-contained `/done` page, reads `?rc`, redeems with `Bearer + flow_id + rc`, strips `rc` via
    `history.replaceState`; device/`--device` polls `redeem` with `Bearer + flow_id`), `gh status`,
    `gh revoke`. Client-only — holds no token; the loopback `rc` is never logged/reflected. S3
    (shared-host loopback) is a documented single-user-host precondition; prefer device on shared hosts.
  - **sp-v40s.20.4 (ownership guard)** — strengthened `TestRedeemOwnershipGuard` into a table-driven
    test proving redeem rejects a cross-account `secret_id` with 403 **and** does not overwrite the
    pre-seeded row (the load-bearing proof against the `account_id = EXCLUDED` overwrite). The
    production guard was already folded in by .20.1; account-derived `secret_id` makes it normally
    unreachable, so it stands as defense-in-depth.

  **Residuals:** S1/S2/S3 spikes are resolved (S1 web round-trip wired as the .20.1/.20.2 test; S2
  resolved via sp-v40s.3; S3 by local demonstration → single-user-host precondition). The link flow
  ships single-default-link for MVP; multi-link selection and the per-user loopback isolation
  (network namespaces) remain out of scope. E1 (mount→link resolution) tracks under sp-dl62.
