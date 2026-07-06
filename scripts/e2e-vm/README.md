# scripts/e2e-vm — prod-stack-in-a-VM e2e harness (epic sp-te3y)

Spin up a **disposable libvirt/QEMU VM** running the whole spawnery stack in **prod mode**
(runsc/systrap node + CP + AS + web), roll the branch's fresh code in, and run the real
`acceptance/` suite against it. Design: `docs/superpowers/specs/2026-07-03-prod-stack-vm-e2e-harness-design.md`.

> **STATUS: first-draft orchestration, NOT yet validated on a host.** These scripts were written
> without a live libvirt/KVM host to test against (a sandbox has no `/dev/kvm`/root). Expect to
> iterate on the host. The **golden-image builder (`build-base.sh`) is now a first draft too** — the
> system provisioning (containerd/runsc/CNI/pg/Caddy/PKI) is concrete, but the spawnery **systemd env
> must be reconciled with the `Justfile`** on the host (search `RECONCILE` in `provision/provision.sh`).

## The one command

```bash
GOLDEN_IMAGE=/var/lib/libvirt/images/spawnery-golden.qcow2 \
  scripts/e2e-vm/run.sh [--profile fake|github] [--grep <pattern>] [--keep] [--no-build]
```

`run.sh` does, all namespaced by a per-run `E2E_RUNID` (branch+sha+nonce):
0. **build** fresh `spawnery_cp`/`authsvc`/`spawnlet`/`spawnctl` + sidecar/agent images + web bundle
   (in the `dev-spawnery` distrobox) into a per-run staging dir,
1. **`up.sh`** — boot a fresh VM on a per-run qcow2 overlay of the golden image, on the `spawnery-e2e`
   NAT network (routable per-VM IP), hostname `<runid>.e2e.test`,
2. **`roll.sh`** — copy the fresh code in (binaries + images + web + config — *not just `bin/`*),
   restart the units, wait app-ready (AS health, CP up, **node mTLS re-registration**, Caddy/web),
3. run `acceptance/` (`npm run test:accept`) against `https://<runid>.e2e.test`, with a per-run
   Playwright output dir.

It always tears the VM down on exit (`--keep` to leave it for debugging). Artifacts (journald,
containerd/runsc, Postgres, Playwright report) are collected **before** teardown under
`~/.local/state/spawnery-e2e/<runid>/artifacts/`.

## Concurrency

Safe to run from many branches at once. Every derived name is `E2E_RUNID`-scoped: libvirt domain
`spawnery-e2e-<runid>`, overlay, DHCP IP, hostname `<runid>.e2e.test`, staging dir, Playwright output.
Postgres is inside each VM, so state never collides. A single baked **`*.e2e.test` wildcard cert**
covers every run.

## One-time host setup

1. **libvirt + KVM:** `libvirtd` running, `/dev/kvm` present, your user in the `libvirt`/`kvm` groups
   (or use `qemu:///system` with polkit).
2. **NAT network** `spawnery-e2e`:
   ```bash
   virsh net-define scripts/e2e-vm/templates/net.xml
   virsh net-start spawnery-e2e && virsh net-autostart spawnery-e2e
   ```
3. **Host name resolution** (choose one):
   - `E2E_HOSTS_MODE=nss` (default): install `libvirt-nss` and add `libvirt_guest` to the `hosts:`
     line in `/etc/nsswitch.conf` — `<runid>.e2e.test` then resolves from libvirt leases, no sudo.
   - `E2E_HOSTS_MODE=hosts`: the scripts append/remove `/etc/hosts` lines under `flock` (needs sudo).
4. **CA trust:** install the golden image's throwaway CA into the host trust
   (`sudo cp ca.crt /etc/pki/ca-trust/source/anchors/ && sudo update-ca-trust`) so both Playwright and
   `spawnctl` (Go system roots) validate the VM's TLS. (Alt: `sp-te3y.8` adds a `spawnctl` CA flag.)
5. **SSH key:** `ssh-keygen -f ~/.ssh/spawnery_e2e -N ''`; the pubkey is injected via cloud-init.
6. **Golden image:** build it with `build-base.sh` (task `sp-te3y.1`, TODO) → `GOLDEN_IMAGE`.

## Files

| File | Role |
|---|---|
| `lib.sh` | shared: run-id, per-run names, `vm_ip`/`wait_tcp`/`vm_ssh`, host-resolution helpers |
| `up.sh` | boot one VM (overlay + cloud-init seed + domain XML), wait for SSH, emit `acc.env` |
| `roll.sh` | copy fresh code in, restart units, wait app-ready |
| `down.sh` | collect artifacts, destroy the domain + overlay |
| `run.sh` | the 0–3 orchestrator (build → up → roll → test → down) |
| `templates/domain.xml.tmpl` | transient libvirt domain (NAT iface, per-run overlay, guest-agent, serial log) |
| `build-base.sh` | golden-image builder — live-provision a Fedora image, run `provision/provision.sh`, shut down → golden qcow2 |
| `provision/provision.sh` | in-guest installer (pinned containerd 2.2.3/runsc-20260525.0/CNI + pg + Caddy + PKI + units + image import) |
| `provision/gen-pki.sh` | throwaway CA + AS session key + CP mTLS + `*.e2e.test` wildcard cert |
| `templates/net.xml` | the `spawnery-e2e` NAT network (one-time `virsh net-define`) |

## Env knobs (see `lib.sh`)

`GOLDEN_IMAGE` (required), `E2E_NET`, `E2E_DOMAIN_SUFFIX`, `E2E_VM_MEM_MB`, `E2E_VM_VCPUS`,
`E2E_HOSTS_MODE`, `E2E_SSH_KEY`, `E2E_SSH_USER`, `LIBVIRT_URI`, `E2E_STATE_ROOT`.

## Known gaps / next

- **RECONCILE the spawnery systemd env** (`provision/provision.sh` `/etc/spawnery/env.d/common.env`)
  with the `Justfile` `authsvc-github`/`cp-github`/`node-github` recipes — the exact var set (esp.
  `AS_GITHUB_TOKEN_ENC_KEY`, RPC secrets, `AS_FAKE_GITHUB*` / `GITHUB_CLIENT_ID`) is the single source.
- Verify `spawnery-ca dev`'s actual output filenames vs what `gen-pki.sh` / `common.env` reference.
- `node-list` in `roll.sh` assumes a `spawnctl node-list` verb for the mTLS-registration gate — verify
  the actual CP node-list surface.
- `github` profile needs the real-GitHub seeded-`storageState` suite auth path (`sp-te3y.7`) +
  provisioning (`sp-tq0t.10`).
