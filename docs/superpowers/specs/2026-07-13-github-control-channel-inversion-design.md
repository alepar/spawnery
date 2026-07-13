# Invert the GitHub Control Channel (node pushes; the sidecar never asks)

- **Bead:** `sp-2tx8.9` (epic `sp-2tx8`) — fixes `sp-2tx8.8`; unblocks `sp-wwtc.5` and `sp-2tx8.3.8`
- **Status:** draft
- **Mode:** collaborative (Mode A)
- **Scope note:** this spec covers the **inversion only**. Removing plaintext from the pod network
  (per-spawn SPIFFE identity + mTLS on this channel) is tracked under the unified-auth epic `sp-dvke`,
  built on `sp-dvke.2.1`'s SVID issuance, and depends on this work. See §8.

## 1. Problem

The node runs a **per-spawn inbound HTTP listener** that the sidecar pulls its secrets from —
`/control/spawnca` (the per-spawn MITM CA cert + private key) and `/control/gettoken` (the GitHub access
token). It has two lanes: a UNIX socket bind-mounted into the sidecar on the userns-remap lane, and TCP
everywhere else — **including CRI/runsc, the production lane**.

**The TCP lane is architecturally broken and has never run.** Each spawn gets its own listener on a
*constant* port (`SidecarPort+2`), so the only thing making them unique is the **pod's IP** — and a node
cannot bind a pod IP, because it lives in the pod's netns:

```
listen tcp 10.234.0.2:8082: bind: cannot assign requested address
```

Point it at an address the node *can* bind (the CNI bridge gateway) and every spawn collides:

```
listen tcp 10.234.0.1:8082: bind: address already in use
```

Uniqueness was supposed to come from an address the host can never hold. It survived because the control
server is only constructed when the AS mint lane is configured (`internal/node/attach.go`:
`if cfg.GitHubMint != nil`), and no CRI lane ever had `AS_URL` set — so the branch never executed.
(Full diagnosis: `sp-2tx8.8`.)

## 2. The insight

**The node can freely dial *into* a pod. It just cannot *bind* the pod's IP.** The entire bug exists
because the design chose the one direction that does not work.

And the reverse channel **already exists**: the node dials the sidecar's control listener
(`SidecarPort+1`, bearer `SIDECAR_CONTROL_TOKEN`) for `/control/model`, and — decisively —
**`/control/credentials` already pushes a secret** (the model API key) into a running sidecar. Pushing
secrets to the sidecar is not a new pattern; it is the established one.

## 3. Design: invert it

The node **pushes**; the sidecar never asks. Delete the node's inbound listener entirely.

New sidecar endpoint, mirroring `/control/credentials`:

```
POST /control/github  {ca_cert_pem, ca_key_pem, token, token_expires_at}
```

Idempotent — it replaces the sidecar's current GitHub state.

### 3.1 Delivery points

| when | why |
|---|---|
| **sidecar-ready, before the agent starts** | the node already probes readiness here; the agent must never start without a working proxy |
| **on rotation** | the existing `githubRefresher` schedule (~8h lifetime, 8min refresh lead) |
| **on re-adopt** (SE3) | idempotent re-push after a node restart; covers a token that rotated while the node was down |

The sidecar **never restarts**: neither lane sets a restart policy, and on the Docker lane the agent joins
the *sidecar's* netns (`container:<sidecar>`), so a dead sidecar takes the pod's networking with it. This is
what makes push safe — there is no "restarted sidecar with no secrets and no way to ask" scenario. It starts
once, at pod creation, with the node right there.

### 3.2 Rejection detection: long-poll

Push loses one thing pull had: the sidecar could call `GetToken(spawnID, minRemainingSeconds, force)` and
**force a re-mint** when GitHub *rejected* a token mid-life (revoked link, out-of-band rotation). With push
alone, a dead token would stay dead until the next scheduled rotation — up to ~8 hours.

So the sidecar reports rejection, on a channel the node dials:

```
GET /control/github/events   → {"event":"token_rejected"}   (the MITM proxy saw 401/403 from upstream)
                             → 204 on a bounded timeout (~60s)
```

The node holds one long-poll per github-mounted spawn: dial → wait → handle → re-dial with backoff. **The
bounded timeout is load-bearing**: without it a silently-dead connection would stop detection forever and
nobody would know. Torn down when the spawn stops; re-established on adopt.

On `token_rejected` the node forces a re-mint (the refresher's existing `force` path) and pushes the result.

*Considered:* the node polling `/control/status` every ~60s — cheaper and simpler, same goroutine shape, but
up to a minute of latency. *Considered:* no detection at all — rejected, because it leaves a genuinely
recoverable case (GitHub rotating the token out-of-band) broken for hours with no self-healing.

## 4. Failure semantics

Push means **the node now owns delivery**, and silently failing to deliver a credential is exactly the
failure that must never be invisible.

- **Push at create fails → fatal.** Tear the pod down. (Today's semantics: a failed `FetchCA` makes the
  sidecar `os.Exit(1)`.)
- **Push on rotation fails →** retry with bounded backoff. If still undeliverable when the token expires,
  report the spawn's GitHub credential status as **`STALE`**.
- **Re-mint fails with `ErrGitHubRelinkRequired` →** report **`RELINK_REQUIRED`**. This is the real prize:
  today a revoked GitHub link surfaces to the user as an opaque git 401.

### 4.1 A condition, not a phase

A spawn whose GitHub token is stale is **still healthy for everything that is not git**. The existing phases
are `Starting / Active / Suspending / Stopped / Lost`; adding a `Degraded` phase would corrupt the lifecycle
state machine (what transitions *out* of it? what does the reconciler do with it?).

So this is a **condition**: the spawn stays `Active`, and a new `github_credential_status` enum
(`OK | STALE | RELINK_REQUIRED`) rides on the existing node→CP spawn report and is surfaced in the UI.

## 5. What this deletes

- `internal/node/githubcontrol.go`: `Serve`, the `listeners` map, `tcpAuthMiddleware` (bearer + source-IP).
- The **entire UDS/TCP lane split** in `manager.go` — the control dir, its bind mount,
  `SIDECAR_GETTOKEN_ADDR`, `SIDECAR_GETTOKEN_BEARER`.
- `GETTOKEN_LISTEN_IP` (added only hours earlier — and good riddance).
- The sidecar's `FetchCA` bounded-retry loop.
- **`sp-2tx8.8`'s bind bug, by construction** — there is no listener to bind.

Both lanes existed *only* to host the inbound listener. Removing it removes them.

## 6. Security

**The node ends up with no inbound surface reachable from any pod.** Today's listener sits on the CNI bridge,
reachable from every pod on it, guarded by a per-spawn bearer plus a source-IP check. After this, that surface
does not exist.

The surviving direction (node→sidecar) is authenticated by the control token already in the sidecar's env —
the same credential that already guards `/control/model` and `/control/credentials`.

**Stated plainly, not buried:** the CA private key and the GitHub token travel the pod network
authenticated but **in the clear**. That is *already true today*, in the opposite direction. The posture is
unchanged; this design does not improve it and does not worsen it. Fixing it is §8.

## 7. Testing

- **Hermetic:** push at ready / on rotate / on adopt; a long-poll `token_rejected` drives a re-mint and a
  re-push; a failed rotation push yields `STALE`; a relink-required mint yields `RELINK_REQUIRED`.
- **Adversarial:** assert the node exposes **no** pod-reachable control endpoint (it should not exist).
- **The VM lane (the proof):** the agent-side `git fetch` through the sidecar's MITM (`sp-wwtc.5`) — the
  first end-to-end exercise this proxy has ever had — and then `sp-2tx8.3.8` (the CA survives a node restart).

## 8. Out of scope — removing plaintext from the pod network

Encrypting this channel and giving each spawn a real identity is **tracked under the unified-auth epic
`sp-dvke`**, not here, and depends on this work.

That epic (`sp-dvke.2`) has already re-done the trust chain as **SPIFFE URI-SAN identities**
(`spiffe://<td>/node/self-hosted/<account>/<node>`, `spiffe://<td>/service/cp/<instance>`, …), with issuance
and typed principal verification in `sp-dvke.2.1`. A spawn identity is a natural addition to that scheme, and
the node↔sidecar channel then becomes mTLS with both ends validating — at which point
`SIDECAR_CONTROL_TOKEN` has no job left and goes.

**One trap recorded so the next person does not fall into it** (it caught this author): the obvious design —
give the node a *name-constrained subordinate CA* so it can only mint its own spawns' identities — **does not
work under URI SANs.** `sp-dvke.2`'s own notes state it: *"URI name constraints cannot constrain path."* The
constraint mechanism that makes the DNS-SAN scheme safe has no URI equivalent. Their established answer is a
policy check at verification — *"identity path must correspond to issuer role"* — and any spawn-SVID design
must use that, not a crypto name-constraint.

Sequencing is deliberate: this inversion fixes a **broken production lane**, and coupling that fix to a
certificate-authority change is how neither ships. Inversion first; it also makes the mTLS work *smaller*, by
collapsing two lanes into one channel.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from the
assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
