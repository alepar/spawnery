# Per-Spawn SPIFFE mTLS for the Node-Sidecar Control Channel

**Date:** 2026-07-14
**Beads:** `sp-dvke.6.1` under `sp-dvke.6`
**Status:** draft; governing decisions collaboratively approved; roast BLOCK corrections folded
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
- Replacing the node's existing X.509-SVID profile, issuer custody, or persisted key. This design does pin
  ownership and restart-based transport cutover so the sidecar does not assume a nonexistent hot reload.

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
- a requested one-year validity capped by the earliest chain `NotAfter`, with `NotBefore` allowing the
  existing bounded clock-skew policy.

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

### 5.2 Durable placement reservation before `StartSpawn`

The current `spawn_containers` row is durable and generation-fenced, but its `node_id` remains empty
until `SetActive`, after the blocking `Provision` call. `scheduler.inflight` is process memory and is
therefore not an authorization source. This design replaces that gap for **every** create, resume,
recreate, migrate, and fork-target start, not only starts that happen to carry a GitHub mount.

`spawns` gains `status_generation` and `status_reservation_id`. `spawn_containers` gains
`placement_reservation_id`, `node_class`, `node_account_id`, `placement_reserved_at`, and
`control_protocol_version`; its existing `node_id` participates in the same reservation. Before
`ClaimStarting`, the CP generates a random 128-bit `placement_reservation_id`. `ClaimStarting` atomically
creates generation N and copies `(N, reservation_id)` into both the live container and the spawn status
fence. The reservation ID plus node/class/account/protocol fields are the immutable **reservation tuple**.
The CP then performs this sequence before it sends `StartSpawn`:

1. Pick one currently eligible node and read its class/account from the CP's mTLS-derived registry
   entry, never from self-asserted `Register` fields.
2. In one store transaction call `ReservePlacement(spawn_id, generation, reservation_id, node_id,
   node_class, node_account_id, control_protocol_version, reserved_at)`.
3. `ReservePlacement` updates only the current live row with the exact generation, `phase=starting`,
   `ended_at IS NULL`, and empty placement fields. Repeating the exact tuple is idempotent; any different
   tuple or stale generation returns `ErrConflict`.
4. Pin `scheduler.Provision` to that node. `Provision` may check current connectivity/capacity, but it
   must not choose a different node. Register/await the A4 intent against the same durable tuple where
   that flow applies, then send `StartSpawn` carrying the reserved generation and protocol version.

`SetActive` is replaced by a tuple-fenced `ActivatePlacement`; it no longer assigns placement. Every
lifecycle/status writer supplies the exact `(spawn_id, generation, reservation tuple)` it started with.
The store transaction compares those fields against both rows and the allowed source phase/status before
changing container and high-level spawn state. An exact retry of an already-applied transition is
idempotent; a stale generation, different tuple, or wrong source state returns `ErrConflict` and changes
nothing. Re-adoption verifies the existing tuple; it does not rewrite ownership. The AS authorization path
never reads `scheduler.inflight`, pending-intent maps, router bindings, or an in-memory GitHub index.

The reservation is immutable for one generation. A send failure, node loss, issuance denial, timeout, or
pre-ACTIVE rollback calls a tuple-fenced terminal transition; it may end only the row and high-level state
still owned by that exact operation. Choosing another node first ends N with N's tuple, then
`ClaimStarting` creates generation N+1 and a new tuple. Those operations are separate committed store
transactions in that order; N+1 is never exposed until N is durably terminal. Migration follows the same
ordering when it returns the spawn to suspended before a later target attempt. A delayed failure callback
from N after N+1 has been reserved or activated receives `ErrConflict` and cannot mark N+1 errored,
suspended, or unplaced. No operation clears `node_id`, reuses a generation, or performs an unfenced
high-level status write after its tuple-fenced transition fails.

The authoritative query is a store method over the live `spawn_containers` row, keyed by
`(spawn_id,generation)`. It returns the complete reservation tuple, protocol version, phase,
`reserved_at`, and `ended_at`. A `starting` or `active` live row is authorizable even if a CP restart has
temporarily projected the high-level spawn status as `unreachable`; delete, suspend completion, error,
rollback, or reassignment atomically ends the row and denies it. This preserves a `StartSpawn` already
delivered before a CP crash while keeping generation and operation fencing load-bearing.

CP startup/recovery reconstructs no authority from scheduler memory. A required database-backed test
commits a reservation, destroys the CP/server/scheduler objects, constructs a new CP on the same store,
and proves that an issuance confirmation for the exact tuple succeeds while wrong node fields and a
newly ended/reassigned generation fail. A companion crash-point test covers restart after reservation
and before issuance, followed by the node request; `scheduler.inflight` is empty throughout.

#### 5.2.1 Mandatory reservation crash spike

Before API implementation, a store-level spike runs the exact transactions concurrently against SQLite
and Postgres: two nodes attempt the same generation, only one reserves; the process is reconstructed from
the database; exact authorization survives; ending the row removes authority; generation N+1 cannot be
authorized with N's tuple. It crash-tests reserve, activate, error, rollback, and reassignment before and
after commit. After N+1 is active, it delivers delayed success and failure results carrying N's tuple and
proves every container field and high-level spawn field for N+1 is byte-for-byte unchanged.

**Kill criterion:** if empty/reserved/active/terminal state and the high-level spawn transition cannot be
compared and changed atomically by exact generation plus reservation tuple, add an explicit placement-state
column and generation-keyed placement table owned by the same store transaction. Do not bridge the
ambiguity with a scheduler map, router binding, timestamp guess, unfenced compensation, or best-effort
cleanup.

#### 5.2.2 Repository-wide lifecycle writer inventory

Task 3 begins by checking in an inventory of every production call site and every store method or direct
SQL statement that writes `spawns.status`, `status_seq`, `status_generation`,
`status_reservation_id`, or any `spawn_containers` placement/phase/end field. The initial inventory must
include, then replace or wrap, all current forms of:

- claim/reservation and provision completion: `ClaimStarting`, `ReservePlacement`, `SetActive`, provision
  success, send failure, timeout, and `SetError`;
- connectivity and boot reconciliation: `MarkUnreachable`, `MarkBootUnreachable`, `MarkReachable`, delayed
  registry disconnect/reconnect callbacks, and route rebinding;
- adoption/inventory reconciliation: `Adopt`, `reconcileInventory`, `adoptOrStop`, and readoption-driven
  reachable/unreachable writes;
- suspend/resume/migrate/fork: `TransitionClaimed`, `SetSuspending`, `SetSuspended`, `RevertSuspended`,
  `ReconcileSuspendedAfterError`, rollback, and target reassignment;
- delete/end/terminal cleanup: `EndContainer`, `MarkDeleted`, `MarkDeletedClaimed`, lost/error terminal
  transitions, claim expiry recovery, and startup recovery.

Each asynchronous message/callback captures its episode fence when registered and passes it unchanged to
the store. `MarkUnreachable(ids)` becomes a batch of exact fences; a delayed disconnect for N cannot mark
N+1 unreachable. `MarkReachable`, adoption, and reconnect require the same exact fence. Boot reconciliation
may derive fences transactionally from the database, but its update still joins the spawn and live
container on generation+reservation ID. Status-only terminal paths retain the last episode fence on the
spawn row rather than accepting a bare spawn ID. No caller may perform a compensating status write after a
fenced transition returns `ErrConflict`.

An AST/static test fails if production code adds a direct lifecycle-column write or a call to an
unfenced compatibility method outside the inventoried store implementation. A table-driven SQLite and
Postgres matrix creates active N+1, then delivers every N event: provision success/failure, activation,
error, rollback, reassignment, disconnect/unreachable, reconnect/reachable, adoption, suspend complete,
late suspend-after-error, delete, end/lost, terminal cleanup, and claim recovery. Each returns conflict or
an explicit stale-event no-op and leaves every N+1 spawn/container field unchanged. The same matrix covers
N reconnect after N+1 disconnect and N disconnect after N+1 reconnect.

### 5.3 AS to CP reservation confirmation

Before every signature, the AS calls `AuthorizeSpawnWorkloadSVID` on a new, separate protobuf service,
`cpinternal.v1.WorkloadIssuanceAuthorizationService`, over the existing AS-service-to-CP mTLS client. The
request contains the AS-derived node class, node account, node ID, spawn ID, generation, CSR SPKI digest,
and the root-revocation epoch/digest captured by the AS. The CP route permits only an `authsvc` service
principal. Authorization returns the bounded issuance lease described in section 5.5, not a timeless
boolean.

The CP confirms from the durable placement query that:

1. the spawn ID and generation are current;
2. the generation is reserved to or actively hosted by that exact stamped node;
3. the stamped node class and account match the AS-derived principal fields;
4. the stamped protocol version is `mtls-v1`; and
5. the live container phase is `starting` or `active`.

Unknown, stale, suspended, deleted, differently placed, or unreserved tuples are denied. CP timeout or
unavailability is retryable and the AS does not sign. A denial is not converted into a new reservation.
The SPKI digest is included in audit correlation; the CP does not approve a caller-supplied identity.

The initial node call occurs after the CP has committed placement/generation and before `StartPod`.
Renewal occurs only while that generation remains live. The check cannot constrain a fully compromised
AS that already holds the workload issuer key; it prevents confused-deputy and stale-node issuance in the
honest service and gives the CP an authoritative audit point.

### 5.4 Listener and principal boundaries

`IssueSpawnWorkloadSVID` is registered only on the AS direct-TLS internal listener and only in the
`node:cloud` and `node:self-hosted` policy rows. It is absent from `PublicHandler`. Anonymous callers,
CP/AS service principals, workload principals, malformed node principals, and wrong-role certificates
are rejected before the issuance handler and signer are reached.

`AuthorizeSpawnWorkloadSVID` and `CommitSpawnWorkloadSVID` are the only methods on
`cpinternal.v1.WorkloadIssuanceAuthorizationService`. That generated service handler is registered only on
the CP direct-TLS internal mux and only for an `authsvc` service principal. Neither method is ever added to
the public `cp.v1.SpawnService`, and the generated internal service is never mounted wholesale or by path
prefix on the browser/CLI handler. Anonymous callers, nodes, CP service peers, workload principals, and
wrong-role certificates are rejected before the store query. Route-matrix tests enumerate every principal
kind on both listeners and assert that public-listener requests are unregistered rather than delegated to
application authorization.

### 5.5 Linearizable root-revocation epoch and issuance lease

Fresh local CRL checks alone leave a publication race between CP authorization and AS signing. The CP
store therefore owns one durable `root_revocation_epoch` row with `{epoch, crl_number, crl_digest,
next_update, state=active|draining}` and an `issuance_leases` table. The active epoch identifies the exact
root CRL that AS and CP must both have durably applied. An issuance lease is random, single-use, expires in
30 seconds, and is bound to the epoch/digest, reservation tuple, CSR SPKI digest, authenticated node
leaf+issuer serials, AS service leaf+issuer serials, the authorizing CP service leaf+issuer/instance,
selected workload issuer serial, and request ID. Authorize and commit target that same CP instance; a CP
connection or identity change discards the buffered result and retries from authorization.

The AS durable checkpoint stores `{epoch, crl_number, crl_digest}` as one record delivered by the
coordinator; applying CRL bytes without their matching epoch metadata cannot enable issuance. CP compares
that pair to its database row on every authorize and commit.

The offline-root ceremony still signs the CRL, but a deployment publication coordinator on the CP host is
the sole holder of write credentials for the static CRL object and owns publication ordering. The offline
ceremony can only deliver signed candidate bytes; it cannot publish them. The coordinator never receives a
root private key. To publish candidate E+1 it:

1. atomically changes the database row from `active(E)` to `draining(E+1, candidate number/digest)`, which
   denies every new issuance lease;
2. waits for all E leases to commit, abort, or expire;
3. distributes the signed candidate to AS and every CP replica, waits for each active instance to durably
   apply and acknowledge the exact number/digest, then publishes the same bytes at the static CRL URL; and
4. atomically changes the row to `active(E+1)` and permits new leases.

The transition to `draining` is the publication fence. No E+1 bytes become externally visible before all
E signing operations have drained, and no new signing operation starts until every issuing/authorizing
process has E+1. Failure after the fence leaves issuance blocked. Once a higher-numbered candidate is
distributed or published, recovery must finish that exact candidate; it may not roll back to E. Multiple
coordinators serialize on the database row.

The AS and CP each open their own durable root-issuer CRL checkpoint/refresher before making their internal
listener ready. Their internal server verifier and client transport compose ordinary chain/profile/leaf-
CRL validation with section 8.2's root-issuer check. Both listeners and the AS-to-CP client register actual
connections by peer issuer serial. A higher root CRL closes newly revoked connections; `NextUpdate` closes
all connections dependent on stale state. A missing, stale, or database-mismatched epoch poisons workload
issuance even if public service remains available.

`IssueSpawnWorkloadSVID` executes one issuance boundary:

1. capture local active epoch E and the authenticated node leaf/issuer; verify the node issuer, local AS
   service issuer, and selected workload issuer are absent from E;
2. call `AuthorizeSpawnWorkloadSVID(E, digest, spawn, generation, SPKI)`; CP checks its local CRL equals
   database-active E, revalidates the authenticated AS leaf/issuer, resolves and checks the exact live
   reservation tuple, and atomically inserts the bound lease;
3. capture the verified CP peer leaf/issuer from that call, then re-read local E and revalidate the node,
   CP peer, local AS service, and selected workload issuers inside the lease boundary;
4. sign into an in-memory response buffer only; and
5. call `CommitSpawnWorkloadSVID(lease, E, certificate fingerprint)`. CP atomically requires the same
   active epoch, unexpired unused lease, same authenticated AS peer, and still-live exact reservation,
   then consumes the lease. The AS re-reads local E and all four issuer checks once more before returning
   the buffered certificate.

Any epoch/digest change, draining state, lease expiry, peer change, reservation change, or revocation
before successful commit causes the AS to zero/discard the buffered certificate and return retryable
unavailable; no certificate bytes reach the caller. An operation committed before the publication fence
linearizes in E. An operation overlapping the fence either committed before it or observes the changed
epoch and returns no certificate. `defer` aborts an uncommitted lease; expiry handles AS crashes.

A deterministic adversarial test pauses before and after every boundary above: AS epoch capture, each
issuer check, CP TLS verification, CP epoch read, placement query, lease insert/return, CP-peer capture,
signer invocation, buffered signature, commit request/transaction, final AS epoch read, and handler return.
It attempts publication at each pause. The only allowed outcomes are a certificate committed entirely in
E before the fence or no returned certificate followed by retry in E+1. Tests also cover publisher crash
at every four-step publication boundary, lease expiry, AS crash, and two competing publishers.

The epoch store, publication coordinator, AS/CP readiness, socket eviction, and lease gate land before the
workload signer or either issuance RPC is enabled.

**Kill criterion:** if any deployment, ceremony, replica, or manual path can publish a valid higher root
CRL without first acquiring the database fence and draining leases, remove that path or its object-store
write authority. If exclusive publication and AS/CP acknowledgment cannot be enforced, workload issuance
does not ship; repeated local CRL checks are not an acceptable substitute for linearization.

## 6. Key custody and injection

The node generates the workload private key. It is never generated by, returned from, or stored at the
AS or CP. The node writes one per-incarnation material directory below its existing protected state root:

```text
workload-svid/<spawn-id>/<generation>/
  versions/<version-id>/tls.crt
  versions/<version-id>/tls.key
  versions/<version-id>/root.pem
  versions/<version-id>/node-issuers/<issuer-fingerprint>.crt
  versions/<version-id>/root-issuer.crl
  versions/<version-id>/node-leaf-crls/<issuer-fingerprint>.crl
  versions/<version-id>/workload-leaf.crl
  versions/<version-id>/revocation-sources.json
  versions/<version-id>/manifest.json
  current -> versions/<version-id>
```

The host-private ancestor is `0700` and owned by the spawnlet account. The bind-source projection below
that ancestor uses `0755` directories and read-only `0444` files, including `tls.key`. This apparently
broad file mode is deliberate: Docker userns-remap means container root is not the host file owner, while
a rootless spawnlet cannot `chown` to the remap base. Host users still cannot traverse the `0700`
ancestor, the agent never receives the bind mount, and the sidecar sees a read-only mount. Security rests
on the private host ancestor plus mount-namespace separation, not on a UID mapping that differs by lane.

`version-id` is a unique monotonic node-local sequence, independent of the workload leaf serial, so a
trust-only node-issuer update can publish a new coherent version without reissuing the workload leaf.
`manifest.json` records that version, the exact principal, workload serial, validity, protocol version,
accepted issuer-set generation, and SHA-256 of every file. A staging version is created with owner-only
modes, fully written, synced, validated, then chmoded into its read-only projection before an atomic rename
changes `current`. A partial version is never served. Teardown removes all versions after the pod is
stopped; suspend/delete follows the existing spawn-incarnation cleanup rules.

The whole per-incarnation projection is bind-mounted read-only into the sidecar at
`/run/spawnery/workload`. It is a sidecar-only mount through `PodSpec.SidecarMounts`; it is never included
in `AgentSpec.Mounts`. The agent receives no SVID file, key path, trust bundle, or duplicate environment
value. Both Docker and CRI contract tests must prove this separation.

Docker uses a new private SELinux relabel mode (`:Z`), not the existing shared `:z`, for this one-container
mount. The CRI backend currently ignores `PodSpec.SidecarMounts`; implementation is blocked until it maps
them through `toCRIMounts` on the sidecar `ContainerConfig` with `Readonly=true` and applies an equivalent
private host label acceptable to containerd/runsc on enforcing hosts. Relabeling must never cover the
host-private ancestor or any sibling spawn.

The root is the same environment root already provisioned to the node. The CRLs in the version directory
are fresh bootstrap inputs, not the sidecar's ongoing state. `revocation-sources.json` contains only the
deployment's deterministic untrusted CRL base URL and expected issuer fingerprints. The sidecar verifies
the bootstrap CRLs, persists its own monotonic checkpoint inside its container rootfs, and starts its own
refresher before readiness. The node separately maintains its own durable revocation state and refresher;
neither process treats the other's in-memory snapshot as authority. A malicious parent node can still
withhold or redirect bootstrap material, but that node also owns the sidecar's host and files; revocation
does not claim to contain a workload from its creator. It does prevent acceptance of a revoked wrong-node
certificate under the stated host-honest threat model.

No private material is placed in container environment, a PodSpec label, logs, CP state, or Beads.

### 6.1 Mandatory runtime mount spike

Before production implementation, a root-required spike must exercise the exact material layout on both
real lanes:

- Docker with the development daemon's userns-remap and a rootless spawnlet account;
- containerd/CRI with runsc under the production VM's enforcing SELinux policy.

It must prove: the sidecar can read and use `tls.key`; the agent cannot stat any workload path; the
sidecar cannot write the mount; CRI actually places every `SidecarMounts` entry on the sidecar and none on
the agent; and SELinux relabeling remains scoped to the one spawn path. After the pod and read-only mount
are already running, the spike creates a wholly new `versions/<version-id>` directory with a new key,
certificate, manifest, and inodes. It captures host and container UID/GID plus SELinux labels for the
original files, new directory, new files, mount root, and resolved `current` target before and after the
atomic swap. Without restarting or remounting the pod, it swaps `current`, forces a fresh TLS handshake,
and proves the sidecar presents the new certificate fingerprint and can use the new private key.

**Kill criterion:** if the common read-only projection cannot give post-start newly created versions a
usable private SELinux label, expose the atomic target change, and satisfy all separation assertions
without putting the key in env, an agent-visible/shared mount, or a host-traversable ancestor,
implementation stops and this design is revised to a runtime-mediated secret injection mechanism.
Pre-creating future version directories, relabeling the private ancestor recursively, falling back to
plaintext, or broadening the mount is not a pass.

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

Every accepted TLS connection is registered at the listener boundary by `(issuer serial, leaf serial)`
with the peer leaf and issuer `NotAfter` values and an actual `net.Conn.Close`, not only a request-context
cancel function. The sidecar schedules a timer for the earliest peer-chain expiry. A newer node-issuer
leaf CRL or root issuer CRL closes matching connections immediately; CRL freshness expiry closes all
connections whose verification depends on that stale list and refuses new handshakes. Closing the socket
forces HTTP/1.1 and HTTP/2 clients, including the GitHub long-poll, to reconnect and present a currently
valid node leaf. Revocation and expiry therefore affect existing connections; routine node leaf rotation
uses section 8.3's restart, which closes every old-identity socket before cutover.

The sidecar also schedules a listener-wide timer for its own workload leaf/intermediate/root expiry. If
its workload issuer appears in the root issuer CRL, its own leaf appears in the workload leaf CRL, or the
root issuer CRL becomes stale, it closes every accepted connection and stops accepting new ones. This is
fatal control-listener state, not a chance to keep serving an already authenticated socket.

### 7.2 Node client

Every spawn gets a control transport bound to its exact expected spawn principal. The transport sends the
spawnlet process's immutable node-identity snapshot and verifies the sidecar chain, workload issuer role,
trust domain, validity, revocation, and every field of the expected spawn principal.

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

The node uses the symmetric connection registry for sidecar peers. Each registered connection has an
expiry timer at the earliest workload leaf/intermediate/root `NotAfter`. Workload-leaf CRL or root issuer
CRL updates close matching sockets immediately; stale CRL state closes every dependent socket and fails
new dials. An HTTP transport retry must complete a new exact-principal handshake and never continue on a
connection accepted before the revocation/freshness transition.

### 7.3 Mandatory connection-eviction spike

A compiled Go spike must run real HTTP/1.1 and HTTP/2 control servers with a blocked long-poll, apply a
leaf revocation, expire the root/leaf CRL clock, and advance the peer leaf/issuer expiry timer. On both the
sidecar-server and node-client sides it must prove that the underlying socket closes, the blocked request
returns, and the next request performs a new handshake. Request-context cancellation alone is not a pass.

**Kill criterion:** if the existing HTTP middleware registry cannot force socket-level eviction for both
protocols, the implementation must move registration to a tracked listener/`ConnContext` plus custom
dialer before any production route is migrated. Shipping revocation that affects only future idle
connections is prohibited.

## 8. Rotation, revocation, and expiry

### 8.1 Workload leaf rotation

The requested workload SVID lifetime is one year, but the AS sets leaf `NotAfter` to the earliest of
`now+1 year`, the signing workload intermediate's `NotAfter`, and the environment root's `NotAfter`.
It never emits a child that outlives any chain parent. The node defines the effective chain expiry as the
same minimum and renews 30 days before that time, even if a malformed/external leaf advertises a later
date. This deliberate long lifetime differs from normal short-lived SPIFFE practice because there is no
independent Workload API: this SVID is the channel used to maintain the sidecar.

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

At effective chain expiry, the node and sidecar expiry registries close the relevant connections, the
node reports `EXPIRED`, and further control operations fail. It does not silently fall back to HTTP, a
bearer, or an unverified TLS client. The agent may continue running, but operator/user recovery requires
recreating the pod with a new generation.

### 8.2 Revocation

Every accepted chain requires two independently numbered revocation layers:

1. **Issuer CRL:** the environment root signs one CRL whose entries are serials of revoked intermediates
   it issued. This is a new offline-root artifact and a distinct durable checkpoint; the current
   intermediate-only `CreateCRL` API is not reused as if it covered its own issuer. Its consumer profile
   requires the configured root's signature and issuer name, matching AKI when present, a positive CRL
   number, canonical serial encoding, and `ThisUpdate <= now < NextUpdate`. A consumer cannot prove the
   certificate type of an unknown serial from a CRL entry alone, so it does not reject unknown entries or
   claim to validate that every entry names a CA. Instead, for each presented or configured intermediate
   it compares that known certificate's canonical serial against the list and rejects an exact match.
   The offline ceremony owns the stronger production invariant: it constructs entries only from the
   root-issued intermediate inventory and refuses leaf, foreign-root, zero, duplicate, or malformed
   serials before signing.
2. **Leaf CRL:** each role-bearing intermediate signs the existing-form CRL for its leaves according to
   that issuer's custody ceremony. The AS-held workload and self-hosted-node issuers sign their online
   leaf CRLs. The offline cloud-node issuer signs its node-leaf CRL during the offline CRL ceremony. The
   sidecar loads only the node-leaf CRL applicable to its exact parent class.

The deployment publishes these canonical CRLs at deterministic, issuer-fingerprint-addressed URLs on an
untrusted static endpoint. Signatures provide authenticity; transport location grants no authority.
Root-issuer and leaf-CRL numbers are monotonic independently per signing certificate. Equal number/equal
digest is idempotent, equal number/different digest is equivocation, and a lower number is rollback.
Each process durably persists its highest accepted number and digest before publishing an immutable
reader snapshot.

The operational profile is:

- online workload/self-hosted-node leaf CRLs: `NextUpdate <= ThisUpdate+24h`, refreshed/published at
  least every six hours;
- offline cloud-node leaf CRL and root issuer CRL: `NextUpdate <= ThisUpdate+30d`, renewed/published at
  least seven days before expiry and immediately after their respective revocation or issuer ceremony;
- refresh poll: five minutes with bounded fetch timeout and response size.

AS and CP internal-listener readiness requires the fresh root issuer CRL described in section 5.5 in
addition to their existing relevant leaf CRLs. Node startup requires a fresh root issuer CRL plus a fresh
workload-issuer leaf CRL for every accepted current/next workload intermediate. Sidecar control readiness
requires a fresh root issuer CRL, its own workload-issuer leaf CRL, and a fresh leaf CRL for every accepted
current/next node issuer of its parent class. A refresh failure retains the last verified snapshot only
until `NextUpdate`; freshness expiry atomically poisons that
verifier, closes all dependent connections, and refuses new ones until a strictly current signed list is
durably applied. Metrics and alerts distinguish fetch failure, signature/profile failure, rollback,
equivocation, and freshness expiry.

AS, CP, nodes, and sidecars run separate `CRLRefresher`/durable-state instances and hot-reload
independently. AS and CP state protects their internal peer verifiers as specified in section 5.5. Node
state tracks the root issuer CRL and all overlapping workload issuer leaf CRLs. Each sidecar tracks the
root issuer CRL, its own workload issuer leaf CRL, and only the overlapping cloud or self-hosted node
issuers relevant to its parent class. On a newly revoked workload leaf, the node closes that sidecar
transport and the sidecar closes its listener. On a newly revoked parent node leaf, the sidecar closes the
parent connection. On an intermediate serial appearing in the root issuer CRL, both processes close every
connection chaining through it.

Suspected workload-key compromise revokes the individual leaf; ordinary leaf rotation does not grow the
CRL. Workload-intermediate compromise is an offline-root CRL update followed by replacement. The root and
trust domain remain unchanged, but spawns chaining through the revoked issuer become `EXPIRED`/unusable
and must be recreated.

### 8.3 Node identity ownership and restart cutover

`internal/node/nodeid` and the spawnlet process own the persisted node key and certificate chain. The node
key remains the enrollment-generated key already used for node identity and signed subkeys; this design
does not copy it into a sidecar material directory. Self-hosted node leaf renewal uses a node-only internal
AS `RenewNodeSVID` procedure under the online self-hosted-node issuer; it derives the exact existing node
principal from current mTLS plus an identity-free CSR and is absent from the public listener. Cloud node
renewal remains an artifact from the existing offline cloud-node issuer ceremony. In both cases the node
identity manager validates key match, exact principal, chain/profile, both CRL layers, and validity before
atomically staging the replacement chain beside the unchanged protected key.

The identity manager renews against the earliest node leaf/intermediate/root expiry with a 30-day lead.
For self-hosted nodes it requests renewal and retries with bounded backoff; for cloud nodes it reports the
required offline artifact and refuses a replacement not produced by that ceremony. Repeated failure sets
`node_identity_status=STALE` and removes the node from new scheduling. At effective expiry it closes
CP/AS/control transports and leaves pods running for operator recovery; it never continues with an expired
identity.

There is no node-identity hot reload in this slice. `nodeCPClient`, `nodeASClient`, the signed-subkey holder,
and all per-spawn sidecar transports are constructed from one identity snapshot at process start. Routine
node leaf renewal therefore uses a coherent restart cutover:

1. if the new leaf uses a successor node intermediate, the running old-identity spawnlet first publishes a
   trust-only workload material version containing current+next node issuers and fresh leaf CRLs to every
   sidecar and waits for each sidecar to locally validate the staged public chain under its exact-parent
   policy and acknowledge the new verifier generation; no control request uses the new leaf yet;
2. atomically stage the validated replacement node certificate chain while retaining a rollback copy of
   the still-valid current chain;
3. stop node admission, close CP, AS, and every sidecar transport, and perform the existing graceful
   spawnlet shutdown that leaves pods running;
4. restart, load exactly one new identity snapshot, reconnect to CP/AS, and re-adopt every exact-fenced pod;
   each rebuilt sidecar transport performs a new handshake with the new node leaf; and
5. report rotation complete only after CP/AS attach and every sidecar handshake succeeds. Then retain the
   old issuer/CRL in sidecars until its last possible node leaf expires.

No process may mix an old CP/AS identity with a new sidecar identity or select certificates per
destination. If restart fails before the replacement is used, operators may atomically restore the old
chain and restart only while it remains valid, unrevoked, and accepted by all peers. Otherwise the node
stays unavailable and leaves pods running for recovery; it does not fall back to bearer or plaintext.
Compromise rotation may revoke the old leaf immediately after successful cutover, which evicts any
straggling connection.

The node-issuer rollover spike starts sidecars with current only, hot-adds initially unknown next trust
without restarting the sidecar, stages a next-issued node leaf, restarts the spawnlet, and proves all CP,
AS, and sidecar connections use the new leaf. It also crashes before/after trust acknowledgment,
certificate rename, transport close, process exit, and re-adoption, proving recovery exposes one coherent
identity snapshot.

### 8.4 Workload intermediate rollover

The root issues a successor workload intermediate before the current one enters its final 180 days.
During overlap the AS holds current and next keys as separate signer entries, but next is hot-added through
the durable transaction below rather than preloaded at process start. The successor certificate, a fresh
empty successor leaf CRL, and a fresh root issuer CRL from which the successor serial is absent are
published before the AS may sign with it.

Nodes must support overlapping intermediates with the same issuer role, keyed by issuer serial rather
than rejecting duplicate roles. The AS starts signing new/renewed leaves with next no later than 120 days
before current expiry. Every active spawn still on current is reissued no later than 90 days before
current expiry; signing with current stops at that point. Current's leaf CRL remains published and fresh
until the last leaf it signed has expired or been revoked. Telemetry must show zero active leaves on
current before its final 60 days.

An issuance response chaining through an unknown successor is accepted only after the node validates the
successor to the environment root, proves it absent from a fresh root issuer CRL, and obtains its fresh
leaf CRL. The node then injects the successor chain and CRL bootstrap into the sidecar version. Rollover
never changes the SPIFFE ID, root, or trust domain and does not require a process restart.

Each process persists a generation-numbered immutable issuer-set snapshot. Its manifest identifies every
accepted issuer by certificate fingerprint and serial, its verifier disposition, the AS-only signer
disposition where applicable, the exact leaf-CRL checkpoint number/digest, and the root-CRL checkpoint
number/digest against which admission was checked. Adding next is ordered as follows:

1. validate next's certificate to the configured root and its exact workload-issuer profile; on the AS,
   also stage the protected signer key and prove its public key matches the certificate;
2. durably apply a fresh next-signed leaf CRL and a fresh root CRL from which next's serial is absent;
3. write and fsync the candidate snapshot and directory, atomically publish its `current` pointer, then
   publish the same coherent snapshot to the in-memory verifier; and
4. only after the AS has published the coherent verifier snapshot may it atomically change its signer
   state to issue new leaves from next; a node that first learns next from that issuance response performs
   steps 1-3 locally before accepting the leaf.

A crash before the `current` swap leaves next unaccepted even if its independent CRL checkpoints were
already advanced; that is safe and retryable. A crash after the swap but before the in-memory publish
loads the complete snapshot on restart. No reader infers issuer membership by scanning partially written
files. Retirement reverses no checkpoint: next becomes `signing`, current becomes `verify-only`, and
current is removed only after telemetry and a durable inventory query prove zero live leaves chaining
through it and its last possible leaf validity has elapsed. Its CRL checkpoint may then be archived but is
never reused for a different issuer.

### 8.5 Mandatory revocation and rollover-state spike

The current revocation store admits role-bearing intermediate CRLs and rejects duplicate issuer roles;
it does not model a root-signed issuer CRL, dynamic issuer-set manifests, or overlapping workload
intermediates. A compiled spike starts a real verifier and signer in **current-only** state, with next
unknown to both. Without restarting either process it performs the ordered transaction above, adds next,
and proves a next-signed leaf is accepted while current remains valid. It injects a crash after each
certificate/signer-key staging, leaf-CRL checkpoint apply, root-CRL checkpoint apply, manifest fsync,
snapshot rename, in-memory publish, and signer-state change. Each restart must expose either the complete
prior snapshot or the complete next snapshot, preserve all independently monotonic checkpoints, and never
accept an issuer without both fresh CRL layers. It then restarts with both issuers, moves current through
verify-only, and retires current after the zero-live-leaf gate.

The same spike rejects rollback/equivocation at either layer, revokes current without affecting next,
expires one CRL while the others remain fresh, hot-adds analogous current+next node-issuer trust to a
running sidecar followed by the spawnlet restart cutover in section 8.3, and proves a root-CRL update evicts
AS/CP, node, and sidecar connections by exact issuer serial.

**Kill criterion:** if one state object cannot preserve signer type, dynamic issuer membership, issuer
serial, independent monotonic number, and freshness without ambiguous lookup, use separate
`IssuerSetState`, `IssuerRevocationState`, leaf `RevocationState`, and signer-state objects with one
composed snapshot. If any crash point can expose next before both CRLs are durable, lose a higher
checkpoint, require a restart to learn next, or let a delayed current-only writer overwrite overlap state,
the state format/order must be redesigned before issuance ships. Do not preload next to make the spike
pass, weaken the existing duplicate-role check, or treat absence from a leaf CRL as proof that its issuing
intermediate is unrevoked.

## 9. Re-adoption and recovery

The workload material is durable node state because the sidecar and its read-only mount survive a
spawnlet restart. Re-adoption no longer calls `ContainerEnv` to recover a control bearer. After the CP
returns an `ADOPT` verdict, but before the node issues any control request, the node:

1. requires the observed protocol and reservation labels to match the CP-confirmed
   `(spawn ID, generation, reservation tuple)`, then derives the expected spawn principal from its own
   verified node identity;
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

`mtls-v1` is the immutable control-protocol version. The node stamps
`spawnery.control-protocol=mtls-v1` and `spawnery.placement-reservation-id=<id>` on the sandbox, sidecar,
and agent at creation. The CP stores the same values in the generation's durable placement reservation and
includes them in `StartSpawn`.
`ManagedPod.ControlProtocolVersion` carries the value returned by `ListManaged`; both Docker and CRI must
report inconsistent values across a pod's sandbox/containers as a typed incompatibility rather than
selecting one. `ObservedPod` carries both the discovered value and a
`control_protocol_status=unspecified|exact|missing|unknown|conflicting` result. `unspecified`, including an
old sender's protobuf zero value, is incompatible. The CP returns `ADOPT` only when the status is `exact`
and discovered, reserved, and node-supported versions and reservation IDs all exactly match. Labels are
never mutated in place; a protocol or reservation change requires a new generation.

Missing version means legacy bearer HTTP. Unknown, missing, or conflicting versions are incompatible with
the new spawnlet: it locally quarantines the pod, reports `incompatible-control-protocol`, and does not
probe it with HTTP, TLS, or a recovered env token. On readoption, the CP checks protocol status **before
every ledger lookup and every normal `REAP` predicate**. Any incompatible status returns `DEFER` even when
the CP currently sees no live row, a deleted spawn, or a superseding generation. Thus a stale or partially
restored ledger cannot turn version uncertainty into destruction. Only an exact `mtls-v1` observation may
continue to the existing generation/orphan predicates and receive `ADOPT` or `REAP`; incompatible pods
require an explicit operator cleanup path outside automatic re-adoption. This differs from corrupt
`mtls-v1` material after an exact CP `ADOPT`, which follows the security-failure capture-before-reap rule
in section 9.

The new spawnlet advertises the compiled capability
`spawnery.capability/control-mtls-v1-reconcile` on its authenticated CP attach. That capability means it
understands the protocol and reservation labels, reports typed incompatibility, preserves pods on
`DEFER`, and can re-adopt exact `mtls-v1` material. It is not inferred from binary version, image name,
successful TLS attach, an empty inventory, or a `DEFER` response.

The transition CP stores `control_mtls_activation=dormant|active`, initially `dormant`. While dormant it
accepts old and new node attachments and reconciliation traffic for already-running pods, but denies every
new create/resume/recreate/migrate/fork-target before `ClaimStarting`; it does not reserve placement,
issue workload SVIDs, or send `StartSpawn`. The explicit activation transaction requires:

1. every currently attached scheduling-eligible node advertises the exact reconciliation capability;
2. every such attachment has completed an exact inventory round-trip with no missing/conflicting protocol
   or reservation labels and no legacy bearer pod;
3. the configured eligible fleet roster contains no unattached node that still owns a legacy pod; an
   intentionally removed/disconnected node must be drained and removed from eligibility first; and
4. AS/CP root-epoch gates, workload issuer/CRLs, sidecar image, and VM smoke evidence are ready.

The transaction records the acknowledged node attachment generations and atomically sets `active`. A
disconnect, omitted decision, quarantined pod, or `DEFER` never counts as acknowledgment and cannot open
the gate. After activation, a newly attaching/reconnecting node is individually ineligible for scheduling
or adoption until it advertises the capability and completes exact inventory; an old node is rejected from
the managed attach path without receiving `StartSpawn`, `ADOPT`, or `REAP`. It cannot downgrade or close
the already-active fleet gate.

Attachment capability is represented by a short durable `node_attachment_capability` lease keyed by node
ID and random connection generation. Exact inventory acknowledgment updates only that lease. Attach,
disconnect, lease expiry, and the activation transaction serialize through the activation row: activation
checks every unexpired scheduling-eligible attachment lease in one transaction, and a concurrently
attaching old node either lands before the check and blocks it or observes `active` and is immediately
ineligible. The scheduler requires an unexpired acknowledged capability lease for each target even after
global activation.

The supported binary matrix is:

| CP | Node | Behavior |
|---|---|---|
| old | old | legacy bearer behavior before the transition release only |
| old | new | deployment-preflight error; new node refuses old `StartSpawn` lacking protocol/reservation fields |
| transition dormant | old | existing legacy pods continue; reconciliation only; all new starts globally closed |
| transition dormant | new | capability/inventory may acknowledge; all new starts remain closed until activation |
| transition active | old | attach is non-managing/ineligible; no lifecycle verdicts or starts are sent |
| transition active | new | exact `mtls-v1` create and reconciliation path |

Rollout is ordered:

1. Publish the root issuer CRL and deploy its durable epoch/lease gate, composed verifiers, and eviction to
   AS and CP. No workload signer or issuance route exists yet.
2. Deploy the transition CP in dormant mode plus the internal issuance services, workload issuer/CRLs,
   mTLS-only sidecar image, and capable spawnlet artifact. The global new-start gate closes.
3. For each old node, drain or suspend every legacy pod, stop it, install the capable spawnlet, attach, and
   complete exact empty-or-`mtls-v1` inventory acknowledgment. Suspended workloads resume only after
   activation. A node is never upgraded in place while it owns a legacy pod.
4. After all attached eligible nodes satisfy the activation transaction and the VM smoke passes, activate
   once and create the first `mtls-v1` spawn.
5. Run source/config/image scans proving the token and plaintext URL paths are absent.

Before activation, CP rollback to the old release is allowed only while the durable ledger proves no
`mtls-v1` reservation was ever created and every node/pod is again old-compatible; capable nodes must be
rolled back only after their inventories are empty. After activation or the first `mtls-v1` reservation,
deployment preflight permanently refuses an old CP or old spawnlet candidate. Rollback is only to a release
that understands the activation marker, tuple fence, capability, and `mtls-v1`; otherwise operators must
drain/recreate all `mtls-v1` pods and execute an explicit audited decommission migration, never flip a
compatibility flag. A pre-transition binary without this guard is not an allowed artifact after cutover.
There is no emergency plaintext mode.

Tests cover new-node/legacy-pod, new-node/unknown-version, CP expected/discovered mismatch, inconsistent
CRI sandbox/container labels, and rollback-candidate/`mtls-v1` inventory. For missing, unknown, and
conflicting observations, table tests pair each version state with unknown spawn, missing live row,
deleted spawn, and stale generation; every combination returns `DEFER`, leaves the pod quarantined, and
proves zero handler requests and zero destructive reaps. Exact `mtls-v1` stale-generation controls still
return `REAP`. A source test pins the label constant and verifies every creation and inventory lane carries
it.

### 10.1 Mandatory protocol-stamp spike

A real Docker and CRI/runsc spike must create one pod stamped `mtls-v1`, restart the spawnlet-side runtime
client, and recover the exact protocol and reservation ID through `ListManaged` from every relevant
object. It then deliberately creates missing and conflicting sandbox/sidecar/agent labels for each field
and proves the inventory layer reports an incompatibility rather than selecting one value.

**Kill criterion:** if a runtime does not preserve either label on a durable object available after
restart, the protocol and reservation ID must be stored in a sidecar-only immutable file covered by the
workload manifest and returned by a backend inspection API. An in-memory default or inference from image
name is not acceptable.

For new spawns, AS or CP-confirmation unavailability fails before `StartPod`, so no unauthenticated
sidecar or partially provisioned agent is created. Running spawns continue until renewal or certificate
expiry.

## 11. Observability and operations

### Metrics

- AS issuance requests and outcomes by `initial|renewal` and bounded reason, plus CP-authorization latency.
- CP confirmation allow/deny counts by bounded reason (`unknown`, `stale-generation`, `wrong-node`,
  `wrong-state`, `unavailable`).
- Root-revocation epoch/state, active issuance leases, lease expiry/abort, publisher drain duration, and
  buffered-certificate discard count.
- Control-mTLS activation state, capable/acknowledged/expired attachment leases, and per-node scheduling
  ineligibility reason.
- Node workload SVID seconds-to-expiry, rotation attempts/outcomes, `STALE`/`EXPIRED` counts, and mTLS
  handshake failures by bounded verification stage.
- Node identity seconds-to-expiry and restart-cutover stage/outcome, without key or certificate bodies.
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
- Start AS and CP with fresh durable root-issuer CRL checkpoints; revoke the node issuer before AS entry,
  revoke the AS service issuer before CP entry, revoke either on an established pooled connection, and
  expire the CRL at each pre-query/pre-sign pause. Separately revoke the selected workload intermediate
  after CP approval but before signing. Every case leaves the workload signer untouched; peer cases also
  evict the matching socket, and pre-CP cases leave the placement query untouched.
- Run the section 5.5 publication adversary at every authorization/sign/commit boundary. Assert one
  linearization: committed-before-fence E certificate or zero returned certificate and retry under E+1;
  never a certificate signed after the fence. Crash/restart the publisher, AS, and CP with live leases.
- Prove the node independently rejects an AS response for a different exact principal.
- Prove the AS public listener has no issuance route and its internal route rejects anonymous, CP/AS
  services, workload, malformed-node, and wrong-role principals before the signer is called.
- Prove the CP public listener has neither authorize nor commit route and both internal methods reject
  anonymous, node, CP-service, workload, and wrong-role principals before lease or placement state is
  touched. Prove AS `RenewNodeSVID` is likewise absent publicly and node-only internally.
- Commit a generation-fenced reservation, reconstruct the CP over the same database with empty scheduler
  state, and prove exact issuance authorization survives while rollback/reassignment ends authority.
- Run section 5.2.2's complete repository inventory and delayed-N matrix, including provision,
  activation/error/rollback/reassign, disconnect/unreachable, reconnect/reachable, adoption,
  suspend/late-suspend, delete/end/terminal, and claim-recovery events after N+1. Exact generation plus
  reservation-tuple fencing leaves all N+1 container and spawn fields unchanged on SQLite and Postgres.
- Cap leaf validity at each possible earliest chain member; rotate current/next workload intermediates
  from current-only through hot-added initially unknown next, crash/restart every state boundary, retire
  current, and reject issuer/leaf CRL rollback, equivocation, absence, and freshness expiry independently.

### Control-channel tests

- A valid sidecar SVID for another spawn, another generation, or another node fails even when the pod IP
  is the expected/recycled address.
- The sidecar rejects another valid node, a CP/AS service leaf, a workload leaf, no certificate, and a
  revoked or expired parent-node certificate.
- Plain HTTP to the control port fails; an `Authorization: Bearer ...` header grants nothing; a TLS client
  with no complete verifier cannot be constructed.
- Every control route works over the same exact-principal mTLS transport without an Authorization header.
- Connection pooling never crosses spawn transports; parent/workload leaf revocation, issuer revocation,
  CRL freshness expiry, and leaf/issuer expiry timers close the real socket and force a new handshake.
- Atomic rotation tolerates a crash before and after the `current` rename, response loss, repeated reload,
  and concurrent events/model/GitHub requests. A fresh handshake proves the new fingerprint.

### Runtime and adoption tests

- Docker, fakepod, and CRI contracts prove workload files are present only in `SidecarMounts` and absent
  from agent mounts/env/labels.
- The rootless userns-remap and VM runsc/SELinux lanes prove the mapped-UID projection, private relabel,
  read-only atomic update, and CRI sidecar-mount mapping required by section 6.1.
- Creation tears down before `StartAgent` when issuance, material validation, sidecar readiness, or mTLS
  proof fails.
- Re-adoption succeeds without `ContainerEnv`; missing/corrupt/mismatched/revoked/expired material refuses
  adoption; CP unavailability still reaps nothing.
- `mtls-v1` is stamped consistently on sandbox/sidecar/agent, round-trips through `ManagedPod` and node
  inventory, and returns pre-ledger `DEFER` for legacy/unknown/conflicting labels even against stale,
  missing, or superseded ledger rows, without a handler call or destructive reap.
- The old/new CP-node matrix proves dormant CP closes every new-start path, only the explicit capability
  plus exact inventory can activate, old nodes and `DEFER` cannot acknowledge, and post-activation old
  CP/node rollback candidates are refused.
- Node SVID rotation stages successor trust into live sidecars, atomically stages the node chain, closes all
  old CP/AS/sidecar transports, restarts, and re-adopts with one new identity; every crash point leaves one
  coherent process identity and no pod reap.
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
   failure reports and later clears `STALE`;
6. apply fresh node/workload leaf CRLs and a root issuer CRL while HTTP/2 control and event connections are
   live, proving socket eviction and reauthentication;
7. race root-CRL publication at every CP-authorization/AS-sign lease boundary, proving each attempt either
   committed before the fence or returns no certificate and retries under the new epoch;
8. exercise dormant activation with old+new nodes, prove all starts remain closed until exact capability
   and inventory acknowledgment, then prove old-node reconnect cannot downgrade or reopen a gate;
9. stage a next-issuer node leaf, distribute trust to live sidecars, restart the spawnlet, and prove CP, AS,
   re-adoption, and every sidecar use the same new leaf with no mixed old/new transports; and
10. restart the CP after durable placement reservation but before issuance, then complete issuance and
   spawn start from the reconstructed CP/store without `scheduler.inflight`.

## 13. Implementation decomposition

The implementation should be split into Beads children with these dependencies:

1. **Mandatory feasibility spikes**: run sections 5.2.1, 5.2.2, 5.5, 6.1, 7.3, 8.3, 8.5, and 10.1 on
   their required store, process, Docker, and VM/CRI lanes; record evidence against every kill criterion.
   Any kill stops dependent implementation and returns to design.
2. **Root-revocation substrate and internal-boundary enforcement** (depends on spike 1): implement the
   offline root-CRL profile, durable epoch/lease tables, publication coordinator, issuer-revocation state,
   dynamic current/next issuer-set snapshot, composed verifier, AS/CP refreshers, actual-socket eviction,
   readiness/freshness gates, and pre-query/pre-signer gate APIs. This task exposes no workload issuance
   RPC and lands first.
3. **Durable lifecycle fencing, protocol, and internal API contract** (depends on 1 and 2): own the checked-in
   writer inventory and store migration; replace every lifecycle/status writer listed in section 5.2.2
   with atomic exact-generation plus reservation-tuple fencing, including reachability/adoption and all
   terminal paths; refactor every start path before `Provision`; add immutable protocol/reservation labels,
   typed incompatible inventory status, capability/activation state, pre-ledger `DEFER`, and the separate
   `cpinternal.v1.WorkloadIssuanceAuthorizationService`. This task owns all shared protobuf edits and
   generated code, AST enforcement, old/new matrices, and complete delayed-N-after-N+1 tests.
4. **Workload PKI, issuance, and confirmation** (depends on 2 and 3): add the issuer role/OID, typed spawn
   principal, server-only chain-capped leaf profile, AS node-only internal handler, CP AS-only confirmation
   authorize+commit lease handlers on the already-generated internal service, CSR validation, in-memory
   signing buffer/discard, current/next signer choice, leaf CRLs, public-listener absence tests, complete
   principal matrices, publication-race tests, audit, and rate limits. It may invoke the signer only inside
   task 2's active epoch lease.
5. **Runtime material projection** (depends on 1, 2, 3, and 4): key/CSR material manager, atomic versions,
   userns-safe modes, private SELinux relabel, CRI `SidecarMounts`, lifecycle cleanup, and both real-lane
   mount/version contracts.
6. **Sidecar mTLS and revocation runtime** (depends on 2 and 5): coherent material and dynamic node-issuer
   set loader, exact-parent verifier, TLS-only readiness, independent root/node CRL state+refresher,
   certificate/trust hot reload, listener connection registry and expiry timers.
7. **Spawn-specific node transport and bearer deletion** (depends on 4, 5, and 6): process-snapshot node
   identity, exact-spawn client, independent root/workload CRL state+refresher, socket eviction/expiry,
   migrate every route, HTTPS URLs, and remove token/env/header/recovery and dead `ContainerEnv` APIs.
8. **Rotation, conditions, rollover orchestration, and adoption** (depends on 2, 3, and 7): retry
   scheduler, `STALE`/`EXPIRED`, durable workload issuer transitions, node identity manager/renewal custody,
   internal self-hosted `RenewNodeSVID`, cloud offline-artifact validation, sidecar node-issuer trust
   staging, coherent spawnlet restart cutover, restart reconstruction,
   zero-live-leaf retirement gate, activation/preflight/quarantine, incompatible rollback behavior, and
   fail-closed expiry.
9. **Deployment and full verification** (depends on all): offline issuer/issuer-CRL ceremony,
   deterministic CRL publication, transition/rollback guard, drain gate, source/image scans,
   hermetic/race/lint gates, and the production VM acceptance sequence.

Task 2 serializes the epoch schema before task 3's spawn schema, and it must be deployed to AS and CP before
task 4 enables either issuance RPC. Task 3 serializes all shared `proto/` and lifecycle-store changes before
branches consume generated, placement, inventory, or activation code. The remaining tasks proceed in
isolated worktrees only after their declared gates land.

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
  exact durable live reservation; every start reserves before `StartSpawn`, and restart/reassignment does
  not depend on `scheduler.inflight`; every lifecycle writer is fenced by exact generation and reservation
  tuple, so a delayed N result cannot mutate N+1.
- Root-CRL publication is linearized by the durable draining epoch and single-use issuance lease across CP
  authorization, buffered AS signing, and commit. An epoch change returns no certificate; only the fenced
  coordinator can publish higher CRL bytes.
- Requested one-year validity is capped at the earliest chain expiry and rotates 30 days early;
  current/next workload intermediates overlap without a root change; repeated failure surfaces `STALE`;
  expiry prevents control and refuses re-adoption.
- Root-signed issuer CRLs and intermediate-signed leaf CRLs are independently monotonic, fresh, durable,
  distributed, and hot-reloaded by AS, CP, node, and sidecar wherever relevant. AS and CP reject and evict
  root-revoked internal peers before placement confirmation or workload signing; revocation or expiry
  closes existing sockets as well as denying future handshakes.
- Sidecar keys are node-generated, atomically stored, mounted read-only only into the sidecar, and never
  exposed to the agent or control-plane services on either userns-remap or CRI/runsc/SELinux lanes.
- `mtls-v1` is immutable and round-trips through CP reservation, pod labels, `ManagedPod`, and inventory;
  incompatible upgrade/rollback paths quarantine and receive `DEFER` before all automatic `REAP`
  predicates, including with a stale or missing ledger.
- The transition CP starts dormant and denies every new lifecycle start until every attached eligible node
  holds an unexpired explicit reconciliation-capability plus exact-inventory lease. Old nodes and `DEFER`
  cannot activate or downgrade the fleet; old CP/node rollback is forbidden after activation.
- One spawnlet process uses one immutable node identity for CP, AS, signed subkeys, and sidecars. Node SVID
  renewal stages successor trust, closes all old transports, restarts, and re-adopts before declaring
  cutover; no dynamic mixed-identity state exists.
- Issuance and confirmation procedures are absent from public listeners and reject every principal role
  except the explicitly authorized node-to-AS and AS-to-CP callers; CP confirmation lives on a separate
  internal-only protobuf service, never public `SpawnService`.
- Hermetic race tests, lint, both runtime-lane contracts, and the real prod-stack VM suite pass.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
