# Prod-Stack-in-a-VM E2E Harness — Design

**Status:** draft
**Date:** 2026-07-03
**Tags:** testing, e2e, acceptance, vm, libvirt, qemu, runsc, prod, ci

## Problem

The acceptance suite (`acceptance/`, epic `sp-tq0t`, Phases 0–6) is built but has **never had a live
end-to-end run** — it needs a target. We want to manufacture that target on demand: run the **entire
server stack in prod mode** (runsc/gVisor node + control plane + auth service + web) inside a single
**standalone VM**, creatable from scratch, that boots fast, so we can spin one up per big change and
run the real e2e tests against it as an acceptance gate — then throw it away.

"Prod mode" is the point: exercise the real integration paths that dev mode hides — enforced mTLS
CP↔node, real Ed25519 session tokens (no dev-token bypass), the Postgres store path, and genuine
gVisor pod isolation — none of which the hermetic suites or `just dev` cover.

## Locked decisions (with the user)

- **Substrate:** local **full VMs on QEMU/KVM + libvirt** (not cloud, not microVMs).
- **Boot model:** a **memory snapshot of a warm "services-ready" clean install**, restored on demand,
  with the **fresh binaries-under-test rolled on top**; scripts rebuild the base VM + snapshot **from
  scratch** rarely (to upgrade the base). This resolves "from scratch" (rare base build) vs "minimize
  startup" (frequent snapshot restore).
- **Store:** **Postgres in-VM** — matches `config/cp.prod.yaml` (`store.driver=postgres`); exercises
  the real prod store path + goose migrations.
- **Auth:** **dual-profile, real-GitHub included in v1** — a headless `fake` profile (Phases 0–6) and
  a `github` profile (Phase 7 real mount/push). See §6.
- **runsc-in-VM:** use the **systrap** platform — no `/dev/kvm`, no nested virtualization required
  (gVisor default since 2023; "default to systrap unless on dedicated bare-metal"). This removes the
  nested-virt crux entirely.

## Context (from codebase exploration, 2026-07-03)

- **Greenfield:** zero VM/cloud/IaC tooling exists (no Packer/Terraform/cloud-init/QEMU/Ansible). The
  `Justfile` recipes, the env-var contracts, and the runsc provisioning checklist
  (`PROVISIONING.md`, `docs/superpowers/notes/2026-06-13-runsc-node-provisioning.md`,
  `E2E_TEST_RUNSC.md`) are the complete recipe to translate into image-bake + systemd units.
- **Prod delta is thin** (`config/cp.prod.yaml`): `auth.mode=prod`, `node.auth_mode=enforced`,
  `store.driver=postgres`. Prod secrets are `${sops:…}` refs; for a disposable VM we instead mint a
  **throwaway** PKI with `spawnery-ca dev` (root CA + intermediates + AS Ed25519 session key + CP
  node-TLS cert + node identity) — exactly its intended ephemeral-test use.
- **runsc host needs** (none checked in — the base build must author them): containerd 2.2.3 + runsc
  release-20260525.0 + `containerd-shim-runsc-v1`; `/etc/containerd/config.toml` runsc handler;
  `/etc/runsc/runsc.toml` with `overlay2="none"` (mandatory for delta capture) + `--platform=systrap`;
  a CNI bridge/firewall/portmap conflist in `/etc/cni/net.d` (NOT hostinet — it bypasses the egress
  floor); `POD_DNS=1.1.1.1,8.8.8.8` (systemd-resolved's `127.0.0.53` stub is unreachable in-pod);
  `USERNS_MODE=native`; root + iptables for the `SPAWNLET-EGRESS` chain; agent/sidecar images
  imported into containerd's **`k8s.io`** namespace.
- **No health endpoints** except AS `/healthz`; CP has no `/healthz`/`/readyz` yet — readiness gating
  must poll a CP RPC.
- **No reverse proxy / TLS termination in-repo** — the VM supplies one.

## Architecture

### 1. Two layers

**Base build — rare, from scratch, reproducible.** `scripts/e2e-vm/build-base.sh`:
1. `virt-install` a Fedora guest from a cloud image + cloud-init.
2. Provision (cloud-init / shell): Docker + containerd 2.2.3 + runsc-20260525.0 + CNI + the runsc
   host config files (§Context); Postgres; Caddy (reverse proxy); the spawnery config tree
   (`config/` + `SPAWNERY_ENV=prod`); a **throwaway PKI** via `spawnery-ca dev`; systemd units for
   AS → CP → node → web (translated from the `Justfile` `*-github`/`*-enforced` recipes); import the
   agent/sidecar images into the `k8s.io` containerd namespace.
3. Boot to **services-ready**, verify (`E2E_TEST_RUNSC.md` Phase 2/3 smoke), then capture the **memory
   snapshot** (`virsh managedsave` or memory+disk `snapshot-create`). This is the golden warm image.

**Spin-up — frequent, fast, on demand.** `scripts/e2e-vm/up.sh [profile]`:
1. Define a transient domain on a **fresh qcow2 overlay** (backing = golden disk) so disk state
   reverts to a pristine baseline every run; restore the memory snapshot (~seconds).
2. **Roll fresh binaries** (§2), select the auth profile (§6), `systemctl restart` the spawnery units.
3. Wait-ready (§7), emit the `ACC_*` env for the acceptance suite, optionally run it, then `down.sh`
   discards the domain + overlay.

### 2. Rolling fresh binaries — virtiofs

The host's `bin/` (freshly built `spawnery_cp`/`authsvc`/`spawnlet`/`spawnctl` from the branch under
test) is exposed to the guest via a **virtiofs** mount; spin-up restarts the units against it. No
copy, no rebake — the snapshot is binary-agnostic, so a base rebuild is only needed for OS/dep/runsc
upgrades, not for code changes. (Fallback: `scp` the binaries + restart.)

### 3. Prod-mode config + secrets

`SPAWNERY_ENV=prod` selects `config/cp.prod.yaml` etc.: `auth.mode=prod` (dev-token **off**, Ed25519
session tokens), `node.auth_mode=enforced` (mTLS CP↔node), `store.driver=postgres`. The throwaway PKI
is baked into the snapshot (regenerated only on the rare base rebuild). **Postgres in-VM**, empty +
`internal/cp/seed.go`-seeded in the snapshot; the fresh binary auto-applies goose migrations on restart
(covers schema drift when the branch adds a migration).

### 4. runsc node lane inside the guest

Node runs `CONTAINER_RUNTIME=runsc` under **systrap** — the only genuinely prod-fidelity isolation in
the harness (real gVisor sandbox, egress floor, userns). All the host config the repo doesn't ship
(§Context) is baked at base build. Pod subnet chosen to avoid Podman's `10.88.0.0/16`.

### 5. Networking, TLS, reachability

- libvirt NAT with a **fixed hostname + static IP** (e.g. `spawnery-e2e.test`) so `ACC_WEB_ORIGIN`,
  `ACC_CP_ENDPOINT`, and the OAuth redirect (`https://spawnery-e2e.test/callback`) are stable across
  restores.
- **Caddy** in the guest terminates TLS on `:443` (self-signed cert; the suite trusts its CA via
  `NODE_EXTRA_CA_CERTS` + Playwright `ignoreHTTPSErrors`) and routes `/` → web, `/cp.v1.*` → CP :8080,
  `/oauth|/refresh|/github|/device` → AS :8090. The SPA's WebCrypto needs the HTTPS secure context —
  Caddy provides it.
- The **node terminal `:9092`** is a **direct dial** (mosh/`spawnctl exec` bypass the CP) → exposed on
  the VM address; `ACC_NODE_ADDR`/`ACC_NODE_TERMINAL_ADDR` point at it.
- **Concurrency** (multiple VMs from one snapshot): per-VM host-port forwarding + an IP-SAN cert, or a
  per-VM hostname. v1 may cap at one VM at a time and note the seam; the orchestrator allocates a
  unique domain name + overlay + address per invocation.

### 6. Auth — dual profile

Prod mode disables dev-token, so the suite authenticates via **real OAuth-PoP**. Two profiles on one
base, selected at spin-up:

- **`fake` (default, fast, headless):** AS runs `AS_FAKE_GITHUB` (reachable multi-user — T2). Full real
  login/PoP/Ed25519-session/refresh path with a stubbed IdP; **no github.com**. Covers Phases 0–6
  (the suite's `oauth-pop` mode). Every run is headless.
- **`github` (Phase 7):** AS wired to a **real GitHub App + test org**; a **pre-linked bot** is baked
  into the AS db in the snapshot (linking persists across restores). Exercises real repo mount/push
  (Phase 7), verified via the GitHub REST API. `AS_FAKE_GITHUB` and a real App are **mutually
  exclusive** on one instance — hence a profile, not a toggle.

**Two dependencies this creates (v1 scope):**
1. **Provision a real GitHub App + test org + bot** (`sp-tq0t.10`) — external, human/infra.
2. **A new auth path in the acceptance suite** — real-github.com login via a **seeded `storageState`**
   (the bot's github.com session, captured once, reused so linking/login is a single automated
   "Authorize" click, not credential-typing). The suite today does only `dev-token` + `oauth-pop`
   (vs fake-GitHub); this is net-new acceptance-suite work.

Because the `github` profile's real-github.com login taxes **every** phase (the AS does a real
`github.Exchange` for the session), the `fake` profile stays the default for speed; `github` is opt-in
for GitHub validation.

### 7. Orchestration + readiness

A `just e2e-vm-up [profile]` / `just e2e-vm-down` (or a small Go CLI) callable **manually, by an agent,
or from CI**: restore → roll binaries → **wait-ready** (poll AS `/healthz` + a CP RPC such as
`ListApps`, since CP has no `/healthz`) → print `ACC_*` → optionally `cd acceptance && npm run
test:accept` → `down`. This is what finally delivers the acceptance suite's **first live smoke run**
(`sp-tq0t.14`).

### 8. Scope / non-goals

Single-instance CP + local Postgres is the **correct** shape (the ORR roadmap treats it as the
pre-multi-CP baseline — no LB/multi-CP needed for one disposable host). Out of scope: real prod cert
automation, HA/graceful-shutdown, observability stack, backup/Litestream — all irrelevant for a
throwaway VM.

## Risks / open questions

- **Real-github.com auth path is net-new work** in the acceptance suite (seeded `storageState`) and
  gates the `github` profile on `sp-tq0t.10` provisioning — the largest cost in v1.
- **CP has no `/healthz`** — readiness gates on a CP RPC poll; a real `/readyz` (ORR-1) would be
  cleaner (note as an upstream follow-up, don't block on it).
- **Snapshot staleness:** a base rebuild is required when OS deps / containerd / runsc / the CNI
  config / the baked images change — not for code (virtiofs). The base-build script must be the
  single reproducible source so a rebuild is one command.
- **Concurrency & certs:** running N VMs from one snapshot needs per-VM address + cert SAN handling;
  v1 may ship single-VM and leave the multi-VM seam.
- **systrap overhead** (~a few % on syscall-heavy workloads) is acceptable for a test harness and is
  the correct platform for a VM.
- **virtiofs availability** in the guest/host libvirt stack — verify at base build; `scp` fallback.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
