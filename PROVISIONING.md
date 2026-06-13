# Spawnery Node Provisioning

How to provision a host to run spawns, per **lane**. A lane is the combination of OS + container
engine + isolation runtime a node uses to run the spawn pod (sidecar + agent). The control plane
(CP) and auth service (AS) are lane-independent and covered at the end.

> Spawns run as a **2-container pod** (sidecar holds the model key; agent shares the sidecar's
> netns). The node daemon is `spawnlet`. All node config is via environment variables — see the
> [full env reference](#environment-variable-reference) at the bottom.

## Lane summary

| Lane | OS | Engine / runtime | Status | Use |
|------|----|------------------|--------|-----|
| **Docker/runc** | Linux | Docker Engine + runc | ✅ shipped | Self-hosted dev + self-hosted prod |
| **CRI/runsc (gVisor)** | Linux | containerd + runsc | ✅ shipped | Cloud / multi-tenant |
| **macOS native** | macOS (Apple Silicon) | Containerization/Virtualization.framework | 🚧 not implemented ([`sp-ei4.2`](#macos)) | Self-hosted Mac (future) |

The **writable-rootfs** feature (agent can `apt install`/`useradd`/`chown` freely; rootfs changes
survive suspend/resume) is its own axis, available on both shipped lanes — see
[Writable rootfs](#writable-rootfs--delta-capility). Its enabling mechanism differs per lane
(userns-remap on Docker; gVisor sentry on runsc), which is why provisioning differs below.

---

## Docker/runc lane (self-hosted)

The default lane. Set `CONTAINER_RUNTIME` empty (or unset) so spawnlet uses the Docker backend.

### Minimal (no writable rootfs)

Works out of the box against any Docker daemon. The agent runs `--cap-drop=ALL` (degraded), so
**`apt`/`useradd`/`chown` will fail** inside spawns (`setgroups … Operation not permitted`). That
is expected and safe — it just means the writable-userspace feature is off. Same-node delta
capture/survival still works if `DELTA_CAPTURE=1`.

```bash
# Build + run a dev node attached to a local CP (see `just dev` / `just node`).
just dev          # CP + node + web in mprocs
```

### With writable rootfs (recommended for dev + self-hosted prod)

Requires **two** things — the daemon must run with `userns-remap`, AND spawnlet must be told
`USERNS_MODE=remap`. Either alone is insufficient; with only one, spawnlet probes the daemon,
logs a warning, and falls back to `cap-drop=ALL`.

**1. Enable userns-remap on the Docker daemon (one-time host setup):**

```bash
sudo mkdir -p /etc/docker
echo '{"userns-remap":"default"}' | sudo tee /etc/docker/daemon.json
sudo systemctl restart docker
```

Docker auto-creates the `dockremap` user and a `/etc/subuid`/`/etc/subgid` range. Verify:

```bash
docker info --format '{{.SecurityOptions}}'   # must contain  name=userns
grep dockremap /etc/subuid                     # e.g.  dockremap:100000:65536
```

> ⚠️ Enabling remap **shifts the Docker storage root** (e.g. to `/var/lib/docker/100000.100000`).
> Images/containers built before the switch live in the old tree and become invisible — they
> re-pull/rebuild once under the new root. Other local containers are shadowed until you revert
> (delete `daemon.json` + restart). The remap range must be **≥ 65536 wide** (Docker's default
> is) so service UIDs like `_apt` (100) and `nobody` (65534) are mapped.

**2. Tell spawnlet to use it:** the `just node` recipe already sets `USERNS_MODE=remap`. For a
hand-rolled node, export `USERNS_MODE=remap`. On startup you should see:

```
userns-remap active: base UID=100000 (USERNS_MODE=remap confirmed)
```

Now `apt update && apt install`, `useradd`, `chown -R`, `chmod` all work inside spawns. The agent
runs the **default Docker cap set** — never with `--cap-add` (an assertion rejects added caps;
`CAP_NET_ADMIN` in particular would let the agent flush the egress floor in the shared netns).

### Self-hosted on macOS / Windows (today)

There is no native macOS lane yet ([`sp-ei4.2`](#macos)). You can run the **Docker lane** via
Docker Desktop (which runs a Linux VM), but Docker Desktop does **not** support `userns-remap`, so
spawns run **degraded** (`cap-drop=ALL`, no `apt`). Use a Linux host (or a Linux VM you control the
daemon config of) when you need writable rootfs.

---

## CRI/runsc lane (cloud / multi-tenant)

gVisor (`runsc`) under containerd. The sentry virtualizes privilege, so the agent gets writable
userspace **with no kernel userns needed** — set `USERNS_MODE=native`. This is the
strongest-isolation lane and the one to use for untrusted multi-tenant workloads.

Select the lane with `CONTAINER_RUNTIME=runsc`. spawnlet then dials containerd at `CRI_ENDPOINT`
and uses the `CRI_RUNTIME_HANDLER` (default `runsc`).

**Host requirements** (full detail + version pins in
[`docs/superpowers/notes/2026-06-13-runsc-node-provisioning.md`](docs/superpowers/notes/2026-06-13-runsc-node-provisioning.md)):

1. **containerd** with a CRI runsc runtime handler in `/etc/containerd/config.toml`:

   ```toml
   [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]
     runtime_type = "io.containerd.runsc.v1"
     [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc.options]
       ConfigPath = "/etc/runsc/runsc.toml"
   ```

2. **`overlay2 = "none"` is MANDATORY** in `/etc/runsc/runsc.toml`:

   ```toml
   [runsc]
   overlay2 = "none"
   ```

   The default (`root:self`) hides container writes in a sentry-private filestore the host
   snapshotter can't see — delta capture would record garbage. `=none` routes writes to the host
   upperdir (cost is noise-level: apt +8%). This is required for delta capture to work at all.

3. **Pod DNS:** if the host's `/etc/resolv.conf` is the systemd-resolved `127.0.0.53` stub (it's
   unreachable from inside the pod, and there's no kubelet to supply DNS), set
   `POD_DNS=1.1.1.1,8.8.8.8` (or your resolver) so spawns can resolve names.

4. **Version pins (verified-good floor):** containerd **2.2.3**, runsc **release-20260525.0**.
   Validate any upgrade against the spike checklist in the runsc note before deploying.

Run with:

```bash
CONTAINER_RUNTIME=runsc USERNS_MODE=native \
CRI_ENDPOINT=unix:///run/containerd/containerd.sock \
POD_DNS=1.1.1.1,8.8.8.8 \
NODE_CLASS=cloud EGRESS_ENFORCE=true \
… (CP/auth/journal env) … \
bin/spawnlet
```

> KEP-127 kernel pod-userns is **not** used on this lane — it's broken under runsc (the gofer
> can't `setns` a pinned userns) and unnecessary (the sentry already isolates). `USERNS_MODE=native`
> tells spawnlet to relax caps without expecting a remap daemon.

---

## Writable rootfs / delta capability

Orthogonal to the userspace-writability question above: **delta capture** persists the agent's
rootfs changes across suspend/resume (same node) and is the basis for cross-node migration. It
works on either lane, with or without userns, but is **off by default**.

| Env | Default | Meaning |
|-----|---------|---------|
| `DELTA_CAPTURE` | `false` | Capture the agent rootfs delta on suspend; resume from it. |
| `DELTA_SQUASH_DEPTH` | `16` | Suspend captures before a SQUASH-NEEDED warning fires (chain-length guard, < overlayfs ~122 ceiling). |
| `DELTA_SCRUB_PATHS` | `/var/cache/apt,/var/lib/apt/lists,/tmp` | Comma-separated path prefixes `rm -rf`'d from the agent before each capture (keeps deltas small; also the secrets-copy mitigation hook). |
| `DELTA_QUOTA_SOFT_MB` | `0` (off) | Delta size at which the watchdog suspends-and-warns. |
| `DELTA_QUOTA_HARD_MB` | `0` (off) | Delta size at which the watchdog hard-stops the spawn. |

The base image is **pinned per spawn by digest** at create; resume always uses that digest. Data
mounts (`/app/*`) and the secrets tmpfs are excluded from the delta. Cross-node migration of
deltas via the Kopia journal is **not yet shipped** ([`sp-ei4.1.13`](#)). Design:
[`docs/superpowers/specs/2026-06-12-writable-rootfs-survival-design.md`](docs/superpowers/specs/2026-06-12-writable-rootfs-survival-design.md).

---

## Egress floor (cloud / enforced nodes)

The per-pod default-deny egress floor applies iptables rules in the pod netns. It needs the node
to manage iptables (root on the Docker lane; the CNI floor applier on the CRI lane).

- `EGRESS_ENFORCE=true` (default) — fail-closed: a spawn whose floor can't be applied won't start.
- `EGRESS_ENFORCE=false` — dev only (the `just node` recipe sets this; root-free, no floor).
- `EGRESS_ALLOW_CIDRS=10.0.0.0/8,…` — extra always-allowed destinations above the floor.

**Invariant:** never grant the agent `CAP_NET_ADMIN` — it shares the sidecar's netns and could
flush the floor. The backends assert this.

---

## Storage journal (transient tier, optional)

Per-spawn data mounts can be continuously journaled to an S3-compatible store (self-hosted
[Garage](https://garagehq.deuxfleurs.fr/) in dev). Off unless `JOURNAL_BACKEND` is set.

```bash
JOURNAL_BACKEND=s3                       # or "filesystem" for a local blob dir
JOURNAL_S3_ENDPOINT=http://127.0.0.1:3900
JOURNAL_S3_BUCKET=spawnery
JOURNAL_S3_ACCESS_KEY=… JOURNAL_S3_SECRET_KEY=…
JOURNAL_S3_REGION=garage JOURNAL_S3_DISABLE_TLS=true
```

Dev shortcut: `just garage` starts a local Garage and writes `deploy/garage/dev-creds.env`, which
the `just node` recipe sources automatically. The node's journal key lives at `JOURNAL_NODE_KEY`
(default under the journal root).

---

## Control plane, auth & node identity

These are lane-independent. Dev runs everything insecure on loopback; production enables
node↔CP mTLS and AS-signed tokens.

### Dev (insecure, loopback)

```bash
just dev          # CP (:8080) + node + web, no auth, dev tokens
```

CP knobs: `CP_LISTEN` (`127.0.0.1:8080`), `CP_AUTH_MODE=dev`, `CP_DEV_TOKENS=tok=user`,
`CP_ALLOWED_ORIGINS`, `CP_TELEMETRY`.

### Enforced (mTLS node auth + signed tokens)

`NODE_AUTH_MODE=enforced` on both CP and node turns on the node-auth handshake. The node enrolls
with the auth service and stores its identity under `NODE_ID_DIR` (`/var/lib/spawnlet/identity`).

| Side | Key env |
|------|---------|
| CP | `NODE_AUTH_MODE=enforced`, `CP_NODE_LISTEN`, `CP_NODE_ROOT_CA`, `CP_NODE_TLS_CERT`, `CP_NODE_TLS_KEY`, `CP_AS_SESSION_PUBKEYS`, `CP_AS_REVOCATION_URL` |
| Node | `NODE_AUTH_MODE=enforced`, `CP_NODE_ADDR` (`https://…:8081`), `NODE_ID_DIR`, `NODE_ROOT_CA`, `NODE_AS_PUBKEYS`, `AS_URL` + `ENROLL_TOKEN` (first-enrollment) |

Dev scaffolding: `just gen-dev-ca`, then `just cp-enforced` / `just authsvc-enforced` /
`just node-enforced` (or `just dev-enforced` for the lot). See the Justfile recipes for the exact
wiring. Auth design: [`docs/superpowers/specs/2026-06-11-auth-identity-design.md`](docs/superpowers/specs/2026-06-11-auth-identity-design.md).

---

## macOS

A macOS-native isolation backend (Apple Containerization / Virtualization.framework microVMs) is
**designed but not implemented** — epic [`sp-ei4.2`](https://github.com/) (`bd show sp-ei4.2`).
Until it lands, the only way to run a node on a Mac is the Docker lane via Docker Desktop, in
**degraded** mode (no userns-remap → no writable userspace). Cloud nodes stay Linux/gVisor
regardless.

---

## Environment variable reference

### Node (`spawnlet`)

| Var | Default | Notes |
|-----|---------|-------|
| `AGENT_IMAGE` | `spawnery/stubagent:dev` | Agent container image. |
| `SIDECAR_IMAGE` | `spawnery/sidecar:dev` | Sidecar (inference proxy) image. |
| `OPENROUTER_API_KEY` | — | Model key handed to the sidecar. |
| `DATA_ROOT` | `/var/lib/spawnlet/spawns` | Host root for per-spawn mount dirs. |
| `NODE_ID` | `node-1` | Node identifier. |
| `NODE_CLASS` | `cloud` | `cloud` \| `self-hosted` (placement + policy). |
| `NODE_OWNER` | — | Owner account for self-hosted nodes. |
| `CP_ADDR` | — | CP base URL; unset = standalone (no CP attach). |
| `CONTAINER_RUNTIME` | _(empty = Docker)_ | `runsc` selects the CRI/gVisor lane. |
| `CRI_ENDPOINT` | `unix:///run/containerd/containerd.sock` | CRI socket (runsc lane). |
| `CRI_RUNTIME_HANDLER` | `runsc` | containerd runtime handler name. |
| `POD_DNS` | — | Comma-separated pod resolvers (runsc lane). |
| `USERNS_MODE` | `off` | `remap` (Docker+userns) \| `native` (runsc) \| `off` (degraded). |
| `EGRESS_ENFORCE` | `true` | Fail-closed egress floor; `false` for dev. |
| `EGRESS_ALLOW_CIDRS` | — | Extra allowed egress CIDRs. |
| `MEM_LIMIT_MB` / `CPU_LIMIT` / `PIDS_LIMIT` | `1024` / `1.0` / `256` | Per-pod resource caps. |
| `DELTA_CAPTURE` | `false` | Capture/restore agent rootfs delta. |
| `DELTA_SQUASH_DEPTH` | `16` | Chain-length warning threshold. |
| `DELTA_SCRUB_PATHS` | apt caches + `/tmp` | Capture-time scrub prefixes (CSV). |
| `DELTA_QUOTA_SOFT_MB` / `DELTA_QUOTA_HARD_MB` | `0` / `0` | Delta-size suspend / stop thresholds. |
| `NODE_ADVERTISE_IP` | `127.0.0.1` | IP advertised for terminal attach. |
| `NODE_TERMINAL_ADDR` | `127.0.0.1:9092` | mosh terminal listen addr. |
| `JOURNAL_BACKEND` | _(off)_ | `s3` \| `filesystem`; see [journal](#storage-journal-transient-tier-optional). |
| `JOURNAL_*` | — | S3 endpoint/bucket/keys/region/prefix/TLS + `JOURNAL_ROOT`/`JOURNAL_NODE_KEY`. |
| `NODE_AUTH_MODE` | `insecure` | `enforced` enables node↔CP mTLS. |
| `CP_NODE_ADDR` / `NODE_ID_DIR` / `NODE_ROOT_CA` / `NODE_AS_PUBKEYS` / `AS_URL` / `ENROLL_TOKEN` | — | Enforced-mode node identity. |

### Control plane (`spawnery_cp`)

| Var | Default | Notes |
|-----|---------|-------|
| `CP_LISTEN` | `127.0.0.1:8080` | Client/web Connect listener. |
| `CP_AUTH_MODE` | `dev` | `dev` dev-tokens vs AS-signed sessions. |
| `CP_DEV_TOKENS` | `dev-token=dev` | `token=user[,…]` for dev auth. |
| `CP_ALLOWED_ORIGINS` | — | CORS allowlist for the SPA. |
| `CP_TELEMETRY` | `telemetry/events.jsonl` | Event log path. |
| `NODE_AUTH_MODE` | `insecure` | `enforced` turns on the node mTLS listener. |
| `CP_NODE_LISTEN` / `CP_NODE_ROOT_CA` / `CP_NODE_TLS_CERT` / `CP_NODE_TLS_KEY` | — | Node-facing TLS (enforced). |
| `CP_AS_SESSION_PUBKEYS` / `CP_AS_REVOCATION_URL` / `CP_AS_CP_SECRET` | — | AS token verification + revocation feed. |

> Store driver (sqlite/postgres) config is read via `storeConfigFromEnv` — see
> [`docs/superpowers/specs/2026-06-01-cp-store-driver-sp-ylw.md`](docs/superpowers/specs/2026-06-01-cp-store-driver-sp-ylw.md).
