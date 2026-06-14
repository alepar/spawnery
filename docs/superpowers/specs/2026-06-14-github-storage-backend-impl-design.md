# GitHub Storage Backend — Implementation Design

**Bead:** `sp-u53.1`
**Status:** Approved in brainstorming (Mode A, section-by-section)
**Date:** 2026-06-14
**Implements:** [E3 Storage §2a/§4/§5/§7](2026-05-28-spawnery-e3-storage-design.md),
[Per-Mount Data Backends](2026-05-29-data-mounts-design.md)
**Cooperates with:** [Transient Tier — Kopia Journal](2026-06-10-transient-tier-kopia-journal-design.md)
**Credential owned by (deps):** `sp-v40s` (GitHub token provisioning, user-to-server OAuth),
`sp-7h6.1` (user secrets store), `sp-7h6.1.9` (required-secrets gate at create/resume),
`sp-7h6.1.4` (secret injection), `sp-7h6.1.8` (in-flight delivery guards)

This is the **implementation design** that closes the code-level gaps E3 + the data-mounts spec
left open for the GitHub-native `storage.Backend`. The high-level decisions (binding
`github:owner/repo`, per-turn + teardown persistence, non-fast-forward → LWW-surfaced,
private repos, "your data stays yours") were already made there; this doc resolves the seam shape,
credential ownership, persistence/push mechanics, and the branch/conflict model against the
**actual codebase** and the **active parallel credential epics**.

---

## 0. What changed since E3

E3 §2a/§3 assumed **server-to-server installation tokens minted at the CP** with reactive
refresh-on-401. Two facts override that:

1. **The credential model flipped to user-to-server OAuth** (`sp-v40s`): the GitHub App's
   *expiring user authorization tokens* (access + refresh + expiry + login), acting **as the
   user**, captured/refreshed by the **owner/client assisted by the AS** and stored as a generic
   typed secret in the server-blind user secrets store (`sp-7h6.1`).
2. **The CP must be unable to mint or read any token, by design** — it is a high-value compromise
   target. The CP only **relays owner-sealed ciphertext**. Plaintext exists *only* where the node
   unseals it with its HPKE sub-key at inject time (node + pod side).

Consequence: token minting, refresh, proto delivery, and the AS endpoints are **out of scope for
`sp-u53.1`** — they belong to `sp-v40s` + `sp-7h6.1.*`. `sp-u53.1` is the **storage-backend
mechanics** that *consume* the delivered secret. The repo is **the user's own repo**, which is
exactly the "your data stays yours" posture.

---

## 1. Architecture & the backend seam

Today `internal/spawnlet/manager.go` holds a single `m.backend` (always `Scratch`) and the create
command's `[]MountBinding` (carrying `backend_uri`) reaches only the auth correspondence check
(`internal/node/intentverify.go`) — it is **not** passed into `CreateWithSelection`
(`internal/node/attach.go:489`).

- **Thread the bindings through.** Pass the create command's `[]MountBinding` into
  `CreateWithSelection`; match each manifest mount (`mf.Storage.Mounts`, by `name`) to its binding.
- **Per-mount factory** replaces the single `m.backend`: `scratch:`/unset → `Scratch` (unchanged);
  `github:owner/repo` → `GitHub`. An unbound persistent mount defaults to `scratch`
  (data-mounts §4).
- **Interface.** `Backend{Prepare, Finalize}` stays the *materialization* seam. GitHub's extra
  lifecycle (suspend-push, conflict handling) is **manager-level**, hooked at the **existing
  suspend barrier** beside the journal's `FinalSnapshot` — *not* crammed into `Prepare/Finalize`.
  Concretely a small superset (e.g. `PersistentBackend{ Suspend(ctx, hostDir, binding, gen) }`)
  the manager calls at suspend; `Scratch` no-ops it. (Internal shape — open to refinement at plan
  time.)

---

## 2. Credential — consumed, never minted here

The github-token is a **generic typed secret** provisioned/refreshed/delivered by the parallel
epics. `sp-u53.1`:

- **Declares** the github-token secret-ID as **required** for any `github:`-bound mount, so the
  `sp-7h6.1.9` secrets-ready gate blocks `StartAgent` until it is delivered (fail-closed if
  missing). "No spawn runs without its secrets."
- **Consumes** the node-injected plaintext (rendered as `~/.config/gh/hosts.yml` **+ a git
  credential helper** by `sp-7h6.1.4`) for both in-pod agent git and the node-side ops below.
- Holds **no minting/refresh logic** — refresh is owner+AS driven (`sp-v40s` AS `/github/refresh`);
  re-delivery to a live pod rides `sp-7h6.1.8`'s versioned delivery.

The node retains the unsealed plaintext at inject time, so the **node-side** provisioning/backstop
ops can authenticate without the token ever needing a second custody path.

---

## 3. Provisioning (node-side; E3 §4 adapted)

Because the vault is server-blind, **the CP cannot provision the repo** — provisioning moves
**node-side**, between secret injection and `StartAgent`:

```
StartPod (sidecar + control up)
  -> inject secrets  (token plaintext -> pod tmpfs; node keeps a copy)
  -> GitHub backend Prepare:
       - repo absent (as the user)  -> create private repo, clone, seed from /app/<seed>,
                                        initial commit, push
       - repo present               -> clone/fetch the working tree
       - configure remote + credential helper in the mount/pod
  -> secrets-ready gate clears
  -> StartAgent  (cwd /app; agent works in /app/<path>)
```

- Repo defaults **private**. The binding `github:owner/repo` names the user's own repo.
- Provisioning acts **as the user** via the injected OAuth token (`gh repo create` / git over the
  credential helper) — consistent with zero CP minting.
- **Spike (in the backend task):** confirm `gh`/REST repo-create + clone/push all authenticate
  cleanly with a user-to-server token rendered through `hosts.yml` + the git credential helper
  (E3 noted hosts.yml *alone* does not authenticate `git push`).

---

## 4. Persist / push model + journal interaction

Two cooperating layers (confirmed in brainstorming):

- **Permanent layer = GitHub.** The agent makes semantic commits and pushes real branches at its
  own cadence in-pod (AGENTS.md guidance). On suspend/stop the **node backstop** (§5) pushes any
  committed-but-unpushed work so nothing committed is lost.
- **Transient layer = Garage/Kopia journal.** Uncommitted working-tree state (+ `.git`) is
  journaled exactly as today, generation-fenced. The layers cooperate; neither replaces the other.
- **Resume.** The journal restore is **authoritative** for the working tree (incl. un-pushed local
  commits) — the agent resumes exactly where it was. The GitHub clone in `Prepare` runs only on
  **fresh create** or **cross-node-without-journal**; when a journal record will restore, `Prepare`
  **skips the clone**.

No separate node-side debounced/per-turn push for MVP — agent-driven push + journal cover the loss
window; periodic node push is deferred.

---

## 5. Branch & conflict model

- The agent pushes **real branches** during the session (natural git UX).
- **Suspend backstop (the guard).** At the suspend barrier (beside journal `FinalSnapshot`) the
  node enumerates local branches, finds any **ahead of its origin tracking ref** (committed but
  unpushed), and pushes the missing commits to a **namespaced ref**:

  ```
  refs/heads/spawn/<spawn-id>/<generation>/<branch>
  ```

  *Why namespaced:* pushing leftover commits onto the real branch at suspend would clobber
  (non-ff on external edits) or silently promote half-finished work onto `main` — an editorial
  decision the agent didn't make. Namespacing makes the committed work **durable in GitHub**
  (survives journal GC) yet **quarantined** off the user's real branches; the user / a later resume
  reviews/merges/cherry-picks deliberately.
  *Why `<generation>`:* generation fences **concurrent writers** — a stale resume (old generation)
  can never clobber the current one's refs, mirroring the journal's generation fencing.

- **Conflict (external edit → non-fast-forward).** E3 policy: **last-write-wins, surfaced via
  ACP**. MVP guard: before any LWW overwrite of a real branch, the node first saves the remote's
  current tip under the **same `spawn/<id>/<generation>/<branch>` namespace** (safety ref), then
  surfaces an ACP notification pointing at both. No silent, unrecoverable clobber. The namespace is
  thus the single "nothing committed is ever lost, even when we can't fast-forward" mechanism.

---

## 6. Scope & task decomposition (children of `sp-u53.1`)

1. **Node: per-mount backend dispatch** — thread `[]MountBinding` into `CreateWithSelection`;
   scheme factory (`scratch`/`github`); default unbound → scratch. *(`internal/node/attach.go`,
   `internal/spawnlet/manager.go`.)*
2. **storage: `GitHub` backend** — `Prepare` (repo-create-if-absent as the user, clone/fetch,
   seed + initial commit, configure remote + credential helper, **skip clone when journal will
   restore**), `Finalize` (drop the local clone). Includes the §3 auth spike.
   *(`internal/storage/`.)*
3. **Node: suspend backstop push + conflict** — ahead-branch detection → push to
   `spawn/<id>/<generation>/<branch>`; non-ff → LWW + safety ref + ACP notification; hooked at the
   suspend barrier beside journal `FinalSnapshot`. *(`internal/spawnlet/manager.go`,
   suspend path.)*
4. **Declare github-token as required secret** at create/resume for `github:` mounts — integrate
   the `sp-7h6.1.9` required-secrets gate (declare the secret-ID; fail-closed if undelivered).
5. **e2e (`github`-build-tagged)** — create → agent commit → suspend (backstop push) → resume,
   against a **local git server (gitea)** with a **static token**, so the suite does not hard-block
   on the OAuth capture path; fails (not skips) when its dep is down (project test convention).

**Cross-epic deps to wire in bd:** `sp-u53.1` *soft-deps* on `sp-v40s` (token provisioning) and
`sp-7h6.1.9` / `sp-7h6.1.4` (gate + injection). Implementation of tasks 1–3 + 5 can proceed
against the local git server + static token; only the **real-GitHub** end-to-end path needs the
OAuth credential live.

**Sequencing:** task 1 unblocks 2 and 3; 2 and 3 are largely disjoint file sets (storage vs
suspend path) and can parallelize after 1; 4 is small and rides whenever the `sp-7h6.1.9` API
lands; 5 last. No `proto/` changes are owned by this epic (the proto deltas live in the credential
epics), so no proto serialization constraint here.

---

## 7. Deferred (post-MVP)

Pre-upgrade snapshot tags (ride the upgrade epic) · auto-merge on conflict · periodic/debounced
node-side push between turns · readable mirror · blob/managed backends (`sp-u53.2`/`sp-u53.3`) ·
proactive token-refresh tuning (owned by `sp-v40s`).

---

## Appendix — decision log (this slice)

| # | Decision | Choice |
|---|---|---|
| G.1 | Credential model | **User-to-server OAuth** (not installation tokens); consumed as a generic secret; **CP never mints/reads** — owner+AS mint, CP relays owner-sealed ciphertext |
| G.2 | Git locality | Agent pushes real branches in-pod; **node** does provisioning + the suspend backstop, using the node-held injected plaintext |
| G.3 | Layering | GitHub = permanent layer (committed, synced on suspend); Garage/Kopia journal = transient layer (uncommitted + `.git`); they cooperate |
| G.4 | Resume | Journal restore authoritative for the working tree; GitHub clone only on fresh create / cross-node-without-journal |
| G.5 | Backstop namespace | `refs/heads/spawn/<spawn-id>/<generation>/<branch>` — durable-but-quarantined; generation fences concurrent writers |
| G.6 | Conflict | E3 LWW-surfaced-via-ACP + pre-overwrite safety ref in the same namespace |
| G.7 | Backend seam | Per-mount factory by URI scheme; `Backend{Prepare,Finalize}` for materialization; suspend/conflict are manager-level beside journal `FinalSnapshot` |
| G.8 | Required secrets | `github:` mounts declare the github-token as a required secret at create/resume (`sp-7h6.1.9` gate; fail-closed) |
