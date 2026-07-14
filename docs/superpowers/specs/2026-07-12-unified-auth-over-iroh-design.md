# Unified Authentication over Iroh

**Date:** 2026-07-12
**Beads:** `sp-dvke.4` under `sp-dvke`; folds in `sp-4n9` and `sp-zdd`
**Status:** draft, collaboratively approved
**Builds on:** [Unified Root for AS Authorization Signing](2026-07-12-unified-root-as-auth-signing-design.md),
[Unified Service mTLS](2026-07-12-unified-service-mtls-design.md),
[Production Client-to-Node Authorization](2026-07-12-production-client-node-authorization-design.md),
and Iroh research recorded by `sp-9693` at commits `a5ee702`, `1a38961`, and `e613b39`.

## Problem

Iroh can remove the Control Plane (CP) from the client-to-node streaming path and provide direct
QUIC connectivity with encrypted relay fallback. Its `EndpointId`, however, is an Iroh raw public
key rather than a Spawnery PKI identity. Treating it as authoritative would create a second trust
chain beside the environment Spawnery root. The legacy CP relay also carries plaintext application
frames today, so retaining it as a fallback would let a compromised CP observe or modify sessions.

The transport must therefore reuse the same Spawnery node identity, client token, and signed intent
on every path. Iroh and CP relay may provide different encryption mechanisms, but neither may become
an identity authority.

## Goals

- Keep the environment Spawnery root as the only long-lived Spawnery trust anchor.
- Authenticate the live Iroh peer as the exact root-verified node authorized by the client.
- Use random Iroh EndpointIds solely as routing and connection-binding values.
- Support native direct/hole-punched Iroh and encrypted Iroh relay without changing authorization.
- Support browsers through Iroh WASM over relay WebSockets without implementing TLS over a browser
  byte stream.
- Make any enabled CP relay fallback confidential and integrity-protected end to end.
- Prevent authentication, authorization, or confidentiality failures from causing downgrade.

## Non-Goals

- Making Iroh EndpointIds stable application identities or issuing certificates for them.
- Giving the Rust Iroh sidecar the node's Spawnery certificate private key.
- Requiring the CP or an Iroh relay to inspect client tokens, intents, or application plaintext.
- Building a second Iroh-specific authorization grant or endpoint allowlist authority.
- Providing perfect forward secrecy for the transitional CP fallback. Its exposure window is bounded
  by the short-lived node HPKE subkey and prompt deletion; the Iroh path retains Iroh's native
  transport forward secrecy.
- Supporting silent fallback after any security check fails.

## Architecture

The per-node Rust sidecar selected by the `sp-9693` spikes remains the Iroh transport adapter. It
owns a random Iroh endpoint, accepts the Spawnery ALPN, and bridges reliable byte streams to spawnlet
over a permission-restricted Unix socket. It passes connection metadata including both EndpointIds
and ALPN to spawnlet. It holds no Spawnery identity key and makes no application authorization
decision.

Spawnlet owns the node certificate key and all authentication logic. On every new Iroh connection it
signs a fresh connection transcript. The client verifies that proof against the environment root and
then binds its existing `aud=node` token plus `SignedIntent` to the transcript hash. This makes a
successful Iroh handshake necessary but insufficient: Iroh identifies a transport endpoint, while
Spawnery identifies and authorizes the node and client.

Native clients may establish direct or hole-punched Iroh paths and transparently use an Iroh relay
when direct connectivity is unavailable. Browser clients use Iroh WASM over relay WebSockets. These
are path changes inside one authenticated Iroh connection and do not require reauthorization unless
Iroh creates a new connection.

The legacy CP relay is a separately negotiated fallback. It uses the already implemented,
certificate-signed rotating X25519 node subkey to deliver a fresh session secret, then protects every
application frame with directional AES-256-GCM keys. The node certificate, node token, intent, and
authorization rules remain identical to the Iroh path.

## Iroh Connection Proof

The client begins the Spawnery protocol on an established Iroh stream with a `ClientHello` containing
a fresh 32-byte client nonce and the intended session ID. Spawnlet obtains the Iroh metadata from the
sidecar, generates a fresh 32-byte node nonce, and returns:

```text
NodeProof {
  protocol_version
  alpn
  client_endpoint_id
  node_endpoint_id
  client_nonce
  node_nonce
  session_id
  node_certificate_chain
  signature
}
```

The signature covers a canonical, length-prefixed encoding with domain
`spawnery/iroh/channel-proof/v1`. EndpointIds have no meaning outside this transcript. The client:

1. requires exact protocol version, ALPN, EndpointIds, nonce, and session correspondence;
2. validates the certificate chain to its locally pinned environment root;
3. applies the node issuer-role, SPIFFE trust-domain, class, account, node-ID, validity, and
   revocation checks from `sp-dvke.2` and `sp-dvke.3`;
4. verifies the transcript signature with the node leaf public key; and
5. hashes the complete signed transcript as the channel-binding value.

The client then signs the session intent from `sp-dvke.3` with that channel-binding value included.
Spawnlet accepts the session only if the `aud=node` token, persistent-key proof, exact operation,
target node, spawn generation, session ID, and channel binding all verify. A copied proof cannot be
used on another connection because both fresh nonces and both live EndpointIds are covered.

The same binding is retained across Iroh's direct-to-relay path migration. A replacement Iroh
connection performs a new proof and a new session-open or session-reauth intent before application
traffic resumes.

## Encrypted CP Fallback

The CP may advertise fallback availability and relay opaque frames, but it does not select keys or
assert identity. Before opening the fallback, the client obtains the target node certificate chain
and current signed X25519 HPKE subkey. It verifies the certificate and subkey using the shared
root-anchored verifier. The existing owner-sealed-secrets suite is reused at the primitive level:

```text
DHKEM(X25519, HKDF-SHA256) + HKDF-SHA256 + AES-256-GCM, HPKE Base mode
```

Transport code uses a distinct `spawnery/transport/cp-relay-bootstrap/v1` HPKE domain and envelope;
secret-delivery ciphertext cannot be replayed as a transport bootstrap. The client generates a
fresh 32-byte session master secret and HPKE-seals it to the verified node subkey. Bootstrap AAD
covers protocol version, target SPIFFE ID, node certificate fingerprint, subkey ID and expiry,
spawn ID, generation, session ID, and a fresh client nonce.

Spawnlet opens the bootstrap only with an unexpired retained subkey, derives directional traffic
keys and nonce prefixes with HKDF-SHA256, deletes the recovered master secret after derivation, and
returns an encrypted key-confirmation frame containing a fresh node nonce. Both sides hash the full
bootstrap plus confirmation transcript to obtain the fallback channel-binding value. The client
then sends its channel-bound node token and intent inside the encrypted channel.

Every subsequent frame uses AES-256-GCM with:

- independent client-to-node and node-to-client keys;
- a derived four-byte nonce prefix plus an unsigned 64-bit sequence number;
- AAD covering protocol version, channel binding, direction, sequence, and frame type; and
- exact-next sequence enforcement on the ordered relay stream.

Duplicate, skipped, reordered, unauthenticated, or counter-overflowing frames close the channel.
Subkey rotation retains only the existing bounded overlap needed for in-flight bootstrap, then
deletes old private halves. Compromise of a retained node subkey can decrypt recorded bootstrap
messages made to that subkey, so this fallback does not claim perfect forward secrecy. Its purpose
is a secure transitional availability path, not a peer transport to preserve indefinitely.

## Path Selection and Downgrade Rules

Clients attempt Iroh first. Native clients may use direct or Iroh relay; browsers use Iroh relay.
The CP fallback is eligible only when Iroh cannot establish a transport within the configured
connection budget and the deployment explicitly enables encrypted CP fallback.

The following failures are terminal and never make CP fallback eligible:

- certificate, SPIFFE identity, issuer-role, or revocation failure;
- Iroh transcript mismatch or bad node signature;
- node-token or intent verification failure;
- HPKE subkey, bootstrap, key-confirmation, or AEAD failure; or
- target node, spawn, generation, session, or channel-binding mismatch.

Clients record the selected path and a reason code without logging certificates beyond fingerprints,
tokens, intents, session secrets, plaintext, or raw encrypted payloads. Deployments that do not ship
the encrypted CP protocol must disable CP session relay in production rather than retain plaintext
fallback.

## Discovery and Lifecycle

CP session metadata carries, as untrusted routing material:

- target node certificate chain and expected typed SPIFFE identity;
- current Iroh EndpointAddr/EndpointId and protocol ALPN;
- current signed HPKE subkey for optional CP fallback; and
- the spawn ID, generation, and session ID already required for intent signing.

The client verifies all identity material independently. EndpointId rotation requires only refreshed
routing metadata because EndpointIds are not allowlisted identities. Node certificate or HPKE subkey
rotation follows the overlap and revocation rules of the unified PKI designs.

Session reauthentication uses the existing `session-reauth` intent and the current channel binding.
Reconnection establishes a new binding first. Node-token revocation closes sessions on both paths;
transport selection does not change revocation behavior.

## Failure Boundaries

- The Iroh sidecar may restart or lose routing state without gaining identity authority. Existing
  connections fail and reconnect through a fresh Spawnery proof.
- CP compromise can suppress, delay, reorder, or drop fallback frames and can lie about routing. It
  cannot read or forge accepted payloads, substitute a node, or alter an intent.
- Iroh relay compromise exposes connection metadata and can deny service, but not session plaintext
  or a Spawnery-authenticated identity.
- Node certificate-key compromise impersonates that node on either transport and is handled by the
  unified certificate revocation path. No separate EndpointId revocation system exists.
- AS unavailability follows the bounded offline token and revocation behavior in `sp-dvke.3`; it
  does not cause transport downgrade.

## Testing and Acceptance

### Protocol and crypto tests

- Go and TypeScript golden vectors for canonical Iroh transcript bytes, signature verification, and
  channel-binding hashes.
- Go and TypeScript golden vectors for fallback HPKE bootstrap, HKDF traffic derivation, nonces, AAD,
  AES-GCM frames, and key confirmation.
- Negative vectors for every changed transcript field, wrong root/role/SPIFFE identity, expired or
  revoked cert/subkey, wrong direction or sequence, replay, truncation, and corrupted ciphertext.
- Tests proving secret-delivery HPKE envelopes and transport-bootstrap envelopes are not
  interchangeable.

### Integration tests

- Native client to node over direct Iroh, forced Iroh relay, and direct-to-relay path migration.
- Browser Iroh WASM to node over relay WebSocket with the same certificate proof and intent.
- Encrypted CP fallback with CP logging/inspection assertions that plaintext is absent.
- Forced transport timeout permits configured fallback; every security failure refuses fallback.
- Reconnect, node-token refresh, `session-reauth`, revocation, node certificate rotation, EndpointId
  rotation, and HPKE subkey rotation during bootstrap.
- A malicious CP substitutes endpoint, certificate, subkey, intent fields, or ciphertext and every
  attempt fails before application traffic is accepted.

### Operational gates

- `sp-9693.4` validates native cross-NAT hole punching plus relay/firewall behavior before GA. The
  former runsc subcase is not load-bearing because the per-node Iroh sidecar is host-side.
- Production configuration cannot enable plaintext CP session relay.
- Metrics distinguish Iroh direct, Iroh relay, encrypted CP fallback, transport failure, and security
  rejection without exposing secrets.

## Rollout

1. Ship the node proof and channel-bound intent over Iroh behind a client/node capability bit.
2. Validate native direct, forced relay, browser WASM relay, reconnect, and revocation paths.
3. Ship encrypted CP fallback under a separate capability bit and complete `sp-4n9`/`sp-zdd`.
4. Enable Iroh-first selection. Permit CP fallback only where both peers advertise the encrypted
   protocol.
5. Remove or hard-disable the plaintext CP relay before production rollout.

There is no mixed-auth compatibility mode: an old peer lacking Spawnery connection proof or
encrypted fallback cannot carry a production client-to-node session.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged
from the assumptions above — append a dated note here, whether or not a formal debugging skill
was used.*
