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

**Demo MVP = B + Y only.** One execution environment (the Spawnery home server), one inference
path (local DeepSeek v4 Flash). Everything is Spawnery-operated and **audited for abuse,
disclosed**.

---

## 2. What the demo MVP *is*

> A **free, one-click, hosted** personal-AI app — launch lineup zork / **LLM Wiki** (flagship) /
> habit-goal coach / system-design interview coach — running on Spawnery's DeepSeek box, with
> your content saved to **your own GitHub repo**, audited for abuse.

It proves the **wedge** (the viral LLM-Wiki pattern has a turnkey hosted home) and the **data**
half of the thesis (your content lives in your GitHub). It does **not** yet deliver the **model**
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
| **Permissions / consent / egress enforcement** | **Deferred** (already post-MVP; E8 audit is the safety net). |

---

## 4. What stays in the demo (largely as designed)

- **E1 runtime core** — control plane + node agent + ephemeral cold-start spawn pods. Simplified:
  isolation can be **plain containers** (no untrusted operator/code → microVM/gVisor deferred);
  the spawn pod is `base[agent + bridge + common toolset]` + `sidecar→local DeepSeek`.
- **E3 storage** — git-repo substrate, GitHub adapter (Spawnery GitHub App), `.spawnery/` layout,
  per-turn semantic-commit persist. (Blob/`git bundle` adapter optional for the demo — GitHub-only
  is fine.)
- **E5 packaging & catalog** — manifest, definition repos, poll-model versioning, catalog. Demo
  apps are first-party/open; the private-app review pipeline can wait.
- **E6 web client** — catalog → spawn → chat. Onboarding collapses to **OAuth → connect GitHub →
  spawn** (no vault, no agent/model pickers — one agent, one model).
- **E7 launch coaches** — all four apps, on the one agent + local DeepSeek.
- **E8 trust/safety/audit** — audit at the **sidecar** (one managed path → trivial), abuse
  scanner/classifier, flag/takedown. (Classifier shares the one GPU — see roast `sp-iui`.)
- **E4 identity** — **OAuth (Google + GitHub) + account model only.**

**Corrected continuity model (from roast `sp-0ah`):** cold start = a **new session**, *not* a
replay of the prior conversation thread. Continuity comes from the agent **reading the structured
files in `/data`** at session start (goals.md, progress.md, wiki pages, the zork save) — like a
coding agent reading a codebase. Idle timeout tuned to the LLM provider's prompt-cache TTL.

---

## 5. Brand / messaging rules for the demo

- **Lead with:** "free, one-click, your content in your own GitHub, the LLM Wiki you can actually
  run." Honest, demo-true.
- **Do NOT claim at demo launch:** "your choice of AI" / "model-agnostic" (it's DeepSeek-only);
  "your data, we see nothing" (everything is audited); "self-host / no lock-in" (deferred).
- **Disclose plainly:** runs on Spawnery's hardware; conversations audited for abuse (30-day TTL);
  Spawnery holds a scoped write-token to your connected repo (roast `sp-gl2`).
- The full differentiation ("your choice of AI," self-host, true privacy) is the **named next
  phase**, not vaporware — the full design exists.

---

## 6. Capacity posture (forced by B+Y, no burst)

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
| `sp-pgw` coach validation | **Demo-relevant** — apps ship; dogfood the habit coach before launch. |
| `sp-vf8` vault hardening | **Full-design only** — no vault in demo. |
| `sp-gl2` storage-token honesty | **Demo-relevant** — disclose CP repo write-token; consider a storage-access log. |
| `sp-izq` stale-reference sweep | **Folded here** — the E2E/bridge-cert refs are all "full design"; demo uses plain TLS. Reconcile when editing. |
| `sp-oh1` open-core false claim | **Demo-relevant** — demo claims no self-host/open-core; fix the claim in the full design too. |
| `sp-7fj` GitHub polling / persist | **Demo-relevant** — storage + catalog ship; cache tag→SHA, debounce persist. |
| `sp-ba5` consent/egress presented as shipped | **Demo-relevant** — align docs: deferred in demo too. |
| `sp-nxp` protocol/contract drift | **Partial** — createSpawn/startSpawn + POST /spawns sequencing apply to demo; session-token/node-key parts are full-design. |
| `sp-iui` ops grab-bag | **Partial** — classifier off the one GPU + audit-store sizing + ad-hoc-URL limits + **verify all 4 apps run acceptably on DeepSeek** apply to demo; burst parts are full. |

---

## 8. Demo build order

1. **Runtime + storage + one agent + local DeepSeek + minimal web client + zork** — the end-to-end
   spawn→chat→persist slice (E1 + E3 + E2-minimal + E6-minimal + E7/zork).
2. **LLM Wiki** (the wedge) on GitHub storage.
3. **Catalog + the other three apps**, audit/scanner, capped-beta gating + status page + CP backup.

Self-host, BYOK, burst, billing, the E2E channel, and the vault all begin **after** the demo
validates the wedge — using the full design that already exists in E0–E8.
