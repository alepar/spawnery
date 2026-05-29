# Spawnery E7 — Launch Coach Repos (Design)

**Bead:** `sp-2tb`
**Status:** Draft v1 (interview complete; pending user review)
**Date:** 2026-05-28
**Depends on:** [E0](2026-05-26-spawnery-e0-contracts-design.md),
[E1](2026-05-27-spawnery-e1-runtime-core-design.md),
[E2](2026-05-27-spawnery-e2-model-layer-design.md),
[E3](2026-05-28-spawnery-e3-storage-design.md),
[E5](2026-05-28-spawnery-e5-packaging-catalog-design.md)

The four launch Apps — the proof that the platform's abstractions express real, distinct Apps.
Each is a **definition repo** (per E5 §1) targeting the MVP agent. Apps are open + first-party.

> **⚠️ Demo-MVP overlay** ([Demo MVP Scope](2026-05-28-spawnery-demo-mvp-scope.md)): the **seed
> lineup is NOT finalized**. The LLM Wiki is **one** candidate, not the flagship — the architecture
> is the bet, apps are interchangeable demonstrations. **E11 (`sp-4jg`) brainstorms the portfolio +
> retention hypotheses first and gates this lineup.** The four below are the starting candidates;
> validate retention/fit (esp. the habit coach, `sp-pgw`) and let E11 + the session-event metrics
> (`sp-yjw`) decide the seed set. Seed apps ship at the **reviewed** trust tier.

---

## 1. The MVP agent (concrete)

- **One existing ACP coding-style agent** ships in the MVP per-agent base image (E1 §2). All four
  launch apps target it. A coding-style agent natively does **file read/write/grep/edit +
  tool-calling** over a working dir — exactly the shape the wiki + coaches need (`/data` = its cwd).
- **Spike (build-time):** select + validate the concrete agent (candidates: Zed's agent,
  Gemini-CLI, a Claude-Code-style agent via ACP). Acceptance: speaks ACP (`initialize`,
  `session/new` w/ cwd, `session/prompt`, `session/update`); accepts a **configurable
  OpenAI-compatible model endpoint** (→ the sidecar, E2); imports skills via its native process.
- **Common toolset** baked into the base (E1 §2): shell/file ops, `git` (for E3 semantic commits),
  full-text search (`qmd`-class). All four apps use only the common toolset (no app-specific
  native tools → keeps them inside the MVP image-model limit, E1 §2).

---

## 2. zork — vertical-slice smoke test

- **Purpose:** the thinnest end-to-end proof of spawn→chat→persist. A text-adventure the agent
  runs conversationally.
- **State:** **minimal data repo** with a **save file** (e.g. `save.json` / `transcript.md` at
  root). Teardown persists (E3 per-turn / idle autosave); resume reloads → real save/continue.
- **Build-order note:** this pulls **E3 into the zork slice**. The smoke test MAY run **E1+E2+E6
  storage-less first** (game ends on teardown), then add the save file once E3 lands. Zork *as
  shipped* depends on E3.
- **Manifest sketch:**
  ```yaml
  id: spawnery/zork
  agents: { support: any, requiresAcp: [prompt] }
  tools: []                       # narrative-only; common toolset suffices
  persona: ./persona.md           # "you are the zork game engine + narrator"
  model: { requires: { toolUse: false, minContextTokens: 16000 }, recommendedDefault: deepseek-v4-flash }
  storage: { required: true, schema: ./storage-schema.md, seed: ./seed/ }  # seed = new-game save
  visibility: open
  ```

---

## 3. llm-wiki — flagship / wedge

- **Purpose:** the viral personal-knowledge-base pattern. Agent grows + cross-links a Markdown
  wiki in the user's repo.
- **Data shape (`/data` root):** Markdown pages + links; `README.md` as the landing/index page.
  The agent reads/greps/edits files and **commits semantically** (E3 §5).
- **Retrieval (MVP):** **file navigation + full-text search** (common toolset); embeddings/vector
  index deferred (system design §11).
- **Skills:** page creation/linking conventions, restructuring, summarization, "grow from this
  conversation" capture.
- **Manifest sketch:**
  ```yaml
  id: spawnery/llm-wiki
  agents: { support: any, requiresAcp: [prompt, tools] }
  tools: [qmd]
  persona: ./persona.md
  skills: [ ./skills/*.md ]
  model: { requires: { toolUse: true, minContextTokens: 32000 }, recommendedDefault: deepseek-v4-flash }
  storage: { required: true, schema: ./storage-schema.md, seed: ./seed/ }  # seed = empty wiki + README
  visibility: open
  ```

---

## 4. habit / goal coach

- **Purpose:** ongoing accountability — track goals, log check-ins, reflect on progress across
  sessions.
- **Data shape:** `goals.md`, `checkins/<date>.md`, `progress.md` at root (human-readable; the
  user can browse their own coaching record in their repo — on-brand).
- **Cross-session memory = files in `/data`**, reloaded on each (cold-start) session — the same
  mechanism as the wiki; no special memory subsystem needed.
- **Skills:** goal-setting framework, check-in prompts, streak/progress summarization,
  encouragement tone.
- **Manifest:** `model.requires.toolUse: true`; storage required; `seed/` scaffolds an empty
  goals/checkins structure.

---

## 5. system-design interview coach

- **Purpose:** mock system-design interviews + feedback; track performance over time.
- **Data shape:** `sessions/<date>-<topic>.md` (transcripts + scored feedback), `progress.md`
  (skill areas, recurring gaps). Reloaded each session for continuity.
- **Skills:** interview rubric, question bank / prompting, structured feedback format, follow-up
  drilling, progression across difficulty.
- **Manifest:** `model.requires` likely a **larger context window** (long interview transcripts) +
  `toolUse: true`; storage required.

---

## 6. Cross-cutting notes

- **All four are "files in `/data`" apps** — they differ only in persona, skills, and schema. This
  is the intended proof: the platform expresses a game, a knowledge base, and two coaches with
  **one agent + one storage model + manifest differences only**.
- **Interaction-pattern risk → explicit pre-launch gate (roast `sp-pgw`):** a coding-style agent is
  tuned to "do a file task," not long conversational/coaching turns. The habit/goal coach is the
  *worst* fit (pure tone/relationship) and the one most likely to define the emotional brand; the
  interview coach is the best fit (it's a structured task). **Acceptance gate:** dogfood the habit
  coach with real (non-technical) users for ~2 weeks and score "felt like a coach vs. a robot doing
  file ops." **Be willing to drop the habit coach from launch** rather than ship an uncanny flagship
  coach. Do not treat persona-steering as assumed-working.
- **Free-model compatibility gate (roast `sp-iui`/F6):** the demo MVP runs **local DeepSeek only**
  (no model choice). Each app's `model.requires` **must be satisfiable by the free DeepSeek** — in
  particular the interview coach's "larger context window" must not silently filter the only
  available model out, or it's a bait-and-switch. **Verify all four apps run acceptably on the demo
  DeepSeek before launch.**
- **Repo hygiene:** each definition repo follows the E5 §1 layout (`spawneryapp.yml`, `persona.md`,
  `skills/`, `seed/`, `icon.png`, `README.md`) + semver tags.

---

## 7. Deferred (post-MVP)

App-specific native tools beyond the common set · embeddings/vector retrieval for the wiki ·
second agent to exercise real agent-choice · richer coach analytics/visualizations · more launch
apps beyond the initial four.

---

## Appendix — E7 decision log

| # | Decision | Choice |
|---|---|---|
| E7.1 | Launch agent | **One existing ACP coding-style agent** in the MVP base; concrete pick is a build-time **spike** w/ acceptance criteria |
| E7.2 | zork state | **Minimal data repo + save file** (real save/continue); pulls E3 into the zork slice (storage-less E1+E2+E6 smoke test allowed first) |
| E7.3 | wiki (asserted) | Markdown files in `/data`, file-nav + full-text retrieval (embeddings deferred) |
| E7.4 | coaches (asserted) | Cross-session memory = human-readable files in `/data`, reloaded per session; no special memory subsystem |
| E7.5 | uniformity (asserted) | All four = "files in `/data`"; differ only by persona/skills/schema; one agent + one storage model |
