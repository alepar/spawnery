# Spawnery E1 — Runtime & Orchestration Core (Design)

**Bead:** `sp-ei4`
**Status:** Draft v1 (interview complete; pending user review)
**Date:** 2026-05-27
**Depends on:** [E0 contracts](2026-05-26-spawnery-e0-contracts-design.md)
**Parent:** [System design](2026-05-26-spawnery-system-design.md)

> **⚠️ Demo-MVP overlay** ([Demo MVP Scope](2026-05-28-spawnery-demo-mvp-scope.md)): demo = B+Y
> execution/inference **+ an open third-party marketplace**, so isolation is **HARDENED, not plain
> containers** — open creator-authored agents run on other users' data: gVisor-class isolation +
> **cgroup CPU/mem/disk/pids limits** + per-user concurrency cap + a **per-spawn egress allowlist
> floor** (block cloud-metadata + RFC1918) (`sp-eha`/`sp-ach`/`sp-rpa`). **No cloud burst** (§5
> trigger deferred); placement is trivially "the home server." Cold-start (§3): continuity =
> **file reads in a fresh session**, *not* prior-thread replay (`sp-0ah`); accept cold-start times.

The spine of the platform and of the zork vertical slice: the node agent, the cold-start spawn
lifecycle, placement + the burst trigger, the image model, and the isolation-backend abstraction.

> Cross-epic: storage `materialize`/`persist` → **E3**; inference routing & central gateway →
> **E2**; cloud-burst *provisioning* → **E9**; session token / rendezvous → **E0**.

---

## 1. Spawn unit composition

A spawn is a **pod of two containers sharing one network namespace** (so the agent reaches the
sidecar on `localhost`):

```
 pod (per spawn, ephemeral)
 ├── base container  =  per-agent base image: [ chosen agent (ACP/stdio) + ACP-bridge + common toolset ]
 │       mounts:  /app   (App@sha definition: persona + skills + repo-shipped scripts, read-only)
 │                /data  (the spawn's git working tree — owned by E3)
 └── sidecar container = model gateway (OpenAI-compatible; config injected; BYO key client-delivered)
```

- The **ACP-bridge** spawns the agent as a stdio subprocess and exposes ACP to the **node over
  loopback** (the node terminates the client E2E channel and forwards; E0 §10). It lives **in the
  base image** (must be co-located to speak the agent's stdio).
- The **sidecar** is a separate Spawnery container (matches "model gateway is a sidecar container").

---

## 2. Image model (refines E0 §7/§3)

- **One base image per supported agent** (+ agent version): `agent + ACP-bridge + common toolset`.
  Maintained by Spawnery; distributed via a small registry; nodes (home/burst/self-host) pull by
  digest. **No per-`(App, agent)` build, no per-App registry.**
- **At spawn start**, the node **mounts** the `App@sha` definition (persona, skills, repo-shipped
  scripts) read-only at `/app`. The agent **imports skills via its native process**.
- **Tools** = the curated **common toolset baked into the base** + static/script tools the App
  ships in its repo. **App-specific compiled/installed native tools beyond the common set →
  post-MVP.** Launch apps (zork, wiki, coaches) fit the common set.

---

## 3. Cold-start lifecycle (state machine)

**Wake strategy = cold start every wake** (no warm pool; warm pools are a post-MVP latency
optimization). In-memory agent state never survives idle — continuity is via the persisted thread.

```
 IDLE (no container) ──wake──▶ STARTING ──ready──▶ ACTIVE(session) ──┐
   ▲                                                                  │ idle-timeout
   │                                                                  │ OR explicit close
   └──────────────── STOPPING ◀──────────────────────────────────────┘ OR max-session cap
```

**STARTING sequence (cold start):**
1. Scheduler selects a node (§5) and sends `startSpawn` over the node's outbound gRPC stream.
2. Node ensures the per-agent base image is present (pull by digest if missing).
3. Node creates the pod; mounts `/app` (App@sha definition) and `/data` (E3 `materialize`).
4. Start the **sidecar** with injected model config (provider/model/baseUrl; managed→central
   gateway token, or BYO→awaits client-delivered key).
5. Start the **base** container; the ACP-bridge launches the agent (stdio), points it at the
   sidecar on `localhost`, and the agent imports skills from `/app`.
6. Client + node establish the **per-session E2E channel** (E0 §10): client fetches the CP-vended
   **node pubkey** + presents the CP-issued **session token** (authorizes the owner); key agreement
   (node-static + client-ephemeral) → per-session symmetric key.
7. ACP traffic + any secrets flow **AEAD-encrypted over that channel, CP-relayed (opaque)**; the
   **node decrypts and forwards to the in-container agent over loopback** → **ACTIVE**.

**Resume:** a later message on an idle spawn re-runs STARTING; the agent reloads the prior thread
from `/data` to restore context.

---

## 4. Session teardown

Triggered by **idle timeout** (configurable, ~10 min default) **OR explicit session close** **OR a
max-session-duration cap**. On teardown: final `persist` (E3) if needed, then **destroy the pod**
(no cross-user reuse). Spawn returns to IDLE (no standing container).

---

## 5. Placement & burst

**Placement rule:**
- **Open/public app AND the owner has an attached self-hosted node → run on the owner's node**
  (unaudited).
- **Otherwise → cloud** (Spawnery home machine). **Private apps are always cloud.**
- Within the chosen pool, pick the least-loaded node with capacity.

**Burst = two independent signals on the home server (carried in the node heartbeat):**
- **CPU overloaded → offload *agent containers*** — schedule new spawn pods onto cloud burst nodes
  (E1 trigger; provisioning is **E9**).
- **GPU overloaded → offload *inference*** — the central gateway routes away from local DeepSeek to
  cloud model providers (**E2**).

So a spawn's **container placement (CPU-bound)** and its **inference routing (GPU-bound)** burst
independently. Thresholds are tunable.

---

## 6. Node agent responsibilities

- Maintain the **persistent outbound gRPC stream** to the central CP: `register`, `heartbeat`
  (capacity + **CPU + GPU** metrics), `spawnStatus`, **relay frames** (node end of the rendezvous).
- Execute `startSpawn` / `stopSpawn`; own the spawn-pod lifecycle.
- Drive containers through the **isolation-backend interface** (§7).
- Invoke **storage adapters** (E3) for `materialize` / `persist`.
- Terminate the node end of the E2E relay (opaque ciphertext only).

---

## 7. Isolation backend

Pluggable behind the E0 §8a interface (`start/attach/stop/status`). Concrete impls per environment;
**MVP builds the Docker/Podman backend first** (local dev + home machine). gVisor-class (self-host
hardening) and microVM / VM-per-App (cloud burst) come later behind the same interface.

---

## 8. Deferred (post-MVP)
Warm pools (wake latency) · App-specific native tool provisioning beyond the common set · gVisor /
microVM isolation backends · cloud-burst provisioning (E9) · P2P data path (E0).

---

## Appendix — E1 decision log

| # | Decision | Choice |
|---|---|---|
| E1.1 | Image assembly | Pre-baked **per-agent base** (agent + bridge + common toolset) + **mount** App definition; no per-`(App,agent)` build |
| E1.2 | Wake strategy | **Cold start every wake** (warm pools post-MVP) |
| E1.3 | Placement | Open app + owner's attached node → owner's node (unaudited); else cloud (private always cloud) |
| E1.3b | Burst | **2-D**: CPU overload → offload agent containers; GPU overload → offload inference to cloud |
| E1.4 | Session teardown | Idle timeout **or** explicit close **or** max-session cap; resume = cold start + reload thread from /data |
| E1.5 | Composition (asserted) | Pod: base[agent+bridge+toolset] + sidecar, shared netns; `/app` + `/data` mounts |
| E1.6 | Isolation MVP (asserted) | Docker/Podman first; gVisor/microVM later, same interface |
