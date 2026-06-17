# Owner-Facing GitHub Link Flow (web + spawnctl)

**Bead:** `sp-v40s.20` (epic `sp-dl62` — End-user GitHub mounts) · **Date:** 2026-06-17 · **Status:** draft

**Builds on / amends:**
[`2026-06-14-github-credentials-and-storage-unified-design.md`](2026-06-14-github-credentials-and-storage-unified-design.md)
(§3 Custody, §16 round-3 AS-custodial) and round-2 finding **F14**
([`2026-06-16-github-storage-backend-adversarial-review-r2.md`](2026-06-16-github-storage-backend-adversarial-review-r2.md)).
**This spec amends §16.2** (see Decision L1).

> **Revision r1 (2026-06-17), post-roast (BLOCK → revised).** A `superpowers:roast` pass (5 critics,
> 3-judge panel) found the original "unify on an initiator-held in-memory `link_session`, retire the
> SameSite cookie" spine **unsafe**: the retired cookie was *channel-bound to the OAuth-completing
> browser*, and moving the handle to the initiator opened two credential-capture/identity-injection
> attacks (§5 threat model A1/A2). This revision **binds the redemption handle to the OAuth-completing
> context** (cookie for web, loopback `rc` for CLI), keeps the AS-custodial custody decisions (L1/L2),
> and closes the cross-account `secret_id` takeover, the flow-lifecycle/TTL gaps, the concurrent-link
> race, the device error channel, the multi-instance-AS constraint, and the relink identity-continuity
> gap. See the Decision Log and "Roast disposition" for the per-finding mapping.

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
   copy goes stale within ~8h and — being CP-blind — cannot be refreshed by CP/AS. The owner's "DR copy"
   has no coherent operational job.
2. **Who may redeem (F14 + roast).** The handoff between the GitHub-initiated callback and the owner's
   commit must be bound so that (a) the redeemed credential belongs to the GitHub identity the owner
   *intended*, and (b) a third party can neither capture a victim's credential nor inject their own
   identity into the owner's link. The binding must not leak the handle to URL/history/Referer/logs.
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

- **No DR.** A lost AS keystore forces every user to relink. Explicitly accepted (relink is cheap; the
  refresh chain is intentionally single-homed). **Supersedes §16.2's "CP stores an owner-sealed copy of
  the credential for owner disaster-recovery / cross-device relink"** clause — there is no sealed copy;
  cross-device relink is served by account-bound AS endpoints (§6.4).
- **No tuple handoff to the client.** `redeem` returns **metadata only**. The access/refresh tokens
  never transit to the browser/CLI — strictly safer than the merged endpoint, which still returns the
  full tuple JSON (`github_link.go:313-330`); that emission **must be removed in the same edit** (L2).
- **Fanout unaffected.** §16.4's per-node sealed fanout is a runtime path independent of the
  (now-removed) owner DR copy.

## 4. Single default link & account-scoped `secret_id` (closes cross-account takeover)

MVP exposes **one GitHub link per account**. The link's `secret_id` is **server-derived from the
account** (e.g. `gh:<accountID>`), **never** client-supplied as a raw cross-account string. The store
must enforce ownership so a guessed/colliding `secret_id` cannot cross-account-clobber:

- **Key by account.** Either make the durable key composite `(account_id, secret_id)`, or keep
  `secret_id` the PK *and* derive it from `account_id` (so it is unique per account by construction).
- **Redeem ownership guard (required).** Before `Upsert`, `redeem` MUST reject (`403`) when an existing
  link row's `AccountID != caller` — mirroring the guard `revoke` already has (`github_link.go:376`).
  The merged `redeem` lacks this guard; that is a latent cross-account-overwrite bug in shipped code
  (file a fix bead).

The store stays `secret_id`-addressable so multiple named links per account remain an additive change
later. `start` does **not** accept an arbitrary cross-account `secret_id`; for MVP it takes at most a
client-chosen *label* that is namespaced under the account server-side.

## 5. Security spine — handle bound to the OAuth-completing context

**Principle (corrected from the pre-roast design):** the secret that gates `redeem` is minted **at the
callback** (post-OAuth) and delivered **only to the context that completed the GitHub OAuth** — never to
the initiator. This restores the channel-binding the merged SameSite cookie provided and that the
pre-roast initiator-held `link_session` destroyed.

### 5.1 Flow and the three-state lifecycle

```
start  (authenticated)  → AS creates an account-bound `state` row (PKCE verifier, secret_id, host,
                          client_kind∈{web,loopback:<port>,device}, status=ISSUED, TTL≈15m) and a
                          separate poll-correlation `flow_id` stored IN the state row. Returns
                          {authorize_url, flow_id}. No redemption secret is issued here.
(browser opens authorize_url; user approves on GitHub)
callback (GitHub→AS, correlated by `state`) → exchange code, FetchUser, transition the flow to READY
                          and stash the pending tuple. Mint the COMPLETER-bound redemption secret and
                          deliver it to the completing context:
                            • web      → HttpOnly,Secure,SameSite=Strict cookie on the callback response
                            • loopback → redirect browser to http://127.0.0.1:<port>/done?rc=<one-time>
                            • device   → no completer channel exists (see 5.3); flow just marked READY
                          On OAuth error, transition the flow to ERROR(code) (see §6.1 error channel).
redeem (authenticated)  → present the completer-bound secret (web cookie / loopback rc) OR, device-only,
                          the initiator `flow_id`. AS verifies caller==flow account, atomically pops the
                          READY entry, applies the relink identity-continuity check (§6.5), Upserts, and
                          returns metadata only (incl. resolved `login` for confirmation).
```

`redeem` status mapping (enables device polling and fail-fast): **ISSUED → `202 {status:"pending"}`**,
**ERROR → `4xx {status:"error", code}`** (terminal), **READY → `200` + metadata**,
**unknown/expired → `404`**.

### 5.2 Threat model (what the binding defends)

- **A1 — attacker-initiates / victim-completes (credential capture).** Attacker calls `start` (own
  account), phishes the victim into opening `authorize_url`; GitHub silently re-consents a
  previously-authorized App, so the callback binds the **victim's** refresh chain to the pending entry.
  *Defended for web/loopback:* the redemption secret (cookie / `rc`) is delivered to the **victim's**
  completing context, which the attacker does not control, so the attacker cannot `redeem`; the victim
  cannot `redeem` either (flow account ≠ victim). The pending entry expires. *Device:* un-closable —
  see §5.3.
- **A2 — attacker-completes / owner-redeems (identity injection).** Attacker obtains the owner's
  `authorize_url` and approves as themselves; owner redeems and unknowingly operates as the attacker's
  GitHub identity. *Defended:* (1) for web/loopback the completer-bound secret lands in the *attacker's*
  context, not the owner's, so the owner's `redeem` finds no secret; (2) **redeem-time `login`
  confirmation** (defense-in-depth, all paths): `redeem` surfaces the resolved `@login`/`github_user_id`
  and the client requires explicit owner confirmation before the commit, so a mismatched identity is
  caught.
- **PKCE caveat (documented).** GitHub App PKCE (S256, added 2025-07-14, optional/non-enforcing) protects
  *code* confidentiality but, because the confidential-client AS holds both verifier and `client_secret`
  and performs the exchange, **PKCE provides no defense against A1/A2** — the completer-binding above is
  the real defense.
- **`authorize_url` is possession-sensitive.** It carries `state`; treat it as a secret in the threat
  model (it lands in browser history on the web path and is printed on the device path). State is
  single-use and the callback consumes it; see the prefetch-DoS note in §6.1.

### 5.3 Device flow residual (accepted, gated)

The device path has **no return channel to the completing browser**, so the completer-bound secret
cannot be delivered; device `redeem` falls back to the initiator-held `flow_id` + Bearer. This makes
**A1 cryptographically un-closable for device flow** (redeem-time confirmation does not help — the
attacker is the redeemer and *wants* the victim's identity). MVP keeps device flow for headless/SSH
parity but:
- requires the redeem-time `@login` confirmation,
- prints a consent warning ("only approve a link URL you started yourself"),
- documents A1 as an accepted device-flow residual (phishing-class, bounded by the consent UX).

## 6. Component design

### 6.1 AS HTTP surface (`internal/authsvc`)

- **`POST /github/link/start`** (was `GET …/authorize` 302). Authenticated (`githubLinkAccountFromReq`).
  Creates the account-bound `state` row {PKCE verifier, **`flow_id`**, account-derived `secret_id`,
  host, `client_kind`(+loopback port), `status=ISSUED`, TTL≈15m}; returns `{authorize_url, flow_id}`.
  *Why not the 302:* a top-level browser navigation cannot carry the SPA's `Authorization: Bearer`;
  returning the URL lets the client self-navigate and is identical for the CLI.
- **`GET /github/link/callback`** (route unchanged; logic reworked). Correlate by `state`, exchange,
  `FetchUser`, transition `flow_id`→`READY` with the pending tuple. Deliver the completer-bound secret
  per `client_kind`: web cookie / loopback `…/done?rc=…` / device (none). On OAuth failure transition
  `flow_id`→`ERROR(code)` **and** keep the existing browser `?error=` redirect.
- **`POST /github/link/redeem`** (route unchanged; reworked). Authenticated. Accepts the completer
  secret (cookie / `rc`) or, device-only, `flow_id`. Verify caller==flow account; **atomic CAS pop** of
  the READY entry; **ownership guard** (§4); **relink continuity** (§6.5); `Upsert` with an
  **atomic/DB-side version increment** (§6.6); return metadata only `{secret_id, host, login,
  github_user_id, version, updated_at, status}`. Honors the §5.1 status codes. **Remove the tuple
  emission** (L2).
- **`GET /github/links`** (new). Authenticated, account-bound. Returns the account's link metadata with
  an explicit **`status ∈ {linked, revoked, relink_required}`** (do not silently filter revoked rows to
  look never-linked); MVP 0/1 row. Needs `GitHubLinks().List(ctx, accountID)`.
- **`POST /github/link/revoke`** (unchanged) — account-bound kill switch.
- **CORS:** the three SPA-called routes (`start`, `redeem`, `GET /github/links`) must be wrapped with
  the established `corsBearerSimple` (per the `/devices` precedent) so cross-origin SPA→AS Bearer fetches
  are not browser-blocked.
- **Prefetch-DoS note:** `state` is single-use and the callback deletes it on first hit; a third party
  who sees `state` (device-printed URL) can burn it with a junk callback, forcing the owner to restart.
  MVP mitigation: delete `state` **only on a successful exchange** (so a junk callback does not consume
  it); documented as a minor accepted residual otherwise.

**Multi-instance AS constraint (restated from `sp-v40s.4`).** `state`/`flow_id`/pending are AS-side and
today in-memory; `start`, `callback`, and `redeem` are three independently-routed requests, and the
**GitHub→AS callback cannot honor browser sticky-session affinity** (GitHub originates it). Therefore a
horizontally-scaled production AS MUST use the **shared volatile store with atomic redeem-and-delete**
from `sp-v40s.4` (encrypted payloads, short TTL, auth-bound redemption); single-instance/sticky is a
dev-only posture. MVP states this as a binding deployment constraint.

### 6.2 Web driver (`web/`)

Settings → GitHub panel reads `GET /github/links` → "Linked as @login (vN)" / "Relink" / "Revoke" /
"Link GitHub", rendering the `status`. Link/Relink:
`POST start` (Bearer, `client_kind=web`) → record an in-progress marker (non-secret) in `sessionStorage`
→ top-level navigate to `authorize_url`. On return to the settings page: **after `bootstrap()` /
silent-refresh completes** (the SPA's Bearer lives only in memory and is wiped by the navigation), if
the in-progress marker is present → `POST redeem` (the HttpOnly callback cookie is auto-sent same-site;
Bearer attached) → **confirm the returned `@login`** → clear the marker → refresh the panel.

- **Sequencing/recovery (required):** redeem MUST be gated on bootstrap success; if silent-refresh fails
  (key-lost / cnf-mismatch / revoked / login-required), clear the stale marker and surface a retry — do
  not strand a flow while the AS pending entry expires.
- **Topology constraint (document):** the SameSite=Strict callback cookie requires the SPA origin and
  the AS to be **same-site**; state this as a deployment requirement.
- **TTL:** the callback cookie's lifetime must outlast a cold reload + bootstrap; pin a concrete value
  (≈5m) distinct from the ≈15m flow TTL.
- **New-tab caveat:** if the user opens `authorize_url` in a different tab, the return tab lacks the
  marker; surface a "finish in the original tab / retry" affordance.

### 6.3 spawnctl driver (`cmd/spawnctl/`)

Structurally mirrors `login.go`'s loopback/device auto-selection (note: the device path is **not** RFC
8628 — there is no short `user_code`; the user opens the full `authorize_url` — so "mirrors login.go" is
structural only).
- **`spawnctl gh link`** → `POST start` (Bearer):
  - **loopback** (default when a browser is reachable): bind `127.0.0.1:0`, `client_kind=loopback`+port,
    open the browser to `authorize_url`; on `/done?rc=…` read `rc`, `POST redeem` (Bearer + `rc`),
    **confirm `@login`**, print result. A per-attempt `rc` (minted at callback) also restores the
    loopback CSRF-correlation `login.go`'s `/cb` has.
  - **device** (`--device`, or auto when headless / `--no-browser`): `client_kind=device`, print
    `authorize_url` + the §5.3 consent warning; poll `POST redeem` (Bearer + `flow_id`) honoring the
    §5.1 status codes (`202` keep polling, `4xx{error}` fail fast, `200` confirm `@login` then done).
- **`spawnctl gh status`** → `GET /github/links`. **`spawnctl gh revoke`** → `POST revoke`.

### 6.4 Multi-device

Falls out of account-binding: every endpoint authorizes by account; the AS `version` (atomic, §6.6) is
the authority. Any owner-authenticated device may link / relink / revoke. No per-device copies, no
deviceset sealing, no client-side sync.

### 6.5 Relink semantics + identity continuity

Relink = a fresh OAuth that `Upsert`s `version+1`, replacing the AS chain (no explicit revoke of the
prior chain — §16.5). **Identity-continuity guard (required):** on `redeem`, if the newly-authorized
`github_user_id` differs from the existing link's, do **not** silently swap — require an explicit
"switching from @old to @new" confirmation (web modal / `spawnctl --confirm`) before `Upsert`.

**Running-spawn behavior on relink — needs a spike (S2).** L9 previously assumed running spawns keep
their minted access token until ~8h expiry. It is **unverified** whether a fresh OAuth re-authorization
of the same user+App invalidates the prior still-live access token immediately (GitHub: "tokens
associated with a revoked authorization are revoked"). If it does, running spawns break at relink, not
at ~8h. Resolve before relying on the "graceful until expiry" framing.

### 6.6 Concurrency

`redeem`'s version bump must be **atomic** (DB-side increment / CAS in `Upsert`), not the merged
non-atomic `Get→+1→Upsert` (which lets two concurrent redeems write the same `version` and a colliding
`deliveryID`). Define the loser's outcome: `409` + retry. MVP has at most one default link per account,
but web+CLI double-submit makes the race reachable.

## 7. Scope

**In:** the AS surface rework (§6.1) incl. completer-bound secret delivery, the 3-state flow lifecycle,
status codes, account-scoped `secret_id` + ownership guard, atomic version bump, CORS, retiring the
tuple emission, and updating `github_link_test.go`; the `List` store query + `status`; the web panel
(§6.2); the spawnctl `gh link|status|revoke` (§6.3).

**Out / dependencies (tracked elsewhere):** egress floor (`sp-u53.1.6`); AS-custodial mint/refresh + CP
fanout (built); spawn-start initial access-token delivery (runtime); suspend backstop (deferred §16.7);
multiple named links per account (additive later); make-before-break relink. **Escalation E1 — mount→link
resolution:** how a `github:owner/repo` mount resolves to the account-default `secret_id`
(`storage/github.go` `CredentialSecretID` is empty; CP `spawn_mounts.credential_secret_id` must point at
it) is the epic's load-bearing binding and is **not** answered here — needs an owner under `sp-dl62`.

## 8. Testing

- **AS handlers** (hermetic, table-driven): `start` mints account-bound `state`+`flow_id`; `callback`
  transitions ISSUED→READY/ERROR and delivers the secret per `client_kind` (web sets cookie; loopback
  redirects to `/done?rc`; device sets none); `redeem` enforces caller==flow account, **ownership guard
  (A start+redeem against B's `secret_id` is rejected)**, atomic CAS pop + version, the §5.1 status
  codes incl. the **terminal ERROR** path, returns **metadata-only (assert no token material)**, and the
  **relink identity-continuity** confirmation gate. Assert no redemption secret appears in any redirect
  `Location`. Concurrency test: two simultaneous redeems → one `200`, one `409`, single version bump.
- **Web** (vitest): panel renders `linked|revoked|relink_required`; redeem is gated on bootstrap; marker
  cleared on success and on silent-refresh failure; `@login` confirmation; `?error=` surfaced.
- **spawnctl:** loopback `/done?rc`→redeem (asserts `rc`/secrets never logged) and device poll honoring
  status codes incl. fail-fast on terminal error; `@login` confirmation; device consent warning printed.

## 9. Decision Log

- **L1 — AS is sole custodian; no owner/CP copy (amends §16.2).** Recovery = relink; lost AS keystore =
  relink-all. *(roast: confirmed net-positive)*
- **L2 — `redeem` returns metadata only;** remove the merged tuple emission in the same edit. *(roast
  minor: unretired today)*
- **L3 — `authorize` 302 → authenticated `start` returning `{authorize_url, flow_id}`** (fixes the
  Bearer-vs-top-level-nav mismatch). The redemption secret is **not** issued here.
- **L4 — Redemption handle is bound to the OAuth-completing context** (web cookie / loopback `rc`),
  minted at callback — **reverses** the pre-roast initiator-held `link_session`. Closes threat-model
  A1/A2 for web/loopback. *(roast blocker)*
- **L5 — 3-state flow lifecycle** (ISSUED@start → READY/ERROR@callback → popped@redeem), `flow_id` stored
  in the `state` row, explicit `202/4xx/200/404` status mapping. *(roast blocker: was unimplementable)*
- **L6 — Account-scoped `secret_id` + redeem ownership guard** (composite key or account-derived id);
  closes cross-account takeover (also a latent merged-code bug). *(roast blocker)*
- **L7 — Atomic/DB-side version bump** with `409` on the concurrent-redeem loser. *(roast major)*
- **L8 — Pinned TTLs:** flow ≈15m (covers human round-trip + device poll), callback secret ≈5m;
  reconciled with `state` TTL. *(roast major)*
- **L9 — Relink identity-continuity confirmation** (@old→@new) before `Upsert`; running-spawn
  invalidation behavior is **spike S2**, not assumed. *(roast major + escalation)*
- **L10 — Device flow kept with redeem-time `@login` confirmation + consent warning + documented A1
  residual** (no completer channel). *(user decision)*
- **L11 — Callback records terminal OAuth errors against `flow_id`;** redeem surfaces them so device/
  loopback clients fail fast instead of polling to timeout. *(roast major)*
- **L12 — Web redeem sequenced after `bootstrap()`/silent-refresh** with stale-marker recovery; SameSite
  cookie ⇒ SPA same-site with AS (documented); `corsBearerSimple` on the new routes. *(roast major+minor)*
- **L13 — Multi-instance AS:** single-instance/dev or the `sp-v40s.4` shared volatile store; sticky
  sessions cannot bind the GitHub-originated callback. *(roast major)*
- **L14 — Multi-device falls out of account-binding;** AS `version` is authority. *(unchanged)*

## Roast disposition (r1)

| Roast finding (clustered) | Disposition |
|---|---|
| Cross-account `secret_id` takeover (blocker) | Fixed — §4, L6 |
| Handle bound to initiator, not completer; A1/A2 (blocker) | Fixed — §5, L4; device residual §5.3/L10 |
| Flow not implementable / `flow_id` not persisted / 202-vs-404 (blocker) | Fixed — §5.1, L5 |
| TTL too short for the reshaped flow (major ×) | Fixed — §6/L8 |
| Relink silently swaps identity (major) | Fixed — §6.5, L9 |
| Multi-instance AS constraint dropped (major) | Restated — §6.1, L13 |
| Concurrent-link version race (major) | Fixed — §6.6, L7 |
| Device poll has no terminal-error channel (major) | Fixed — §6.1, L11 |
| Web in-memory Bearer wiped by nav (major) | Fixed — §6.2, L12 |
| Tuple still emitted / CORS / `GET /github/links` revoked contract / "fresh" wording / state-DoS / "mirrors login.go" overclaim / sessionStorage new-tab (minors) | Folded into §6/§8 |
| Escalation E1 — mount→link resolution | §7 (separate `sp-dl62` bead) |
| Escalation S2 — relink token-invalidation | §6.5 spike |
| PKCE feasible (S256, 2025-07-14) but no A1/A2 defense | Documented — §5.2 |

## 10. Spikes

- **S1 — Web round-trip timing.** Prototype start→`sessionStorage` marker→top-level nav→cold
  `bootstrap()`+silent-refresh→redeem; confirm it completes within the callback-cookie TTL. *Kill:* if
  bootstrap routinely exceeds the cookie TTL, raise the TTL or re-architect the return.
- **S2 — Relink token invalidation.** Against the throwaway App: link, mint an access token, then run a
  fresh OAuth re-authorization; check whether the prior access token + refresh token are immediately
  invalidated. *Kill:* if immediate, L9's "graceful until ~8h" framing is wrong → document relink as
  break-now and adjust the running-spawn story.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from the
assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
