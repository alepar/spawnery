# Prod-Stack-in-a-VM E2E Harness — Design

**Status:** draft (roast-revised r1)
**Date:** 2026-07-03
**Tags:** testing, e2e, acceptance, vm, libvirt, qemu, runsc, prod, ci

## Problem

The acceptance suite (`acceptance/`, epic `sp-tq0t`, Phases 0–6) is built but has **never had a live
end-to-end run** — it needs a target. We want to manufacture that target on demand: run the **entire
server stack in prod mode** (runsc/gVisor node + control plane + auth service + web) inside a single
disposable **QEMU/KVM + libvirt VM**, creatable from scratch, so we can spin one up per big change,
run the real e2e tests against it as an acceptance gate, then throw it away.

"Prod mode" is the point: exercise the real integration paths that dev mode hides — enforced mTLS
CP↔node, real Ed25519 session tokens (no dev-token bypass), the Postgres store path, and genuine
gVisor pod isolation.

## Locked decisions (with the user)

- **Substrate:** local **full VMs on QEMU/KVM + libvirt**.
- **Boot model (v1):** **cold-boot a golden disk image** on a fresh overlay (~30–60s, correct clock via
  NTP), then roll fresh code and start services. The **memory-snapshot fast-boot** the user originally
  wanted is **deferred** — the roast surfaced three web-verified showstoppers rooted in it (see
  *Memory-snapshot feasibility*), and for a harness invoked per-big-change ahead of a minutes-long
  acceptance run, a ~30–60s cold boot is negligible. Snapshot stays a **later optimization** with a
  clean seam; its feasibility is spiked below.
- **Store:** **throwaway Postgres in-VM** — real prod store path (`store.driver=postgres`), but the DSN
  is **overridden** to a local throwaway instance (NOT the real prod `${sops:}` secret — see §3).
- **Auth:** **dual-profile, real-GitHub included in v1** — `fake` (headless, Phases 0–6) + `github`
  (Phase 7 real mount/push). See §6.
- **runsc-in-VM:** the **systrap** platform — no `/dev/kvm` in-guest, no nested virtualization.

## Memory-snapshot feasibility (spiked 2026-07-03)

The user asked to spike whether the original memory-snapshot + roll-binaries fast-boot is feasible and
record the verdict. **Verdict: plausibly feasible on this host, not yet proven — deferred to a later
optimization.** The roast's three snapshot-rooted showstoppers and their status:

1. **virtiofs ⨯ memory-snapshot (web-verified):** libvirt runs `managedsave` as a *migration* and
   vhost-user-fs is non-migratable → historically `cannot migrate domain: non-migratable device:
   vhost-user-fs` (libvirt kbase; GitLab #452; qemu-devel). **BUT** this is gated on `virtiofsd < 1.11`;
   this host runs **virtiofsd 1.13.3** (Fedora, QEMU 10.2.2, libvirt 12.0.0, `/dev/kvm` present), which
   is past the threshold — so the block is *likely* lifted. Needs a **live confirm** (define a domain
   with a virtiofs mount, `virsh save`, restart, verify the guest sees post-restore host changes).
2. **Wrong primitive:** `managedsave` is one-shot + domain-bound and can't restore repeatedly into a
   fresh transient domain on an overlay. The workable primitive is **`virsh save <file>` → `virsh
   restore <file> --xml <per-run-overlay>`** (restore a copy). Documented, not yet exercised.
3. **Stale guest clock breaks auth:** a restored guest resumes frozen at capture time → OAuth-PoP ±90s,
   Ed25519 token validity, and TLS to github.com all fail. Fix available: **`qemu-guest-agent` (10.2.2
   present) + `virsh domtime --sync`** on restore, or `chrony makestep`. Not needed for cold-boot.

**If/when snapshot is added:** save-file (not managedsave) + hot-plug virtiofs *after* restore (or scp)
+ `domtime --sync`, and it still must refresh the pod images/web/config (§2). Because it's "plausible
not proven" and cold-boot moots all three, v1 ships cold-boot.

## Context (from codebase exploration + roast verification, 2026-07-03)

- **Greenfield:** zero VM/cloud/IaC tooling exists. The `Justfile` recipes + env-var contracts + the
  runsc provisioning checklist (`PROVISIONING.md`, `docs/superpowers/notes/2026-06-13-runsc-node-provisioning.md`,
  `E2E_TEST_RUNSC.md`) are the recipe to translate into an image build + systemd units.
- **Prod delta** (`config/cp.prod.yaml`): `auth.mode=prod`, `node.auth_mode=enforced`,
  `store.driver=postgres`, and `store.dsn: "${sops:store.dsn}"` — **resolved from the real
  `secrets.prod.sops.yaml` via `SOPS_AGE_KEY_FILE` at startup** (verified). So `SPAWNERY_ENV=prod`
  alone tries to pull real prod secret ciphertext — the harness must override it (§3).
- **runsc host needs** (none checked in — the base build authors them): containerd 2.2.3 + runsc
  release-20260525.0 + shim; `/etc/containerd/config.toml` runsc handler; `/etc/runsc/runsc.toml`
  (`overlay2="none"` + `--platform=systrap`); a CNI bridge/firewall/portmap conflist (NOT hostinet);
  `POD_DNS=1.1.1.1,8.8.8.8`; `USERNS_MODE=native`; root + iptables egress chain; agent/sidecar images
  imported into containerd's **`k8s.io`** namespace.
- **Health:** AS has `/healthz`; **CP has no `/healthz`/`/readyz`** (verified — the roast's claim it
  does was a false confirm). Readiness polls a CP RPC.
- **`spawnctl` TLS uses Go system roots** — the `schemeTransport` accepts a custom `RootCAs` but the CLI
  exposes no way to set one; against a self-signed cert, CLI scenarios fail unless the CA is in the host
  trust store (§5).
- **The acceptance guardrail is default-deny** — any host but `localhost`/`127.0.0.1` is treated as prod
  unless in `ACC_NONPROD_HOSTS`; the harness must set it (§5).

## Architecture

### 1. Two layers

**Base build — rare, from scratch, reproducible.** `scripts/e2e-vm/build-base.sh`:
1. `virt-install` a Fedora guest from a **pinned** cloud image (checksum-verified) + cloud-init.
2. Provision (cloud-init / shell), **all versions pinned by checksum** (Fedora image, distro packages,
   containerd 2.2.3, runsc-20260525.0, CNI plugins): Docker + containerd + runsc + the runsc host config
   (§Context); Postgres; Caddy; the spawnery `config/` tree; a **throwaway PKI** via `spawnery-ca dev`;
   systemd units for AS → CP → node → web (from the `Justfile` `*-github`/`*-enforced` recipes); import
   the agent/sidecar images into `k8s.io`.
3. Verify, then **shut down cleanly** and keep the qcow2 as the golden disk. (No memory snapshot in v1;
   capture happens *before* any runsc smoke so the baseline carries no residue.)

**Spin-up — on demand.** `scripts/e2e-vm/up.sh [profile]`:
1. Create a transient domain on a **fresh qcow2 overlay** (backing = golden) + a unique
   name/address; boot cold (correct clock via NTP).
2. **Roll fresh code** (§2), select the auth profile (§6), start the spawnery units.
3. **Wait-ready** (§7), configure host reachability (§5), emit `ACC_*`, optionally run the suite, then
   `down.sh` collects artifacts (§7) and destroys the domain + overlay.

### 2. Rolling fresh code — ALL of it, not just `bin/`

The whole stack is under test, so a spin-up refreshes **every** first-party artifact that a branch can
change — the roast's key correction:
- **Host Go binaries** (`spawnery_cp`/`authsvc`/`spawnlet`/`spawnctl`) — via **scp** (or virtiofs) +
  restart.
- **Sidecar + agent container images** — `cmd/sidecar` is the inference proxy *inside every pod*;
  rebuild the images and `ctr -n k8s.io images import` them into the guest.
- **Web SPA bundle** — build (`vite build`) and deploy to Caddy's web root (the SPA is under test and
  has no prod serving story today — Caddy serves the static build).
- **`config/` tree** — copy the branch's `config/` in (a config-key change must not run against stale
  config while binaries are fresh).

Cold-boot makes this natural: the golden image is a runtime **base**; the branch's *code* (binaries,
images, web, config) is layered on each spin-up. A base rebuild is then genuinely only for OS/dep/runsc/
CNI/Caddy changes.

### 3. Prod-mode config + secrets

`SPAWNERY_ENV=prod` for the real code paths (`auth.mode=prod`, enforced mTLS, `store.driver=postgres`),
but the harness **must not use the real prod secrets layer**: override every `${sops:}` ref (notably
`store.dsn`) via `--set`/env to point at the **local throwaway Postgres**, and supply the throwaway PKI
(root CA + AS Ed25519 session key + CP node-TLS + node identity from `spawnery-ca dev`, regenerated on
base rebuild). No real `secrets.prod.sops.yaml` / age key on the box. Postgres migrations auto-apply on
CP start.

### 4. runsc node lane inside the guest

`CONTAINER_RUNTIME=runsc` under **systrap** — the one genuinely prod-fidelity isolation (real gVisor
sandbox, egress floor, userns). All host config baked at base build; pod subnet avoids Podman's
`10.88.0.0/16`.

### 5. Networking, TLS, reachability

- libvirt NAT, **fixed hostname + static IP** (e.g. `spawnery-e2e.test`) for a stable `ACC_WEB_ORIGIN` +
  OAuth redirect across runs.
- **Host-side name resolution (roast gap):** libvirt's dnsmasq answers only for *guests*. The host
  (Playwright, Node, `spawnctl`, the real-github OAuth redirect hop) must resolve `spawnery-e2e.test`
  too → `up.sh` writes a **host `/etc/hosts` entry** (needs a privilege grant) or an NSS libvirt-guest
  hook. Concurrency: per-VM hostname/IP + cert SAN, or one VM at a time in v1.
- **Caddy** terminates TLS on `:443` (self-signed CA). **CA trust:** install the Caddy CA into the
  **host system trust** (`update-ca-trust`) so *both* Playwright (`NODE_EXTRA_CA_CERTS`) **and
  `spawnctl`** (Go system roots — no CA flag) validate; or add a CA env to spawnctl (small change).
  Routes: `/` → web (static build), `/cp.v1.*` → CP :8080, `/oauth|/refresh|/github|/device` → AS :8090.
  Use a long-lived cert (not Caddy `tls internal`'s ~12h leaf).
- **Node terminal `:9092`** direct-dial (mosh/`exec` bypass the CP) → exposed.
- The **fake-IdP endpoint** (`AS_FAKE_GITHUB_ADDR`/`_BASE_URL`) must also be **host-reachable** (the
  suite fetches the AS's 302 authorize URL host-side) — advertise it at a host-resolvable address.
- **Acceptance guardrail:** `up.sh` sets `ACC_NONPROD_HOSTS` to include `spawnery-e2e.test` (else the
  suite default-denies it as prod).

### 6. Auth — dual profile

Prod mode disables dev-token → real OAuth-PoP. Two profiles on one base, chosen at spin-up:
- **`fake` (default, headless):** AS runs `AS_FAKE_GITHUB` (reachable multi-user — T2). Full real
  login/PoP/Ed25519-session/refresh path, no github.com. Phases 0–6.
- **`github` (Phase 7):** AS wired to a **real GitHub App + test org**. **Link fresh per run** — do
  **not** bake a pre-linked bot: GitHub App user tokens expire ~8h and refresh tokens are **single-use**,
  so a baked link is invalidated by the first refresh (roast, web-verified). Each run completes the link
  via the bot's seeded **github.com `storageState`** (one automated "Authorize" click). **Caveat
  (documented, not solved):** github.com sessions expire (~2wk) and trip device/anti-automation
  challenges from fresh CI egress — the `storageState` needs periodic reseed; treat `github`-profile
  flakiness as expected and quarantine, not a hard gate.

**Two dependencies (v1 scope):** (1) provision a real GitHub App + test org + bot (`sp-tq0t.10`);
(2) a **new real-github.com seeded-`storageState` auth path** in the acceptance suite (it does only
`dev-token` + `oauth-pop` today) — net-new suite work.

### 7. Orchestration, readiness, teardown

`just e2e-vm-up [profile]` / `down` (or a Go CLI), callable **manually, by an agent, or from CI**.
- **Wait-ready** gates ALL of (roast gap — not just AS `/healthz` + a CP RPC): AS `/healthz`; a CP RPC
  where **prod-auth `Unauthenticated`-over-HTTP counts as "up"** (an anonymous poll won't authenticate);
  the **spawnlet re-registered with the CP over enforced mTLS** (a node-present check); the **runsc/CNI
  lane functional** (a canary `CreateSpawn` or node-status); Caddy + web serving.
- **Teardown collects artifacts BEFORE destroy** (roast gap): `journalctl` for the four units,
  containerd/runsc logs, Postgres tail, Caddy logs, and the acceptance HTML report/traces → a host dir,
  so a failed run is diagnosable after the disposable VM is gone.
- **Orphan reaper:** transient domains/overlays/virtiofsd leak on unclean exit (Ctrl-C, CI timeout) →
  label domains with a run tag and provide a `reap` that sweeps stale-by-age domains + overlays.
- This delivers the acceptance suite's **first live smoke run** (`sp-tq0t.14`).

### 8. Base build reproducibility & CI

- The **base build is the single reproducible source**: every input pinned by checksum; the trigger to
  rebuild is any change to OS deps / containerd / runsc / CNI / Caddy / baked images / **the host virt
  stack (QEMU/libvirt/machine-type — can rot an image)** / config *schema*. Named owner + one-command
  rebuild.
- **CI requirements (roast gap):** a **`/dev/kvm`-capable runner** (absent on most hosted runners →
  self-hosted/bare-metal), `libvirtd`, `virtiofsd`, a privilege grant for the host `/etc/hosts` +
  CA-trust install, and distribution/caching of the multi-GB golden image.

### 9. Scope / non-goals

Single-instance CP + local Postgres is the **correct** shape (ORR's pre-multi-CP baseline — no
LB/multi-CP). Out of scope: memory-snapshot fast-boot (deferred; spiked above), real prod cert
automation, HA/graceful-shutdown, observability stack, backup/Litestream.

## Risks / open questions

- **Real-github.com auth path is net-new suite work** and gates the `github` profile on `sp-tq0t.10`
  provisioning + `storageState` durability (periodic reseed) — the largest v1 cost.
- **Baked secrets:** the `github`-profile golden image embeds the real GitHub App secret/private key +
  the throwaway-but-trusted CA/session-key → treat that image as **sensitive** (access-controlled, not
  shared); the `fake` profile carries no real third-party secret.
- **`spawnctl` CA trust** may need a small CLI change (CA env) if host-trust install is unavailable in CI.
- **Config/systemd drift:** the systemd units + baked `config/` duplicate the `Justfile` launch
  contract — keep the base-build script generating them from one source to avoid drift.
- **Snapshot feasibility** remains "plausible not proven" — revisit only if cold-boot latency ever
  measurably matters.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*

- **2026-07-03 (roast r1 — Opus triage / Fable critics / Sonnet judges — BLOCK → folded):** 97 confirmed
  (same-family inflation) but distinct roots, most verified. Web-verified showstoppers rooted in the
  memory-snapshot + virtiofs fast-boot (virtiofs⨯managedsave non-migratable; managedsave wrong
  primitive; stale-clock-breaks-auth) → **v1 switched to cold-boot**, snapshot spiked (plausible on
  virtiofsd 1.13.3, deferred). Folded: "fresh binaries = `bin/` only" was false → also refresh
  sidecar/agent images + web bundle + config; baked pre-linked GitHub bot dies on single-use refresh
  tokens → link fresh per run; `SPAWNERY_ENV=prod` pulls real `${sops:}` secrets → override to throwaway
  pg; readiness must gate node re-registration + treat prod-auth `Unauthenticated` as up; host can't
  resolve the libvirt-only hostname → `/etc/hosts`; `spawnctl` uses system roots → host CA-trust install;
  acceptance guardrail default-deny → set `ACC_NONPROD_HOSTS`; leak GC + failure-artifact capture before
  teardown; pinned reproducible base build + host-virt-stack rebuild trigger; capture golden before smoke.
  Corrected a roast false-confirm (CP has no `/healthz`).
