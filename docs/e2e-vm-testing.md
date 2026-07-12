# Real end-to-end testing against a prod-stack VM

This is how you run the **real** `acceptance/` suite against a full spawnery stack running in
**prod mode** inside a disposable VM — the highest-fidelity test we have. It exercises the whole
system the way production does: a **runsc/gVisor node under enforced node-auth**, the **control
plane**, the **auth service**, the **web SPA**, **Postgres**, and **Caddy TLS**, with real
**A4 intent-signing** on spawn creation and **mTLS** node registration. Use it to gain confidence
before a big change lands, or to reproduce an issue that only appears on the real stack.

- **Scripts:** `scripts/e2e-vm/` (see its `README.md` for per-script detail + env knobs).
- **Design:** `docs/superpowers/specs/2026-07-03-prod-stack-vm-e2e-harness-design.md`.
- **Suite under test:** `acceptance/` (Playwright/TS; design
  `docs/superpowers/specs/2026-06-22-live-instance-acceptance-suite-design.md`).
- **Run everything in the `dev-spawnery` distrobox** (Go/buf/docker toolchain); the VM itself runs
  via host libvirt/KVM.

## The one command

```bash
GOLDEN_IMAGE=/var/lib/libvirt/images/spawnery-e2e/golden.qcow2 \
  scripts/e2e-vm/run.sh --profile fake [--grep <pattern>] [--keep] [--no-build]
```

`run.sh` is fully self-contained and namespaced by a per-run `E2E_RUNID` (so it is **safe to run
from several branches at once**). It:

0. **builds** the branch's fresh code — `spawnery_cp`/`authsvc`/`spawnlet`/`spawnctl` **+ the
   sidecar/agent container images + the web bundle + `config/`** (not just `bin/`) — into a per-run
   staging dir, in the distrobox;
1. **boots** a fresh VM on a copy-on-write overlay of the golden image (`up.sh`);
2. **rolls** the fresh code in and restarts the stack, waiting until it is app-ready — AS health, CP
   up, **node re-registered over enforced mTLS**, Caddy/web serving (`roll.sh`);
3. **runs** `acceptance/` against `https://<runid>.e2e.test` with the validated env (dev-token auth
   wired to the fake-GitHub identity pool, demo app/model, longer active-timeout, the golden CA).

The VM is **always torn down on exit** (`--keep` leaves it up for debugging). Failure artifacts
(journald, containerd/runsc, Postgres, the Playwright report) are captured **before** teardown under
`~/.local/state/spawnery-e2e/<runid>/artifacts/`.

## Prerequisites (one-time host setup)

1. **libvirt + KVM** running, `/dev/kvm` present (the node uses runsc **systrap**, so no nested virt
   is needed inside the VM).
2. **The `spawnery-e2e` NAT network:**
   ```bash
   virsh -c qemu:///system net-define scripts/e2e-vm/templates/net.xml
   virsh -c qemu:///system net-start spawnery-e2e && virsh -c qemu:///system net-autostart spawnery-e2e
   ```
3. **Host name resolution** — default `E2E_HOSTS_MODE=nss`: install `libvirt-nss` and add
   `libvirt_guest` to the `hosts:` line in `/etc/nsswitch.conf` (so `<runid>.e2e.test` resolves from
   libvirt leases). Alternatively `E2E_HOSTS_MODE=hosts` edits `/etc/hosts` under `flock` (needs sudo).
4. **Host CA trust** — install the golden image's throwaway CA so both Playwright and `spawnctl` (Go
   system roots) validate the VM's TLS:
   ```bash
   sudo cp /var/lib/libvirt/images/spawnery-e2e/golden-ca.crt /etc/pki/ca-trust/source/anchors/
   sudo update-ca-trust
   ```
   (`build-base.sh` writes `<golden>-ca.crt` next to the image.)
5. **SSH key** the golden trusts: `ssh-keygen -f ~/.ssh/spawnery_e2e -N ''` (the pubkey is injected via
   cloud-init at boot).
6. **A golden image** — build it once (below).

## Building / rebuilding the golden image (rare)

The golden is a Fedora cloud image pre-provisioned with the whole runtime (pinned containerd + runsc +
CNI + **crictl** for the CRI exec path, Postgres, Caddy, a throwaway `spawnery-ca dev` PKI, the systemd
units, and the imported sidecar/agent images). You only rebuild it to **upgrade the base** (bump a
pinned version, change provisioning) — day-to-day runs roll fresh code onto a fresh overlay instead.

```bash
OUT=/var/lib/libvirt/images/spawnery-e2e/golden.qcow2 scripts/e2e-vm/build-base.sh
# then install the emitted CA into host trust (step 4 above)
```

`build-base.sh` boots a throwaway Fedora VM, `scp`s the payload in, runs `provision/provision.sh`
live (so containerd is actually up for the image import), verifies, and shuts down cleanly — the disk
**is** the golden. It bakes **current** binaries + images + web, so rebuild after a runtime-affecting
change (e.g. a `spawnlet` fix) if you want it in the base rather than rolled per-run.

## Deploying fresh binaries to a *running* VM (iterating)

To iterate without a full `run.sh`, keep a VM up and re-roll:

```bash
# boot once, keep it:
E2E_RUNID=myrun GOLDEN_IMAGE=/var/lib/libvirt/images/spawnery-e2e/golden.qcow2 scripts/e2e-vm/up.sh --profile fake
# rebuild + push fresh code (binaries + images + web + config) and restart the stack:
E2E_RUNID=myrun STAGE=~/.local/state/spawnery-e2e/myrun/stage scripts/e2e-vm/roll.sh
# ... iterate: edit, rebuild into STAGE, roll again ...
E2E_RUNID=myrun scripts/e2e-vm/down.sh   # when done
```

For a **single binary** hot-patch (e.g. testing a `spawnlet` change) you can `scp` it directly:
`ssh -i ~/.ssh/spawnery_e2e spawnery@<ip> 'sudo install -m0755 /tmp/spawnlet /usr/local/bin/spawnlet && sudo systemctl restart spawnery-node'`.

## Running the suite against a VM you already have up

`up.sh` writes `~/.local/state/spawnery-e2e/<runid>/acc.env` (base `ACC_*` values). To run the suite
by hand against it, source that and set the validated env (this is exactly what `run.sh` step 3 does —
see `scripts/e2e-vm/run.sh`):

```bash
set -a; . ~/.local/state/spawnery-e2e/<runid>/acc.env; set +a
export ACC_AUTH_MODE=dev-token \
  ACC_IDENTITY_POOL="devtoken1=acc-owner-1,devtoken2=acc-owner-2,devtoken3=acc-owner-3" \
  ACC_TARGET_REF=vm ACC_BUILD_REF=vm ACC_SPAWNCTL_BIN="$PWD/bin/spawnctl" \
  ACC_TEST_APP_ID=spawnery/secret-app ACC_LIFECYCLE_APP=spawnery/secret-app \
  ACC_AGENT_APP_ID=spawnery/secret-app ACC_APP_ID=spawnery/secret-app \
  ACC_TEST_MODEL=openai/gpt-4o-mini ACC_AGENT_MODEL=openai/gpt-4o-mini \
  ACC_SPAWN_ACTIVE_TIMEOUT_MS=240000 \
  NODE_EXTRA_CA_CERTS=/var/lib/libvirt/images/spawnery-e2e/golden-ca.crt
( cd acceptance && npx playwright test [tests/...] -g "cli" --reporter=list )
```

## Auth profiles

- **`--profile fake`** (default, fully headless): the AS stubs GitHub (`AS_FAKE_GITHUB`, reachable
  multi-user) and the CP runs **dev-token** auth (`CP_DEV_TOKENS` → the identity pool). Covers
  Phases 0–6. The node is still **`NODE_AUTH_MODE=enforced`**, so spawn creation still requires real
  A4 intent-signing — the acceptance oracle and `spawnctl` both sign.
- **`--profile github`** (not yet wired end-to-end): real GitHub App, session linked fresh per run.
  Needs the seeded-`storageState` suite auth path + provisioning (deferred; see the design spec).

## What is actually verified

The suite drives the stack through **layered drivers** and asserts the *real* result of each action:

- **`webDriver`** — a real browser (Playwright) clicking the SPA; asserts **rendered DOM**.
- **`cliDriver`** — shells out to `spawnctl`; asserts CLI output + exit codes. Verbs `spawnctl`
  lacks (`rename`/`suspend`/`stop`/`delete`) are **failing stubs** — the parity gap shows as red by
  design.
- **the SDK oracle** (`acceptance/src/drivers/oracle.ts`, over `@spawnery/client`) — an independent
  Connect cross-check that **signs** (so it can create spawns on the enforced node) and reads
  `ListSpawns`/status to confirm what a surface did.

Scenario coverage (`acceptance/tests/`):

| Area | Verifies |
|---|---|
| `lifecycle/` | create + list, delete, rename, set-model, stop — spawn lifecycle across web + cli (rename/stop/delete are cli parity-gap reds). |
| `sessions/exec-exitcode` | `spawnctl exec` runs a command in the spawn's container and **propagates its exit code + captures stdout** (the runsc/crictl exec path). |
| `sessions/prompt-transcript.agent` | a **real LLM agent**: prompt → structurally-rendered transcript, survives reload, and its exec side-effect is fresh (structure, never agent prose; cost-capped, model-pinned). |
| `suspend-fork` | fork inherits a per-run marker (fork = clone a running spawn); suspend is a cli parity gap. |
| `tenancy/` | owner **non-leakage** (A sees only A's spawns, on api + cli) and per-owner **quota** (spawn cap + `resource_exhausted`/429). |
| `customization/` | profiles, catalog, and **secrets** CRUD (secrets is the api-only surface), and a profile-attached spawn (injection). |
| `marketplace/` | app-version register, browse, listing, and **spawn-from-a-market-app**. |

The suite also verifies the restart itself: `tests/lifecycle/node-restart.spec.ts` (`@noderestart`, run by
`run.sh` in its own serial pass) creates a spawn, leaves a marker file and a long-running process inside the
agent, then runs `sudo systemctl restart spawnery-node` over ssh (`ACC_NODE_RESTART_CMD`) and asserts the
spawn returns to ACTIVE with no operator action, the file and the process survived, and git-over-HTTPS still
works inside the agent (the per-spawn MITM CA did not change under it). That is SE3's acceptance criteria,
end to end — `docs/superpowers/specs/2026-07-12-spawnlet-restart-readoption-design.md` §6.

Because the target is the prod-mode stack, passing these also transitively verifies: **A4
intent-signing** end-to-end (create/resume/fork), **enforced mTLS** node registration, **runsc/gVisor**
pod isolation, the **Postgres** CP store, **Caddy TLS** at a fixed hostname, and dev-token (or, when
wired, OAuth-PoP) auth.

**Out of scope here** (owned by other, host-gated lanes): agent *answer quality* (only structural
transcript is asserted), egress-floor/cgroup isolation internals, and — until wired — the real-GitHub
profile.

## Troubleshooting

- **`<runid>.e2e.test` won't resolve** → nss-libvirt not configured (prereq 3), or the VM has no
  DHCP lease yet (`virsh -c qemu:///system console <domain>`).
- **TLS errors from `spawnctl`/Playwright** → golden CA not in host trust (prereq 4);
  `NODE_EXTRA_CA_CERTS` must point at `<golden>-ca.crt` for the suite.
- **`spawn limit reached`** → prior runs' spawns are at the per-owner cap; the suite sweeps at
  start/teardown, but a `--keep`'d VM accumulates — delete via the CP or tear down.
- **Everything hangs on boot** → a stale/crash-looping domain or the `spawnery-e2e` net is down
  (`virsh -c qemu:///system net-list --all`).
