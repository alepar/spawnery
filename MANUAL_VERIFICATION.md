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
- [ ] 🖥️ Create a spawn (`just spawnctl "hi"` or via 🌐 web), then call `ListSpawns` (web Chat/sidebar or a Connect call) — the spawn appears with status `active`, the right `app_id`, and your owner.
- [ ] 🖥️ Stop/delete the spawn (`StopSpawn`/`DeleteSpawn`) — it disappears from `ListSpawns` (soft-deleted), and the agent/sidecar containers are gone (`docker ps`).
- [ ] 🖥️ A second `ListSpawns` as a **different** dev token shows only that owner's spawns (ownership isolation).
- [ ] 🖥️ Kill the node process mid-spawn, restart the CP — the orphaned spawn shows `unreachable` (boot reconcile), not `active`.
- [ ] ✅ `go test ./internal/cp/... -race` is green (store invariants, guarded transitions, ledger).
- [ ] Spot-check the SQLite file (`CP_STORE_DSN`, default `cp.db`): `spawns`/`spawn_containers` rows look right; at most one live container per spawn (`ended_at IS NULL`).

---

## B. Marketplace catalog — read surface (`sp-0sc`, E5 slice 1)

**Summary.** `ListApps(query)` (browse/search) and `GetApp(id)` (detail) over a CP catalog model:
apps carry display name/summary/tags/visibility/listed; versions carry a **trust tier**
(`unverified`/`scanned`/`reviewed`). The CP is seeded with a 4-app demo lineup (wiki, language,
interview, zork), all `reviewed`.

**Verify:**
- [ ] 🖥️ `ListApps` with empty query returns the 4 seeded apps, each with a tier of `reviewed`.
- [ ] 🖥️ `ListApps` with a query (e.g. `research`) returns only matching apps (case-insensitive over name/summary/tags).
- [ ] 🖥️ `GetApp` for a seeded id returns its summary + version list + tiers.
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
- [ ] ✅ `manifest.Validate` table test + store sticky-creator test green.

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
- [ ] Browse tab shows the seeded apps as cards with correct tier badges; the search box filters them.
- [ ] Click a card → Detail shows the manifest fields + a versions list + a Spawn button; Back returns to Browse.
- [ ] Click **Spawn** → the view switches to Chat, status reaches `ready`, and a prompt gets a streamed reply (confirms the marketplace→spawn→ACP path end-to-end).
- [ ] Spawn a **second** app from the marketplace while one is running → the old session tears down cleanly and the new one reaches `ready` (no stale/duplicate WS, no clobbered status — the re-entrancy guard).
- [ ] **My Apps** tab lists your registered apps incl. unlisted; the listing toggle takes one down (it drops out of Browse) and relists it.
- [ ] **Publish** tab: fill id/title/version/ref (+ a mount row), submit → success toast, and the app appears in My Apps as `unverified`. A bad submit surfaces the CP error as a toast.
- [ ] Light/dark theme + no raw-color regressions; no console errors.
- [ ] ✅ `cd web && npm test` (24 Vitest tests) green; `npm run build` clean. (Playwright `marketplace.spec.ts` needs a browser + the stack.)

---

## H. Egress allowlist floor + node class (`sp-rpa`) 🔒

**Summary.** Each spawn pod's network namespace gets a host-applied iptables **block-floor**: drop
cloud-metadata (`169.254.0.0/16`) + RFC1918, allow loopback + DNS + operator CIDRs, default-allow
public. Applied **after the sidecar, before the agent** (no unprotected window), **fail-closed**
(can't apply → spawn aborts). Governed by node class: **cloud** always enforces (non-disableable);
**self-hosted** honors `EGRESS_ENFORCE` (default on).

**Verify:**
- [ ] 🔒 `just test-egress` PASSES on a privileged host (this is the real packet-drop test).
- [ ] 🔒✅ Already host-verified here: metadata + RFC1918 dropped (iptables counters + curl blocked), public-by-IP reachable, DNS `:53` allowed.
- [ ] 🖥️ Start a spawn on a cloud node and `nsenter` into the sidecar netns (`docker inspect` → pid) → `iptables -S OUTPUT` shows the lo/DNS ACCEPTs then the metadata/RFC1918 DROPs.
- [ ] 🖥️ Inside a running spawn's agent: `curl http://169.254.169.254/` fails, `curl https://1.1.1.1/` works, `curl https://api.openrouter.ai/` resolves+connects (DNS carve-out).
- [ ] 🖥️ Fail-closed: on a host **without** iptables (or with `EGRESS_ENFORCE` effective but the applier broken), a cloud spawn **does not start** (and the sidecar is torn down) rather than running unprotected.
- [ ] 🖥️ `NODE_CLASS=self-hosted EGRESS_ENFORCE=false` → spawn runs unrestricted with a loud WARNING log; `NODE_CLASS=cloud` ignores `EGRESS_ENFORCE=false` and still enforces.

---

## I. Node-class propagation to CP (`sp-2as`)

**Summary.** The node reports its `node_class` at registration; the CP records it on the in-memory
registry node and stamps it on the `spawn_create` telemetry event.

**Verify:**
- [ ] 🖥️ Start the node with `NODE_CLASS=self-hosted`; after a spawn, the `spawn_create` line in `telemetry/events.jsonl` has `node_class: "self-hosted"`.
- [ ] 🖥️ An unset `NODE_CLASS` is recorded as `cloud` (safe default) in telemetry.
- [ ] ✅ Registry-records-class + telemetry-carries-class tests green.

---

## J. Resource limits, per-user quota, isolation runtime (`sp-ach`) 🔒

**Summary.** Each pod container gets cgroup **mem/CPU/pids** limits (`MEM_LIMIT_MB`/`CPU_LIMIT`/
`PIDS_LIMIT`, defaults 1024/1.0/256) via Docker `HostConfig`; an optional **gVisor** runtime
(`CONTAINER_RUNTIME=runsc`); and a CP-side **per-user concurrent-spawn cap**
(`CP_MAX_SPAWNS_PER_OWNER`, default 5).

**Verify:**
- [ ] 🔒✅ Already host-verified: a container started with the limits has kernel cgroup-v2 `memory.max`/`pids.max`/`cpu.max` matching the config exactly.
- [ ] 🖥️ Start a spawn, `docker inspect` the agent + sidecar → `HostConfig.Memory`/`NanoCpus`/`PidsLimit` reflect the env (both containers).
- [ ] 🖥️ Per-user cap: with `CP_MAX_SPAWNS_PER_OWNER=1`, the **second** `CreateSpawn` for the same owner → `ResourceExhausted`; `=0` → unlimited.
- [ ] 🔒 (If `runsc` installed) `CONTAINER_RUNTIME=runsc` → `docker inspect` shows `Runtime: runsc`; spawn still works.
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
- [ ] ✅ PickFor placement + the three policy-outcome tests green.
- [ ] ⚠️ Known gap: the web Detail "Spawn" sends no version → latest reviewed, so spawning an unverified version from the **UI** needs the (filed) version-selector follow-up — verify via API/`spawnctl` for now.

---

## L. Postgres store driver (`sp-ylw`)

**Summary.** `CP_STORE_DRIVER` selects `sqlite` (default) or `postgres`; postgres requires an
explicit `CP_STORE_DSN`.

**Verify:**
- [ ] 🖥️ Default (unset) → CP boots on SQLite as before.
- [ ] 🖥️ `CP_STORE_DRIVER=postgres CP_STORE_DSN=postgres://…` → CP boots, migrations apply, a spawn round-trips through Postgres.
- [ ] 🖥️ `CP_STORE_DRIVER=postgres` with no DSN → CP `log.Fatal`s at boot with a clear message (no silent SQLite fallback).
- [ ] ✅ `storeConfigFromEnv` table test green.

---

## M. Egress floor DNS carve-out (`sp-sac`) 🔒

**Summary.** Host-verification found the blanket RFC1918 drop broke DNS when resolvers are RFC1918
(common on a home server / LAN), which would break the sidecar's `api.openrouter.ai` lookup and thus
all inference. Fixed by allowing `udp/tcp :53` before the drops; the e2e now checks public egress by
IP so it's robust where outbound DNS is restricted.

**Verify:**
- [ ] 🔒✅ Host-verified: DNS `:53` ACCEPT rule counter-matched; `just test-egress` green.
- [ ] 🖥️ On a host whose resolvers are RFC1918 (e.g. `192.168.1.x`): a running spawn's agent can still resolve `api.openrouter.ai` (so inference works) while RFC1918 hosts on non-53 ports stay blocked.

---

## Notes / not-yet-verifiable here
- **gVisor** (`CONTAINER_RUNTIME=runsc`) — needs `runsc` on the host (absent in the dev sandbox).
- **Full DNS resolution end-to-end** — this dev sandbox blocks outbound DNS regardless of the floor; verify on a host with working DNS.
- **Postgres path** — schema is dialect-tested but exercised end-to-end only against a real Postgres.
- **Playwright web e2e** — needs a browser + the running stack.
- Reference: [`deployment.md`](deployment.md) (env/prereqs), [`ISOLATION.md`](ISOLATION.md) (security posture + the verified floor).
