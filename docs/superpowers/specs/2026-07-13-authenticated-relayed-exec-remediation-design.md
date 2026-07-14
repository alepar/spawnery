# Authenticated Relayed Exec and Production Authorization Remediation

**Date:** 2026-07-13  
**Status:** approved  
**Bead:** `sp-dvke.3.7`  
**Amends:** [Production Client-to-Node Authorization](2026-07-12-production-client-node-authorization-design.md)  
**Adjacent:** [Unified Authentication over Iroh](2026-07-12-unified-auth-over-iroh-design.md)

## Goal

Close the remaining production authorization gaps without creating another trust chain.

The concrete security defect is the CP-attached spawnlet's public `/exec` and `/terminal` HTTP
surface. `spawnctl exec -spawn ...` currently bypasses stored credentials and sends an unauthenticated
request directly to that surface. In the production VM it is bound to `0.0.0.0:9092`; possession of a
spawn ID is therefore enough to execute a command in another user's spawn.

The replacement uses the existing root-anchored `aud=node` token, persistent client proof-of-possession
key, target-node verification, and node-enforced owner/generation checks. CP is a carrier, not an
authorization authority. The same application authorization envelope remains valid when Iroh replaces
CP as the carrier.

This amendment also closes the configuration, CRL-profile, destructive-boundary, cleanup, and real-client
evidence gaps found during final review of `sp-dvke.3.7`.

## Decisions

### 1. Exec uses an authenticated application channel

Add a distinct `exec-open` intent domain under the existing signed-intent scheme. It covers:

```text
operation = exec-open
spawn_id
generation
target_node_id
session_id
argv (ordered, exact strings)
issued_at
jti
```

The client obtains the current spawn generation and node certificate chain from CP, verifies the target
against the pinned root and current CRL bundle, obtains its paired `aud=node` token, and signs the intent
with its stored P-256 session key. No new credential, certificate, pin, or shared secret is introduced.

The command is present in the signed body rather than represented by an informal hash convention. The
node compares the exact received argv with the signed argv before starting a process. This prevents CP or
another carrier from substituting the command while keeping the correspondence check straightforward.

### 2. CP relays exec through the existing session stream

The first `cp.v1.Frame` selects an explicit `session_id`, carries the `exec-open` authorization envelope,
and carries the exec argv. CP performs its existing caller/spawn ownership check and forwards a
`node.v1.SessionOpen` containing the same values. It does not mint, resign, or reinterpret authorization.

The node performs the full token, target, owner, generation, intent-signature, freshness, and replay
checks before invoking `Manager.ExecStream`. Stdout, stderr, and exit status use the existing
`internal/execstream` typed frame encoding inside the opaque session data bytes. After the exit frame is
delivered, the node closes that attachment and CP completes the client stream.

`spawnctl exec` must load the paired credentials from its isolated or normal config directory. Supplying
only a CP token is a fail-closed error. The direct node address is no longer part of the production exec
contract.

### 3. Direct terminal HTTP is not a production trust path

In CP-attached `node.auth_mode=enforced` mode, spawnlet does not register or bind `/exec` or `/terminal`.
The production listener on `0.0.0.0:9092` is removed from deployment wiring and rejected by topology
checks. This fixes both unauthenticated command execution and the equivalent interactive-shell exposure.

Standalone development mode may retain the current local HTTP handlers. They are explicitly outside the
production trust boundary and must not be reachable in an enforced CP-attached deployment.

`spawnctl attach` and `spawnctl shell` fail closed when pointed at an enforced CP-attached deployment until
their interactive byte transport is moved behind the same authenticated session-open envelope. We do not
add a temporary bearer header or a second TLS/client-certificate scheme merely to preserve those legacy
paths.

### 4. Carrier independence is mandatory

Authorization is complete before transport selection:

1. verify the root-anchored node identity and revocation state;
2. obtain the paired `aud=node` token;
3. sign the exact operation intent with the persistent client key;
4. let the destination node verify all three artifacts and operation correspondence.

CP relay and Iroh may provide confidentiality and peer reachability, but neither establishes Spawnery
identity. Iroh EndpointIds and transport certificates remain routing/channel facts, not a second chain of
trust. The future Iroh exec path carries the same `exec-open` envelope and execstream bytes.

## CRL Verifier Parity

The browser verifier must implement the same canonical complete-CRL profile as
`internal/pki.VerifyCRL`, not merely verify the signature and leaf serial. It must require:

- a non-delegating role-bearing intermediate CA with the exact certificate-signing and CRL-signing key
  usage, valid identity, trust-domain URI SAN, and subject key identifier;
- an exact issuer match and authority key identifier match;
- exactly the non-critical authority-key-identifier and positive CRL-number extensions;
- a positive CRL number of at most 20 DER octets;
- `thisUpdate < nextUpdate`, both inside issuer validity, current at the supplied clock;
- only positive, unique revoked serials with revocation times no later than `thisUpdate`;
- no entry extensions, delta CRL, indirect CRL, unknown extensions, or critical extensions;
- a valid issuer signature before checking whether the leaf is revoked.

Tests use deterministic DER/PEM vectors and include negative cases for delta and indirect CRLs, AKI
mismatch, absent/invalid CRL number, unsupported extensions, invalid issuer profile, invalid update
windows, and malformed or duplicate entries. Browser and Go acceptance must agree on every vector.

## Acceptance Configuration and Destructive Boundaries

Split VM acceptance configuration into:

- base production-client configuration required by ordinary lifecycle and authorization specs;
- destructive root-artifact configuration requiring `ACC_DESTRUCTIVE_DEV_TOKEN` and
  `ACC_E2E_VM_RUNID`.

Ordinary production and manual non-destructive runs must not require destructive-only variables.
Destructive helpers receive the destructive configuration type. `deployCurrentRevocation` itself calls
`assertDisposableVM` immediately before changing VM trust state, even if its caller already checked.
The check is deliberately duplicated at the destructive boundary because time and refactoring both
exist.

## Real-Client Negative Evidence

Both the deployed SPA and the real stored-credential `spawnctl` process must refuse each of these before
`SubmitIntent` and before any runtime object exists:

- a structurally valid node chain rooted in a different CA;
- a valid same-root node chain whose issuer has no stamped CRL;
- a valid same-root node chain whose stamped CRL is stale.

The test proxy substitutes only the public target material returned by `GetPendingIntent` or
`GetSpawnNodeKey`; it never exports or injects a client private key. The harness records zero
`SubmitIntent` calls and verifies the rejected generation created no pod/container. The stale-CRL browser
case uses a bounded page-local clock advanced beyond the stamped `nextUpdate`; host and cleanup clocks are
unchanged.

## Cleanup and Failure Semantics

Revocation lifecycle cleanup attempts every recorded deletion. Each deletion failure is retained. The
test's primary failure, auth-service restoration failure, and all cleanup failures are returned in one
`AggregateError`. Cleanup-only failure therefore fails the spec, while a primary failure is never masked
by restoration or cleanup noise.

## Test Strategy

Focused gates:

- protocol generation and breaking-change checks;
- Go unit and integration tests for client intent construction, CP session routing, router session IDs,
  node correspondence/replay/owner enforcement, execstream completion, and enforced listener wiring;
- spawnctl tests proving stored paired custody and rejection of CP-only or absent node authorization;
- browser CRL vector parity tests and target-verifier tests;
- acceptance unit tests for base/destructive configuration, destructive helper self-gating, negative
  substitutions, and aggregated cleanup errors;
- shell topology and fail-closed runner tests.

Final evidence is one fresh, unfiltered production VM run:

```bash
GOLDEN_IMAGE=/var/lib/libvirt/images/spawnery-e2e/golden.qcow2 \
  scripts/e2e-vm/run.sh --profile fake
```

No `--no-build`, cache reuse, filtered project, mocks, retries, or retained VM are acceptable substitutes.
The run must complete with clean teardown, followed by fresh specification-compliance and code-quality
reviews of the full `sp-dvke.3.7` range.

## Superseded Claims

The direct unauthenticated production HTTP design in
[spawnctl exec - Non-Interactive Runner](2026-06-19-spawnctl-exec-noninteractive-design.md) is superseded
for CP-attached enforced nodes. Its execstream payload format remains useful; its transport and
authorization posture do not.
