# Per-Spawn SPIFFE mTLS for the Node-Sidecar Control Channel

**Date:** 2026-07-14
**Beads:** `sp-dvke.6.1` under `sp-dvke.6`
**Status:** draft; governing decisions collaboratively approved
**Builds on:** [Unified mTLS for CP, Auth Service, and Spawnlets](2026-07-12-unified-service-mtls-design.md),
[Unified Root for AS Authorization Signing](2026-07-12-unified-root-as-auth-signing-design.md),
[Spawnlet Restart Re-adoption](2026-07-12-spawnlet-restart-readoption-design.md), and
[Invert the GitHub Control Channel](2026-07-13-github-control-channel-inversion-design.md)

## 1. Problem

The node reaches each spawn sidecar over the pod bridge. That control channel carries the model API
key, the per-spawn GitHub MITM CA private key, and the GitHub access token. It is currently HTTP
authenticated by `SIDECAR_CONTROL_TOKEN`, so the bearer prevents an unauthenticated write but does not
encrypt any of those values. The agent is a separate, untrusted container in the same network namespace.
Its reduced capability set makes passive capture and ARP spoofing harder, but that is a mitigation, not
an authenticated confidentiality boundary.

The bearer also creates restart state: a new spawnlet must recover `SIDECAR_CONTROL_TOKEN` from the live
sidecar's environment before it can re-adopt the pod. More importantly, neither a bearer nor a source IP
proves that the pod at a recycled address is the spawn the node intended to reach.

The fix must not create another trust root or an independent application-layer credential system.
Spawnery will use the existing environment root and SPIFFE trust domain for both ordinary service mTLS
and this pod-local channel.

## 2. Goals and non-goals

### Goals

- Encrypt and mutually authenticate every request on the node-sidecar control listener.
- Bind the server identity to the exact spawn ID and generation expected by the node.
- Bind the client identity to the exact parent node named by the sidecar's own identity.
- Keep one environment root as the only Spawnery trust anchor.
- Remove `SIDECAR_CONTROL_TOKEN`, bearer middleware, and environment recovery during re-adoption.
- Preserve the current control operations: model changes, live credential delivery, GitHub credential
  delivery and rejection events, and inference-idle status.
- Rotate long-lived workload SVIDs without restarting the sidecar or exposing material to the agent.
- Fail closed at creation, rotation expiry, and re-adoption.

### Non-goals

- SPIRE or the SPIFFE Workload API.
- A general identity system for arbitrary agent processes or additional containers.
- Defending a sidecar from the root-equivalent host that created and runs it. The boundary addressed here
  is an untrusted agent or wrong pod on the shared pod network, plus accidental cross-spawn/node routing.
- Changing the public inference listener on `SIDECAR_ADDR`; the agent must continue to call that listener.
- Moving GitHub, model, or user-secret custody away from their existing owners.
- Replacing the node's existing X.509-SVID or its rotation path.

## 3. Decisions and rejected alternatives

### Selected: an AS-held, root-signed workload issuer

The offline environment root signs one non-delegating `spawn-workload-issuer` intermediate. The Auth
Service holds that intermediate certificate and key online. A node requests a leaf over its existing
node-to-AS mTLS connection; the AS derives all node fields from the verified client principal and asks
the CP to confirm the exact live reservation before it signs.

This puts the AS on initial spawn creation and workload renewal. That is a deliberate availability cost:
the AS is already the online identity authority, and central issuance avoids giving every node CA power.
A long leaf lifetime and wide renewal lead keep an AS outage from disrupting running spawns.

### Rejected: a subordinate CA on every node

URI name constraints constrain a URI host, not a path prefix. They cannot limit a node issuer to
`/spawn/<that-node>/...`. Spawnery could add a verifier policy for a per-node issuer role, but a
compromised node would still possess an online CA key and every verifier would need to correlate that CA
to a node. Central issuance is smaller and keeps signing authority out of the node fleet. No URI name
constraint or per-node CA will be introduced.

### Rejected: random transport certificates plus an inner secure channel

An ephemeral TLS channel followed by a separate signed or encrypted application handshake would create
two identity mechanisms and two protocol state machines. It would still need to bind the inner identity
to the spawn reservation. A workload SVID gives TLS the exact identity it needs under the existing root,
so an inner channel would add machinery without another security property.

## 4. Identity and PKI profile

### 4.1 Canonical identity

The sidecar's canonical SPIFFE ID is:

```text
spiffe://<td>/spawn/<node-class>/<node-account>/<node-id>/<spawn-id>/<generation>
```

`<node-class>` is exactly `cloud` or `self-hosted`. `<node-account>` is copied from the verified node
principal; for a cloud node this remains the reserved system account, not the spawn owner's account.
`<generation>` is the canonical unsigned base-10 form with no sign or leading zero and must be non-zero.
All other values obey the existing non-empty, unescaped SPIFFE path-segment rules. Every comparison is
field-for-field after typed parsing; no prefix, wildcard, normalization, or percent-decoding is allowed.

`internal/pki.Principal` gains a spawn kind and the fields `SpawnID` and `Generation`. The node fields on
a spawn principal remain the same typed `Role`, `AccountID`, and `NodeID` used by a node principal. Code
outside `internal/pki` and `internal/mtls` must not parse the URI itself.

### 4.2 Issuer role and leaf profile

The existing environment root signs this additional intermediate:

```text
Environment Spawnery Root (offline; unchanged)
  -> Spawn Workload Intermediate (online at AS; MaxPathLen=0)
       -> per-spawn sidecar leaves
```

The intermediate carries exactly one new Spawnery policy OID for the
`spawn-workload-issuer` role, the trust-domain URI SAN with no path, `CA=true`, certificate-signing and
CRL-signing key usages, and `MaxPathLen=0`. It has no URI path name constraint. The shared verifier
accepts this role only for the exact `/spawn/...` grammar. Existing service and node issuers cannot issue
an accepted spawn identity, and the workload issuer cannot issue an accepted service or node identity.

Each workload leaf has:

- one ECDSA P-256 public key generated by the node;
- exactly one URI SAN containing the canonical spawn identity;
- `CA=false`, `KeyUsageDigitalSignature`, and exactly the server-auth EKU;
- no DNS, IP, email, subject-CN identity, or client-auth authority;
- a random 128-bit-or-larger serial;
- a one-year validity, with `NotBefore` allowing the existing bounded clock-skew policy.

The server-auth-only profile is intentional. The sidecar is the TLS server. The node authenticates with
its existing node SVID and therefore receives no second leaf or private key. Verification continues to
require ordinary RFC 5280 path validation, exact profile validation, issuer-role/path correspondence,
the configured trust domain, and revocation.

## 5. Issuance and reissuance

### 5.1 Node to AS API

Add an internal ConnectRPC procedure, `IssueSpawnWorkloadSVID`, to the AS node-only policy. Its request
contains only:

```text
request_id
spawn_id
generation
csr_der
```

It deliberately contains no node class, account, node ID, requested identity, lifetime, or issuer. The
AS obtains the typed node principal from the mTLS request context. The CSR must be a valid P-256 CSR whose
signature verifies and whose requested extensions, SANs, and subject do not assert identity. The AS
builds the certificate template itself. Requests have strict field and body limits.

The response contains the leaf DER, the workload intermediate DER, canonical SPIFFE ID, serial/fingerprint,
and `not_before`/`not_after`. It never includes the environment root or a private key. The node verifies
the complete response against its locally installed root and the exact expected principal before writing
it to disk. A retry may produce another valid serial for the same CSR and principal; security and
rotation logic do not depend on `request_id` deduplication.

The same API handles initial issuance and routine reissuance. There is no unauthenticated bootstrap,
renewal token, or possession of an old workload key as authority.

### 5.2 AS to CP reservation confirmation

Before every signature, the AS calls a new CP internal procedure, `AuthorizeSpawnWorkloadSVID`, over the
existing AS-service-to-CP mTLS client. The request contains the AS-derived node class, node account, node
ID, spawn ID, generation, and CSR SPKI digest. The CP route permits only an `authsvc` service principal.

The CP confirms atomically from its authoritative ledger that:

1. the spawn ID and generation are current;
2. the generation is reserved to or actively hosted by that exact node;
3. the node class and node account match the registered node principal; and
4. the spawn is in a state where a sidecar exists or is about to be created (`starting` or `active`).

Unknown, stale, suspended, deleted, differently placed, or unreserved tuples are denied. CP timeout or
unavailability is retryable and the AS does not sign. A denial is not converted into a new reservation.
The SPKI digest is included in audit correlation; the CP does not approve a caller-supplied identity.

The initial node call occurs after the CP has committed placement/generation and before `StartPod`.
Renewal occurs only while that generation remains live. The check cannot constrain a fully compromised
AS that already holds the workload issuer key; it prevents confused-deputy and stale-node issuance in the
honest service and gives the CP an authoritative audit point.

## 6. Key custody and injection

The node generates the workload private key. It is never generated by, returned from, or stored at the
AS or CP. The node writes one per-incarnation material directory below its existing protected state root:

```text
workload-svid/<spawn-id>/<generation>/
  versions/<serial>/tls.crt
  versions/<serial>/tls.key
  versions/<serial>/root.pem
  versions/<serial>/node-crls.pem
  versions/<serial>/manifest.json
  current -> versions/<serial>
```

The parent directories are `0700`; private keys are `0600`; public material is at most `0644`.
`manifest.json` records the exact principal, serial, validity, and SHA-256 of every file. A version is
fully written, synced, and validated before an atomic rename changes `current`. A partial version is never
served. Teardown removes all versions after the pod is stopped; suspend/delete follows the existing
spawn-incarnation cleanup rules.

The whole per-incarnation directory is bind-mounted read-only into the sidecar at
`/run/spawnery/workload`. It is a sidecar-only mount through `PodSpec.SidecarMounts`; it is never included
in `AgentSpec.Mounts`. The agent receives no SVID file, key path, trust bundle, or duplicate environment
value. Both Docker and CRI contract tests must prove this separation.

The root is the same environment root already provisioned to the node. `node-crls.pem` is a signed,
monotonic snapshot for the node issuers used to verify the parent node. The sidecar remembers the highest
CRL number accepted during its process lifetime and rejects rollback. A malicious parent node can withhold
that snapshot, but that node also owns the sidecar's host and files; revocation does not claim to contain
the workload from its creator. It does prevent acceptance of a revoked wrong-node certificate under the
stated host-honest threat model.

No private material is placed in container environment, a PodSpec label, logs, CP state, or Beads.

## 7. TLS channel and exact peer verification

### 7.1 Sidecar server

The control listener remains on the pod-reachable address and port, but serves TLS 1.3 only. If a control
address is configured, missing, malformed, mismatched, revoked, not-yet-valid, or expired workload
material is a fatal startup error. Readiness is not reported until the listener has loaded the current
snapshot.

The sidecar parses its own SVID first. That typed spawn principal becomes the only client policy for the
listener. On every handshake it requires a client certificate and verifies the client chain to the
mounted environment root with client-auth usage and revocation. It then requires exactly:

```text
kind=node
role=<own spawn node-class>
account_id=<own spawn node-account>
node_id=<own spawn node-id>
```

A different valid node, a CP/AS service, another spawn leaf, an anonymous client, and a certificate whose
node fields came from configuration rather than the SVID are all rejected during the handshake.

The control handler no longer performs bearer authentication. Reaching the handler means the TLS layer
has already supplied the exact parent-node principal. All existing routes share this one policy; no route
has a plaintext or bearer exception.

### 7.2 Node client

Every spawn gets a control transport bound to its exact expected spawn principal. The transport sends the
node's current node SVID through a dynamic client-certificate callback and verifies the sidecar chain,
workload issuer role, trust domain, validity, revocation, and every field of the expected spawn principal.

The workload leaf has no pod-IP SAN because the pod IP does not exist when the sidecar certificate must be
injected. The dedicated client TLS constructor therefore disables the standard DNS/IP hostname check and
performs the complete X.509 path and Spawnery principal verification in `VerifyConnection`. This is not a
skip-verification mode: a callback failure aborts the handshake, and tests pin that removing the callback
cannot produce a usable config.

`ControlURL` becomes `https://<PodIP>:<control-port>/control/model`. The client is per spawn, so HTTP
connection reuse cannot carry one spawn's verifier into another spawn's recycled IP. Teardown closes idle
connections and cancels long polls. Source IP and network location are not authentication inputs.

The following operations all use that transport and remove the `Authorization` header:

- `GET/POST /control/model`;
- `POST /control/credentials`;
- `POST /control/github`;
- `GET /control/github/events`;
- `GET /control/status`.

The JSON bodies and failure semantics above TLS remain unchanged.

## 8. Rotation, revocation, and expiry

### 8.1 Workload leaf rotation

The default workload SVID lifetime is one year. Renewal starts 30 days before `NotAfter`. This deliberate
deviation from normal short-lived SPIFFE practice is required because there is no independent Workload
API: this SVID is the channel used to maintain the sidecar. Short lifetimes would turn a modest AS or CP
outage into an unrecoverable running pod.

At renewal the node generates a new P-256 key and CSR, repeats CP-authorized AS issuance, validates the
response, writes a complete version, and atomically changes `current`. The sidecar's TLS configuration
loads and validates a changed current manifest before using it and swaps the coherent certificate/trust
snapshot in memory. A fresh node handshake must observe the new leaf fingerprint before the node records
rotation success. Existing requests may drain on the old connection; new connections use the new leaf.
Routine rotation does not revoke the old leaf.

Retries use bounded exponential backoff. Three consecutive failures spanning the 1-, 5-, and 15-minute
attempts set a new `spawn_workload_credential_status=STALE` condition on the existing spawn report. The
node keeps retrying, capped at six hours, and clears the condition only after a fresh handshake proves the
new leaf is live. The spawn remains active while its current SVID is valid.

At `NotAfter`, the node closes the control transport, reports `EXPIRED`, and refuses further control
operations. It does not silently fall back to HTTP, a bearer, or an unverified TLS client. The agent may
continue running, but operator/user recovery requires recreating the pod with a new generation.

### 8.2 Revocation

The workload issuer publishes signed, monotonically numbered CRLs through the existing CRL distribution
path. Nodes check workload leaf revocation on every new handshake and terminate mapped control transports
when a newly accepted CRL revokes their peer. Suspected key compromise revokes the individual workload
leaf; ordinary rotation does not grow the CRL.

Compromise of the workload intermediate uses an offline-root ceremony to revoke and replace that
intermediate. Nodes must reject a revoked issuer as well as a revoked leaf. Replacement does not change
the root or trust domain, but affected spawns must be recreated because their chain is no longer valid.

Node-certificate rotation is independent. The workload client transport obtains the node's current leaf
on each new handshake. The sidecar continues to require the same typed node principal, so a replacement
node leaf works without changing the workload SVID.

## 9. Re-adoption and recovery

The workload material is durable node state because the sidecar and its read-only mount survive a
spawnlet restart. Re-adoption no longer calls `ContainerEnv` to recover a control bearer. After the CP
returns an `ADOPT` verdict, but before the node issues any control request, the node:

1. derives the expected spawn principal from its own verified node identity plus the CP-confirmed spawn
   ID and generation;
2. loads `current` from the generation-specific material directory;
3. validates the manifest, private-key match, full chain, issuer role, exact principal, revocation, and
   validity; and
4. builds the spawn-specific control transport and proves a fresh mTLS handshake to the live sidecar.

A missing, corrupt, mismatched, revoked, or expired workload SVID refuses adoption. In line with the SE3
rebuild-failure contract, a CP-confirmed but cryptographically unadoptable pod follows capture-before-reap
and reports a specific security failure; it is never partially inserted into the Manager store. CP
unavailability remains non-destructive: without an `ADOPT` verdict the node leaves the pod alone and does
not treat lack of a verdict as a certificate failure.

A valid SVID already inside the 30-day lead may be adopted. Renewal begins immediately; repeated renewal
failure reports `STALE`. An expired SVID is not repaired across the adoption boundary, because doing so
would turn an unauthenticated legacy/live pod into a newly trusted endpoint without first proving its
existing workload identity.

`SIDECAR_CONTROL_TOKEN`, `Spawn.ControlToken`, `SidecarControlTokenEnv`, the token generator, bearer test
fixtures, and the adoption token-recovery branch are deleted. `PodBackend.ContainerEnv` may be removed if
the implementation-wide reference scan confirms it has no remaining caller; it must not be retained just
for speculative future recovery.

## 10. Rollout and compatibility boundary

There is no mixed-mode control listener. A sidecar accepts mTLS or it is a legacy sidecar; no binary
accepts both bearer HTTP and mTLS for the same endpoint. This is the operational boundary that prevents a
downgrade from becoming permanent compatibility code.

Rollout is ordered:

1. Generate the workload intermediate in the offline-root ceremony, provision it to the AS, and deploy
   the additive AS issuance and CP confirmation APIs plus CRL distribution. No node uses them yet.
2. Publish the mTLS-only sidecar image and matching spawnlet, but do not roll nodes with live legacy pods.
3. Drain or suspend/recreate every legacy pod on one node, then atomically roll that node's spawnlet and
   configured sidecar image. Its first new spawn proves issuance and mTLS before the node returns ready.
4. Repeat node by node. A pre-roll inventory gate rejects a node upgrade while a bearer-era pod remains.
5. After the fleet is converted, run source/config/image scans proving that the token and plaintext URL
   paths are absent.

AS/CP versions may overlap because their new APIs are additive and dormant. Node/sidecar versions may not
overlap on a pod. A new node encountering an old pod label/version refuses adoption with an explicit
`legacy-control-channel` reason; operators must recreate it. There is no emergency plaintext flag.

For new spawns, AS or CP-confirmation unavailability fails before `StartPod`, so no unauthenticated
sidecar or partially provisioned agent is created. Running spawns continue until renewal or certificate
expiry.

## 11. Observability and operations

### Metrics

- AS issuance requests and outcomes by `initial|renewal` and bounded reason, plus CP-authorization latency.
- CP confirmation allow/deny counts by bounded reason (`unknown`, `stale-generation`, `wrong-node`,
  `wrong-state`, `unavailable`).
- Node workload SVID seconds-to-expiry, rotation attempts/outcomes, `STALE`/`EXPIRED` counts, and mTLS
  handshake failures by bounded verification stage.
- Sidecar control handshakes and rejections by bounded principal/profile reason.
- Workload and issuer CRL number, age, rollback rejection, and revocation-triggered disconnect count.

Spawn IDs, node IDs, URI strings, serials, and fingerprints belong in structured audit logs, not metric
labels. Logs may record public certificate fingerprints and validity but never CSR private material, PEM
bodies, authorization headers, model keys, GitHub tokens, or request bodies carrying secrets.

### Alerts and conditions

- Alert before the workload intermediate has less than 180 days remaining.
- Alert on any `EXPIRED`, sustained `STALE`, CRL rollback, issuer revocation, or issuance authorization
  denial for a tuple the node believes is current.
- Surface `spawn_workload_credential_status` as a condition, not a lifecycle phase. `STALE` does not make
  inference unhealthy; it means the authenticated maintenance channel is approaching failure.

## 12. Verification and negative tests

### Hermetic PKI and API tests

- Accept the canonical spawn path only from `spawn-workload-issuer`; reject the same path from service,
  cloud-node, self-hosted-node, root-direct, and unknown-policy issuers.
- Reject wrong trust domain, escaped or non-canonical path segments, zero/leading-zero generation,
  duplicate URI SAN, DNS/IP SAN, CA leaf, wrong EKU, wrong key usage, expired/not-yet-valid leaf, revoked
  leaf, revoked issuer, CRL rollback, and a CSR carrying identity extensions.
- Prove AS ignores/has no request fields for node identity and derives them from mTLS context.
- Prove CP denies an AS request for the wrong node/class/account, stale generation, missing reservation,
  suspended/deleted spawn, and non-AS caller. Prove AS signs nothing after denial or timeout.
- Prove the node independently rejects an AS response for a different exact principal.

### Control-channel tests

- A valid sidecar SVID for another spawn, another generation, or another node fails even when the pod IP
  is the expected/recycled address.
- The sidecar rejects another valid node, a CP/AS service leaf, a workload leaf, no certificate, and a
  revoked or expired parent-node certificate.
- Plain HTTP to the control port fails; an `Authorization: Bearer ...` header grants nothing; a TLS client
  with no complete verifier cannot be constructed.
- Every control route works over the same exact-principal mTLS transport without an Authorization header.
- Connection pooling never crosses spawn transports, and CRL updates close a revoked live long-poll.
- Atomic rotation tolerates a crash before and after the `current` rename, response loss, repeated reload,
  and concurrent events/model/GitHub requests. A fresh handshake proves the new fingerprint.

### Runtime and adoption tests

- Docker, fakepod, and CRI contracts prove workload files are present only in `SidecarMounts` and absent
  from agent mounts/env/labels.
- Creation tears down before `StartAgent` when issuance, material validation, sidecar readiness, or mTLS
  proof fails.
- Re-adoption succeeds without `ContainerEnv`; missing/corrupt/mismatched/revoked/expired material refuses
  adoption; CP unavailability still reaps nothing.
- The source tree, generated deployment config, and built sidecar image contain no
  `SIDECAR_CONTROL_TOKEN` or `http://<PodIP>:<control-port>` control path.

### Production VM acceptance

The prod-stack VM is load-bearing. Against the real runsc/CRI stack it must:

1. create a spawn, deliver a unique marker as model/GitHub credential data, and exercise every control
   route including the bounded GitHub event long-poll;
2. capture the pod-bridge control traffic and prove the marker and HTTP request line are absent while TLS
   records are present;
3. attempt plaintext, wrong-spawn, wrong-generation, wrong-node, expired, and revoked-certificate calls and
   prove they fail before handler effects;
4. restart the spawnlet and prove the live pod re-adopts without environment-token recovery, then re-run
   model switching and git-over-HTTPS;
5. force renewal, prove the sidecar serves the new leaf without restart, and verify a simulated repeated
   failure reports and later clears `STALE`.

## 13. Implementation decomposition

The implementation should be split into Beads children with these dependencies:

1. **PKI profile and typed principal**: issuer role/OID, spawn path parser/builder, workload leaf issue and
   verification, leaf/issuer revocation tests.
2. **Issuance and authorization APIs** (depends on 1): protobufs, AS node-only handler, CP AS-only
   confirmation, principal derivation, audit and negative matrices.
3. **Node material manager** (depends on 1 and 2): key/CSR generation, response verification, atomic
   version store, lifecycle cleanup, sidecar-only mount contract.
4. **Sidecar mTLS listener** (depends on 1 and 3): coherent material loader, exact-parent verifier,
   TLS-only readiness, hot certificate/trust reload.
5. **Spawn-specific node control transport and bearer deletion** (depends on 3 and 4): exact-spawn client,
   migrate every route, HTTPS URLs, remove token/env/header/recovery and dead `ContainerEnv` APIs.
6. **Rotation, revocation, conditions, and adoption** (depends on 5): scheduler/backoff, CRL disconnects,
   `STALE`/`EXPIRED`, restart reconstruction and fail-closed expiry.
7. **Deployment and full verification** (depends on all): offline issuer tooling/config, drain gate,
   source/image scans, hermetic/race/lint gates, and the production VM acceptance sequence.

The protobuf task must serialize shared `proto/` edits before implementation branches that consume
generated code. The mTLS implementation tasks can otherwise proceed in isolated worktrees once their
dependencies land.

## 14. Acceptance criteria

- Every node-sidecar control byte is carried over TLS 1.3 with both peers verified to the one configured
  environment root and trust domain.
- The node accepts only the exact spawn/class/account/node/generation principal it expected; the sidecar
  accepts only its exact parent node principal.
- No per-node CA, URI path name constraint, random-cert trust, inner identity protocol, bearer fallback,
  or second root exists.
- `SIDECAR_CONTROL_TOKEN` and all bearer/plaintext control code, config, image, and adoption dependencies
  are absent.
- Initial issuance and every renewal require an authenticated node principal plus CP confirmation of the
  exact live reservation.
- A one-year SVID rotates 30 days early; repeated failure surfaces `STALE`; expiry prevents control and
  refuses re-adoption.
- Sidecar keys are node-generated, atomically stored, mounted read-only only into the sidecar, and never
  exposed to the agent or control-plane services.
- Hermetic race tests, lint, both runtime-lane contracts, and the real prod-stack VM suite pass.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
