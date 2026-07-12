# Unified mTLS for the Control Plane, Auth Service, and Spawnlets

**Date:** 2026-07-12
**Beads:** `sp-dvke.2` under `sp-dvke`
**Status:** draft, collaboratively approved
**Builds on:** [Node Auth & Unified Identity](2026-06-05-node-auth-unified-identity-design.md),
[Auth & Identity](2026-06-11-auth-identity-design.md), and
[Unified Root for AS Authorization Signing](2026-07-12-unified-root-as-auth-signing-design.md)

## Problem

Spawnery's internal authentication is inconsistent. Spawnlet-to-CP uses node mTLS rooted in the
Spawnery CA, but CP-to-AS and AS-to-CP calls use static `X-Spawnery-AS-Secret` or bearer secrets.
Spawnlet-to-AS is described as mTLS, yet `authsvc` listens on cleartext h2c and the production VM's
Caddy listener does not request or forward client certificates. The application middleware can
verify a client certificate only on a path that does not currently deliver one.

This creates multiple credential-distribution systems, leaves procedure authorization coupled to
headers, and makes the documented production node-to-AS identity path nonfunctional.

## Goals

- Authenticate every internal CP, AS, and spawnlet connection from the environment Spawnery root.
- Use one SPIFFE trust domain and one identity grammar per environment.
- Preserve the cryptographic cloud-node versus self-hosted-node issuance boundary.
- Replace static service-authentication secrets with mTLS identities and method authorization.
- Terminate internal TLS in the Go services so authorization sees the verified peer certificate.
- Give unauthenticated node enrollment a server-authenticated bootstrap path.
- Keep browser and CLI authentication behavior unchanged.

## Non-Goals

- Introducing SPIRE, a service mesh, or the SPIFFE Workload API.
- Replacing public WebPKI on browser-facing listeners.
- Changing GitHub OAuth or client bearer-token semantics.
- Designing AS authorization-artifact signing (`sp-dvke.1`), production client-to-node
  authorization (`sp-dvke.3`), or Iroh transport (`sp-dvke.4`).
- Making one certificate or private key serve multiple principal roles.

## One Trust Domain per Environment

The environment root defines one SPIFFE trust domain:

```text
development: spiffe://dev.spawnery.internal
staging:     spiffe://staging.spawnery.internal
production:  spiffe://prod.spawnery.internal
```

The exact hostnames are configurable deployment constants, but a certificate's trust domain must
match the root bundle selected for that environment. Environments never share roots or trust domains.

Canonical identities are:

```text
spiffe://<td>/service/cp/<instance-id>
spiffe://<td>/service/authsvc/<instance-id>
spiffe://<td>/node/cloud/<account-id>/<node-id>
spiffe://<td>/node/self-hosted/<account-id>/<node-id>
```

Here `<td>` denotes the trust-domain authority without the `spiffe://` prefix. Every identity field
must be a non-empty SPIFFE path segment containing only the character set allowed by the SPIFFE ID
specification. Values are canonicalized when created, never percent-encoded, and compared exactly.

Each leaf contains exactly one URI SAN. A TLS service leaf may additionally contain DNS or IP SANs
for ordinary server-name verification. Authorization reads only the verified SPIFFE URI. Subject CN
and DNS SANs never carry principal identity.

## PKI Hierarchy and Issuer Roles

```text
Environment Spawnery Root (offline)
  -> Service Intermediate (offline)
       -> CP instance leaves
       -> Auth Service instance leaves
  -> Cloud Node Intermediate (offline)
       -> cloud node leaves
  -> Self-Hosted Node Intermediate (online at AS)
       -> self-hosted node leaves
```

All certificates belong to the same SPIFFE trust domain. The intermediate split limits issuance
authority; it does not create additional trust domains.

X.509 URI name constraints constrain URI hosts, not path prefixes. They therefore cannot prevent the
self-hosted intermediate from syntactically issuing `/node/cloud/...`. Each intermediate instead
carries a root-signed Spawnery issuer-role policy:

- `service-issuer`;
- `cloud-node-issuer`;
- `self-hosted-node-issuer`.

The policies use fixed Spawnery OIDs and are included in the root-signed intermediate certificate.
Intermediates have `MaxPathLen=0`, so they cannot delegate their role to another CA.

Spawnery verification requires correspondence between issuer role and leaf path:

| Issuer role | Permitted leaf paths |
|---|---|
| `service-issuer` | `/service/cp/<instance>` or `/service/authsvc/<instance>` |
| `cloud-node-issuer` | `/node/cloud/<account>/<node>` |
| `self-hosted-node-issuer` | `/node/self-hosted/<account>/<node>` |

A standard X.509 chain that violates this table is invalid for Spawnery authentication. Every TLS
client and server must call the shared Spawnery principal verifier after RFC 5280 path validation;
using the platform TLS verifier alone is insufficient.

This replaces the current class-bearing DNS SAN and DNS name-constraint scheme. The class remains
cryptographically rooted because a self-hosted issuer cannot change its own root-signed role policy,
cannot issue a subordinate CA, and cannot produce a leaf accepted under the cloud path.

## X.509-SVID Profile

Leaves follow the SPIFFE X.509-SVID profile:

- exactly one SPIFFE URI SAN with a non-root path;
- `BasicConstraints CA=false`;
- `KeyUsageDigitalSignature`;
- service leaves carry TLS client-auth and server-auth EKUs;
- node leaves carry TLS client-auth and server-auth EKUs because they authenticate as clients to CP
  and AS and may serve direct client channels;
- CA signing and CRL-signing usage are absent from leaves;
- certificate validity is bounded and monitored.

Signing certificates use `CA=true`, appropriate CA key usages, and the environment trust-domain URI
without a path when a SPIFFE URI is present. The implementation uses the existing Go PKI rather than
introducing SPIRE. Conformance is enforced by hermetic profile tests.

Each service replica receives its own leaf and private key. Sharing one CP or AS certificate among
replicas is prohibited because it destroys instance-level audit, rotation, and revocation.

## Shared Principal Verifier

Refactor `internal/pki` around one verification result:

```go
type Principal struct {
    TrustDomain string
    Kind        string // service | node
    Role        string // cp | authsvc | cloud | self-hosted
    InstanceID  string
    AccountID   string
    NodeID      string
}
```

The verifier:

1. validates the leaf chain to the configured environment root at the current time;
2. requires exactly one valid SPIFFE URI SAN in the configured trust domain;
3. validates the X.509-SVID leaf profile;
4. reads and validates the root-signed issuer-role policy from the verified chain;
5. parses the path according to the exact role grammar;
6. enforces issuer-role/path correspondence;
7. checks certificate revocation state;
8. returns a typed principal used by authorization middleware.

Consumers authorize typed fields and never reparse URI strings independently. A malformed,
unknown-role, wrong-environment, or revoked principal fails closed.

## Listener Architecture

CP and AS each expose separate public and internal listeners.

### Public listeners

Public listeners remain behind public WebPKI and serve browser/CLI traffic:

- AS: OAuth, PKCE, refresh/logout, device flow, and authenticated enrollment-token issuance;
- CP: user-facing ConnectRPC and WebSocket APIs.

User requests authenticate with AS-issued bearer tokens and proof-of-possession mechanisms. Public
listeners never accept a service certificate as a substitute for user authorization.

### Internal listeners

Internal TLS is terminated directly by the Go process. It is not terminated by Caddy or another
reverse proxy. The application therefore receives the actual peer chain in `r.TLS` and authorizes the
verified principal.

The CP internal listener requires a client certificate on every connection. It serves node attach
and the small AS-to-CP service RPC surface.

The AS internal listener uses server-authenticated TLS and verifies a client certificate when one is
present. Its router enforces:

- `/enroll`: no client certificate required; fingerprint-bound enrollment token required;
- all post-enrollment node routes: node certificate required;
- CP service routes: CP service certificate required;
- no other anonymous internal routes.

An invalid presented client certificate fails the TLS handshake. Absence is tolerated only so the
router can admit `/enroll`; every other route rejects it before handler execution.

## Enrollment Bootstrap

An unenrolled node is provisioned out-of-band with:

- the environment root certificate;
- the AS internal endpoint and expected DNS name;
- a short-lived enrollment token bound to the node key fingerprint.

The node generates and persists its key before enrollment. It dials the AS internal listener with
server-authenticated TLS, explicitly using the pinned Spawnery root and expected AS DNS name. It
submits the same-key CSR and enrollment token. The AS verifies the token, CSR proof of possession,
fingerprint binding, requested node identifier, and account/class scope before issuing the node leaf.

This fixes the current `RunEnrollWithKey` gap: reading a root file without using it to authenticate
the AS connection is not sufficient.

After enrollment, the node uses the same node X.509-SVID as its mTLS client identity when dialing CP
and AS. It never uses the enrollment token again.

## Internal Authorization Matrix

| Caller principal | Target | Allowed operations |
|---|---|---|
| cloud or self-hosted node | CP | attach, register/inventory, heartbeat, lifecycle/session responses |
| cloud or self-hosted node | AS | node revocation query, node-scoped credential mint/refresh |
| CP service | AS | revocation feed, GitHub link status, other explicitly registered policy queries |
| AS service | CP | GitHub mint authorization, token-rotation notification |
| AS service | CP node operations | reject |
| node | CP or AS service-only operations | reject |
| CP service | AS node-only operations | reject |
| anonymous | AS internal `/enroll` only | fingerprint-bound enrollment token required |

Each listener maintains a compiled-in method allowlist keyed by principal role. Adding a new internal
RPC requires an explicit authorization entry and negative tests for every other principal class.
Network location, source IP, headers, and DNS names are not authentication inputs.

For self-hosted nodes, account tenancy continues to derive from the verified identity path. Cloud
nodes remain multi-tenant and use the reserved system account in their path.

## Client Construction and Server Names

All internal clients share a TLS constructor that accepts:

- environment root bundle;
- caller leaf, chain, and private key when enrolled;
- exact expected peer SPIFFE role;
- explicit TLS server name;
- revocation state.

The TLS server name is required even when the network address is localhost or an IP. DNS/IP SAN
verification authenticates the endpoint name; the post-handshake SPIFFE verifier authenticates the
service role and instance. Both checks must pass.

CP and AS servers use a generic certificate-presence TLS policy plus a `VerifyConnection` callback
that invokes the shared principal verifier. Route middleware then applies the method matrix.

## Secret Removal

Remove these service-authentication mechanisms:

- `X-Spawnery-AS-Secret`;
- `CP_AS_RPC_SECRET` and `AS_CP_RPC_SECRET`;
- `CP_AS_CP_SECRET` and the revocation-feed bearer;
- service-scoped header interceptors;
- dev headers that claim node identity on production-capable listeners.

Secrets used for unrelated purposes, such as GitHub credentials, storage credentials, token
encryption, or OAuth client secrets, are unchanged.

## Revocation and Rotation

Service and node certificate revocation is distinct from user-session revocation.

- Each issuing intermediate can sign a CRL.
- CP, AS, and spawnlets persist the highest accepted CRL number per issuer and reject rollback.
- Distribution may use an untrusted static endpoint or deployment channel because signatures and
  monotonic numbers are verified locally.
- Internal listeners refresh revocation state without replacing the root.
- Revoking one service instance or node must not require rotating the root or sibling leaves.

Routine leaf rotation overlaps old and new certificates. Long-lived streams reauthenticate on
reconnect; a separately delivered revocation update terminates live streams belonging to a revoked
principal where the server can map connections to certificate serials.

The existing AS node-revocation data is reconciled into this mechanism during implementation rather
than retained as a parallel certificate-revocation authority.

## Development and Migration

Development generates an ephemeral root, role-bearing intermediates, and leaves, then exercises the
same principal parser, issuer correspondence, listener split, and method authorization. Cleartext h2c
is allowed only in explicit unit-test fixtures and an opt-in local compatibility lane. It is not a
second production authentication mode.

Spawnery is pre-production, so node identities migrate from class-bearing DNS SANs to SPIFFE URI SANs
as a flag-day profile change. There is no production dual parser. The VM harness regenerates its
throwaway PKI and proves all directions after the change.

## Testing

Hermetic tests cover:

- every valid service and node identity path;
- exactly-one-URI-SAN enforcement and optional service DNS SANs;
- wrong root, trust domain, path grammar, issuer role, leaf usage, expiry, and revocation;
- a self-hosted issuer signing a syntactic cloud-node path;
- service issuer signing a node path and node issuer signing a service path;
- per-method authorization for every principal class;
- anonymous enrollment success and anonymous access rejection everywhere else;
- enrollment server-name/root verification and fingerprint-bound same-key CSR enforcement;
- CRL signature, issuer, number monotonicity, persistence, rollback, and live-connection termination;
- removal of every static service-secret fallback in production configuration.

The production VM lane proves:

- node-to-CP attach over mTLS;
- node-to-AS credential mint over the same node identity;
- CP-to-AS and AS-to-CP service RPCs over instance mTLS;
- missing, wrong-role, wrong-environment, expired, and revoked certificates fail;
- Caddy is absent from internal authentication paths;
- browser and CLI public flows remain functional.

## Rejected Alternatives

### Separate SPIFFE trust domains per principal class

The SPIFFE URI authority denotes the trust domain. Using `services.*`, `cloud-nodes.*`, and
`self-hosted-nodes.*` would model three trust domains despite the one-root requirement and would make
federation semantics misleading.

### Class only in the SPIFFE path without issuer correspondence

A self-hosted intermediate could sign a syntactic cloud path that passes generic chain validation.
The root-authorized issuer role and path check are mandatory.

### Caddy or proxy TLS termination

The application would not receive the original TLS peer chain without trusting a new identity
forwarding protocol and protecting that hop. Direct termination is smaller and stronger.

### Service mesh or SPIRE

Both can operate this model, but neither is required to establish it. Adding a control plane and
automated issuer before the system needs dynamic workload issuance exceeds the current requirement.

### Shared service certificates

They erase instance attribution and make targeted rotation and revocation impossible.

### Static header or bearer secrets over TLS

They duplicate service identity, require independent rotation/distribution, and authorize possession
of a string rather than a root-issued principal.

## References

- [SPIFFE Identity and Verifiable Identity Document](https://github.com/spiffe/spiffe/blob/main/standards/SPIFFE-ID.md)
- [SPIFFE X.509-SVID specification](https://spiffe.io/docs/latest/spiffe-specs/x509-svid/)
- [SPIFFE Trust Domain and Bundle specification](https://spiffe.io/docs/latest/spiffe-specs/spiffe_trust_domain_and_bundle/)

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged
from the assumptions above — append a dated note here, whether or not a formal debugging skill was
used.*
