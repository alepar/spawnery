# CP Deployment — Auth & Revocation (A2)

## Auth mode

```
CP_AUTH_MODE=prod    # required for production; default is "dev"
```

**IMPORTANT:** the default is `dev` (mirrors `NODE_AUTH_MODE`). A misconfigured prod instance
is permissive (dev tokens accepted). Always set `CP_AUTH_MODE=prod` on production.

In `prod` mode, `CP_DEV_TOKENS` is silently ignored and `CP_AS_SESSION_PUBKEYS` is required.
In `dev` mode, both verifiers are active: AS tokens (if keys are configured) and dev tokens.

## AS session pubkeys

```
CP_AS_SESSION_PUBKEYS=/etc/spawnery/cp/as-session-pub.pem,/etc/spawnery/cp/as-session-next-pub.pem
```

Comma-separated ordered list of PEM-encoded Ed25519 public key files (PKIX format, current first
then next). The CP verifies session tokens against this set; a token whose `key_id` doesn't match
any key is refused.

**Rotation procedure** (mirrors AS `AS_SESSION_KEY_NEXT_PEM`):
1. Pre-publish the "next" key to `CP_AS_SESSION_PUBKEYS` (append to the comma list) on all CP
   instances — both current and next are now valid.
2. Wait for the overlap window to drain in-flight tokens signed by the current key (≥ 15 min, the
   access-token TTL).
3. Switch the AS to sign with the next key (`AS_SESSION_KEY_PEM` = next; remove `AS_SESSION_KEY_NEXT_PEM`).
4. Retire the old key: remove it from `CP_AS_SESSION_PUBKEYS` — tokens signed by it are now refused.

**Emergency (compromise) path:** use the AS PKI chain (enrollment-pinned root) to sign a
replacement statement; see the auth-identity design §3 [AM4].

## Revocation feed

```
CP_AS_REVOCATION_URL=https://auth.spawnery.example/revocations
CP_AS_CP_SECRET=<shared-secret>          # optional; required on prod AS (AS_CP_SECRET must match)
CP_REVOCATION_POLL_INTERVAL=30s          # default 30s
```

The CP polls `GET <CP_AS_REVOCATION_URL>?since=<checkpoint>` on the configured interval.
Each valid entry is verified against `CP_AS_SESSION_PUBKEYS` (same key set, distinct domain prefix)
then applied: revoked token_ids and account_ids are added to the in-process registry, and all
live WS/gRPC sessions bound to those identifiers are terminated immediately.

**Ops gaps (code seam present, ops-pending):**
- Real AS pubkey distribution (mounting PEM files to the CP) is not automated — ops/Helm/cloud
  init responsibility (A3/A5).
- The shared CP secret (`CP_AS_CP_SECRET` / `AS_CP_SECRET`) must be provisioned out-of-band.
- `just dev` dev-AS-key wiring (dev mode only, A3/A5).

## In-band session reauth

```
CP_SESSION_REAUTH_INTERVAL=15m    # default 15m (aligned with access-token TTL)
```

AS-token sessions in prod mode must re-present a current token within this interval (+30s grace)
or the connection is closed with `StatusPolicyViolation`.

**WS protocol:** a TEXT frame `{"type":"reauth","token":"<wire>"}` resets the deadline.
**gRPC Session:** a `Frame{reauth_token: "<wire>"}` is consumed by the recv loop (never forwarded).

Client-side implementations land in A3 (spawnctl) and A5 (web). In dev mode, the CP is tolerant
if the peer doesn't re-present (logs only, connection stays open). Dev-token sessions are always
exempt.

## Dev tokens

```
CP_DEV_TOKENS=dev-token=dev,alice-token=alice    # honored only in CP_AUTH_MODE=dev
```

A comma-separated `token=owner` map. Dev-token sessions have no `token_id` and are not tracked
for revocation by token (but are cancelled on account revocation if the account matches).
