# Spawnery — Demo MVP Scope (the reduced cut)

**Status:** Active scope definition
**Date:** 2026-05-28
**Relationship to the other specs:** The E0–E8 design docs describe the **full target
architecture** — self-hosting, BYOK, model-agnosticism, cloud burst, bottleneck solutions.
That design stands as the goal. **This document defines the deliberately reduced subset we
build for the first "demo MVP,"** and what is explicitly deferred. Where a design doc and this
doc disagree on *what ships first*, this doc wins for the demo; the design doc remains the
target for the full build.

---

## 1. The cut, on two axes

**Execution** (where the agent container runs) — A self-host · B home server · C cloud burst
**Inference** (where the LLM runs) — X BYOK · Y home DeepSeek · Z managed 3rd-party

| | In the **full design** | In the **demo MVP** |
|---|---|---|
| **A. Self-host (user HW)** | ✅ open-core edition | ❌ deferred |
| **B. Home server (Spawnery box)** | ✅ free tier | ✅ **the only execution env** |
| **C. Cloud burst** | ✅ auto-scale | ❌ deferred |
| **X. BYOK** | ✅ "your choice of AI" | ❌ deferred |
| **Y. Home DeepSeek** | ✅ free default | ✅ **the only inference path** |
| **Z. Managed 3rd-party** | ✅ premium/burst | ❌ deferred |

**Demo MVP = B + Y execution/inference**, **plus an open marketplace and multi-option storage.**
One execution environment (the Spawnery home server), one inference path (local DeepSeek v4 Flash),
everything Spawnery-operated and **audited for abuse, disclosed** — **but** with a **fully open
third-party app marketplace** (automated review scanner) and **three storage options** (managed
default, BYO-cloud, GitHub).

| Cross-cutting axis | Demo MVP |
|---|---|
| **Marketplace** | ✅ **fully open third-party publishing + automated App-review scanner** |
| **Storage** | ✅ **managed default + Drive/iCloud/OneDrive + GitHub** (power-user) |

> **⚠️ SCOPE NOTE (revised 2026-05-28).** Choosing *fully open publishing + the automated scanner*
> (over the curated/minimal options) **re-introduces the trust machinery the B+Y cut had deferred**.
> The demo MVP is therefore **the full cloud platform MINUS** {self-host, BYOK, cloud burst, billing,
> the vault, the per-session E2E channel} — **not** a thin wedge demo. Open third-party code running
> on other users' data + your box makes the App-review scanner, egress enforcement, isolation
> hardening, and per-app permission/consent enforcement **mandatory, not deferred** (see §4). This
> is a deliberate trade: more build, real differentiation, vs. the original "ship cheap/fast."

---

## 2. What the demo MVP *is*

> A **free, one-click, hosted marketplace of personal-AI apps** — seeded with zork / **LLM Wiki**
> (flagship) / habit-goal coach / system-design interview coach, **open for third-party creators to
> publish** (auto-reviewed) — running on Spawnery's DeepSeek box, with your content saved to **your
> own storage** (managed default, your cloud, or GitHub), audited for abuse.

It proves the **wedge** (the viral LLM-Wiki pattern has a turnkey hosted home), the **data** half of
the thesis (content in storage you control), **and now a slice of the marketplace flywheel** (do
creators publish, do users spawn third-party apps — H3/H4). It does **not** yet deliver the **model**
half ("your choice of AI") or self-hosting — those are the headline of the *next* phase.

---

## 3. Subsystems collapsed for the demo (full design keeps them)

| Full-design subsystem | Demo MVP |
|---|---|
| **Vault** (passphrase, BIP-39 recovery, Argon2, envelope encryption) — E4 §3/§4 | **Removed.** No BYO secrets to protect → no vault, no recovery codes, no client-side crypto. |
| **BYO-secret delivery** — E2 §2 | **Removed.** No BYOK. |
| **Model gateway / managed keys / metering** — E2 §3/§4 | **Removed.** Sidecar → local DeepSeek only. |
| **Per-session E2E channel** (node-static + client-ephemeral key agreement, opaque CP relay) — E0 §10, E6 §2 | **Removed.** Everything is Spawnery-operated + audited, so encrypting content *from* the CP buys nothing. Transport = **plain TLS client↔CP**, CP→node internal. The E2E channel returns with self-host (where it matters). |
| **Node enrollment user flow + node-key trust anchor** — E4 §6 | **Internal only.** All nodes are Spawnery's own; no user-facing enrollment. The CA/mTLS/pinning story returns with self-host. |
| **Self-host edition / open-core** — system §1 | **Deferred.** Demo makes no self-host or open-core claim. |
| **Cloud burst** (E9) / **billing & rev-share** (E10) | **Deferred** (already post-MVP). |

> **Permissions / consent / egress enforcement, the App-review scanner, and isolation hardening are
> NO LONGER deferred** — open third-party publishing makes them mandatory. They move to §4.

---

## 4. What stays / is REQUIRED in the demo

- **E1 runtime core** — control plane + node agent + ephemeral cold-start spawn pods, pod =
  `base[agent + bridge + restricted toolset]` + `sidecar→local DeepSeek`. **Isolation is HARDENED,
  not plain containers** — third-party apps run untrusted-creator-authored agents on other users'
  data, so: gVisor-class isolation (or equivalent), **cgroup CPU/mem/disk/pids limits**, per-user
  concurrency cap. (roast `sp-eha`/`sp-ach`)
- **Egress allowlist floor (REQUIRED, roast `sp-rpa`)** — per-spawn network policy: only the
  localhost sidecar + the spawn's storage host + the app's **declared** egress domains; **drop
  cloud-metadata (169.254.169.254) + RFC1918**. Owner = E1 (pod netns).
- **Per-app permissions + consent + enforcement (REQUIRED — un-deferred)** — the manifest
  `permissions` block (storage scope + egress allowlist) is back; the user **consents at spawn**;
  the egress floor **enforces** it. Open third-party apps cannot run unscoped. (was roast `sp-ba5`)
- **App trust tiers (REQUIRED)** — every App@version carries a tier; **runtime restrictions +
  what the user is told scale with it**, and the **enforced sandbox floor (egress + isolation +
  consent) applies to ALL tiers** — so a scanner miss is *contained*, not catastrophic:
  | Tier | How earned | Runtime posture | User-facing |
  |---|---|---|---|
  | **Unverified** | published, not (yet) scanned / scan declined | tightest: egress = storage+model only, lower quotas, can't request broad scope | loud "NOT checked — run at your own risk" |
  | **Scanned** | passed the automated **App-review scanner** (E8 §5 LLM-as-judge) | standard: its **declared** permissions (consent + enforced) | "automatically checked" badge |
  | **Reviewed** | a **human** reviewed it | eligible for elevated capability + featured placement (and later private/paid) | "human-reviewed" badge |
  - **Per-version:** tier is for `App@sha`; a new version drops to *scanned* (re-scan) and any
    **permission escalation drops it out of *reviewed*** until re-reviewed. Seed apps = *reviewed*.
  - The **scanner is the *scanned* tier mechanism**, not a perfect gate — it no longer single-
    handedly carries "open publishing is safe"; the tier + floor + informed consent do.
- **E3 storage — three options:** **managed default** (Spawnery-hosted repo, **web-browsable in-app
  + one-click download/clone**), **BYO-cloud** (Google Drive / iCloud / OneDrive via OAuth, `git
  bundle`), **GitHub** (power-user). Default = managed (zero-setup for non-techs). Default any
  spawned/managed repo to **private**. (roast `sp-0a2`/`sp-gl2`)
- **E5 packaging & catalog — OPEN registry:** manifest, definition repos, poll-model versioning,
  catalog browse/search, **open third-party publish flow** gated by the automated scanner +
  flag/takedown. Ad-hoc URL spawn keeps the SSRF guard (`sp-iui`).
- **E6 web client** — catalog browse → spawn (storage picker + **consent screen**) → chat;
  **creator publish flow**. Onboarding default path = **OAuth → managed storage → spawn** (no vault,
  no model picker — one model). Agent picker hidden (one agent).
- **E8 trust/safety/audit** — audit at the **sidecar** (run the classifier **off the inference
  GPU**, `sp-iui`); **don't persist severe-flagged content**; flag/takedown for the open registry.
- **E4 identity** — **OAuth (Google + GitHub) + account model only** (no vault; no BYO secrets).
- **E7 seed apps** — the four ship as the seed catalog; the marketplace is open for more. (Coaches
  stay as seeds; validate per `sp-pgw`. The "drop coaches" advice is moot now — an open catalog
  *wants* a populated seed set.)

**Corrected continuity model (from roast `sp-0ah`):** cold start = a **new session**, *not* a
replay of the prior conversation thread. Continuity comes from the agent **reading the structured
files in `/data`** at session start (goals.md, progress.md, wiki pages, the zork save) — like a
coding agent reading a codebase. Idle timeout tuned to the LLM provider's prompt-cache TTL.

---

## 5. Brand / messaging rules for the demo

- **Lead with:** "free, one-click marketplace of personal-AI apps you can spawn and own your data —
  the LLM Wiki you can actually run, plus a catalog anyone can publish to." Marketplace + data are
  now both demo-true.
- **Do NOT claim at demo launch:** "your choice of AI" / "model-agnostic" (it's DeepSeek-only);
  "your data, we see nothing" (everything is audited); "self-host / no lock-in" (deferred).
- **Disclose plainly:** runs on Spawnery's hardware; conversations audited for abuse (30-day TTL);
  if you connect GitHub, Spawnery holds a scoped write-token to that repo (roast `sp-gl2`) — the
  managed + BYO-cloud options keep data in your own storage.
- The full differentiation ("your choice of AI," self-host, true privacy) is the **named next
  phase**, not vaporware — the full design exists.

---

## 6. Capacity posture (one box, no burst)

One DeepSeek box, no burst relief → small concurrent ceiling + hard SPOF. Therefore the demo is a
**capped / waitlisted beta** with an honest status page. Minimum resilience still required:
**CP-state backup** (accounts + spawn index) so a box failure isn't unrecoverable (roast `sp-jf7`).

---

## 7. Roast-finding triage under the demo cut

| Finding | Under demo MVP |
|---|---|
| `sp-gtm` CP key-vending MITM | **Full-design only** — no E2E channel in demo (plain TLS to CP). Returns with self-host. |
| `sp-8hc` dishonest privacy framing | **Demo-relevant** — apply the §5 messaging rules. |
| `sp-dcj` BYO-audit gap | **Moot for demo** (no BYO; audit at sidecar, single path). Reapplies when BYOK lands. |
| `sp-9um` CP SPOF / relay cliff | **Mostly deferred** — no E2E relay in demo (plain TLS); single CP acceptable for capped beta. Seam noted for full. |
| `sp-jf7` home singleton / DR | **Demo-relevant (partial)** — capped beta + **CP-state backup** required. Failover/HA = full design. |
| `sp-73q` onboarding gauntlet | **Largely resolved** — no vault/BYOK → OAuth + connect GitHub + spawn. |
| `sp-0ah` cold start | **Resolved** — accepted; continuity = file reads (§4). |
| `sp-pgw` coach validation | **Demo-relevant** — coaches ship as seed apps; dogfood the habit coach before launch. |
| `sp-vf8` vault hardening | **Full-design only** — no vault in demo. |
| `sp-gl2` storage-token honesty | **Demo-relevant** — disclose CP repo write-token; managed/Drive options give real ownership; storage-access log. |
| `sp-izq` stale-reference sweep | **Done** — reconciled to node-terminates / plain-TLS-in-demo. |
| `sp-oh1` open-core false claim | **Done** — claim corrected; demo makes no self-host claim. |
| `sp-7fj` GitHub polling / persist | **Demo-relevant** — cache tag→SHA, debounce persist, incremental blob. |
| **`sp-rpa` egress floor** | **REQUIRED in demo** (was full-design) — open 3rd-party code makes it mandatory. |
| **`sp-eha` threat-model inversion** | **REQUIRED in demo** — hardened isolation; open creator code is the worst case. |
| **`sp-ach` resource limits / Sybil** | **REQUIRED in demo** — limits + per-user quotas + concurrency cap. |
| **`sp-ba5` consent/egress enforcement** | **REQUIRED in demo (un-deferred)** — open 3rd-party apps need declared scope + consent + enforcement. |
| **`sp-0a2` storage / market** | **Adopted** — managed default + Drive + GitHub; default repos private. |
| **App-review scanner (E8 §5)** | **REQUIRED in demo** — gates open publishing (committed ML build item). |
| `sp-nxp` protocol/contract drift | **Done** — E0 §5a/§6 reconciled. |
| `sp-iui` ops grab-bag | **Demo-relevant** — classifier off the GPU + audit-store sizing + ad-hoc-URL SSRF guard + restricted toolset + verify seed apps run on DeepSeek. |
| `sp-54f` weak experiment | **Improved, not solved** — open marketplace now tests a slice of H3/H4; still need a success metric + retention hook + representative audience (the wiki's PKM cold-start-value risk persists). |

---

## 8. Demo build order

1. **Vertical slice:** runtime + **managed storage** + one agent + local DeepSeek + minimal web
   client + **zork** (E1 + E3-managed + E2-minimal + E6-minimal + E7/zork) — proves spawn→chat→persist.
2. **Safety floor (before any third-party or public exposure):** egress allowlist + metadata/RFC1918
   block (`sp-rpa`), isolation hardening + resource limits + per-user quotas (`sp-eha`/`sp-ach`),
   restricted toolset.
3. **LLM Wiki** (the wedge) + the **storage options** (Drive/iCloud + GitHub) + web-browsable/export.
4. **Open marketplace:** catalog browse/search + **open publish flow** + **App-review scanner**
   (E8 §5) + **per-app permissions + consent + enforcement** (`sp-ba5`) + flag/takedown.
5. **Seed apps** (coaches + community), audit pipeline + classifier-off-GPU, capped-beta gating +
   status page + **CP-state backup** (`sp-jf7`).

Self-host, BYOK, burst, billing, the per-session E2E channel, and the vault all begin **after** the
demo — using the full design that already exists in E0–E8. **The demo is now the MVP cloud platform,
not a thin wedge demo** (see §1 scope note).
