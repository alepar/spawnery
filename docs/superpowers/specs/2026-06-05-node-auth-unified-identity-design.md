# Spawnery — Node Authentication, Auth Service & Unified Identity (Design)

**Bead:** `sp-ova` (P0 security)
**Status:** Draft v2 (brainstorm in progress; pending user review)
**Date:** 2026-06-05
**Builds on:** [E4 Identity & Secrets](2026-05-28-spawnery-e4-identity-secrets-design.md) (§6 node enrollment, §7 session tokens),
[node-class propagation `sp-2as`](2026-06-01-node-class-propagation-sp-2as.md),
[scheduler routing `sp-t5p`](2026-06-01-scheduler-routing-sp-t5p.md)
**Related (referenced, not designed here):** `sp-gtm` (CP key-vending MITM), `sp-7h6` (E4 epic), `sp-l5o` (authz review)

---

## 0. Problem

The node→CP `Attach` stream has **no authentication** (`cmd/cp/main.go` — "node side: no auth"; plain
h2c, no TLS). A node **self-reports** `node_class` and `node_owner` in `Register`, and the scheduler
routes on those values. So **any node that reaches the CP can claim `class=cloud`** (or
`owner=victim`) and be treated as trusted — receiving other users' spawns and any cross-user/secret
data. The entire trust split rests on unauthenticated self-assertion; it is only safe today because the
demo runs solely Spawnery-operated nodes on a closed CP.

E4 §6 *designed* the node-identity trust anchor, but the demo-MVP overlay **deferred** it, shipping
self-assertion as the stopgap. This spec makes the trust anchor real and goes further: it isolates the
identity authority into a **separate Auth Service (AS)** so that even a **fully compromised CP** cannot
silently place a user's workload on an unverifiable node or MITM the agent channel — and clients can
*detect* attempts to.

**Deliverable:** this design only. No code lands this session; implementation follows via a plan.

---

## 1. Architecture — three trust domains

```
                          ┌─────────────────────────────────────────────┐
   OFFLINE CEREMONY  ──▶  │ Root CA (pinned by every client + CP + node) │
                          │  ├─ Cloud Intermediate     (OFFLINE/operator)│
                          │  └─ Self-Hosted Intermediate ───────┐        │
                          └─────────────────────────────────────┼────────┘
                                                                 ▼ (held online by)
   ┌──────────────┐   identity / enroll / sessions       ┌──────────────────┐
   │   Clients    │ ◀──────────────────────────────────▶ │  Auth Service AS │  holds: self-hosted
   │ web/node/cli │   (separately pinned channel)        │  (own container) │  intermediate key,
   └──────┬───────┘                                      └──────────────────┘  AS session-signing key
          │ orchestration (spawn, relay)                          ▲ public keys only
          ▼                                                       │
   ┌──────────────┐   node Attach (mTLS, AS-issued cert)   ┌──────┴───────┐
   │  Control     │ ◀───────────────────────────────────▶ │    Nodes     │
   │  Plane (CP)  │   holds NO signing keys — only pins    │  (spawnlet)  │
   └──────────────┘                                        └──────────────┘
```

Three domains, separated so each holds the *least* authority:

- **Root CA + Cloud Intermediate — offline.** Used only in operator ceremonies. Cloud node certs are
  minted here at deploy time. Nothing online can issue a cloud identity.
- **Auth Service (AS) — online, its own container/node.** The root of trust for *identity*. Holds the
  **self-hosted intermediate** private key (name-constrained, §4) and the **AS session-signing** key.
  Authenticates users and node enrollment; the self-hosted minting key **never leaves the AS**.
- **Control Plane (CP) — online, the big attack surface.** Orchestrates spawns and relays traffic.
  Holds **no signing keys** — only *public* verification material (the pinned Root CA, the AS pubkeys).
  A CP compromise yields no power to forge identity or session authority (§6).

This is the crux of the design: **the CP — the component most exposed to app code, scheduling, and
relay — is demoted to an orchestrator that holds only public keys.** Trust in "the infra" reduces to
trust in the AS + the offline CA, both of which clients can pin independently of the CP.

---

## 2. Unified identity model

One CP/AS **account model** (E4 §1: `accountId` (UUID), handle, oauthSubs, …). Two principal types
authenticate **to the AS**, both account-scoped:

| Principal | Authenticates via | Yields |
|---|---|---|
| **User** | OAuth (E4 §2), at the AS | `accountId` + an AS-signed session |
| **Node** | enrollment (§3), at the AS | an AS/CA-issued cert (identity in the SAN, §2a) |

A reserved **Spawnery system account** (`systemAccountId`, non-login) owns all cloud nodes — the
account-level anchor for the multi-tenant tier.

### 2a. Identity is the certificate name — no class/owner claim in any payload

A node's identity is its **certificate**, and the identity lives in the **SAN**:

```
Self-hosted leaf SAN:  <nodeId>.<accountId>.self-hosted.nodes.spawnery.internal
Cloud leaf SAN:        <nodeId>.<systemAccountId>.cloud.nodes.spawnery.internal
```

`{nodeId, accountId, class}` are **all read from the verified SAN**; `notAfter` from the cert; `caps`
(future, §2b) from a signed extension. **No `class` field and no self-reported `owner` field exist in
`Register` or any token** — they are derived from cryptographic provenance only (§4). A `Register`
message that *says* a class/owner is ignored; the CP overwrites it from the verified peer cert.

### 2b. caps follow the same rule
`caps` are signer-vouched, so a trust-bearing cap is only as good as the authority allowed to grant it;
any such cap must be bound to signer scope the same way class is. MVP uses none. (Recorded so a future
cap can't reintroduce self-assertion.)

---

## 3. Enrollment — performed by the Auth Service

Nodes enroll **with the AS**, not the CP. The CP never participates in identity issuance.

### 3.1 Self-hosted (owner self-service)
1. An owner, **authenticated to the AS** (their OAuth session), requests a node enrollment token.
2. **The AS authenticates the requestor's principal == the account the token is for** — it can, because
   it *is* the identity provider. The enrollment token is bound to that `accountId`; an owner can only
   ever mint tokens for **their own** account. (Closes "node claims owner=victim" at the source.)
3. The token is **one-time, short-lived (~10 min), single-use, account-scoped**.
4. Operator runs the node with the token. On first contact the node generates a keypair (private key
   never leaves the box), submits a CSR + token to the AS; the AS verifies the token and **issues a
   self-hosted cert** with SAN `<nodeId>.<accountId>.self-hosted…`, signed by the **self-hosted
   intermediate it holds**.

### 3.2 Cloud (operator-only, offline)
- Cloud node certs (`…cloud.nodes…`, `accountId = systemAccountId`) are minted in the **offline
  ceremony** from the cloud intermediate and provisioned to Spawnery-operated nodes at deploy. No online
  service — not even the AS — can issue one.

### 3.3 Using the identity
The node connects to the CP over **mTLS** presenting its cert; the CP verifies the chain to the pinned
Root CA and reads identity from the SAN (§5). The node's **pubkey (its cert) is also what clients
verify and encrypt to** for the E2E channel and BYO secrets (§6, E4 §6.6) — but now clients verify it
against the AS root, not the CP's word (this is the sp-gtm fix).

---

## 4. Class-scoped signers (why class is unforgeable)

Class is bound into the **name**, and the self-hosted signer is **name-constrained**, so RFC 5280 chain
validation — enforced by the verifier — makes a self-hosted authority *cryptographically incapable* of
producing a cloud identity:

```
Root CA  (pinned)
 ├─ Cloud Intermediate        PermittedDNSDomains = cloud.nodes.spawnery.internal        ← OFFLINE
 └─ Self-Hosted Intermediate  PermittedDNSDomains = self-hosted.nodes.spawnery.internal  ← held ONLINE by the AS
```

A leaf `<nodeId>.<accountId>.self-hosted.nodes.spawnery.internal` ends with the permitted suffix; a
`…cloud.nodes…` leaf does not, so the self-hosted intermediate **cannot** sign a cloud leaf that
validates. Therefore:

- **Compromise of the AS** (worst identity-domain breach) ⇒ attacker can forge *self-hosted* certs for
  arbitrary accounts, but **cannot forge a cloud identity** (offline intermediate). Cloud/multi-tenant
  confidentiality survives an AS compromise.
- The `accountId` label is *not* itself name-constrained per-account — the AS is trusted to stamp the
  correct `accountId` after authenticating the owner (§3.1.2). That trust is the AS's job; the chain
  enforces only the class boundary.

**Go feasibility:** `crypto/x509` creates (`PermittedDNSDomains`, …) and enforces DNS/URI/IP/email name
constraints during `Verify`. Class is encoded in a **DNS SAN** (not the subject DN — Go's
DirectoryName-constraint support is weak). Implementable on the current stack.

---

## 5. CP-side enforcement

On the node `Attach` (mTLS) connection, **before honoring any `Register`**:

1. **Verify the peer cert** chains to the pinned Root CA (enforcing name constraints).
2. **Derive `{nodeId, accountId, class}` from the verified SAN** — never from `Register`. Self-reported
   fields are ignored (and removed from the proto).
3. Populate the registry node entry with the verified identity. The scheduler/registry/router are
   unchanged in mechanism — they now filter on **trusted** values.
4. **Tenancy gate (§6).**

The CP holds only the pinned Root CA pubkey + AS pubkeys; it verifies, it never issues.

---

## 6. Tenancy invariant (replaces the old review-tier routing)

**Apps run anywhere regardless of review status.** The *only* trust distinction between node classes is
tenancy:

- **Cloud node = multi-tenant:** may run **any** account's spawns (Spawnery-operated, trusted).
- **Self-hosted node = single-tenant:** may run **only** spawns owned by **its bound `accountId`**.

CP placement rule: a spawn owned by account `O` may be scheduled onto **(a)** any cloud node, or **(b)**
a self-hosted node whose verified SAN `accountId == O`. The CP must **never** place `O`'s spawn on a
self-hosted node bound to `O' ≠ O`. (The earlier "reviewed apps require cloud" tightening is **removed**
— review status is an isolation concern, not a trust-boundary concern, and is out of scope here.)

The same check is what a **client** runs to detect compromise (§7): *my self-hosted spawn must land on a
node whose cert `accountId` is mine; my cloud spawn must land on a `class=cloud` node.*

---

## 7. Compromise model & client-side verification (justifying the AS split)

**Assume the CP is fully compromised; the AS, the offline CA, and the AS's keys are intact.** The
attacker controls scheduling, the node registry, the relay, key-vending-to-clients, and session
issuance. Clients hold, pinned **independently of the CP**: the Root CA pubkey, the AS endpoint + its
TLS pin, and the AS session-signing pubkey.

| Attacker goal | Can a CP-only compromise achieve it? | Client check that detects/prevents it |
|---|---|---|
| **Host my workload on an attacker node** | **No.** The hosting node must present a cert chaining to the pinned Root with a SAN matching the expected `(class, accountId)`. Attacker has no CA key; can't enroll cloud (offline); can only enroll self-hosted for *its own* account → wrong `accountId`. | Client verifies the node cert chains to the pinned Root **and** SAN `accountId == mine` (self-hosted) or `class == cloud` (multi-tenant). Mismatch ⇒ refuse. |
| **Decrypt my agent traffic** | **Only if** traffic is *not* E2E-encrypted to the verified node key. With E2E, the CP relays ciphertext it can't read. | Client encrypts to the node pubkey it verified above. **Caveat (gap):** today the relay is *plaintext* through the CP — see below. |
| **Forge a session to my spawn / impersonate me to a node** | **No, if sessions are AS-signed.** The node verifies the session token against the pinned AS pubkey; the CP can't mint one. | Node rejects any session token not validly AS-signed for the spawn's owner. (Requires moving session signing to the AS — §8 decision.) |
| **Serve malicious web JS that fakes all checks** | **Yes, if the CP serves the SPA.** | **Only defensible if the SPA is delivered independently of the CP** (AS-served, or a pinned static origin with subresource integrity / signed release). Native clients (node, spawnctl) are immune — pins are baked into the binary. |
| **Deny service / lie about state** | **Yes** (CP is the orchestrator). | Detectable ("not running") but not preventable; confidentiality is unaffected. Out of scope. |

**What each client can verify:**
- **node** (native, AS-enrolled, pins baked in): strongest — verifies peer/session identities against AS
  keys; won't relay to/from a peer or accept a session it can't tie to the AS root.
- **spawnctl** (native, pinned): same class of checks; can independently re-verify a spawn's node cert.
- **web** (only meaningful if delivered independently of the CP): verifies the node cert + SAN against
  its own AS-issued session's `accountId`. **If CP-served, web verification is theater** — the most
  important finding of this section.

**Two findings that gate the guarantee (carried as decisions in §8):**
1. **E2E relay channel must exist.** The AS split makes node-key vending *verifiable* (fixing sp-gtm),
   but confidentiality of relayed agent traffic additionally requires the E2E channel (E0 §10);
   today's plaintext relay means a compromised CP reads everything regardless of node identity. The
   split is **necessary but not sufficient** on its own.
2. **The web SPA must not be CP-served** (or web users get no real verification).

**Conclusion / justification.** With the AS split, a CP-only compromise **cannot** place a client's
workload on — or MITM the E2E channel of — a node the client can't cryptographically tie (via the AS
Root) to the expected `account + class`, *provided* (a) the E2E relay channel exists, (b) session
authority is AS-anchored, and (c) the verifying client is delivered independently of the CP. The split
converts "trust the CP" into "trust the AS + verify the CP's claims," shrinking the confidentiality TCB
to the **AS + offline CA**. That is the concrete payoff that justifies separating the auth
responsibility out of the CP.

---

### 7a. Session authority — AS-signed, not CP-signed

A **session token** authorizes a client to talk to a spawn's agent *at the node* (distinct from node
identity and from user login): a short-lived JWT the node **verifies offline** against a pinned signing
pubkey. E4 §7 had the **CP** sign it — but under the "CP compromised" model that is the same hole as
node spoofing: a compromised CP holding the signing key can **mint a session for any spawn/owner** and
walk into a victim's session on a legitimately-verified node. So session signing **moves to the AS**;
the node verifies against the **AS** pubkey (pinned at enrollment). Same offline-verified JWT shape, no
latency regression — only the signer changes, and the CP no longer holds it.

The spawn→owner→node binding is orchestration state (CP), while identity is the AS — so authorization
splits by tenancy:

- **Self-hosted (single-tenant) — clean.** The node serves exactly one account (its SAN `accountId`),
  so any session must be that account. The token is just **AS-signed proof of account O**; the node
  checks `O == my accountId` locally. The CP can neither forge it (no AS key) nor place a foreign-owner
  spawn there (§6). Fully covered by identity + tenancy.
- **Cloud (multi-tenant) — split model.** The **AS** attests *identity* (authenticated account O), the
  **CP** supplies *routing* (spawn X on node N), and the **node** enforces ownership/tenancy. A CP
  compromise can reroute/DoS but cannot forge *who you are*; an AS compromise cannot place cloud nodes.
  Full cloud multi-tenant isolation additionally leans on storage/vault gating (E4 §3, server-blind),
  not the session token alone.

## 8. Operating modes & environments (replaces migration)

We are pre-demo; no dual-accept migration is needed. Instead, two **modes**, selected by config:

- **`insecure` mode** — no cert enforcement; the node↔CP link may be plaintext on a separate port; no
  AS required. For **unit / e2e / local** development. Identity falls back to a dev-supplied
  `nodeId/accountId/class` (clearly non-production).
- **`enforced` mode** — full PKI: mTLS on the node link, AS-issued certs, name-constraint + tenancy
  enforcement, AS-signed sessions.

**PKI-soundness e2e tests** (a dedicated test class) run in `enforced` mode against a **hardcoded test
Root CA** + fixtures, and must include **negative** assertions that enforcement actually *rejects*:
wrong class (self-hosted intermediate signing a cloud-subtree leaf), wrong `accountId` tenancy
placement, an unchained/self-signed cert, an expired cert, a forged (non-AS) session token.

**Environments** — each real environment has its **own separate Root CA** (no shared CA across envs).
Staging/prod CA creation is a **next phase, blocked on staging/prod existing** (we have neither yet).

---

## 9. Threat-model summary

| Attack | Before | After this design |
|---|---|---|
| Self-hosted node claims `class=cloud` | Trusted → gets others' spawns + secrets | Class from name-constrained provenance; **cloud unforgeable online** |
| Node claims `owner=victim` | Trusted → gets victim's spawns | `accountId` from verified cert; enrollment token is owner-authenticated at the AS |
| **CP fully compromised** | Total: reroutes workloads, MITMs relay, forges sessions | Cannot forge node identity (no CA key) or sessions (AS-signed); client-detectable (§7) — *given* E2E relay + independent client delivery |
| **AS fully compromised** | n/a | Can forge *self-hosted* certs for any account; **cannot forge cloud** (offline intermediate). Cloud/multi-tenant confidentiality survives. |
| Enrollment token theft | n/a | Single-use, short, account-scoped, owner-authenticated |
| Stolen leaf key replay | n/a | mTLS channel-binds identity; cert revocation is an AS/CA action |
| Transport MITM on node↔CP | Total (plaintext) | `enforced` mode = mTLS |

**Residuals (handed off / accepted):** (1) plaintext relay (E2E channel = E0 §10 / sp-gtm) — the
confidentiality half this design *enables* but does not itself implement; (2) web-SPA delivery origin
(must be independent of the CP); (3) session-signing authority placement (recommended: AS).

---

## 10. Components / interface changes (for the plan)

- **New: Auth Service** — its own container. OAuth user auth; node enrollment (token issuance with
  owner-principal authentication; CSR → self-hosted cert from the held intermediate); AS session-token
  signing; publishes its verification pubkeys. Holds the self-hosted intermediate key (never exported).
- **Offline CA tooling** — Root + Cloud/Self-Hosted intermediate generation; cloud leaf minting;
  per-environment Root CA.
- **`cmd/cp/main.go`** — `enforced` mode: mTLS listener for `NodeService` with the pinned Root CA;
  `insecure` mode: current plaintext h2c on a dev port. Load AS pubkeys.
- **`internal/cp/server.go runNode`** — derive `{nodeId, accountId, class}` from the verified peer cert
  SAN, not `Register`; **tenancy gate** in placement (`placementFor`/scheduler).
- **`internal/cp/registry`** — node entries from verified identity; (placement) self-hosted ⇒
  owner-bound.
- **`proto/node`** — drop self-asserted `node_class`/`node_owner`; add enrollment/CSR flow (AS-facing).
- **Node (`internal/node`/`cmd/spawnlet`)** — keypair gen + CSR to the AS; store cert; mTLS dial to CP;
  verify AS-signed session tokens.
- **Clients (web/node/spawnctl)** — pin the Root CA + AS keys; **verify the hosting node's cert + SAN**
  (`accountId`/`class`) before trusting it; web SPA delivered independently of the CP.
- **Session tokens** — signed by the AS (moved from CP, E4 §7), verified offline by nodes.

---

## 11. Out of scope (referenced)

- **`sp-gtm`** CP key-vending MITM — this design provides the AS-anchored, client-verifiable node
  identity that closes it; the E2E *relay* encryption (E0 §10) is the remaining half.
- **Full E4** vault/OAuth/recovery — consumed, not redesigned (except session-signing moves to the AS).
- App review/isolation tiers (gVisor, egress floor) — separate tracks; no longer a node-trust concern.

---

## Appendix — decision log

| # | Decision | Choice |
|---|---|---|
| OVA.1 | Deliverable | **Design spec only**; implementation via a later plan |
| OVA.2 | Identity authority | **Separate Auth Service** (own container); CP holds **no signing keys**, only public pins |
| OVA.3 | Class assertion | **Derived from cryptographic provenance** (name-constrained signer); **no class/owner field** in any payload |
| OVA.4 | Identity encoding | SAN `<nodeId>.<accountId>.<class>.nodes.spawnery.internal`; class in the DNS subtree |
| OVA.5 | Signer scoping | **Cloud intermediate OFFLINE**; AS holds only the **name-constrained self-hosted intermediate** — an AS compromise can't forge cloud |
| OVA.6 | Enrollment | At the AS; owner-principal authenticated before token issuance; node-generated keypair + CSR; key never leaves node/AS |
| OVA.7 | Tenancy | **Cloud = multi-tenant; self-hosted = single-tenant (owner-bound)**; review status is *not* a trust boundary |
| OVA.8 | Sessions | **AS-signed** session tokens (moved from CP), offline-verified by nodes. Self-hosted closed by identity+tenancy; cloud uses the split AS-identity / CP-routing / node-enforced model (§7a) |
| OVA.9 | Rollout | **No migration** (pre-demo); `insecure` vs `enforced` **modes**; PKI-soundness e2e tests w/ negative cases; per-env Root CA (staging/prod = next phase) |
| OVA.10 | Confidentiality caveats | Guarantee requires the **E2E relay channel** (E0 §10) and an **independently-delivered web client**; both flagged as gating residuals |
