# Spawnlet — Vertical Slice (Design)

**Status:** Approved in brainstorming — pending written-spec review
**Date:** 2026-05-29
**Context:** First code in the repo. Grounds the node-side runtime (the "spawnlet") from
[E1 runtime core](2026-05-27-spawnery-e1-runtime-core-design.md) +
[E0 contracts](2026-05-26-spawnery-e0-contracts-design.md) +
[E2 model layer](2026-05-27-spawnery-e2-model-layer-design.md), reduced to a single-box slice.

> **⚠️ Amended by [Per-Mount Data Backends](2026-05-29-data-mounts-design.md):** the mount model
> changed after this slice shipped. **cwd is now `/app`** (not `/data`); data lives in **named rw
> mounts inside `/app`** (`/app/<path>`); the slice implements only the **`scratch`** backend
> (seed-then-nuke) behind a `StorageBackend` seam; the `/data` mount, the `copySeed`-to-`/data`, and
> the entrypoint `AGENTS.md` copy are **removed** (Goose reads `/app/AGENTS.md` in place). `Stop` now
> finalizes mounts (scratch nukes). See the data-mounts design for the authoritative mount/teardown
> shape; §5/§7/§8 below describe the pre-amendment slice.

---

## 1. Goal & boundary

Prove **end to end**, on one box, with no CP:

> client → spawnlet → **`CreateSpawn`** for a test Spawnery app → spawnlet creates an **agent
> container + inference-proxy sidecar + `/app` and `/data` mounts** → client **sends input to the
> agent and receives streamed output**.

Everything is Go + ConnectRPC. The slice deliberately omits the CP, auth, the E2E channel, storage
adapters, trust tiers/scanner, isolation hardening, and scheduling (see §10).

---

## 2. Components (each independently testable)

| Component | What it is | Depends on |
|---|---|---|
| **spawnlet** | Go service: ConnectRPC server + SpawnManager + ContainerRuntime + Relay + SpawnStore | Docker Engine API |
| **sidecar** | tiny Go image: OpenAI-compatible reverse proxy → OpenRouter, injects the key | OpenRouter |
| **agent base image** | **Goose** (ACP) + toolset, configured to talk ACP over stdio + use the sidecar | Goose |
| **example spawn app** | a definition dir: `spawneryapp.yml` + `persona.md` + `seed/` | — |
| **slice client** | Go CLI: Connect client + minimal **ACP client**; `createSpawn` then interactive loop | spawnlet |
| **stub ACP agent** | deterministic **test double** (tiny Go program speaking minimal ACP) | — |

**spawnlet internal units:**
- **ConnectRPC server** — `SpawnService` (§4).
- **SpawnManager** — per-spawn lifecycle state machine.
- **ContainerRuntime** — thin adapter over the **Docker Engine API** (Go SDK), so the runtime is
  swappable later (Podman/gVisor/microVM per E1).
- **Relay** — pipes the Connect `Session` stream ↔ the agent container's **stdio** (transparent
  bytes; see §5).
- **SpawnStore** — in-mem `spawnId → { sidecarID, agentID, attachStream, status }`.

---

## 3. The agent — Goose

- **Goose** (Apache-2.0): speaks **ACP** (Zed's Agent Client Protocol — JSON-RPC/stdio), runs
  headless, works over a working directory with built-in file/shell tools.
- **Model routing:** Goose's **OpenAI provider with a custom base URL** points at the sidecar on
  `localhost:<port>` — so Goose calls our proxy, which forwards to OpenRouter. Goose never holds the
  OpenRouter key.
- **Base image** = `goose + toolset`, launched in ACP mode over stdio; mounts `/app` (ro) + `/data`
  (rw, = cwd).
- **Implementation unknowns to confirm during build (small):**
  1. exact Goose **ACP launch invocation** (subcommand/flags) for headless stdio driving;
  2. how to **inject the app's persona/instructions** (Goose system-prompt / hints mechanism, from
     `/app`);
  3. setting **cwd = `/data`** (likely via the ACP `session/new` `cwd`, sent by the client; confirm
     Goose honors it).
- **The stub ACP agent** (§2) is a *test double*, not the product — it lets us build + test the
  spawnlet/relay/sidecar/client with no LLM or network. Swapping stub ↔ Goose is just a base-image
  change (the spawnlet is agent-agnostic).

---

## 4. API — ConnectRPC (client↔spawnlet now; CP↔spawnlet later)

```proto
service SpawnService {
  rpc CreateSpawn(CreateSpawnRequest) returns (CreateSpawnResponse);  // unary
  rpc Session(stream Frame) returns (stream Frame);                   // bidirectional (HTTP/2)
  rpc StopSpawn(StopSpawnRequest)   returns (StopSpawnResponse);      // unary
}

message CreateSpawnRequest {
  string app_path  = 1;          // local dir with spawneryapp.yml (+ persona, seed) — slice only
  string data_path = 2;          // optional; empty → spawnlet allocates a fresh dir
  string model     = 3;          // OpenRouter model id (e.g. "anthropic/claude-3.5-sonnet")
}
message CreateSpawnResponse { string spawn_id = 1; }

message Frame { string spawn_id = 1; bytes data = 2; }   // data = opaque ACP JSON-RPC bytes
message StopSpawnRequest  { string spawn_id = 1; }
message StopSpawnResponse {}
```

- **Same definitions serve both surfaces** — the slice client plays the role the CP will later play.
- **Bidi `Session` needs HTTP/2** (fine for the Go client). A *browser* client later would use
  server-stream + unary-send (connect-web lacks full bidi) — recorded, not built now.

---

## 5. Transparent byte relay (key design property)

Per the architecture, **the client is the ACP client** and the node just forwards. So the spawnlet
**does not parse ACP** — it is a transparent pipe:

```
client (ACP client) ──Frame{bytes}──▶ spawnlet ──write──▶ agent container stdin
client (ACP client) ◀─Frame{bytes}── spawnlet ◀──read─── agent container stdout
```

- `Session` opens once `CreateSpawn` has the pod running; the spawnlet wires the Connect stream to
  the agent's **attached stdio** (Docker `ContainerAttach`, hijacked conn).
- ACP framing (newline-delimited / Content-Length — whatever Goose uses) is handled by the **client
  and Goose**; the spawnlet moves bytes, framing-agnostic.
- The **client** drives ACP: `initialize` → `session/new` (cwd=`/data`) → `session/prompt`; renders
  streamed `session/update`. Minimal ACP client logic lives in the CLI.

---

## 6. Data flow

```
client ──Connect/HTTP2 (Frame=ACP)──▶ spawnlet ──docker attach stdio (ACP)──▶ Goose (agent)
                                                       Goose ──localhost HTTP (OpenAI)──▶ sidecar ──HTTPS──▶ OpenRouter
client ◀──Connect stream (Frame=ACP)── spawnlet ◀──stdio (session/update)──── Goose
```

---

## 7. Pod shape (shared netns)

`[ sidecar — netns owner, holds OpenRouter key, egress, listens localhost:<port> ]`
`+ [ Goose — joins netns via --network container:<sidecar>; /app (ro) + /data (rw); stdio attached ]`

Matches E1's production pod (agent ↔ sidecar on localhost). Agent needs **no network ingress** — the
spawnlet drives it over stdio, not the network.

---

## 8. Lifecycle

**CreateSpawn:**
1. Validate request; allocate `spawn_id`.
2. Resolve `/data` (use `data_path`, else allocate `…/spawns/<id>/data`); if the app has `seed/`,
   copy it in (E3-style scaffold).
3. Ensure the agent base image is present (pull if missing).
4. **Start sidecar** (netns owner; OpenRouter key + upstream injected from spawnlet env/config).
5. **Start Goose** container: `--network container:<sidecar>`, mounts `/app` ro + `/data` rw,
   **stdio attached** (`OpenStdin` + `AttachStdin/Stdout`). Goose config: OpenAI provider
   `base_url = http://localhost:<port>` (the sidecar) **and `model` = the request's `model`** (Goose
   puts it in the OpenAI request body; the sidecar just forwards).
6. Record in SpawnStore; return `spawn_id`.

**Session:** the stream is **bound to a `spawn_id`** (carried on the first `Frame`); the spawnlet
looks up the running spawn and bridges the Connect stream ↔ that agent's attach stream (both
directions) until either side closes. (One active Session per spawn in the slice.)

**StopSpawn** (or client disconnect): stop + remove both containers; **`/data` persists on host** for
inspection. (Idle-timeout teardown is out of slice — manual stop.)

State machine: `CREATING → READY → (SESSION active) → STOPPING → GONE`. Any create-step failure →
**tear down whatever started** (no orphans) → return a Connect error.

---

## 9. Error handling (slice-appropriate)

Map to clear Connect error codes + messages: image-pull failure, container start failure, sidecar
not reachable, agent exits early (surface stderr tail), OpenRouter error (surfaced through the
sidecar's response). Partial-create always rolls back started containers. Session stream closes
cleanly when the agent process exits.

---

## 10. Explicitly out of slice

CP connection + node enrollment; auth; the per-session E2E channel (client↔spawnlet is plain Connect
here); storage adapters / GitHub / managed storage (local paths only); trust tiers / scanner /
egress floor / isolation hardening / resource limits; idle-timeout; multi-spawn scheduling +
placement; metering/audit; the real model gateway (OpenRouter via a thin proxy stands in).

---

## 11. Testing

- **ContainerRuntime** — integration against a real Docker daemon (create/start/attach/stop/remove).
- **sidecar** — unit (key injection + upstream rewrite via `httptest`) + one live OpenRouter smoke.
- **Relay** — byte-fidelity against the **stub ACP agent** (bytes in == bytes out, both directions).
- **End-to-end (stub)** — `client → CreateSpawn → prompt → streamed reply` with the stub (no network):
  deterministic CI.
- **End-to-end (Goose)** — same flow against the real Goose base image + a live OpenRouter round-trip
  (the acceptance demo).

---

## Appendix — decision log

| # | Decision | Choice |
|---|---|---|
| S.1 | Slice goal | spawnlet orchestration + bidi message relay, single box, no CP |
| S.2 | Language | **Go** (service + client) |
| S.3 | Transport/API | **ConnectRPC** both surfaces; `CreateSpawn` unary + `Session` bidi + `StopSpawn` |
| S.4 | Agent driving | spawnlet ↔ agent over **container stdio** (ACP), via Docker attach |
| S.5 | Relay model | **transparent byte relay** — ACP lives in client + agent; spawnlet is a dumb pipe |
| S.6 | Pod networking | **shared netns** (sidecar owns; agent joins; agent→sidecar on localhost) |
| S.7 | Sidecar | **minimal custom Go proxy** → OpenRouter, injects key (agent never holds it) |
| S.8 | Inference | **OpenRouter** (OpenAI-compatible) — dev-slice only; cuts against prod privacy, temporary |
| S.9 | Container runtime | **Docker Engine API** (Go SDK), behind a swappable ContainerRuntime adapter |
| S.10 | Agent | **Goose** (Apache-2.0); OpenAI provider w/ custom base URL → sidecar (`sp-07i` resolved) |
| S.11 | Agent dependency | build/test against a **stub ACP agent** test double; swap in Goose (base-image change) |
| S.12 | App/data provisioning | **local paths** (slice); fresh `/data` per spawn + optional seed copy |
