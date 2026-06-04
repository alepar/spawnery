# Manual Verification Checklist

Everything shipped since the web UI adopted the virtualized chat (react-virtuoso, framework
adoption `b83c62e`). Each item has a one-paragraph summary and a checklist of things to verify **by
hand** — the unit/integration suites already cover the internal logic; this list is for end-to-end,
UI, and real-host behavior a human should confirm before the demo.

**Bring-up (referenced throughout):**
```bash
just cp        # control plane → 127.0.0.1:8080  (CP_DEV_TOKENS=dev-token=alice)
just node      # spawnlet attached to the CP (NODE_ID=node-1, goose agent)
just web       # web UI (vite) → printed localhost URL
# one-shots:  just spawnctl "<prompt>"   ·   just test-egress   ·   go test ./...
```
Legend: 🌐 needs the web UI · 🖥️ needs a running CP+node · 🔒 needs a privileged host (Docker+iptables+root) · ✅ already automated.

---

## Phase 5 verification pass — 2026-06-04 (real runsc host: Fedora CoreOS, runsc release-20260525.0)

Driven via `E2E_TEST_RUNSC.md`. Boxes below are ticked where verified this session; per-section status:

- **§A** lifecycle — PARTIAL: create/list/active/stop + rename/suspend/resume/two-instances/reload verified via Playwright (web e2e **13/13**) + CP-CLI; `cp -race` green. Not walked live: 2nd-token ownership isolation, kill-node→`unreachable` boot-reconcile, sqlite row spot-check (all hermetic-covered).
- **§B–F** marketplace — PARTIAL: Browse (4 seeded apps)/Detail/Spawn/Publish→register/My-Apps verified via Playwright; validate / sticky-creator / tier / pinning matrix is hermetic-green. Full CP-API matrix not individually walked.
- **§G** web UI — **VERIFIED**: Playwright marketplace + lifecycle specs 13/13, incl. the second-app-while-running re-entrancy case (a real race fixed this session, commit `7ab6597`); vitest green.
- **§H** egress floor — **VERIFIED (host)**: `just test-egress` + `just test-cni-egress` green; in-agent metadata+RFC1918 blocked, public+DNS reachable. (Floor is host-side `SPAWNLET-EGRESS`/`DOCKER-USER`; the in-netns OUTPUT check is the legacy model.) Fail-closed / enforce-toggle are hermetic-tested.
- **§I** node-class — **VERIFIED (live)**: `spawn_create` telemetry carries `node_class`+`node_id`.
- **§J** limits/quota/runtime — **VERIFIED (live)**: cgroup `memory`=512 MiB + `cpu`=0.5 exact, runtime handler `runsc`, per-user cap (2nd `CreateSpawn` → `ResourceExhausted`). Fork-bomb smoke not run.
- **§K** scheduler routing — HERMETIC ONLY: `PickFor` + 3 policy tests green; live author-self-host routing not walked.
- **§L** Postgres — PARTIAL: default→sqlite boots; no-DSN→fatal + `storeConfigFromEnv` green. Full Postgres round-trip not run (needs a Postgres instance).
- **§M** DNS carve-out — **VERIFIED (host)**: RFC1918 resolver (`192.168.1.1`) pod resolves the model upstream through the floor's `:53` carve-out.
- **§N** runsc one-sandbox pod — **VERIFIED (host)**: full goose round-trip (`QUOKKA-4417`), one runsc sandbox / 2 containers, per-pod floor + `FORWARD` jump, in-agent egress, clean teardown, 5/5 cycles — closes `sp-vaw`/`sp-ghx`. Needed the TCP-on-pod-IP ACP transport + `POD_DNS` fixes (commit `b4e1b4b`).

**Genuinely remaining (not run):** §L full Postgres round-trip (needs Postgres) · §K live routing · §A 2nd-token isolation + node-kill reconcile · §J fork-bomb smoke · §H fail-closed/enforce-toggle live (all hermetic-covered). Observability follow-ups tracked in epic `sp-209`.

---

## A. CP durable state & spawn lifecycle (`sp-mqj`, `sp-pc4` parts 1–3a)

**Summary.** The control plane gained a durable state layer (`internal/cp/store`, Bun over
SQLite/Postgres with goose migrations): owners, apps/versions, and a spawn ledger with a
running-container episode entity (DB-enforced single-live invariant). The CP was rewired off
in-memory maps onto this store — `CreateSpawn` writes a durable `starting`→`active` record under a
per-spawn lock, ownership is store-authoritative on both gRPC and the WS path, `StopSpawn`
soft-deletes, node eviction marks spawns `unreachable`, and boot reconciles orphans. New RPCs:
`ListSpawns` (the durable ledger) and `DeleteSpawn`. Wire contracts for the full lifecycle
(generation fencing, Suspend, mounts, node inventory) are defined but node-side suspend/resume is a
later epic.

**Verify:**
- [x] 🖥️ Create a spawn (`just spawnctl "hi"` or via 🌐 web), then call `ListSpawns` (web Chat/sidebar or a Connect call) — the spawn appears with status `active`, the right `app_id`, and your owner.
- [x] 🖥️ Stop/delete the spawn (`StopSpawn`/`DeleteSpawn`) — it disappears from `ListSpawns` (soft-deleted), and the agent/sidecar containers are gone (`docker ps`).
- [ ] 🖥️ A second `ListSpawns` as a **different** dev token shows only that owner's spawns (ownership isolation).
- [ ] 🖥️ Kill the node process mid-spawn, restart the CP — the orphaned spawn shows `unreachable` (boot reconcile), not `active`.
- [x] ✅ `go test ./internal/cp/... -race` is green (store invariants, guarded transitions, ledger).
- [ ] Spot-check the SQLite file (`CP_STORE_DSN`, default `cp.db`): `spawns`/`spawn_containers` rows look right; at most one live container per spawn (`ended_at IS NULL`).

---

## B. Marketplace catalog — read surface (`sp-0sc`, E5 slice 1)

**Summary.** `ListApps(query)` (browse/search) and `GetApp(id)` (detail) over a CP catalog model:
apps carry display name/summary/tags/visibility/listed; versions carry a **trust tier**
(`unverified`/`scanned`/`reviewed`). The CP is seeded with a 4-app demo lineup (wiki, language,
interview, zork), all `reviewed`.

**Verify:**
- [x] 🖥️ `ListApps` with empty query returns the 4 seeded apps, each with a tier of `reviewed`.
- [ ] 🖥️ `ListApps` with a query (e.g. `research`) returns only matching apps (case-insensitive over name/summary/tags).
- [x] 🖥️ `GetApp` for a seeded id returns its summary + version list + tiers.
- [ ] 🖥️ `GetApp` for an unknown id → `NotFound`; `ListApps` without an auth token → `Unauthenticated`.

---

## C. Marketplace — app-version registration (`sp-ma8`, E5 slice 2)

**Summary.** `RegisterAppVersion` takes the **full app manifest as structured API input** (the source
of truth — the CP never fetches/parses `spawneryapp.yml`); it validates structurally, records the
creator (sticky ownership), and lists the version at tier `unverified`. `spawnctl register` is a
reference CI client that maps a local `spawneryapp.yml` → the API.

**Verify:**
- [ ] 🖥️ `spawnctl -cp http://127.0.0.1:8080 -register -app examples/secret-app -version 1.2.0 -ref creator/app@sha` registers it; the response tier is `unverified`.
- [ ] 🖥️ The just-registered app now appears in `ListApps`/`GetApp` (tier `unverified`, below the reviewed seeds in browse order).
- [ ] 🖥️ Registering a **new version of the same app as a different owner** → `PermissionDenied` (creator is sticky).
- [ ] 🖥️ A malformed manifest (bad `apiVersion`, id without `creator/app`, `visibility: private`, duplicate mount name) → `InvalidArgument` with a clear message.
- [x] ✅ `manifest.Validate` table test + store sticky-creator test green.

---

## D. Marketplace — version selection & pinning (`sp-bm8`, E5 slice 3)

**Summary.** `CreateSpawn` accepts an optional `version` + `pin`. Empty version → latest reviewed
(unchanged default); explicit version is resolved and its tier governs spawnability/placement (see
§K). `pin` records that the spawn won't auto-upgrade.

**Verify:**
- [ ] 🖥️ `CreateSpawn` with no version spawns the latest **reviewed** version of the app.
- [ ] 🖥️ `CreateSpawn` with an explicit reviewed version spawns exactly that version (check the stored `app_version`/`app_ref`).
- [ ] 🖥️ `CreateSpawn` with `pin=true` records `pinned` on the spawn row; `pin=false` does not.
- [ ] 🖥️ An unknown version → `InvalidArgument`.

---

## E. Marketplace — detail enrichment & moderation (`sp-m9f`, E5 slice 4)

**Summary.** `GetApp` now returns the latest version's **parsed manifest** (model/agents/tools/persona/
mounts) for a rich detail view. `SetAppListing` lets the **creator** take down (`listed=false`) or
relist their app; unlisted apps drop out of `ListApps`/`GetApp`.

**Verify:**
- [ ] 🖥️ `GetApp` on a **registered** app returns a non-nil `manifest` with title/description/mounts; on a seed app (no stored manifest) `manifest` is null but the summary still returns.
- [ ] 🖥️ `SetAppListing(listed=false)` as the creator → the app disappears from `ListApps` and `GetApp` → `NotFound`; `SetAppListing(listed=true)` restores it.
- [ ] 🖥️ `SetAppListing` by a non-creator → `PermissionDenied`; on a missing app → `NotFound`.

---

## F. Marketplace — creator "My Apps" (`sp-3pm`, E5 slice 5)

**Summary.** `ListMyApps` returns the caller's own apps **including unlisted/taken-down** ones (so a
creator can find and relist them), each with its tier + listed state. `AppSummary` gained a `listed`
field, populated across all catalog responses.

**Verify:**
- [ ] 🖥️ Register two apps, take one down; `ListMyApps` returns **both**, one `listed=true` one `listed=false`.
- [ ] 🖥️ Another owner's apps never appear in your `ListMyApps`.
- [ ] 🖥️ Public `ListApps` results all carry `listed=true`.

---

## G. Marketplace web UI (`sp-znq`, E6) 🌐

**Summary.** The placeholder Marketplace view is now a 4-tab UI (React/shadcn) consuming the catalog
RPCs: **Browse** (search + tier-badged cards), **Detail** (manifest + versions + **Spawn**), **My
Apps** (takedown/relist toggle), **Publish** (form → `RegisterAppVersion`). "Spawn" re-spawns the
chosen app on-demand (a refactored `spawnApp` with a re-entrancy guard) and jumps to Chat.

**Verify (🌐 `just web` + 🖥️ CP/node up):**
- [x] Browse tab shows the seeded apps as cards with correct tier badges; the search box filters them.
- [x] Click a card → Detail shows the manifest fields + a versions list + a Spawn button; Back returns to Browse.
- [x] Click **Spawn** → the view switches to Chat, status reaches `ready`, and a prompt gets a streamed reply (confirms the marketplace→spawn→ACP path end-to-end).
- [x] Spawn a **second** app from the marketplace while one is running → the old session tears down cleanly and the new one reaches `ready` (no stale/duplicate WS, no clobbered status — the re-entrancy guard).
- [x] **My Apps** tab lists your registered apps incl. unlisted; the listing toggle takes one down (it drops out of Browse) and relists it.
- [x] **Publish** tab: fill id/title/version/ref (+ a mount row), submit → success toast, and the app appears in My Apps as `unverified`. A bad submit surfaces the CP error as a toast.
- [x] Light/dark theme + no raw-color regressions; no console errors.
- [ ] ✅ `cd web && npm test` (24 Vitest tests) green; `npm run build` clean. (Playwright `marketplace.spec.ts` needs a browser + the stack.)

---

## H. Egress allowlist floor + node class (`sp-rpa`) 🔒

**Summary.** Each spawn pod's network namespace gets a host-applied iptables **block-floor**: drop
cloud-metadata (`169.254.0.0/16`) + RFC1918, allow loopback + DNS + operator CIDRs, default-allow
public. Applied **after the sidecar, before the agent** (no unprotected window), **fail-closed**
(can't apply → spawn aborts). Governed by node class: **cloud** always enforces (non-disableable);
**self-hosted** honors `EGRESS_ENFORCE` (default on).

**Verify:**
- [x] 🔒 `just test-egress` PASSES on a privileged host (this is the real packet-drop test).
- [x] 🔒✅ Already host-verified here: metadata + RFC1918 dropped (iptables counters + curl blocked), public-by-IP reachable, DNS `:53` allowed.
- [ ] 🖥️ Start a spawn on a cloud node and `nsenter` into the sidecar netns (`docker inspect` → pid) → `iptables -S OUTPUT` shows the lo/DNS ACCEPTs then the metadata/RFC1918 DROPs.
- [x] 🖥️ Inside a running spawn's agent: `curl http://169.254.169.254/` fails, `curl https://1.1.1.1/` works, `curl https://api.openrouter.ai/` resolves+connects (DNS carve-out).
- [ ] 🖥️ Fail-closed: on a host **without** iptables (or with `EGRESS_ENFORCE` effective but the applier broken), a cloud spawn **does not start** (and the sidecar is torn down) rather than running unprotected.
- [ ] 🖥️ `NODE_CLASS=self-hosted EGRESS_ENFORCE=false` → spawn runs unrestricted with a loud WARNING log; `NODE_CLASS=cloud` ignores `EGRESS_ENFORCE=false` and still enforces.

---

## I. Node-class propagation to CP (`sp-2as`)

**Summary.** The node reports its `node_class` at registration; the CP records it on the in-memory
registry node and stamps it on the `spawn_create` telemetry event.

**Verify:**
- [ ] 🖥️ Start the node with `NODE_CLASS=self-hosted`; after a spawn, the `spawn_create` line in `telemetry/events.jsonl` has `node_class: "self-hosted"`.
- [x] 🖥️ An unset `NODE_CLASS` is recorded as `cloud` (safe default) in telemetry.
- [x] ✅ Registry-records-class + telemetry-carries-class tests green.

---

## J. Resource limits, per-user quota, isolation runtime (`sp-ach`) 🔒

**Summary.** Each pod container gets cgroup **mem/CPU/pids** limits (`MEM_LIMIT_MB`/`CPU_LIMIT`/
`PIDS_LIMIT`, defaults 1024/1.0/256) via Docker `HostConfig`; an optional **gVisor** runtime
(`CONTAINER_RUNTIME=runsc`); and a CP-side **per-user concurrent-spawn cap**
(`CP_MAX_SPAWNS_PER_OWNER`, default 5).

**Verify:**
- [x] 🔒✅ Already host-verified: a container started with the limits has kernel cgroup-v2 `memory.max`/`pids.max`/`cpu.max` matching the config exactly.
- [ ] 🖥️ Start a spawn, `docker inspect` the agent + sidecar → `HostConfig.Memory`/`NanoCpus`/`PidsLimit` reflect the env (both containers).
- [x] 🖥️ Per-user cap: with `CP_MAX_SPAWNS_PER_OWNER=1`, the **second** `CreateSpawn` for the same owner → `ResourceExhausted`; `=0` → unlimited.
- [x] 🔒 (If `runsc` installed) `CONTAINER_RUNTIME=runsc` → `docker inspect` shows `Runtime: runsc`; spawn still works.
- [ ] 🖥️ Smoke: a fork bomb inside the agent is capped at `PIDS_LIMIT` (doesn't take down the host).

---

## K. Scheduler routing — author-self-host for unverified (`sp-t5p`)

**Summary.** The node also reports its `node_owner`. `CreateSpawn` computes node **placement** from
the resolved version's tier: reviewed/scanned route anywhere; an **unverified** version may run
**only on a self-hosted node owned by the app's author** (caller must be the creator). This unblocks
authors iterating on their own apps with zero review and zero added risk.

**Verify (🖥️ needs a self-hosted node with `NODE_OWNER` set):**
- [ ] As the author, with a node `NODE_CLASS=self-hosted NODE_OWNER=alice` registered, `CreateSpawn` of **your own unverified version** spawns on that node.
- [ ] The **same** unverified spawn attempt by a **non-author** → `PermissionDenied`.
- [ ] The author attempting it with **only a cloud node** available → `ResourceExhausted` ("no eligible node").
- [ ] A **reviewed** app still spawns on any node (cloud included) — routing unchanged.
- [x] ✅ PickFor placement + the three policy-outcome tests green.
- [ ] ⚠️ Known gap: the web Detail "Spawn" sends no version → latest reviewed, so spawning an unverified version from the **UI** needs the (filed) version-selector follow-up — verify via API/`spawnctl` for now.

---

## L. Postgres store driver (`sp-ylw`)

**Summary.** `CP_STORE_DRIVER` selects `sqlite` (default) or `postgres`; postgres requires an
explicit `CP_STORE_DSN`.

**Verify:**
- [x] 🖥️ Default (unset) → CP boots on SQLite as before.
- [ ] 🖥️ `CP_STORE_DRIVER=postgres CP_STORE_DSN=postgres://…` → CP boots, migrations apply, a spawn round-trips through Postgres.
- [ ] 🖥️ `CP_STORE_DRIVER=postgres` with no DSN → CP `log.Fatal`s at boot with a clear message (no silent SQLite fallback).
- [x] ✅ `storeConfigFromEnv` table test green.

---

## M. Egress floor DNS carve-out (`sp-sac`) 🔒

**Summary.** Host-verification found the blanket RFC1918 drop broke DNS when resolvers are RFC1918
(common on a home server / LAN), which would break the sidecar's `api.openrouter.ai` lookup and thus
all inference. Fixed by allowing `udp/tcp :53` before the drops; the e2e now checks public egress by
IP so it's robust where outbound DNS is restricted.

**Verify:**
- [x] 🔒✅ Host-verified: DNS `:53` ACCEPT rule counter-matched; `just test-egress` green.
- [x] 🖥️ On a host whose resolvers are RFC1918 (e.g. `192.168.1.x`): a running spawn's agent can still resolve `api.openrouter.ai` (so inference works) while RFC1918 hosts on non-53 ports stay blocked.

---

## N. runsc one-sandbox pod end-to-end (`sp-ghx`, closes `sp-vaw`) 🔒

**Summary.** With `CONTAINER_RUNTIME=runsc`, the node runs the spawn pod as a single containerd CRI
sandbox (handler `runsc`) holding the sidecar + agent containers — so the agent reaches the sidecar
on `127.0.0.1:8080` (which a per-container gVisor pod cannot do, the `sp-vaw` blocker) — with the
egress floor on the `SPAWNLET-EGRESS` chain. Everything below the wire-up is hermetically tested; this
checklist is the **real-host** validation that closes `sp-vaw`. Needs a privileged host with
containerd + runsc + CNI (see `deployment.md` §5 for the containerd `config.toml` handler + CNI
conflist prerequisites).

**Host prep (one-time):**
```bash
# 1. runsc + shim on PATH; runsc CRI handler registered in /etc/containerd/config.toml:
#      [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
#        runtime_type = "io.containerd.runsc.v1"
#    then: sudo systemctl restart containerd
# 2. CNI reference plugins in /opt/cni/bin + a bridge/firewall/portmap conflist in /etc/cni/net.d
# 3. images into containerd's k8s.io namespace (separate from Docker's moby):
make images   # builds spawnery/sidecar:dev + spawnery/goose:dev (Docker)
for img in spawnery/sidecar:dev spawnery/goose:dev; do \
  docker save "$img" | sudo ctr -n k8s.io images import - ; done
```

**Run the runsc spawn (standalone node + spawnctl):**
```bash
make bin/spawnlet bin/spawnctl
sudo env "PATH=/usr/local/bin:/usr/bin:/bin:/sbin:/usr/sbin" \
  CONTAINER_RUNTIME=runsc AGENT_IMAGE=spawnery/goose:dev SIDECAR_IMAGE=spawnery/sidecar:dev \
  OPENROUTER_API_KEY="$OPENROUTER_API_KEY" DATA_ROOT=/tmp/spawns \
  bin/spawnlet &                                  # standalone mode (no CP_ADDR)
printf 'What is the secret word?\n' | \
  bin/spawnctl -addr http://127.0.0.1:9090 -app examples/secret-app -model free
```

**Verify:**
- [x] 🔒 The node logs a successful **runsc preflight** (CRI runtime + network ready) at startup; it
      exits hard if containerd/runsc/CNI is misconfigured (not at first spawn).
- [x] 🔒 The spawn reaches **ACTIVE** and `spawnctl` gets a real model reply (e.g. "The secret word is
      …") — i.e. the agent reached the sidecar on `127.0.0.1:8080` **under runsc** (the `sp-vaw` fix).
- [x] 🔒 `sudo crictl pods` / `crictl ps` show one pod sandbox (handler `runsc`) with two containers
      (sidecar + agent); `sudo iptables -S SPAWNLET-EGRESS` shows the per-pod `-s <podIP>` floor rules
      and `sudo iptables -S FORWARD | head -1` shows the `-j SPAWNLET-EGRESS` jump at position 1.
- [x] 🔒 Inside the agent container, `curl --max-time 3 http://169.254.169.254/` and an RFC1918 host
      are **blocked** while public egress works — the floor enforces under the CRI pod (mirror of
      `just test-cni-egress`, but on the real runsc pod).
- [x] 🔒 After `spawnctl`/stop, the pod sandbox is removed (`crictl pods` clean) and the per-pod
      `SPAWNLET-EGRESS` rules are gone (`iptables -S SPAWNLET-EGRESS` back to just the chain).

Once these pass on a host, **close `sp-vaw`** (the empirical gVisor-pod fix is confirmed).

---

## Notes / not-yet-verifiable here
- **gVisor** (`CONTAINER_RUNTIME=runsc`) — needs `runsc` on the host (absent in the dev sandbox).
- **Full DNS resolution end-to-end** — this dev sandbox blocks outbound DNS regardless of the floor; verify on a host with working DNS.
- **Postgres path** — schema is dialect-tested but exercised end-to-end only against a real Postgres.
- **Playwright web e2e** — needs a browser + the running stack.
- Reference: [`deployment.md`](deployment.md) (env/prereqs), [`ISOLATION.md`](ISOLATION.md) (security posture + the verified floor).
