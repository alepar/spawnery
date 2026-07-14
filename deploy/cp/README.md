# CP Deployment — Auth & Revocation (A2)

## Auth mode

```
CP_AUTH_MODE=prod    # required for production; default is "dev"
```

**IMPORTANT:** the default is `dev` (mirrors `NODE_AUTH_MODE`). A misconfigured prod instance
is permissive (dev tokens accepted). Always set `CP_AUTH_MODE=prod` on production.

In `prod` mode, `CP_DEV_TOKENS` is silently ignored and the certified signer trust settings below
are required. In `dev` mode, dev tokens remain available.

## Certified AS signer trust

```
CP_AUTH_ENVIRONMENT=prod
CP_AUTH_ROOT_CA=/etc/spawnery/cp/root.pem
CP_AUTH_SIGNER_REVOCATION_STATE=/var/lib/spawnery/cp-signer-revocations/state.json
CP_AUTH_SIGNER_REVOCATION_STATEMENT=/etc/spawnery/cp/signer-revocations.pb
```

Each auth artifact carries its signer certificate chain. The CP verifies that chain against the
environment root and rejects revoked signer serials from the durable revocation state.

**Rotation procedure:** provision overlapping current and next certified signers at the AS, switch
issuance to the next signer, wait at least the token TTL, then revoke the old signer certificate by
publishing a newer root-authorized revocation statement.

**Emergency (compromise) path:** use the AS PKI chain (enrollment-pinned root) to sign a
replacement statement; see the auth-identity design §3 [AM4].

## Revocation feed

```
CP_AS_REVOCATION_URL=https://auth.spawnery.example/revocations
CP_REVOCATION_POLL_INTERVAL=30s          # default 30s
```

The CP polls `GET <CP_AS_REVOCATION_URL>?since=<checkpoint>` on the configured interval.
Each valid entry is verified through the certified auth signer chain and then applied: revoked
token_ids and account_ids are added to the in-process registry, and all
live WS/gRPC sessions bound to those identifiers are terminated immediately.

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
