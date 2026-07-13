# VM Lane Fidelity: Gitea Behind TLS as a Real `github.com`

- **Bead:** `sp-wwtc` (epic `sp-2tx8`) — blocks `sp-2tx8.3.8` (the node-restart CA test)
- **Status:** draft
- **Mode:** one-shot (Mode B)

## 1. Problem

**The sidecar's GitHub MITM proxy has zero end-to-end coverage.** It is the component that holds the
per-spawn CA, mints TLS leaves for intercepted hosts, and injects the GitHub credential — and no
automated lane exercises it.

The VM lane *looks* like it covers git. It does not:

- The proxy MITMs an **exact-match allowlist** (`internal/sidecar/githubhost.go`): `github.com`,
  `codeload.github.com` (Basic-auth injection), `api.github.com`, `uploads.github.com`, `gist.github.com`,
  `raw.githubusercontent.com` (Bearer). Gitea at `127.0.0.1:3000` is not in it and cannot be.
- The VM's Gitea is **plain HTTP** (`provision.sh:253` — `ROOT_URL = http://127.0.0.1:3000/`). No TLS
  means no MITM, which means the per-spawn CA is never used.
- The agent cannot reach it anyway: the agent shares the **sidecar's netns**, so `127.0.0.1:3000` is the
  *pod's* loopback, and `NO_PROXY=127.0.0.1,localhost` (`gitproxy.go`) deliberately keeps loopback off
  the proxy.

What the lane actually covers is **the node** cloning from a plain-HTTP git server. `git-persistence.spec.ts`
passes on exactly that path. So CA delivery (sidecar `FetchCA` from the node), leaf minting
(`githubca.leafFor`), the agent's trust of the injected bundle, and the sidecar's **upstream TLS
verification** are all untested outside unit tests.

This also **blocks `sp-2tx8.3.8`**: the node-restart test must prove the per-spawn MITM CA survived a
restart, and the only honest proof is an HTTPS git operation from inside the agent *through* the MITM.
Against a plain-HTTP Gitea the agent cannot even route to, no such test can exist — any test written
against today's setup either cannot pass, or passes without testing the CA.

## 2. Main challenges

The temptation is to make the test pass by weakening something: add Gitea to the MITM allowlist, or set
`InsecureSkipVerify` on the sidecar's upstream leg "just for e2e". **Both delete the property the test
exists to prove.** The allowlist is exact-match precisely because a loose predicate would let a look-alike
host harvest the real GitHub token (a prior roast BLOCKER, `githubhost.go`), and the upstream transport is
strict by design (`newDefaultUpstreamTransport`: no `InsecureSkipVerify`, `Proxy=nil`, HTTP/1.1 only).

The second challenge is that the sidecar **will** inject `Authorization: Basic base64("x-access-token:"+token)`
at `github.com`. That is not optional — it is the real code path. So the fake GitHub has to tolerate it.

## 3. Key decisions

**Make the VM's fake GitHub actually be `github.com`, and change no production code.** Front Gitea with
TLS presenting a cert for `github.com` signed by the golden CA; resolve `github.com` from inside the pod to
it; let the sidecar MITM it exactly as it does in production. The only thing faked is *who is on the far
end of the socket* — which is the correct seam to fake, and the one that leaves every security-relevant code
path intact.

## 4. Decision points

### 4.1 How the golden CA reaches the sidecar's upstream trust

**Chosen: a merged bundle (system roots + golden CA) mounted into the sidecar, with `SSL_CERT_FILE`
pointing at it — in the e2e/VM profile ONLY.**

The sidecar is Go, so `x509.SystemCertPool` honours `SSL_CERT_FILE`. Note it **replaces** rather than
appends, so the bundle must be *merged* (system roots + golden CA), not just the golden CA — otherwise the
sidecar loses its real roots.

- The **production sidecar image stays clean**: no test CA is ever baked into an artifact that also ships to
  production, where anything signed by that CA would then be trusted.
- The strict upstream transport is **untouched**. `InsecureSkipVerify` stays off. The test therefore proves
  *real* upstream certificate verification — which nothing currently does.

*Considered:* baking the golden CA into the sidecar image — rejected: it ships a test trust anchor in a
production artifact. *Considered:* a first-class `SIDECAR_EXTRA_CA_FILE` config knob that appends to the
system pool — cleaner than the env-var trick and avoids the merge, but it is new **production** surface added
purely to serve a test. Revisit only if the merged-bundle approach proves brittle.

### 4.2 The MITM allowlist is not touched

**Chosen: change nothing in `githubhost.go`.**

`github.com` and `codeload.github.com` are already `actionMitmBasic`. Pointing `github.com` at Gitea slots
directly into the production path. Adding Gitea's real hostname to the allowlist would both (a) test a code
path that does not exist in production and (b) erode an exact-match predicate that exists to stop token
theft by look-alike hosts.

`codeload.github.com` is only used for archive/tarball downloads; plain `clone`/`fetch`/`push` over git
smart-HTTP go to `github.com` alone. **Map it anyway** — it is one extra SAN and one extra DNS entry, and a
surprise failure there would be baffling to debug.

### 4.3 Gitea must accept the injected credential

**Chosen: provision a Gitea user whose token matches the credential the CP hands out.**

The sidecar injects `Authorization: Basic base64("x-access-token:"+token)` at `github.com`. Gitea must accept
that (or be configured to ignore it for the test repos). This is the piece most likely to bite, and it must be
made to work rather than bypassed — an injected credential that the far end ignores would leave the injection
path effectively untested.

### 4.4 DNS: `github.com` → the VM's TLS front, from inside the pod

**Chosen: a resolver on the VM that answers `github.com` (and `codeload.github.com`) with the VM's IP, with
the pod's `resolv.conf` pointed at it.**

The CRI backend already supports this: `PodSpec.DNSServers` → `sandboxCfg.DnsConfig`
(`internal/runtime/cri/backend.go:29,88`), and its doc comment already notes that without a kubelet the pod
would otherwise inherit the host's `resolv.conf`. So the wiring exists; the VM profile just has to set it and
run the resolver.

*Considered:* per-container `/etc/hosts` entries (Docker `ExtraHosts`) — works on the Docker lane but the VM
lane is **CRI/runsc**, where there is no kubelet to inject `HostAliases`. DNS is the seam that exists on the
lane we actually run.

### 4.5 The TLS front

Caddy already terminates `:443` in the VM with a golden-CA-signed wildcard
(`/etc/spawnery/pki/wildcard.{crt,key}`). Add a **host-matched site block** for `github.com` /
`codeload.github.com` that reverse-proxies to Gitea on `127.0.0.1:3000`, with a cert whose **SANs cover those
names**, minted from the golden CA at provision time.

Gitea's `ROOT_URL` must then be `https://github.com/` so the URLs it generates are consistent.

## 5. Acceptance criteria

- The acceptance github-slot app clones from **`https://github.com/<org>/<repo>.git`**, not
  `http://127.0.0.1:3000/...`.
- **An agent-side `git fetch` over HTTPS succeeds**, through the sidecar's MITM, trusting the per-spawn CA.
  This is the assertion that does not exist today at all.
- The sidecar's **upstream** leg verifies Gitea's certificate against the golden CA, with
  `InsecureSkipVerify` **off** and `newDefaultUpstreamTransport` unmodified. A run with the golden CA
  *removed* from the sidecar's bundle must **fail** — proving the verification is real and not vacuous.
- `githubhost.go` is **unchanged** (no new allowlist entries; the exact-match predicate is untouched).
- The production sidecar image contains **no golden CA**.
- `sp-2tx8.3.8` becomes implementable: the agent runs `git fetch` before and after a node restart, and a
  changed CA fails with a certificate error.

## 6. Out of scope

Using a real GitHub. Changing the MITM allowlist or the auth-injection logic. Any weakening of upstream TLS
verification — explicitly forbidden, in any test mode.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from the
assumptions above — append a dated note here, whether or not a formal debugging skill was used.*
