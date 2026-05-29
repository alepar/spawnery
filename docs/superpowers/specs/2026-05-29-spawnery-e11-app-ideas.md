# Spawnery E11 — App Idea Exploration (Demo Seed Lineup)

**Bead:** `sp-4jg` · **Gates:** E7 (`sp-2tb`) seed lineup
**Status:** Complete (interview) — pending user review
**Date:** 2026-05-29

The architecture is the product; apps are interchangeable demonstrations of it. This epic picks the
**demo seed lineup** by asking *where the architecture is uniquely advantaged*, not *what's a cool
AI app*. The open marketplace grows everything beyond the seed set.

---

## 1. The lens — where this architecture is uniquely advantaged

Distinctive properties: an **owned, versioned, portable file repo** the agent maintains; a
**persistent single-purpose agent** whose memory *is* that repo; **bring-your-storage / your-model /
clean-exit**. Two selection criteria fall out:

1. **Privacy/ownership must genuinely matter.** "Your data + your model" is a *killer* pitch for
   **sensitive personal data** (private notes, finances, health, unpublished writing) — data you'd
   never put in a vendor cloud — and a mere nice-to-have for public-ish content.
2. **There must be a built-in return loop** that doesn't require the corpus to already be big —
   **external cadence** (the world feeds new input) or a **proactive agent** (it reaches out). This
   is the antidote to the wiki's PKM death-spiral (`sp-54f`).

**Anti-patterns (arch *not* advantaged):** real-time/collaborative/multi-writer data (git is
single-writer), one-shot tasks (nothing to own), huge binaries, anything a stateless chat handles.

---

## 2. The reusable test — standalone App vs. "wiki content"

Data is never the differentiator (it's all files; the wiki could hold it). Something earns
**standalone App** status only if, beyond general capture+retrieval, it has **at least one** of:

- a **structured schema** enabling queries freeform notes can't (e.g. "who's overdue a follow-up?");
- a **proactive / time-driven loop** (the agent *acts*, not just answers);
- **specialized skills/persona/tools** for the job.

Otherwise it's wiki content — fold it in. *(This test is why Personal CRM folded into the wiki — see §4.)*

---

## 3. Cross-cutting finding — "proactive agent" is the retention engine (DEFERRED)

Three of the strongest ideas (CRM follow-ups, wiki proactive cross-linking, language spaced-repetition)
all need the same missing capability: **the agent running and reaching the user when they didn't open
the app** — a per-spawn **scheduler** + **headless wake** + **notification channel**. It is
simultaneously several apps' reason-to-be *and* the portfolio's retention engine (a proactive nudge
*is* a return-loop driver). **Decision: DEFERRED for the demo** (spawns stay ephemeral +
user-initiated). Consequence: every seed app must work **reactively**, and CRM (which depended on it)
folds into the wiki. Proactivity is the **top retention lever to revisit post-demo** (`sp-prx`).

---

## 4. Candidate portfolio & decisions

| Idea | Decision | Why |
|---|---|---|
| **Wiki + Research companion** | **SEED** | general accretion/second-brain; absorbs notes/people/CRM; reactive-but-externally-driven loop |
| **Language-learning partner** | **SEED** | strongest (user-habit) daily loop; tutor/drill role suits the agent (dodges `sp-pgw`) |
| **Interview coach** | **SEED** | different *shape* (episodic, task-structured); best `sp-pgw` fit; demonstrates catalog breadth |
| **zork** | **SEED (toy)** | smoke test + fun; no retention expectation |
| Personal CRM | **folded → wiki** | differentiation rested on proactivity (§3), which is deferred → collapses to structured people-notes |
| Habit/goal coach | **dropped** | worst `sp-pgw` (uncanny) fit; its daily loop needed *proactive* nudges to be sticky |
| Finance / health journal | **not seeded** | finance = sharp privacy showcase (reactive-viable) → strong candidate for a fast-follow / creator app; health carries advice/liability risk |
| Writing / manuscript partner | **not seeded** | excellent arch-fit (git history!) but not chosen now → strong marketplace/fast-follow candidate |

---

## 5. The seed lineup (each works reactively, no proactivity)

### 5.1 Wiki + Research companion — *the general second-brain*
- **Job:** feed it articles / links / notes / info-about-people → it extracts, summarizes, and
  **cross-links** into a Markdown knowledge graph; answers questions over it.
- **`/data`:** Markdown pages + links + source captures + index.
- **Return loop:** reactive but **externally driven** — you return because *you encountered something
  new* and bring it in.
- **Retention hypothesis:** regular readers/researchers return to capture+connect; value compounds as
  the graph grows. **Weakest-loop of the set (PKM-adjacent) — the one to watch in the metrics.**
- **Privacy differentiation lives here** (private notes/people). Arch-fit ★★★.

### 5.2 Language-learning partner
- **Job:** tracks vocab + recurring mistakes + level; drills, converses, adapts; reactive ("you
  return, it picks what to drill").
- **`/data`:** vocab list, mistake log, level/progress, session transcripts.
- **Return loop:** **user-habit daily practice** (strongest in the set; user-driven, not agent-nudged).
- **Retention hypothesis:** daily-practice habit + visible owned progress = sticky.
- **Weak-model risk:** tutoring quality on DeepSeek-Flash is fine for common languages, weaker on
  nuance — verify per `sp-iui` before launch. Arch-fit ★★ (schema + tutor skills).

### 5.3 Interview coach (system-design)
- **Job:** mock interviews + structured scored feedback; track performance over time.
- **`/data`:** `sessions/<date>-<topic>.md` + `progress.md` (skill gaps).
- **Return loop:** **episodic** (you return when interviewing) — weakest cadence, high value-per-use.
- **Retention hypothesis:** a utility/acquisition play, not a daily-retention play. Arch-fit ★★★.

### 5.4 zork — vertical-slice smoke test + toy (per E7 §2). Save file in `/data`.

---

## 6. Implications

- **E7 seed lineup → these four** (replaces the old "habit + sysdesign coaches" framing). Seed apps
  ship at the **reviewed** trust tier. Validate the wiki's and language partner's retention; interview
  coach is episodic-by-design.
- **`sp-54f`:** retention is now a **per-app hypothesis**, read from content-free session events
  (`sp-yjw`): wiki = watch closely (weak loop), language = the daily-loop bet, interview = episodic
  utility. The metrics will show which idea the architecture serves best — which is the actual
  experiment now (architecture across a portfolio, not "does the wiki retain").
- **Proactivity (`sp-prx`)** is the named #1 post-demo retention lever.
- **Marketplace** grows the rest (finance journal, writing partner, CRM-with-proactivity, …) via
  third-party creators.

---

## Appendix — E11 decision log

| # | Decision | Choice |
|---|---|---|
| E11.1 | Selection lens | arch-unique-advantage: privacy/ownership-matters + built-in-return-loop; avoid anti-patterns |
| E11.2 | App-vs-content test | standalone iff structured-schema OR proactive-loop OR specialized-skills beyond capture+retrieval |
| E11.3 | Proactive agent | **DEFERRED** for demo (ephemeral/user-initiated); #1 post-demo retention lever (`sp-prx`) |
| E11.4 | CRM | **folded into wiki** (differentiation needed proactivity) |
| E11.5 | Wiki | **merged with research companion**; absorbs notes/people; the general second-brain seed |
| E11.6 | Seed lineup | **Wiki+Research · Language Partner · Interview Coach · zork** (reactive-only) |
| E11.7 | Dropped/deferred | habit coach dropped; finance journal + writing partner = fast-follow/creator candidates |
