# E2E Verification Runbook — runsc Host

A complete, ordered verification pass for a session running on a **privileged Linux host with gVisor
(runsc)**. The dev sandbox where most of this code was written has no runsc, no outbound DNS, and
runs unprivileged — so the runsc/CRI lane, the real egress floor, and the full goose round-trip were
never validated end-to-end there. This runbook is how you close that gap.

Work the phases **in order**. Each phase gates the next: don't chase a runsc pod failure (Phase 2)
until the hermetic suite (Phase 0) and the setns/floor suites (Phase 1) are green.

> **Companion docs** (don't duplicate — follow them where cited):
> - [`MANUAL_VERIFICATION.md`](MANUAL_VERIFICATION.md) — the per-feature manual checklist (§A–§N). This
>   runbook drives the **runsc/host-gated** subset and points at it for everything else.
> - [`deployment.md`](deployment.md) — env vars + host prerequisites (§4 node, **§5 egress floor**).
> - [`ISOLATION.md`](ISOLATION.md) — security posture + what the floor guarantees.

Legend: ✅ hermetic (no root) · 🔒 needs root + a privileged host · 🟢 needs runsc/containerd/CNI ·
🌐 needs the web UI · 🤖 needs `OPENROUTER_API_KEY` + a real model.

---

## Prerequisites

**Host must have:**
- Linux, **root** (passwordless `sudo`), `iptables`, `nsenter` on PATH.
- **Docker** (for the Docker/runc lane + image builds).
- **containerd + runsc + CNI** for the gVisor lane — see [`deployment.md` §5](deployment.md) and
  [`MANUAL_VERIFICATION.md` §N](MANUAL_VERIFICATION.md) for the exact `config.toml` runsc handler and
  the `/etc/cni/net.d` conflist. Verify the toolchain: `runsc --version`, `sudo crictl version`,
  `ls /opt/cni/bin`.
- **Go 1.26** (the module pins `go 1.26.0`), **node + npm**, **just**, **make**.
- `OPENROUTER_API_KEY` exported (or in a gitignored `.env`; the Justfile auto-loads it) for the
  goose/model phases.
- For `just lint`: a **go1.26-built** golangci-lint (the prebuilt v2.8.0 is go1.24-built and refuses
  to run against go1.26 code). Install once:
  ```bash
  GOTOOLCHAIN=go1.26.0 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.8.0
  ```

**Record results as you go.** At the end, report per-phase PASS/FAIL with the command output for any
failure, and which beads can be closed (see Sign-off).

---

## Phase 0 — Baseline hermetic suite ✅ (no root, no runsc)

Everything here passed in the dev sandbox; re-run to confirm the checkout is clean on this host
before touching the kernel/runtime.

```bash
go test ./... -count=1                 # full Go suite
go test ./... -race -count=1           # again under the race detector
cd web && npm ci && npm test && npx tsc --noEmit && cd ..   # 77 vitest + typecheck
just lint                              # golangci-lint (0 issues) + eslint + tsc
```

**Expected:**
```text
# go test ./...            -> every line "ok  spawnery/…  0.0Xs" (or "[no test files]"), no FAIL
ok  	spawnery/internal/cp	0.18s
ok  	spawnery/internal/node	1.02s
...
# web: "Test Files  20 passed (20)" / "Tests  77 passed (77)"; tsc prints nothing (exit 0)
# just lint:
GOTOOLCHAIN=go1.26.0 ".../bin/golangci-lint" run ./...
0 issues.
cd web && npx eslint . && npx tsc --noEmit      # no output from either => clean
```

- [ ] ✅ `go test ./...` — all packages `ok`.
- [ ] ✅ `go test ./... -race` — green, no data races.
- [ ] ✅ web: vitest green, `tsc --noEmit` clean.
- [ ] ✅ `just lint` exits 0 (golangci-lint "0 issues", eslint clean).

**If Phase 0 fails, stop** — it's a checkout/toolchain problem, not a runsc problem.

---

## Phase 1 — Host-gated tagged suites 🔒 (real kernel, root — still Docker/runc lane)

These are build-tagged out of the default suite because they need real iptables / setns / Docker.
They validate the transport + floor primitives the runsc pod reuses.

### 1a. ACP UDS transport — real setns roundtrip 🔒
```bash
go test -tags acp_e2e -c -o /tmp/acp.test ./internal/runtime/
sudo env "PATH=/usr/local/bin:/usr/bin:/bin:/sbin:/usr/sbin" \
  /tmp/acp.test -test.run TestAttachACPRoundtrip -test.v -test.count=1
```
**Expected:**
```text
--- PASS: TestAttachACPRoundtrip (0.0Xs)
PASS
ok  	spawnery/internal/runtime	0.0Xs
```
- [ ] 🔒 `TestAttachACPRoundtrip` PASS — `AttachACP` enters a pod netns via setns and round-trips the
      abstract `@spawnlet-acp` socket. (This is the in-pod transport both lanes use.)

### 1b. Egress floor — Docker/DOCKER-USER lane 🔒
```bash
just test-egress      # builds tagged egress_e2e, runs as root with iptables/nsenter/docker on PATH
```
**Expected:** `--- PASS: TestEgressFloorEnforced` then `PASS` / `ok`. A FAIL here prints the iptables
counters it checked (e.g. the metadata-DROP counter stayed 0) — that's a real floor break, not a flake.

- [ ] 🔒 `TestEgressFloorEnforced` PASS — metadata `169.254/16` + RFC1918 dropped (iptables counters +
      `curl` blocked), public-by-IP reachable, DNS `:53` allowed. (Mirrors `MANUAL_VERIFICATION.md` §H/§M.)

### 1c. Egress floor — CRI/CNI lane (`SPAWNLET-EGRESS`) 🔒
```bash
just test-cni-egress  # builds tagged cni_egress_e2e, real Docker container proxy for the chain
```
**Expected:** `--- PASS: TestCNIEgressFloorEnforced` / `PASS` / `ok`.

- [ ] 🔒 `TestCNIEgressFloorEnforced` PASS — the `SPAWNLET-EGRESS` chain (jumped from `FORWARD` pos 1)
      enforces the same per-pod block-floor the runsc pod will use.

### 1d. Full goose + sidecar — Docker/runc lane 🔒🤖
```bash
export OPENROUTER_API_KEY=...          # real key; these call a model
just test-e2e                          # make images + go test -tags e2e ./... -count=1 -v
```
**Expected:**
```text
--- PASS: TestEndToEndGooseSecret (…s)      # real goose pod boots + answers
--- PASS: TestWSEndToEndGooseSecret (…s)    # same over the WS relay
--- PASS: TestCPEndToEndStub (…s)           # CP↔node↔stub round-trip
ok  	spawnery/internal/cp	…
ok  	spawnery/internal/spawnlet	…
```
- [ ] 🔒🤖 `TestEndToEndGooseSecret`, `TestWSEndToEndGooseSecret`, `TestCPEndToEndStub` PASS — a real
      goose agent + sidecar pod boots, ACP-inits, answers a prompt (the runc baseline that the runsc
      lane must match). If a model is unavailable, set `-model` to a free one (see `just spawnctl`).

> Phase 1 proves the transport + floor + agent work on a **runc** pod. Phase 2 proves the same under
> **runsc**, where per-container gVisor previously broke the shared-netns pod (`sp-vaw`).

---

## Phase 2 — runsc one-sandbox pod round-trip 🟢🔒🤖 (the core: `sp-ghx`, closes `sp-vaw`)

This is the verification this whole runbook exists for. Follow **[`MANUAL_VERIFICATION.md` §N](MANUAL_VERIFICATION.md)**
— it has the exact host-prep + run commands. Summary of the flow:

**One-time host prep** (containerd runsc handler + CNI + images into the `k8s.io` namespace):
```bash
# containerd config.toml: register the runsc CRI handler, then: sudo systemctl restart containerd
# CNI: reference plugins in /opt/cni/bin + a bridge/firewall/portmap conflist in /etc/cni/net.d
make images
for img in spawnery/sidecar:dev spawnery/goose:dev; do \
  docker save "$img" | sudo ctr -n k8s.io images import - ; done
```

**Run a runsc spawn (standalone node + spawnctl):**
```bash
make bin/spawnlet bin/spawnctl
sudo env "PATH=/usr/local/bin:/usr/bin:/bin:/sbin:/usr/sbin" \
  CONTAINER_RUNTIME=runsc AGENT_IMAGE=spawnery/goose:dev SIDECAR_IMAGE=spawnery/sidecar:dev \
  OPENROUTER_API_KEY="$OPENROUTER_API_KEY" DATA_ROOT=/tmp/spawns \
  bin/spawnlet &
printf 'What is the secret word?\n' | \
  bin/spawnctl -addr http://127.0.0.1:9090 -app examples/secret-app -model free
```

**Expected — node log (stderr):** preflight is *silent on success* and proceeds to listen; it only
speaks up to die. A healthy boot looks like:
```text
spawnlet listening on 127.0.0.1:9090
```
A misconfigured runtime fails *here*, at startup, not at first spawn:
```text
container runtime preflight failed: <runsc/containerd/CNI error>   # process exits non-zero
```
**Expected — `spawnctl` (stdout):**
```text
spawn: <spawn-id>
ready. type prompts:
The secret word is "<word from the app's data>".   # streamed agent reply (exact text is model-dependent)
```

**Verify (the `sp-vaw` close criteria):**
- [ ] 🟢🔒 Node logs a successful **runsc preflight** at startup (CRI runtime + network ready); it
      `log.Fatal`s if containerd/runsc/CNI is misconfigured — *at startup*, not at first spawn.
- [ ] 🟢🔒🤖 The spawn reaches **ACTIVE** and `spawnctl` gets a real model reply — i.e. **the agent
      reached the sidecar on `127.0.0.1:8080` under runsc** (the single-sandbox fix for `sp-vaw`).
- [ ] 🟢🔒 `sudo crictl pods` / `sudo crictl ps` show **one** pod sandbox (handler `runsc`, namespace
      `spawnery`) holding **two** containers (sidecar + agent).

  **Expected:**
  ```text
  $ sudo crictl pods
  POD ID         CREATED         STATE   NAME          NAMESPACE   ATTEMPT   RUNTIME
  a1b2c3d4e5f6   12 seconds ago  Ready   <spawn-id>    spawnery    0         runsc
  $ sudo crictl ps
  CONTAINER      IMAGE                  STATE     NAME      ATTEMPT   POD ID
  f00...         spawnery/sidecar:dev   Running   sidecar   0         a1b2c3d4e5f6
  e11...         spawnery/goose:dev     Running   agent     0         a1b2c3d4e5f6
  ```

- [ ] 🟢🔒 `sudo iptables -S SPAWNLET-EGRESS` shows the per-pod `-s <podIP>` floor (DNS ACCEPT, then
      metadata/RFC1918 DROPs), and the chain is jumped from `FORWARD` ahead of CNI's own rules.

  **Expected** (pod IP e.g. `10.88.0.7`; iptables normalises a single host to `/32`):
  ```text
  $ sudo iptables -S SPAWNLET-EGRESS
  -N SPAWNLET-EGRESS
  -A SPAWNLET-EGRESS -s 10.88.0.7/32 -p udp -m udp --dport 53 -j ACCEPT
  -A SPAWNLET-EGRESS -s 10.88.0.7/32 -p tcp -m tcp --dport 53 -j ACCEPT
  -A SPAWNLET-EGRESS -s 10.88.0.7/32 -d 169.254.0.0/16 -j DROP
  -A SPAWNLET-EGRESS -s 10.88.0.7/32 -d 10.0.0.0/8 -j DROP
  -A SPAWNLET-EGRESS -s 10.88.0.7/32 -d 172.16.0.0/12 -j DROP
  -A SPAWNLET-EGRESS -s 10.88.0.7/32 -d 192.168.0.0/16 -j DROP

  # the jump is the FIRST -A rule in FORWARD (line 1 of `-S` is the policy, not a rule):
  $ sudo iptables -S FORWARD | grep -n SPAWNLET-EGRESS
  2:-A FORWARD -j SPAWNLET-EGRESS
  ```

- [ ] 🟢🔒 Inside the agent (`sudo crictl exec <agent-container-id> …`): metadata + an RFC1918 host are
      **blocked**, public egress works.

  **Expected:**
  ```text
  $ sudo crictl exec <agent-id> sh -c 'curl -s --max-time 3 http://169.254.169.254/; echo exit=$?'
  exit=28                               # 28 = curl timeout -> DROP working (metadata blocked)
  $ sudo crictl exec <agent-id> sh -c 'curl -s --max-time 3 -o /dev/null -w "%{http_code}\n" https://1.1.1.1/'
  200                                   # public-by-IP reachable (or a TLS handshake, not a timeout)
  ```

- [ ] 🟢🔒 After stop, the pod sandbox is gone (`crictl pods` clean) **and** the per-pod
      `SPAWNLET-EGRESS` rules are removed — the chain persists but is empty:

  **Expected (after teardown):**
  ```text
  $ sudo crictl pods            # no Ready spawnery sandbox
  $ sudo iptables -S SPAWNLET-EGRESS
  -N SPAWNLET-EGRESS            # chain stays; the -s <podIP> rules are gone
  ```
- [ ] 🟢🔒 Run the spawn create→teardown **N times** (≥5) — no leaked sandboxes, netns, images, or
      iptables rules accumulate (`crictl pods`, `ip netns`, `crictl images`, `iptables -S`).

---

## Phase 3 — Full stack under runsc (CP + node + web) 🟢🔒🌐🤖

Phase 2 used the standalone node. This runs the **CP-attached** path with the node on the runsc lane,
exercising the long-lived per-spawn pump + multi-client fan-out end-to-end through the browser.

```bash
# terminal 1 — control plane
just cp
# terminal 2 — node on the runsc lane (root; CONTAINER_RUNTIME=runsc). cloud class => floor enforced.
sudo env "PATH=/usr/local/bin:/usr/bin:/bin:/sbin:/usr/sbin" \
  CONTAINER_RUNTIME=runsc CP_ADDR=http://127.0.0.1:8080 NODE_ID=node-1 NODE_CLASS=cloud \
  AGENT_IMAGE=spawnery/goose:dev SIDECAR_IMAGE=spawnery/sidecar:dev \
  OPENROUTER_API_KEY="$OPENROUTER_API_KEY" DATA_ROOT=/tmp/spawns \
  bin/spawnlet
# terminal 3 — web
just web      # open the printed URL
```
**Expected — node log** (preflight silent on success → attaches → registers):
```text
spawnlet attaching to CP at http://127.0.0.1:8080 as node-1
node: connected to CP at http://127.0.0.1:8080 (id=node-1 class=cloud)
```
**Expected — web:** the sidebar dot goes **yellow** (starting) → **green** (active); the chat input
enables and a prompt streams a reply token-by-token. (A red dot = the spawn errored; check the node log.)

- [ ] 🟢🔒🌐🤖 Spawn an app from the web Marketplace → status goes yellow (starting) → green (active),
      a prompt streams a real reply — the full CP→node(runsc)→pump→ACP path.
- [ ] 🟢🌐 Open the **same** spawn in a second browser tab → both see the live transcript and stream
      (multi-client fan-out over the pump's resumable cursor).
- [ ] 🟢🌐 Reload mid-conversation → the transcript is restored from the pump's frame log (cursor 0
      replay), no duplication.
- [ ] 🟢🔒 Floor still enforces (re-run the §N agent-curl checks for a web-spawned pod).
- [ ] Cross-check `MANUAL_VERIFICATION.md` **§A** (lifecycle), **§G** (marketplace UI), **§H** (floor),
      **§N** (runsc) against this live runsc stack.

---

## Phase 4 — Web e2e (Playwright) ✅🌐 (Docker-lane, stub agent — self-contained)

Playwright is self-contained: `globalSetup` launches a spawnlet wired to the **stub** agent
(deterministic, no key/model) and the webServer starts Vite. This is a **Docker/runc-lane** check (not
runsc), included for completeness of "all the verification."

```bash
cd web && npx playwright install chromium && npm run test:e2e
```
- [ ] ✅🌐 `chat.spec.ts`, `marketplace.spec.ts`, `spawn-lifecycle.spec.ts` PASS (the config retries the
      known `ERR_NETWORK_CHANGED` container-churn flake — that's environmental, not an app bug).

---

## Phase 5 — Manual feature sweep 🌐🖥️

The runsc host can finally exercise the items the dev sandbox could not. Walk
[`MANUAL_VERIFICATION.md`](MANUAL_VERIFICATION.md) end to end; the host **uniquely unblocks**:
- **§H** egress floor (real packet drops) · **§J** cgroup limits + quota + `Runtime: runsc` ·
  **§M** DNS carve-out on an RFC1918-resolver host · **§N** the runsc pod (covered above).

The rest (§A–§G, §I, §K, §L) are CP/web behaviors verifiable on any host with the stack up — run them
once here to get a single clean end-to-end pass before the demo.

---

## Sign-off

- [ ] Phases 0–4 PASS; Phase 5 walked with no blockers.
- [ ] **Close `sp-vaw`** (P0) — the empirical gVisor single-sandbox fix is confirmed: the agent reaches
      the sidecar on `127.0.0.1:8080` under runsc, with a clean N-cycle teardown and an enforcing floor.
- [ ] **Close `sp-ghx`** (its only remaining work was this real-host validation).
- [ ] File a bug for any failure with the exact command + output; if a floor/teardown check fails,
      treat it as **P0/P1** (security/leak), not a flake.

**Reporting back:** paste the per-phase PASS/FAIL table and, for runsc, the `crictl pods` + `iptables
-S SPAWNLET-EGRESS` output before/after a spawn (the proof the pod model and floor work). Then update
the relevant `MANUAL_VERIFICATION.md` checkboxes and close the beads above.
