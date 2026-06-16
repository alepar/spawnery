# GitHub Storage Backend — Adversarial Review (Round 2)

> **Verdict: BLOCK** · independence: same-family (Claude) · 2026-06-16
> Target: [Unified GitHub Credentials + Storage design](2026-06-14-github-credentials-and-storage-unified-design.md)
> (the revision that preceded the round-3 AS-custodial rework). Round-1 report:
> [adversarial review r1](2026-06-14-github-storage-backend-adversarial-review.md).
> Beads: `sp-u53.1`, `sp-v40s`.

Run via `superpowers:roast` (dynamic workflow): 10 critic lenses (7 core + domains:
credential-lifecycle, multi-tenant-isolation, distributed-ref-consistency) → 81 raw findings → 36
deduped → 3-judge panel per finding (100% judge completion). **29 confirmed, 1 escalation, 12
judge-raised.**

The disposition of every finding against the round-3 rework is tracked in the design's Section 16 +
Decision Log 21–27. This file is the durable record of what the panel found.

## Headline

The round-2 panel found the revision's prior blockers were **relabeled, not resolved**, and that its
keystone least-privilege mechanism rested on a doc misreading — **empirically confirmed by spike
`sp-v40s.2/.5`**: `grant_type=refresh_token` + `repository_id` does **not** narrow a rotated token.
The node-retains-refresh custody posture was the root of the worst findings; round-3 replaces it with
AS-custodial refresh.

## Confirmed blockers (gate severity = median of confirming judges)

- **F10 / F9 — repo-scoping is a doc misreading (external, confirmed by spike).** `repository_id` is
  honored only on the *initial* device/web-flow grant, not on refresh rotation; a refreshed token
  inherits the original scope. The "node mints per-mount repo-scoped tokens via refresh" keystone
  cannot exist as specified. → spec relaxed to installation-selection scope.
- **F2 — node-side single-use rotation is never written back** to the durable owner-sealed store
  (node can't re-seal to owner custody; owner may be offline). First node mint kills the stored
  refresh token → relink after every spawn. → round-3: AS owns rotation + writeback.
- **F3 — concurrent spawns brick the shared single-use chain;** "one refresher per spawn" never
  establishes per-link exclusivity. → round-3: AS is the sole rotation authority; one shared token.
- **F6 — node-held refresh = whole-user-surface mint + owner lockout;** the node picks
  `repository_id` with no AS-side validation. → round-3: node never holds the refresh token.
- **F30 — refresh credential leaks to the agent** (code-grounded): the node credential provider is
  rooted at `SecretInjector.DirFor(spawnID)`, bind-mounted into the agent at
  `internal/spawnlet/manager.go:946-959`. → round-3: node-only directory.

## Confirmed majors (selected; full set folded into Decision Log 21–27)

- **F1 / F12 / F15** persist-before-confirm is self-contradictory (forgetful AS + tmpfs-only node
  can't recover a lost rotation; scaled-AS retry hits a different instance).
- **F4 (escalation — material dissent)** the "compromised CP can't mint" residual depends on an
  AS≠CP isolation invariant the spec never states. → round-3: hard invariant in §16.2.
- **F5** AS-compromise radius understated (AS sees every user's refresh token on each mint).
- **F7** node→AS mint API authZ undefined (no node-identity binding, no idempotency key, no rate
  limit). → round-3: node-identity-bound §16.3.
- **F11** agent token rendered once with 8h expiry, no mid-session re-render. → round-3: re-render on
  fanout.
- **F16 / F17 / F20 / F32** suspend backstop: post-deletion GC has no credential path; "convenience,
  not durability" layer carries the heaviest machinery; recovery-metadata store unspecified. →
  round-3: backstop deferred from MVP.
- **F18 / F19** dropping the key-independent WIP floor regresses durability and conflicts with the
  kopia-journal-design §1b; non-journaled classes don't journal `.git`. → round-3: MVP github mounts
  require a journaled durability class.
- **F22** clone2leak / CVE-2024-53858 only partially mitigated (no `protectProtocol`, no patched
  `gh`, repo-injected helpers not refused, submodules unspecified).
- **F23 / F24** no exfil kill-switch designed; generation fencing can't stop a zombie prior-gen pod
  pushing for up to 8h. → spike `sp-v40s.3` confirmed `DELETE /token` + `/grant` exist.
- **F25** owner-offline resume contradicts the owner-online-to-reseal substrate. → resolved by AS
  custody.
- **F26** AS becomes a sustained hot-path / SPOF, not bootstrap-only.
- **F34** untrusted agent can embed a token into a journaled `.git/config` via `git remote set-url`.
- **F35 / F36** static-token seam never exercises the hardest new code; replay-guard release gate is
  prose beads-ordering, not an enforcing CI gate.

## Spike confirmations (run 2026-06-16, throwaway App `app_id=4065493`)

- `sp-v40s.1` PASS — user-to-server token created a private repo via `POST /user/repos`.
- `sp-v40s.2` PASS — create-then-clone works on a selected install; **`repository_id` not honored on
  refresh** (the F10 confirmation).
- `sp-v40s.3` PASS — `DELETE /token` (targeted) + `/grant` (grant-wide) revoke pre-expiry; rotation
  invalidates the predecessor immediately (the shared-token constraint behind round-3 §16.4).
- `sp-v40s.4` — scaled-AS nonce: shared volatile GETDEL store.

## Resolution

Round-2 was the second roast iteration and still BLOCK, so per the skill cap this routed to a
human-owned spec revision rather than a round-3 roast. That revision is the design's **Section 16
(round-3): AS-custodial refresh + CP-coordinated fanout**, brainstormed and recorded 2026-06-16.
