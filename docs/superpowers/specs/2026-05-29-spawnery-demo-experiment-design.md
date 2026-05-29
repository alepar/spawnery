# Spawnery — Demo Experiment Design

**Bead:** `sp-54f` · **Date:** 2026-05-29
**Depends on:** [Demo MVP Scope](2026-05-28-spawnery-demo-mvp-scope.md),
[E11 app ideas](2026-05-29-spawnery-e11-app-ideas.md)
**Purpose:** make the demo an experiment whose result — either result — changes what we do next.

---

## 0. Two goals, kept separate

| Goal | Instrument | This doc? |
|---|---|---|
| **Technical proof** — can we build spawn→chat→persist + open marketplace? | the build itself | no (engineering milestone) |
| **Market validation** — does the architecture serve a real, retained need? | this experiment | **yes** |

The danger is reading technical-proof usage as market validation. This doc governs the latter; do
**not** quote raw launch usage as evidence of demand.

---

## 1. Hypothesis

**Primary (portfolio-level):** *the architecture (own-your-data, spawn a persistent single-purpose
agent, marketplace) supports at least one personal-agent app that a **representative** user **returns
to in week 2**, reactively (no proactive nudges).*

Not "does the wiki retain" — the **architecture across a portfolio** is the bet (E11). Each seed app
is a sub-hypothesis; the metrics tell us which idea the architecture serves best.

**Secondary (marketplace, H3/H4):** *third-party creators publish apps that other users spawn.*

---

## 2. Metrics — content-free, session-event only (`sp-yjw`)

**Hard rule:** every metric below derives from the **session-event stream** (hourly Parquet). We
**never read `/data`** (no page counts, vocab counts, etc.). Allowed = session occurrence, timing,
duration, and a runtime-emitted **turn-count** counter (a metadata count of exchanges, *not* content).
*(Add `turn_count` + `session_end` to the `sp-yjw` event schema.)*

| Metric | Definition (session-events only) |
|---|---|
| **Activated** | user whose **first** session has **≥4 turns OR ≥3 min** (got past hello) |
| **W1 / W2 / W4 return** | activated user who **starts ≥1 session** in days 1–7 / 8–14 / 22–28 |
| **★ Headline: W2 return** | **% of activated users who return in week 2** — the retention number |
| **Depth** | sessions/user/week; median session turns (trend over weeks) |
| **Per-app** | all of the above, segmented by app |
| **Marketplace** | # distinct creators publishing a `scanned`+ app; # third-party (non-seed) apps; **% of spawns that are third-party**; # third-party apps with ≥1 non-creator spawn |

> **A clean side effect of deferring proactivity:** there are **no nudges**, so *every* return is
> **unprompted** — W2 return is therefore a *pure* "this became theirs" signal, not a
> notification-gaming artifact.

**Anti-metrics — explicitly ignore as vanity:** signups, waitlist size, launch-day/HN/Twitter
traffic, total page views. They measure reach, not value.

---

## 3. Representative audience (not the viral-tweet crowd)

Retention from curious early-adopters who arrived via a viral tweet **over-reads** and misleads.
Recruit and **bucket separately**:

- **Cohort A — knowledge workers** who already take notes/research (Wiki+Research).
- **Cohort B — active language learners** (Language Partner).
- **Cohort C — active job-seekers** (Interview Coach).
- **Cohort Z — inbound/early-adopter** (viral/HN). **Reported separately and down-weighted** — never
  the basis for a GO.

A GO decision requires clearing thresholds in the **target cohort (A/B/C)**, not Cohort Z.

---

## 4. The concierge pre-build test (do this BEFORE/parallel to the full build)

The cheapest truth serum, and it can run *now* — before the (sizable) platform build.

- **Question:** does a representative user retain to a personal-agent app when it's hand-held and
  *excellent* — i.e. with every confounder (weak model, latency, onboarding) removed?
- **Method:** recruit **10–20** Cohort-A/B users. For **2–3 weeks**, hand-run the top 1–2 candidate
  apps (**Wiki+Research** and **Language Partner**) with a **high-quality model + manual glue** (a
  shared chat; you manually maintain their "repo"/state between sessions).
- **Measure:** do they come back **unprompted** in week 2? Plus qualitative ("felt like mine? would
  keep using? would pay?").
- **Decision rule:** *if they won't return when it's hand-held and excellent, the polished B+Y version
  won't save it* → don't build that app as a flagship; pivot or pick another.
- **Cost:** ~zero engineering; days of human time. **Highest leverage item in the plan.**

Filed as `sp-cnc`.

---

## 5. Pre-committed decision table

Thresholds below are **proposed starting numbers — the team must ratify them before launch** (the
point is to commit *in advance*, so the result can decide). Headline = **W2 return, target cohort**.

| App (loop type) | PIVOT/cut `<` | ITERATE | GO `≥` |
|---|---|---|---|
| **Language Partner** (daily) | 15% | 15–35% | **35%** |
| **Wiki+Research** (reactive/external) | 10% | 10–25% | **25%** |
| **Interview Coach** (episodic) | — *(retention is the wrong metric)* | judge by **repeat-use within a job-search window** + qual. | n/a |
| **zork** | toy — no retention bar | | |

**Portfolio go/no-go (set a date, e.g. 6–8 weeks post-launch or post-concierge):**
- **GO to the next platform phase** (BYOK / self-host / **proactivity**) **iff ≥1 app clears its GO
  bar in a target cohort.**
- **Marketplace GO:** ≥ N third-party apps published by distinct creators **and** third-party spawn
  share ≥ M% (propose N=10, M=20%).

**The crucial branch (don't mis-read a reactive-only result):**
> If retention is **weak across the board but the apps clearly need proactivity** (CRM-style
> follow-ups, resurfacing, nudges), the decision is **not "kill"** — it's **"build the deferred
> proactive-agent capability (`sp-37x`) and re-test,"** since the demo deliberately shipped without
> the portfolio's retention engine. Reserve "kill the wedge" for when *hand-held* retention (the
> concierge test) is also weak.

---

## 6. Cadence & ownership

- **Weekly** content-free retention readout per app per cohort (offline over the Parquet).
- **Decision review** at the pre-set date against §5. Outcome ∈ {GO next phase, build proactivity +
  re-test, iterate an app, pivot the wedge}.
- Owner: founder/PM. Thresholds ratified **before** launch.

---

## 7. Open items (team to close before launch)
- Ratify the §5 thresholds + the portfolio date.
- Confirm `turn_count` + `session_end` added to the `sp-yjw` event schema.
- Stand up the concierge test (`sp-cnc`) now — it doesn't wait on the build.

---

## Appendix — decision log
| # | Decision | Choice |
|---|---|---|
| X.1 | Separate goals | technical-proof ≠ market-validation; don't quote usage as demand |
| X.2 | Hypothesis | portfolio-level (≥1 app retains a representative user in W2, reactively); marketplace H3/H4 secondary |
| X.3 | Metrics | content-free session events only; headline = **W2 return, target cohort**; vanity metrics named + ignored |
| X.4 | Audience | target cohorts A/B/C; early-adopter Cohort Z down-weighted, never the basis for GO |
| X.5 | Concierge test | 10–20 users, 2–3 wks, hand-held, high-quality model — run **before** the build (`sp-cnc`) |
| X.6 | Decision table | pre-committed (proposed) W2 thresholds per app; ratify before launch |
| X.7 | Proactivity branch | weak reactive retention + apps-need-proactivity → build `sp-37x` + re-test, *not* kill |
