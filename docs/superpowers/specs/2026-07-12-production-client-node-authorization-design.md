# Production Client-to-Node Authorization

**Date:** 2026-07-12
**Beads:** `sp-dvke.3` under `sp-dvke`
**Status:** draft, collaboratively approved
**Builds on:** [Auth & Identity](2026-06-11-auth-identity-design.md),
[Unified Root for AS Authorization Signing](2026-07-12-unified-root-as-auth-signing-design.md), and
[Unified Service mTLS](2026-07-12-unified-service-mtls-design.md)

## Problem

Spawnery's node-side authorization chain is implemented only for development. Clients sign
`SignedIntent` messages with fresh per-operation P-256 keys and submit an empty
`node_access_token`. A dev-only Control Plane (CP) private key then mints an `aud=node` token bound to
whatever ephemeral key the intent supplied. The Auth Service (AS) issues only `aud=cp` tokens, so
production clients cannot construct an authorization envelope a production spawnlet will accept.

The dev shortcut also conceals a key-binding mismatch: the AS login session is bound to one
persistent client public key, while current Go and TypeScript SDK intent paths generate unrelated
keys. Production cannot safely mint a token for those keys without turning the CP into an AS signing
authority.

## Goals

- Complete the production `aud=node` credential path without giving the CP signing authority.
- Preserve strict audience separation between CP and node credentials.
- Bind node authorization to the persistent client proof-of-possession key established at login.
- Keep exact operation authorization in short-lived, single-use `SignedIntent` messages.
- Make clients verify the target node identity before authorizing placement or attachment.
- Support lifecycle provisioning, session open, session reauthentication, and revocation.
- Exercise the identical path in development and production.

## Non-Goals

- Requiring proof of possession on every ordinary CP RPC. CP-local calls remain `aud=cp` bearer
  authentication as decided in the existing auth design.
- Making AS availability part of every spawn operation or session attachment.
- Defining the transport carrying client-to-node bytes. Iroh connection binding and the encrypted
  CP fallback are `sp-dvke.4`; this spec defines the authorization carried inside any transport.
- Replacing GitHub OAuth, refresh-family semantics, or the client session-key algorithms.
- Granting the CP authority to mint, attenuate, or translate node credentials.

## Credential Model

The AS issues two short-lived access tokens for one login session:

```text
cp_access_token    audience = cp
node_access_token  audience = node
```

Both tokens:

- identify the same `account_id` and refresh family;
- are bound to the same persistent client P-256 session key through `session_key_hash`;
- use distinct random `token_id` values;
- have aligned issuance and expiry times;
- are self-describing root-anchored artifacts defined by `sp-dvke.1`;
- are included together in refresh idempotency and family revocation bookkeeping.

The `aud=cp` token is sent only to the CP. It remains ordinary bearer authentication for CP-local
operations. The `aud=node` token is sent only to a verified spawnlet, either directly or as an opaque
value relayed by the CP for provisioning.

A node token proves account identity and the public key authorized to act for that login session. It
does not authorize an operation by itself. A node accepts an operation only when a corresponding
`SignedIntent` proves possession of the bound private key and exactly describes the requested action.

## Issuance and Refresh

Every successful web OAuth callback, CLI loopback login, device grant, and refresh returns both access
tokens. The wire response names them explicitly; there is no ambiguous generic access-token field.

Web custody remains:

- `cp_access_token` and `node_access_token` in memory;
- refresh token in the AS HttpOnly cookie;
- persistent non-extractable P-256 key in IndexedDB.

CLI custody remains:

- both access tokens, rotating refresh token, and P-256 key in the protected auth-state directory;
- existing file locking and atomic replacement around refresh.

The AS refresh transaction mints both successors atomically. Its bounded replay cache stores and
returns the same complete token pair plus refresh successor. A partial response or retry cannot
advance one audience independently of the other.

Logout, family-reuse detection, account disable, and sign-out-everywhere revoke both audience token
IDs. Access tokens remain capped at the existing 15-minute lifetime.

## Persistent Client Signer

Every node-verified intent is signed by the persistent P-256 key registered with the AS for that
refresh family. SDKs receive a signer abstraction backed by the platform key store:

```text
SessionSigner
  PublicSPKIDER() -> bytes
  SignP1363(domain, exactBodyBytes) -> signature
```

The web implementation delegates to its non-extractable WebCrypto key. The CLI implementation loads
the protected key from auth state. SDK lifecycle and session functions never generate a replacement
key internally.

Before refresh or intent signing, clients positively verify that the stored key is available and can
sign. Key loss revokes the family where possible, clears local credentials, and requires a clean
login. There is no fallback to an ephemeral operation key.

## Target Node Verification

The CP supplies the target node certificate chain and resolved `target_node_id` with each pending
operation and session attachment description. It is an untrusted carrier of that material.

Before signing, the client verifies through the shared principal verifier from `sp-dvke.2`:

1. certificate chain to the locally pinned environment root;
2. one SPIFFE ID in the expected trust domain;
3. root-authorized node issuer role and matching `/node/<class>/...` path;
4. certificate validity and revocation state available to the client;
5. leaf node ID equals `target_node_id`;
6. cloud placement uses a valid `/node/cloud/<system-account>/<node>` identity; or
7. self-hosted placement uses `/node/self-hosted/<client-account>/<node>`.

The client rejects the pending operation before signing if any check fails. A compromised CP cannot
substitute an untrusted node, change node class, or route a user's self-hosted workload to another
account's node.

For direct transport, the same checks run on the certificate and proof bound to the live transport.
The proved identity must equal the target previously authorized in the intent.

## Lifecycle Authorization Flow

Lifecycle operations retain the existing two-phase sign-after-resolve protocol:

1. Client starts the CP operation using `aud=cp` and records its locally requested parameters.
2. CP commits or resolves spawn ID, generation, target node, immutable image/app references, model,
   mounts, and other execution fields.
3. Client polls `GetPendingIntent` and receives the resolved tuple plus target node chain.
4. Client verifies the node and validates all known tuple fields against its local pending request.
5. Client builds the exact operation-specific `IntentBody` with a fresh JTI and timestamp.
6. Persistent `SessionSigner` signs `domain || exact body bytes`.
7. Client calls `SubmitIntent` with the `aud=node` token and signed intent.
8. CP treats both values as opaque and forwards them unchanged in `AuthEnvelope`.
9. Node verifies the complete chain below before acting.

The CP may observe a relayed node token and signed intent, but it cannot create a new intent, change
any signed field, target another node, or reuse an accepted JTI. For provisioning, the CP already
controls delivery timing; replay protection prevents the observed authorization from becoming an
additional operation.

## Node Verification

The node performs these checks in order:

1. Decode the self-describing token envelope with strict limits.
2. Validate its signer certificate and intermediate to the environment root, including
   auth-artifact purpose and signer revocation.
3. Verify the token signature over exact payload bytes.
4. Require `audience == node`, current validity, and no user-session revocation.
5. Require the intent SPKI hash to equal `session_key_hash` in the token.
6. Verify the P-256 intent signature over the exact received body and operation domain.
7. Require intent operation, target node, spawn, generation, and every execution parameter to equal
   what the node is about to perform.
8. Enforce timestamp bounds, process-start floor, and JTI uniqueness.
9. Derive the authoritative account from the verified node token.

On create, the node stores the token account as the spawn owner and rejects a different or empty CP
owner assertion in enforced cloud mode. A self-hosted node additionally requires the token account to
equal the account in its own SPIFFE identity. Subsequent operations require the token account to equal
the stored spawn owner.

The CP assertion remains a correspondence check, not an identity authority.

## Session Open

A session-open intent covers at least:

```text
operation = session-open
spawn_id
generation
target_node_id
session_id
issued_at
jti
```

The client verifies the target node before signing. The node runs the full token and intent chain
before attaching the client to a session. On success it records:

- account ID;
- node token ID and expiry;
- client session-key fingerprint;
- spawn ID and generation;
- session ID;
- authenticated node identity.

Missing or invalid authorization is fatal in every normal development and production flow. The
current proceed-without-session-auth logging fallback is removed. Narrow unit-test fixtures may inject
an explicit verifier fake; they do not define a runtime mode.

Transport channel binding is added by `sp-dvke.4`. It extends the session intent without changing the
identity, token, owner, or proof-of-possession model defined here.

## Session Reauthentication

Long-lived sessions reauthenticate before the node token expires. Add a `session-reauth` intent
domain and body containing:

```text
spawn_id
generation
target_node_id
session_id
new node token ID
issued_at
jti
```

The client presents its refreshed `aud=node` token and signs the reauthentication intent with the
same persistent session key. The node verifies the full chain, requires the same account and key
fingerprint as the live session, then atomically replaces its token ID and expiry.

Reauthentication with another account, another key, a stale generation, or an expired token closes
the session. Clients begin reauthentication with sufficient margin before the 15-minute expiry;
failure never extends the old deadline.

## Revocation

Nodes consume the AS user-session revocation feed over the service-mTLS path from `sp-dvke.2` and
verify each self-describing entry under the root-anchored auth signer from `sp-dvke.1`.

Each node maintains an in-memory and persisted high-water mark and indexes live sessions by token ID
and account. A matching revocation closes live direct and relayed sessions and rejects later use.
CP consumption of the same feed closes CP-bound sessions sooner but is not authoritative for nodes.

AS unavailability does not immediately invalidate already verified tokens. If the feed is stale:

- existing tokens remain usable only until their signed expiry;
- no session can extend beyond expiry without a fresh token and signed reauthentication;
- restored feed processing resumes from the persisted monotonic sequence;
- rollback or invalid signatures are rejected.

This preserves offline verification with a maximum stale-revocation exposure bounded by token
lifetime. A future policy may fail closed earlier for high-security deployments, but is not required
for this design.

## Protocol Changes

- Login, device-token, and refresh responses return named CP and node access tokens.
- Refresh successor caching persists the pair atomically.
- `SubmitIntent.node_access_token` becomes required in every runtime mode.
- Session-open authorization requires the node token and persistent-key intent.
- Add the `session-reauth` intent domain and reauthentication control message.
- Pending intent/session metadata carries the target node certificate chain and typed expected
  identity information needed for client verification.
- Token revocation bookkeeping records both audience token IDs.

Generated Go and TypeScript clients change together. There is no legacy production wire fallback.

## Configuration and Code Removal

Remove:

- CP possession of a dev AS private key;
- CP `MintNode` calls and `CP_DEV_AS_KEY`;
- empty-node-token acceptance;
- per-operation key generation in Go and TypeScript intent helpers;
- session-open code that logs an authorization failure and proceeds;
- comments and fixtures describing CP node-token minting as production parity.

Keep `MintNode` as an AS-internal issuance function, renamed or relocated so only AS login/refresh
code can call it.

## Development and Migration

Development uses its ephemeral environment root and real AS process to issue paired tokens. Web,
CLI, CP, and node exercise the exact production flow. `NODE_AUTH_MODE=insecure` no longer disables
client authorization; explicit low-level unit tests use injected fakes instead.

Spawnery is pre-production, so the response and intent-key changes are flag-day changes. Existing
dev refresh families are disposable and require login after rollout.

## Testing

Hermetic tests cover:

- atomic paired issuance for OAuth, device, and refresh flows;
- aligned account, key binding, lifetime, and distinct audiences/token IDs;
- refresh idempotency returning the same complete successor pair;
- family/account revocation including both audience tokens;
- `aud=cp` rejection at nodes and `aud=node` rejection at CP;
- persistent web/CLI key signatures matching token `session_key_hash`;
- key loss and wrong-key recovery;
- target node chain, SPIFFE class, account, and node-ID verification before signing;
- CP substitution of every pending-intent and node identity field;
- node owner derivation, cloud asserted-owner correspondence, and self-hosted account matching;
- intent freshness, JTI replay, operation domain, target, generation, and parameter checks;
- session open and reauthentication success/failure matrices;
- revocation feed convergence, live close, persistence, rollback, and AS-outage expiry bound;
- absence of every CP mint and empty-token fallback.

The production VM lane proves web and CLI create, resume, migrate, fork, and session-open against an
enforced cloud node without `CP_DEV_AS_KEY`, raw AS key pins, or omitted node tokens. It also proves
logout closes an active session at the node and that AS unavailability cannot extend a session past
the last token expiry.

## Security Properties

- A stolen `aud=node` token alone cannot authorize an operation without the persistent client key.
- A node never receives a CP bearer that it can replay against CP APIs.
- A compromised CP cannot mint identity, choose a client key, alter an intent, or substitute an
  untrusted target without client detection.
- A compromised client key plus token is a full session compromise until revocation or expiry; this
  is the explicit proof-of-possession security boundary.
- Node authorization remains offline for the signed token lifetime.

## Rejected Alternatives

### Target-specific AS grants

They duplicate node, spawn, generation, and operation binding already supplied by `SignedIntent`, add
an AS request to every operation, and turn AS availability into a provisioning dependency.

### Reusing the CP token at nodes

Ordinary CP APIs accept bearer authentication. Giving that token to a node or relaying CP would allow
replay against CP and defeats audience isolation.

### Universal PoP token for every CP and node request

This is coherent but requires canonical request signing, nonces/replay state, and proof verification
across every CP RPC and WebSocket handshake. The existing design deliberately keeps CP-local calls
bearer-authenticated. Paired audiences close the actual cross-service replay without that expansion.

### CP-minted or CP-attenuated node credentials

Any CP signing authority restores the compromised-CP identity-forgery problem the Auth Service split
was created to remove.

### Per-operation client keys

They cannot prove possession of the key the AS authenticated at login and force another authority to
bless arbitrary operation keys.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged
from the assumptions above — append a dated note here, whether or not a formal debugging skill was
used.*
