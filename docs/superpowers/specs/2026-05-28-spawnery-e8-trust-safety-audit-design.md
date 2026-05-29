# Spawnery E8 — Trust, Safety & Audit (Design)

**Bead:** `sp-ydz`
**Status:** Draft v1 (interview complete; pending user review)
**Date:** 2026-05-28
**Depends on:** [E0](2026-05-26-spawnery-e0-contracts-design.md),
[E1](2026-05-27-spawnery-e1-runtime-core-design.md),
[E2](2026-05-27-spawnery-e2-model-layer-design.md),
[E5](2026-05-28-spawnery-e5-packaging-catalog-design.md)

> **⚠️ Demo-MVP overlay** ([Demo MVP Scope](2026-05-28-spawnery-demo-mvp-scope.md)): demo = B+Y +
> **open third-party marketplace**, so **all spawns are Spawnery-operated and audited** and the
> **App-review scanner (§5) is a COMMITTED demo build item** (it gates open publishing — not
> deferred). Audit at the **sidecar**; run the classifier/scanner **off the DeepSeek GPU** (`sp-iui`);
> **don't persist severe-flagged content**. The full design's self-host-exempt logic returns with
> self-host.

Owns: the **abuse-audit pipeline**, the **App-review content** (owed back to E5), **free-tier
abuse signals**, and the **enforcement/takedown loop**. The governing rule: **Spawnery operates
the box → audited for abuse (disclosed); user self-hosts → not audited** (E0 §9). Permissions /
consent / egress *enforcement* remain **post-MVP** (`TODO.md`); E8 is the safety net that covers
MVP in their absence (first-party launch apps + inspectable source + audit + scanner).

---

## 1. Audit pipeline (Spawnery-operated infra only)

- **Where:** the **model sidecar / node** is the audit point (it already sees managed-inference
  content). Active only when the spawn runs on a **Spawnery-operated node** (home or burst).
  **Self-host → no audit. BYO-on-cloud → still audited** (E0 §9 / E2 §4).
- **What:** **full prompt + response content**, keyed per-spawn, with metadata (spawnId, owner,
  app@sha, model, ts, token counts).
- **Storage:** dedicated **audit store**, **encrypted at rest** (key in **KMS**, rotated; not the
  operational DB), separate from user data. **Sizing/ops (roast `sp-iui`):** full-content per turn
  grows fast — size + budget it, and the purge job must be **monitored/alerted** (a stuck purge is
  a cost *and* compliance incident). The break-glass **access log is append-only / tamper-evident**.
  Confirm the **BYO key never lands in audited content** (scrub).
- **Retention:** **short TTL (~30 days) then auto-purge.** TTL is a config constant.
- **Access:** **break-glass only** — no routine/automated read of content; admin access is
  itself logged (who/when/why), reviewable. Not used for product features, training, or
  analytics.
- **Disclosure:** stated plainly in ToS + surfaced in the UI when a spawn is placed on
  Spawnery-operated infra ("running on Spawnery cloud — audited for abuse").
- **Separate from product metrics:** this content pipeline is **abuse-only**. Product/engagement
  metrics come from a **content-free** session-event stream (hourly Parquet — `sp-yjw`); **user
  `/data` is never inspected** for analytics. Two pipelines, two purposes, two access paths.

---

## 2. Inline classifier (shared infra)

- A safety classifier runs **inline at the sidecar** on Spawnery-operated inference. It produces
  **category verdicts + scores** per turn (stored as metadata regardless of the content TTL).
- Drives **live enforcement** (§4) and feeds abuse signals (§3).
- The same classifier model/infra backs the **App-review scanner** (§5) — one safety-judgment
  capability, two entry points.
- **Don't burn the inference GPU (roast `sp-iui`):** the classifier must **not** run on the same
  scarce DeepSeek GPU that serves the demo (it would ~double GPU load + add per-turn latency). Run
  it on **CPU / a small dedicated model**, and make it **async with a fast path** — only the
  "live block on severe categories" tier (§4) needs to be inline/blocking; everything else scores
  out-of-band.

---

## 3. Free-tier & abuse signals

- On Spawnery-operated infra: **rate/volume signals** (requests, tokens, spawn churn per
  user/IP) + **classifier flag rates** feed an abuse-scoring view.
- **Free-tier caps** are enforced at the gateway (E2 §4); E8 adds **anomaly signals** (sudden
  spikes, automation patterns) → soft throttle / review queue.
- These are **content-blind aggregates** except where the classifier has already flagged content.

---

## 4. Enforcement & takedown — tiered

**(1) Live (mid-session):** the inline classifier can **block/halt a turn** on **severe
categories only** (e.g. CSAM, credible imminent violence) — a hard stop on Spawnery-operated
inference. Narrow by design (low false-positive disruption; not surveillance). **Self-host is
exempt** (no audit/classifier there).

**(2) Async (post-hoc):** findings from the scanner (§5), user **flags/reports**, or abuse
signals (§3) → graduated actions:
- **App:** delist from catalog + **block new spawns** of that `app@sha` (or all versions).
  **Existing spawns' data is untouched in the user's repo** — we stop execution, we don't reach
  into user data.
- **User/creator:** suspend account / revoke `creator` capability.
- **Severity ladder:** warn → delist/suspend → permanent ban, with notes.

**(3) Appeal:** every async action is **human-reviewable with notes**; the affected
creator/user can appeal; reviewer can reverse.

---

## 5. App-review content (the `scanned` and `reviewed` tiers)

E5 §5 owns the *tier mechanics*; E8 owns *what is inspected*. The scanner powers the **`scanned`**
tier; human review powers the **`reviewed`** tier. **The scanner is one tier, not a perfect gate** —
the trust-tier system + the enforced sandbox floor (egress + isolation + per-app consent) + tier
disclosure to the user are what make open publishing safe; the scanner raises the floor, it doesn't
solely carry it.

- **Automated scanner → promotes a version to `scanned`:** an **LLM-as-judge** over the manifest +
  `persona.md` + `skills/**` checking for:
  - **Prompt-injection / jailbreak framing** (instructions to ignore platform rules, escalate
    privileges, or subvert the agent).
  - **Data-exfiltration patterns** (instructions to send `/data` contents to external endpoints,
    encode-and-leak, beaconing).
  - **Egress/tool sanity** — declared behavior vs. what the persona/skills actually try to do.
  - **Deceptive metadata** (title/description mismatch with behavior).
  - **Disallowed-use content** per policy.
- **Verdict routing:**
  - **Open app:** scanner pass → **listed/usable immediately**; scanner flag → **held**, routed
    to human review; plus **spot-checks + on-report** review of already-listed open apps.
  - **Private app:** scanner runs, **then mandatory human review** before listing/sale (closed
    source ⇒ can't rely on public inspectability).
- **Human reviewer checklist** (on flag / private / report): confirm or clear the scanner's
  findings; assess storage-scope proportionality and egress necessity; approve / reject (notes) /
  request changes (feeds E5's queue states).

> **Build note:** the inline classifier (§2) + the review scanner (§5) are a **real ML/eval build
> item**, not config. MVP tuning leans on the small launch-app set + curated red-team prompts;
> false-negative risk on novel attacks is accepted and mitigated by audit + reporting + the small
> initial catalog.

---

## 6. User-facing reporting & transparency

- **Report/flag** affordance on every catalog listing + in-session ("report this App").
- Reports enter the async queue (§4.2) with the reporter's context.
- **Transparency:** the UI always shows the spawn's **placement + audit status** (Spawnery cloud =
  audited; self-host = not), the App's **visibility** (open/inspectable vs private/reviewed), and
  a link to the open app's source.

---

## 7. Deferred (post-MVP)

Permission/consent/egress **enforcement** (TODO.md — E8 currently compensates for its absence) ·
automated egress-vs-behavior runtime correlation · reputation/scoring for creators · published
transparency reports · region-specific legal-hold workflows · classifier on self-host (won't —
by design) · appeal SLA.

---

## Appendix — E8 decision log

| # | Decision | Choice |
|---|---|---|
| E8.1 | Audit data | **Full content** on Spawnery-operated infra, encrypted at rest, **~30d TTL auto-purge**, **break-glass** access, disclosed |
| E8.2 | Audit scope (asserted) | Sidecar/node audit point; self-host exempt; BYO-on-cloud audited (E0 §9) |
| E8.3 | Review content | **Scanner-first** (LLM-as-judge over manifest+persona+skills), **human-on-flag**; open = pass→instant + spot/report; private = scanner + mandatory human |
| E8.4 | Enforcement | **Tiered**: live block (severe only) + async delist/suspend (user data untouched) + appeal |
| E8.5 | Classifier (asserted) | Inline classifier at sidecar; shared infra with the review scanner; **real ML build item** |
| E8.6 | Free-tier signals (asserted) | Content-blind rate/volume + classifier flag-rate aggregates → throttle/review |
