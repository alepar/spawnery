# Spawnery — Eval Harness MVP ("Test Your App") Design

**Bead:** `sp-dkz` (MVP slice of [`sp-kkk`])
**Status:** Draft v1 (brainstorming complete; pending user review)
**Date:** 2026-06-03
**Relationship to existing epics:**
- **MVP slice of [`sp-kkk`]** ("Native eval orchestration: SUBJECT + DRIVER + JUDGE/OPTIMIZER", P3/post-MVP). This doc pulls **only the measurement-only harness** forward, exactly the "START NARROW" path that epic already names. The **optimizer loop is explicitly deferred** to `sp-kkk` proper.
- **Does NOT require [`sp-0tw`]** ("Native spawn-to-spawn comms framework", post-MVP). The harness is a **first-party, operator-run orchestrator** that drives spawns over the **existing ACP client (`internal/acp`, spawnctl-style)** — it is *not* an in-marketplace "app-spawns-app" primitive, so it sidesteps `sp-0tw`'s spawn-creation security crux.
- **Judge reuses [`sp-ydz`]** (E8 trust/safety): the App-review **LLM-as-judge** scanner, extended to grade a **transcript against a rubric** (one safety-judgment capability, a second entry point).

**Research backing:** two independent deep-research passes (`~/Documents/AgentOrchestration_Research_20260603/` and the parallel `~/spawnery-eval-vs-competitors.md`) converged on this as the lowest-hanging, highest-value orchestration taste. Competitive verification of LangSmith / Braintrust / AWS Bedrock AgentCore is folded into §7.

---

## 1. Thesis & scope

**One-liner:** A first-party, offline harness that drives a Spawnery **App-under-test** through a **creator-authored scenario** using a **mock-human spawn**, then has a **judge** grade the resulting **ACP transcript** against a rubric — surfaced to creators as a private **"Test Your App"** report. No optimizer, no public quality badge, no user-facing multi-agent UI.

**Why this is the right first taste:**
1. **Every primitive already exists.** Each of the three roles is a spawn; the conversation *is* an ACP transcript; the judge is the E8 scanner with a rubric; the orchestrator is spawnctl-style ACP driving. The new product surface is essentially "one tab on the App page" + a scenario/rubric artifact.
2. **It attacks the real marketplace problem — quality + discovery — not chat UI.** The GPT Store / HuggingChat Assistants cautionary tale is that a catalog of undifferentiated, configuration-only apps with no quality signal rots (≈95% of custom GPTs abandoned; HuggingChat Assistants sunset July 2025). Spawnery's Apps are the same *shape* (config over a shared runtime), so the differentiator must be **"is this App any good, and can anyone tell?"**
3. **It is proportionate, not gastown-overblown.** No durable-execution engine, no multi-role colony, no state-graph. The most expensive orchestration capabilities (durable/scheduled execution, supervisor swarms) are explicitly out of scope.

**In scope (MVP):**
- A creator-private "Test Your App" run: scenario → 3 spawns → transcript → scorecard.
- Three run modes sharing one judge: **Scenario** (mock-human drives), **Replay** (judge grades a real recorded conversation, no mock-human), **Diff** (re-run saved scenarios against old vs. new `app@sha`).
- Creator-authored **scenario + rubric** artifact (the real deliverable — see §4).

**Explicit non-goals (MVP):**
- ❌ Optimizer / auto-prompt-tuning (→ `sp-kkk` phase 2).
- ❌ Native spawn-to-spawn comms / app-spawns-app (→ `sp-0tw`).
- ❌ Public marketplace quality **badge/score** or eval-gated trust tier (→ long-term arc §6; gated on BYOM, §5 self-preference).
- ❌ User-facing multi-agent (@-mention, group chat, handoffs).
- ❌ Auto-generated scenarios; the creator writes them in v1.

---

## 2. Roles & architecture (capability-level)

Three roles, each a spawn, all driven by **one first-party orchestrator** (the harness) over ACP:

| Role | What it is | Reuses |
|---|---|---|
| **Subject** | The App-under-test — a normal spawn of `app@sha` on its data repo, on the standard hardened container + local DeepSeek. | E1 runtime, spawn lifecycle, sidecar |
| **Driver (mock-human)** | An LLM persona that plays a user, emitting the *user* side of the ACP conversation, following a scenario goal + constraints until a stop condition. In **Replay** mode the driver is replaced by a recorded transcript's user turns. | `internal/acp` client; a Spawnery-provided persona prompt |
| **Judge** | Grades the completed transcript against the scenario's rubric (+ any ground-truth/outcome checks). Produces per-dimension scores + reasoning. | E8 scanner (`sp-ydz`) LLM-as-judge, rubric mode |

**The orchestrator is operator-run, not an App.** It is a Spawnery-internal harness (think "CI runner" / spawnctl) that: boots the subject spawn, runs the driver loop against it, captures the transcript, invokes the judge, and writes the report. Because the harness — not a creator's untrusted App — creates and drives the spawns, the `sp-0tw` security crux ("open apps must not get unscoped spawn-creation") **does not apply** at MVP.

**Data flow (Scenario mode):**
```
creator authors {scenario, rubric}
        │
        ▼
  orchestrator ── boots ──▶ Subject spawn (app@sha + data repo, DeepSeek)
        │                         ▲
        │   drives user turns     │ ACP
        ├── Driver (mock-human) ──┘   (loop until goal met / max turns / stop)
        │
        ▼
  full ACP transcript ──▶ Judge (rubric + ground-truth checks) ──▶ scorecard + reasoning
        │
        ▼
  creator-private report: transcript view + per-dimension scores + summary
```

---

## 3. Product surface (creator-private)

- A **"Test" tab** on the App edit page (creator-only; nothing user-facing changes in v1).
- Creator picks a mode (**Scenario / Replay / Diff**), authors or selects a **scenario + rubric** (templates seeded: "Coach", "Tutor", "Companion", "Toy/Game"), runs it.
- Result view: **full ACP transcript**, **per-dimension scores**, **judge reasoning**, one-line summary. Scenarios are **saved** and re-runnable on each new `app@sha` (Diff).
- **Always show the transcript, not just the score** — per the simulated-user-fidelity caveat (§5), creators must exercise judgment, not trust a number.

**Demo fit (seed apps, from E11):** the **Interview coach** is the sharpest showcase — episodic, task-structured, an obvious scenario ("mock candidate answers questions; judge grades the coaching"). **Language-learning partner** and **zork** (deterministic, ground-truth-checkable) are strong secondary demos.

---

## 4. The real artifact: scenario + rubric

Per `sp-kkk`'s design crux, **the eval dataset/rubric is the actual product, not the runner.** A scenario is creator-authored and carries:
- **Persona** for the mock-human (traits, context, e.g. "user is rude until apologized to", "user doesn't know their account number").
- **Goal + constraints** and a **stop condition** (goal met / max turns).
- **Rubric**: 3–5 named dimensions (e.g. "stayed in persona", "completed the user's goal", "did not hallucinate from the mounted repo"), rubric-based (not pairwise) judging — the 2026 consensus that anchored rubrics reduce verbosity bias.
- **Ground-truth checks where they exist**: deterministic outcomes (a value was set, a structured result returned, a disallowed action refrained from) are checked **programmatically, not by the judge** — the τ-bench lesson (grade environment state, not the agent's prose). This is also the main defense against judge-gaming (a glib transcript can inflate LLM-judge false positives).

This artifact shapes a future addition to `spawneryapp.yml` (an optional `evals/` directory), but at MVP it can live alongside the App without a manifest change.

---

## 5. Risks & mitigations

| Risk | Mitigation |
|---|---|
| **Simulated users are signal, not truth** — ~9pp success miscalibration across user-LLMs ("Lost in Simulation", 2026); ungrounded simulators are stylistically unrealistic ("Formalism Ceiling", 6–8% style match). | Market as **creator-iteration tooling, not ground truth**; **show transcripts not just scores**; pin scenarios (no auto-gen v1); keep scores **creator-private** until they can be cross-validated. |
| **Single-model self-preference** — at MVP it's **DeepSeek-only, so judge and subject are the same model family**; self-preference bias is unavoidable. | Lean on **ground-truth/rule-based checks** where possible; flag the limitation in the report; **public badge waits for BYOM** (cross-family judging). This is the hard reason the badge is deferred (§6). |
| **LLM-judge biases** (position, verbosity). | Rubric-based (no pairwise) sidesteps position bias; a "conciseness" dimension / length-normalization curbs verbosity; if A/B/Diff judging is added, randomize order + swap-and-average. |
| **Judge-gaming / reward-hacking** the rubric. | Ground-truth checks for anything verifiable; judge sees the **full transcript incl. tool calls** (agent-as-judge direction), not just final prose; held-out scenarios. |
| **Cost & nondeterminism on the single demo box** — eval runs compete with live users for the one DeepSeek box. | **Offline / power-creator feature**, rate-limited, off the live path; runs queued; never blocks a user session. (Capacity posture per demo-MVP §6.) |
| **Scope creep into the optimizer or app-spawns-app.** | Hard non-goals (§1); those are `sp-kkk`/`sp-0tw`, designed later. |

---

## 6. Long-term arc

A deliberate ladder — **measure privately first, make it public only when it's trustworthy:**

1. **MVP (this doc):** creator-private "Test Your App" — Scenario / Replay / Diff, rubric + ground-truth judging. No public signal.
2. **Eval-driven trust signal:** graduate results into a **public, opt-in "Tested" badge** alongside the E8 trust tiers (score visible, transcripts redacted). **Gated on BYOM** so the judge can be a different model family than the subject (kills the self-preference objection). This is the **actual differentiation** (see §7) — a fast-follow, not an afterthought.
3. **Quality-aware discovery & community evals:** aggregate eval scores as *one* discovery-ranking input (never the only one; treat like SEO signals — combine + watch for adversarial creators, per the τ-bench data-leakage cautionary tale); users contribute scenarios to public Apps.
4. **The optimizer (`sp-kkk` proper):** judge → propose prompt/instruction edits → re-eval → keep-if-better; per-model prompt tuning. Makes the "your choice of AI" thesis concrete ("scores 8.2/10 with DeepSeek").
5. **Native spawn-to-spawn comms (`sp-0tw`)** and, separately and much later, a **user-facing "second opinion / call-in another App" handoff** — the one proven user-facing orchestration primitive (Poe pattern), kept distinct from eval and never gating the core single-App loop.

---

## 7. Competitive positioning (why this is a moat, post-verification)

"Containerized agents that we eval" is **not** by itself a differentiator — **AWS Bedrock AgentCore already assembles the same primitives**: BYO-container-to-ECR + per-session microVM isolation + an LLM-backed **Simulation** (mock user) + an **Evaluations** (LLM-judge) component. Braintrust's **Sandboxes** (beta) likewise run a full task/agent server-side via Lambda or a user-supplied Modal container. LangSmith's eval, by contrast, never runs the agent (you pass a `target` callable).

What survives that verification — Spawnery's actual seam:
1. **A marketplace of *mutually-untrusted third-party* apps.** AgentCore isolates *one org's own* agents (cross-session leakage); Braintrust/LangSmith assume you own the code. **Nobody treats isolation as a security boundary against untrusted *creators* in a public catalog** — Spawnery's `sp-eha` threat model.
2. **Eval as a *public marketplace trust signal*, not private CI** (the §6 step-2 surface) — AgentCore/Coze Loop/Braintrust use the triad for the deployer's own QA, never as a third-party admission/ranking signal.
3. **Transcript captured at the ACP protocol boundary.** AgentCore Evaluations needs the creator to OTEL-instrument their agent; Spawnery's spawnlet relays ACP, so it gets an **unfakeable, no-instrumentation-required transcript for free** — a real edge when the code is *untrusted*.
4. **Consumer / personal-AI / your-data framing**, vs. AgentCore's enterprise-deployer framing.

**Treat AgentCore as the reference architecture to differentiate against, not ignore.** The MVP harness itself is parity-grade; the moat is the §6 ladder (public trust signal for untrusted third-party personal-AI apps), which AgentCore deliberately doesn't serve.

### Versus configuration-only consumer stores (GPT Store, HuggingChat Assistants)

AgentCore is the *infra-layer* comp; the *consumer-marketplace* comp is the GPT Store — and a Spawnery App is the **same shape** (configuration over a shared runtime), which is exactly why the contrast is sharp. Four structural axes where the configuration-only model can't follow (hypotheses pending verification in [`sp-anj`]):

| Axis | GPT Store / config-only store | Spawnery | Apps it unlocks |
|---|---|---|---|
| **Take-home artifact** | No durable user-owned, portable store; creator "knowledge" files are read-only to the user (Code Interpreter emits only ephemeral one-off downloads). | **Owned, versioned, portable data repo** the App accumulates; export/clone anytime. | LLM Wiki / second-brain (impossible on GPT Store). |
| **Execution** | Prompt + creator knowledge + Actions (calls to a server *the creator hosts elsewhere*) + OpenAI's *own* ephemeral Code-Interpreter sandbox running the model's python — no creator container. | **Untrusted creator code in a hardened OCI container + bundled toolset, driven over ACP.** | Zork game-master, stateful tools (impossible as creator-controlled code on GPT Store). |
| **Authoring** | Bespoke **GPT-Builder UI** (form + conversational builder); can't build-as-repo with your own agent tooling and publish. *(Assistants API is programmatic but a separate product from the Store path.)* | **Repo-built Apps** (`spawneryapp.yml` + persona/skills) — build with Claude Code or whatever you use, publish the artifact. | Real dev workflow; inspectable open Apps. |
| **Quality/eval** | Manual live-preview chat only; **no automated scenario testing / scoring** in the creator studio. | **Eval harness (`sp-dkz`)** — scenario + mock-human + judge over the ACP transcript. | "Test Your App" → (later) public trust signal. |

Each axis maps 1:1 to a Spawnery primitive — **data repo / hardened container + ACP / repo-built apps / eval harness** — so this is a four-axis differentiation frame, not the single "quality + discovery" point. It also feeds the demo-MVP §5 messaging rules. The cautionary half stands: GPT Store proves that *config-only is trivial to make and trivially undifferentiated* → store rot (≈95% abandonment); the four axes are why Spawnery Apps aren't reducible to "a prompt + some files."

---

## 8. Testing & validation approach

- **Harness correctness:** deterministic end-to-end on **zork** (ground-truth outcomes, no LLM-judge noise) — the smoke test that the orchestrator drives subject + driver and captures a faithful transcript.
- **Judge calibration:** a small **human-labeled gold set** of transcripts per seed app; measure judge agreement (Cohen's κ, not raw accuracy) before trusting scores; mandatory before any move toward §6 step-2.
- **Replay mode first:** Replay (grade a recorded conversation) is the cheapest path to a working judge and de-risks the judge independently of the mock-human.
- **Success criteria (MVP):** the three seed apps each have ≥1 scenario; harness runs end-to-end offline on the demo box without disrupting live sessions; creators can read a transcript + scorecard. (Adoption targets — e.g. "≥50% of active creators run ≥3 tests/version" — belong to the §6 step-2 rollout, not the MVP build gate.)

---

## 9. Open questions (for plan stage)

- Where the scenario/rubric artifact lives at MVP (alongside the App vs. an early `evals/` in `spawneryapp.yml`).
- Whether the mock-human driver is a full spawn or a lighter ACP-client persona process (leaning lighter — it only emits user turns).
- Judge rubric templating: Spawnery-seeded templates vs. fully creator-authored at MVP (leaning seeded templates + creator overrides).
- Queueing/rate-limit policy for eval runs against the single demo box.
