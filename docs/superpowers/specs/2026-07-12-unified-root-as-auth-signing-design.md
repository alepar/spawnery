# Unified Root for Auth Service Authorization Signing

**Date:** 2026-07-12  
**Beads:** `sp-dvke.1` under `sp-dvke`  
**Status:** draft, collaboratively approved  
**Builds on:** [Node Auth & Unified Identity](2026-06-05-node-auth-unified-identity-design.md),
[Auth & Identity](2026-06-11-auth-identity-design.md), and its
[adversarial review](2026-06-12-auth-identity-adversarial-review.md)

## Problem

Spawnery currently has two independently provisioned trust anchors. Node certificates chain to the
environment's Spawnery root CA, while the Auth Service (AS) signs access tokens and revocation entries
with raw Ed25519 keys that every Control Plane (CP) and spawnlet pins separately. Routine rotation
requires coordinated verifier configuration, and compromise recovery relies on an incompletely
specified PKI carve-out. This violates the unified-auth invariant: one Spawnery root per environment
must be the only long-lived Spawnery trust anchor.

## Goals

- Make every AS authorization artifact verifiable from the environment's Spawnery root alone.
- Preserve offline verification at the CP and spawnlet.
- Keep Ed25519 artifact signatures and exact-byte, domain-separated signing.
- Allow routine signer rotation without coordinated verifier deployment.
- Prevent an online AS compromise from minting replacement signing authority.
- Give emergency signer invalidation a concrete, independently authorized path.

## Non-Goals

- Changing GitHub OAuth, refresh-token custody, client P-256 proof-of-possession keys, or
  `SignedIntent` semantics.
- Replacing public WebPKI for browser-facing HTTPS.
- Designing service mTLS (`sp-dvke.2`) or production `aud=node` issuance (`sp-dvke.3`).
- Sharing one private key between token signing, TLS, node identity, or any other role.

## Governing Invariant

Each environment has a distinct offline Spawnery root. The root is the only long-lived Spawnery
trust anchor installed in CPs, spawnlets, and native clients. Multiple purpose-limited
intermediates and leaves derive authority from that root. Development, staging, and production do
not share roots.

Each environment also has one corresponding SPIFFE trust domain, for example
`spiffe://prod.spawnery.internal`. Principal roles belong in the SPIFFE path, not in separate URI
authorities. This preserves the one-root/one-trust-domain model defined by the SPIFFE standard.

GitHub OAuth and public WebPKI remain external bootstrap and transport systems. They do not issue or
validate Spawnery identities and therefore are not additional Spawnery trust roots.

## PKI Hierarchy

Add a dedicated offline auth-artifact signing intermediate:

```text
Environment Spawnery Root (offline)
  -> Auth Artifact Signing Intermediate (offline)
       -> AS Auth Artifact Signer, current (online leaf key)
       -> AS Auth Artifact Signer, next    (online leaf key)
```

Existing cloud-node and self-hosted-node intermediates remain separate. The AS continues to hold the
self-hosted-node intermediate for node enrollment, but that intermediate cannot issue a valid
auth-artifact signer. Conversely, the auth-signing intermediate cannot issue a valid node or TLS
service identity.

### Signing leaf profile

The signing leaf uses an Ed25519 subject key and may be signed by the existing P-256 root hierarchy.
It contains:

- `KeyUsageDigitalSignature` only;
- no TLS client-auth or server-auth EKU;
- a Spawnery auth-artifact-signing policy OID;
- exactly one URI SAN, `spiffe://<environment>.spawnery.internal/signer/auth-artifact/<signer-id>`;
- a random 128-bit-or-larger serial number;
- bounded `NotBefore` and `NotAfter` values.

The verifier MUST check the Spawnery policy OID and exact identity URI in addition to ordinary X.509
chain validation. Merely chaining to the root is insufficient. A node, CP service, or unrelated code
signing certificate must not authorize tokens.

The initial operational default is a 90-day leaf lifetime, renewal beginning at 45 days, and at least
24 hours of current/next overlap. These values are operator policy, not wire commitments. Expiry
alarms are mandatory well before the renewal threshold.

## Self-Describing Artifact Envelope

Replace the current `base64url(body).base64url(sig)` token with one base64url-encoded protobuf
envelope:

```protobuf
message SignedAuthArtifact {
  string artifact_type = 1;       // session-token | revocation-entry | future registered type
  bytes payload = 2;              // exact serialized payload bytes
  bytes signature = 3;            // Ed25519(domain || payload)
  repeated bytes signer_chain = 4; // DER: leaf first, then intermediate(s); root omitted
  bytes key_id = 5;               // SHA-256 of leaf SPKI; cache selector only
}
```

The existing payload protobufs remain unchanged except where a later subepic explicitly changes
their semantics. Each artifact class retains a fixed versioned domain, for example
`spawnery/session-token/v1` and `spawnery/revocation/v1`. `artifact_type` must map to exactly one
compiled-in domain; a caller-supplied arbitrary domain is never accepted.

`key_id` is derived from the leaf SPKI. It selects a validation cache entry and appears in logs and
metrics. It grants no authority and must never select a separately pinned raw key.

The root certificate is omitted from the envelope. Verifiers use their locally installed root, so an
attacker cannot substitute a new trust anchor in-band.

## Verification

CP and spawnlet verification follows this order:

1. Decode the envelope with strict size and field-count limits.
2. Map `artifact_type` to a compiled-in domain and expected payload type.
3. Parse the leaf and intermediate certificates; reject malformed, duplicate, or unexpected chains.
4. Verify the chain to the locally pinned environment root at the current time.
5. Require the auth-artifact policy OID, exact signer identity URI, Ed25519 subject key, and digital
   signature usage; reject TLS or node identities.
6. Check signer revocation state.
7. Confirm `key_id == SHA-256(leaf SPKI)`.
8. Verify the Ed25519 signature over `domain || exact payload bytes`.
9. Parse the payload only after the signature succeeds, then apply its existing audience, expiry,
   owner, and revocation rules.

Chain validation results may be cached by `(root fingerprint, leaf fingerprint)` until the earliest
of leaf expiry, intermediate expiry, or signer-revocation-state generation change. Payload signature,
audience, expiry, and user-session revocation checks are never cached across requests.

Malformed or unknown envelopes fail closed. Production has no fallback to raw public-key pins.

## Rotation

### Routine rotation

1. In an offline ceremony, issue the next AS signing leaf from the auth-signing intermediate.
2. Provision its private key and chain to the AS as `next`; the AS proves the private key matches the
   issued leaf before accepting it.
3. Begin signing new artifacts with `next` while retaining `current` for an overlap of at least the
   maximum artifact lifetime plus clock-skew budget.
4. Stop using `current`; retain its public chain only until all artifacts it signed have expired.
5. Remove the old private key and update offline escrow records.

Because every artifact carries its chain, CPs and spawnlets need no signer configuration rollout.

### Emergency invalidation

User-session revocation and signer compromise are different mechanisms. The ordinary AS-signed
revocation feed cannot revoke its own compromised signer.

The offline auth-signing intermediate therefore signs a monotonic signer-revocation statement
containing environment, generation, issue time, revoked certificate serials and SPKI fingerprints,
and an optional minimum accepted signer `NotBefore`. Operators deploy this statement to every CP and
spawnlet through the normal configuration/deployment channel. Verifiers persist the highest accepted
generation and reject rollback.

Emergency response is:

1. Issue and provision a replacement leaf using the offline intermediate.
2. Publish a higher-generation revocation statement for the compromised leaf.
3. Deploy it to CPs and spawnlets and verify fleet convergence.
4. Revoke affected refresh families and require clients to reauthenticate.

There is deliberately no online introspection or online signing intermediate. Immediate compromise
response still requires fleet distribution, as it does today, but the distributed object is signed
authority derived from the existing root rather than a new raw-key pin.

## Configuration Changes

Remove:

- `CP_AS_SESSION_PUBKEYS` / `auth.as_session_pubkeys`;
- `NODE_AS_PUBKEYS` / `node.as_pubkeys`;
- current/next raw AS public-key bundle loading.

Add:

- AS current/next signing key and certificate-chain paths;
- CP and spawnlet signer-revocation statement paths;
- the existing environment root path remains the sole verifier trust anchor.

The AS startup check must prove each configured private key matches its leaf, validate the chain and
purpose against its configured root, and refuse to start on an expired or wrong-environment leaf.

## Migration

Spawnery is pre-production, so this is a flag-day wire and configuration change. Generated clients,
CP, AS, and spawnlets move together. Production mode never accepts both raw-key tokens and
certificate-envelope tokens. Dev mode generates an ephemeral root, offline-equivalent intermediate,
and leaf so it exercises the identical envelope and verification path; only persistence differs.

## Testing

Hermetic tests must cover:

- valid P-256-root to P-256-intermediate to Ed25519-leaf chain and artifact signature;
- wrong root, environment, identity URI, policy OID, key usage, algorithm, chain order, and expiry;
- a node or service leaf attempting to sign an auth artifact;
- payload, artifact type, signature, leaf, intermediate, and `key_id` substitution;
- current/next overlap and old-artifact verification after signer switch;
- signer-revocation statement signature, monotonic generation, persistence, rollback rejection, and
  cache invalidation;
- user-session revocation remaining distinct from signer revocation;
- production refusal of legacy raw-key tokens;
- Go/TypeScript envelope vectors where clients need to inspect token metadata.

The production VM lane must prove that CP and spawnlet start with only the environment root and can
verify the same AS token without raw AS public-key files.

## Security Properties

- Compromising a CP or spawnlet yields no issuance authority.
- Compromising the AS signing leaf allows artifact forgery only until fleet receipt of the offline
  revocation statement or leaf expiry; it does not allow minting replacement signing certificates.
- Compromising the online self-hosted-node intermediate cannot mint auth-artifact signers.
- A valid certificate for another Spawnery role cannot cross purpose into authorization signing.
- Routine signer rotation cannot silently replace the environment root.

## Rejected Alternatives

### Separately pinned raw signer keys

This is the current second trust-anchor distribution mechanism and directly violates the governing
invariant.

### Root-signed active-signer bundle

This preserves one cryptographic root but retains coordinated bundle distribution for routine
rotation. Self-describing artifacts remove that operational coupling at acceptable token size.

### Online auth-signing intermediate

Automatic short-lived leaf issuance is operationally attractive, but compromise of its online issuer
permits indefinite replacement authority. The AS receives leaves, never an issuer.

### Reusing the AS TLS service key

Cross-protocol key reuse couples service compromise, token forgery, certificate rotation, and outage
domains. Separate leaves under one root are cheap and intentional.

### Online token introspection

It makes AS availability part of every CP and node authorization decision and removes the existing
offline-verification property.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged
from the assumptions above — append a dated note here, whether or not a formal debugging skill was
used.*
