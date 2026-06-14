# GitHub Storage Backend — Adversarial Review (roast)

**Reviews:** [2026-06-14-github-storage-backend-impl-design.md](2026-06-14-github-storage-backend-impl-design.md) (`sp-u53.1`)
**Date:** 2026-06-14
**Verdict:** **BLOCK** (2 confirmed blockers + 22 confirmed majors/minors; 1 escalation; 13 spikes)
**Independence:** same-family (Claude) — panel *agreement*, not cross-family verification. The
security-property findings especially warrant a human/cross-family pass.
**Coverage:** 11 critic lenses (premortem, completeness, yagni, failure-mode, feasibility,
maintainer, security, + domain experts crypto-protocol / distributed-persistence / git-ref-mgmt,
+ a **dedicated security-PoC lens** on "compromised CP cannot mint/read any token"). 105 raw →
40 distinct findings; 40/40 verified by a 3-judge opus panel with a **security-expert seat on every
security-tagged finding**.

> Special charge for this review: treat GH-token delivery as the **first PoC of the generic
> secret-delivery mechanism** ([user-secrets-store](2026-06-14-user-secrets-store-design.md)) and
> verify the load-bearing property: **a fully-compromised CP cannot mint or read any user token.**

---

## Security-property conclusion (the PoC's core claim)

**Within `sp-u53.1`'s own boundary the property HOLDS** — the CP only relays owner-sealed
ciphertext and the node is the *legitimate* unsealer (holds the HPKE sub-key private half), so even
the headline blocker does not break CP-blindness. **End-to-end, the property has real cracks that
all live in the dependency epics (`sp-v40s` / `sp-7h6.1`) and must be enforced as binding gates:**

1. **AS as plaintext token custodian** (#12, escalation E1). GitHub refresh needs the App
   `client_secret` (AS-only) → the owner must hand the `refresh_token` to the AS each rotation. If
   the AS shares a DB/process/deployment with the CP, "no server sees plaintext" **collapses**. The
   `sp-v40s` "transient-never-persist hand-off" is still an open question (redirect leaks to browser
   history; poll-endpoint needs a DB row a CP-level compromise can read).
2. **Replay protection unbuilt + unenforced** (#13). HPKE (RFC 9180 §9.7.3) gives no app-level
   replay defense; that lives in `sp-7h6.1.8`, which is unbuilt *and* has a confirmed
   AAD-construction mismatch — with **no release gate** stopping `sp-u53.1` from handling real
   tokens first.
3. **User-token exfil surfaces** that don't break CP-blindness but leak the *user's* token to third
   parties: clone2leak **CVE-2024-53858** via malicious repo content (#16); credential helper may
   write the token to durable disk → into every Kopia snapshot (#15).
4. **Notification integrity residual** (#24): the "no silent clobber" *notification* rides the CP, so
   a compromised CP can suppress/fabricate it (ref integrity holds; user-awareness does not).

---

## Confirmed blockers

- **B1 — Node-side credential path is undesigned (security; does NOT break CP property).** The
  spec's load-bearing "the node keeps a copy of the plaintext" (§2/§3/G.2) is *contradicted by code*:
  `internal/node/secrets.go:122-126` zeroes the plaintext immediately after injecting to the pod
  tmpfs; no token field on the `Spawn` struct (`internal/spawnlet/store.go`); `Backend.Prepare` and
  the proposed `PersistentBackend.Suspend` carry no credential param. A *latent* path exists
  (`SecretInjector.Write` persists plaintext on the per-spawn host tmpfs `DirFor(spawnID)` until
  teardown — spans both Prepare and the backstop) but the spec never names it. Tasks 2 & 3 are
  unbuildable as written. **Judge note:** node is the legit unsealer, so this is a
  completeness/feasibility blocker, not a property break. **Also (judge-raised):** the §3 auth spike
  covers only *in-pod* rendering; node-side reuse of pod-rendered `hosts.yml`/helper (host paths,
  `GH_CONFIG_DIR`, in-container helper binary) is not obviously portable and is uncovered.
- **B2 — Backstop branch-detection silently misses untracked branches.** "Ahead of its origin
  tracking ref" returns *empty* for branches created with `git checkout -b` (no upstream;
  `%(push:trackshort)` empty, not "ahead") — exactly the branches most likely to hold the agent's
  unprotected committed work. Defeats the "nothing committed is ever lost" guarantee. Fix: switch to
  `git log origin/<branch>..HEAD` per-branch / reachability scan vs `git ls-remote`.

## Confirmed majors (grouped)

**Lifecycle / ordering**
- #23 Prepare currently runs *before* StartPod (`manager.go:619-711` vs `:767`); GitHub Prepare
  needs the post-inject token → requires an undesigned **split-phase mount lifecycle** (pre-pod
  Scratch, post-inject GitHub).
- #10 "beside FinalSnapshot" never commits to a suspend phase: in the fail-closed
  `SnapshotForSuspend` gate a push failure blocks suspend forever; in `FinishSuspend` (pod already
  gone) a push failure silently loses the work.
- #21 Re-delivery refreshes the *pod* token but not the node's retained copy → backstop pushes with
  a stale/expired token.

**Resume / data-loss**
- #3 Prepare skips clone when a journal record exists, but **journal-restore-failure has no
  fallback** (re-clone? fail? typed error?).
- #4 Cross-node resume *without* journal does a fresh clone but ignores the quarantined
  `spawn/<id>/<gen>/<branch>` backstop refs → previous session's committed-but-unpushed work is in
  GitHub yet invisible to the resuming agent.
- #22 Same path: committed work that existed only in a now-unavailable node-local journal is
  silently discarded; unacknowledged data-loss scenario.
- #20 Contradicts kopia-journal-design §1b's key-independent `spawnery-suspend/<id>/<gen>` WIP-commit
  durability floor — this spec says "journal restore is authoritative" and describes no WIP-commit
  push at suspend.

**Git ref management**
- #8 Safety-ref (pre-LWW remote tip) and backstop quarantine ref **map to the same path** →
  the second write destroys one of the two copies the conflict design promises to keep.
- #5 Backstop refs have **no GC** (not at delete/close/gen-rollover) → unbounded accumulation,
  pollutes the Branches UI, approaches GitHub's ~100k ref soft limit.
- #19 A zombie pod whose OAuth token is still valid after a generation change (GitHub has no
  per-generation token revocation, unlike Garage per-gen keys) can keep pushing to real branches →
  **agent-vs-agent** concurrent write the user-vs-agent LWW model doesn't cover.

**Credential / expiry / provisioning (security)**
- #2 User-to-server tokens expire in **8h** (verified: `expires_in=28800`) → sessions >8h 401 at
  the backstop; refresh fully deferred with no node-initiated path / no loss-window bound.
- #9 `gh repo create` / `POST /user/repos` private repo needs **`Administration:write`** (also
  grants rename/delete) — unstated, unverified vs the `sp-v40s` App registration, conflicts with
  E3 §2a "never the broad repo scope".
- #12 AS `/github/refresh` unspecified; makes the AS a plaintext custodian of all GitHub refresh
  tokens (see property-conclusion #1).
- #15 In-pod credential helper underspecified: which binary, durable-disk writes
  (`git-credential-store` → `~/.git-credentials` plaintext), `GH_CONFIG_DIR` redirect, whether the
  agent can override it, whether the token lands in the journaled home → every Kopia snapshot.
- #16 clone2leak **CVE-2024-53858**: malicious repo content (`.gitmodules`, `.lfsconfig`,
  `.git/config` `credential.helper`) can redirect the helper to exfiltrate the token; agent runs
  git on untrusted content with no named mitigation.

**Test coverage**
- #7 The gitea + static-token e2e (task 5) covers **zero** of the production credential path
  (HPKE seal/unseal, `sp-7h6.1.9` gate, `hosts.yml`/helper with a user-to-server token, App perms,
  expiry); and **`gh` can't target non-GitHub endpoints**, so `gh repo create` provisioning can't be
  exercised against gitea at all.

**Notification**
- #6 Idle / CP-driven / orphan-reaper suspends often have no live ACP session; the conflict
  notification has no persistence/queue/retry → silently swallowed in the common idle-suspend case.

## Confirmed minors
- #14 Safety-ref push + LWW push are two non-atomic ops; safety-success-then-LWW-failure leaves a
  dangling ref with no notification (half-success unspecified).
- #17 `refs/heads/spawn/*` shows as ordinary branches in the GitHub UI/dropdowns — contradicts
  "quarantined"; a `refs/spawnery/*` non-heads namespace would avoid it (tradeoff not acknowledged).
- #18 `refs/heads/spawn/<uuid>/<gen>/<branch>` can exceed GitHub's 255-byte ref limit; no
  truncation/sanitization strategy.
- #24 LWW notification rides the CP → suppressible/forgeable under CP compromise (residual,
  unacknowledged).

## Escalation (material dissent — needs human)
- **E1** The `sp-v40s` AS→client token hand-off (how the `(access, refresh, expiry)` tuple reaches
  the owner's device after the OAuth callback) is an **open design question**: redirect URL leaks
  `refresh_token` to browser history; poll endpoint requires the AS to store the tuple transiently
  in a DB row readable by any CP-level compromise → breaks "CP cannot read any token." Property-
  critical; resolve in `sp-v40s` before `sp-u53.1` handles real tokens.

## Recommended spikes (highest-leverage first)
1. **Node credential path (B1):** trace every `InjectSecret` caller for a retained plaintext buffer
   after the zero loop; check `Spawn` for a token slot. Kill: any path writing the token to
   `/proc/environ`, subprocess argv, or durable disk violates never-persist.
2. **Untracked-branch detection (B2):** `git checkout -b x && git commit`; run the tracking-ref
   check → confirm empty. Kill: empty → switch to `origin/<branch>..HEAD` reachability.
3. **8h expiry:** obtain a user-to-server token, wait 9h, `git push`. Kill: 401 with no inlined
   refresh → backstop durability void for long sessions.
4. **Provisioning perms:** `POST /user/repos` with/without `Administration:write` (201 vs 403); also
   check user-consent friction / App-review rejection.
5. **AS refresh custody (property-critical):** does the AS persist the GitHub refresh token
   server-side, or does the client provide it per-call sealed? Kill: AS+CP share a DB/process/deploy
   unit → isolation is non-cryptographic. Candidate fix: AS delivers via a short-lived single-use
   nonce the SPA redeems with no DB write.
6. **Replay release-gate:** is there a gate/integration test blocking `sp-u53.1` from real tokens
   before `sp-7h6.1.8` lands? Kill: absent → replay vuln is process-only, not enforced.
7. **Suspend-phase slot:** which phase (`SnapshotForSuspend` pre/post `FinalSnapshot`, or
   `FinishSuspend`), and does backstop-push failure abort suspend?
8. **credential-helper render:** does `sp-7h6.1.4` set `GH_CONFIG_DIR` to a secrets-tmpfs path (no
   `~/.git-credentials`, no journaled-home write)? Is the helper patched against CVE-2024-53858?
9. **Prepare-after-inject ordering:** prototype two-phase `CreateWithSelection` (GitHub mounts
   prepared after `InjectSecret`); check races with the egress floor + `sp-7h6.1.9` gate.
10. **gitea limits:** real-GitHub-App test org (separate build tag) to cover provisioning + the real
    HPKE→hosts.yml→git-credential→push path; ref-length >240 byte push test.

---

*Full raw panel output (243 KB) was produced by the roast workflow run `wf_db184ff3-478`; this doc
is the durable distillation. Roast is report-only — no spec edits or tasks were made by the review.*
