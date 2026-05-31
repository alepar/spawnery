# Make / Just Dev-Stack Ecosystem Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Split build vs. run into two files — a `Makefile` that builds incremental artifacts and a `Justfile` of one-word phony actions — so the dev stack (spawnlet + web UI) and `spawnctl` run via `just` recipes.

**Architecture:** `Makefile` owns artifacts only: `bin/*` (pattern rule on Go sources), proto codegen (stamp), and three Docker image stamps under `.make/` (rebuild only when context changes). `Justfile` owns actions only: run spawnlet/web/spawnctl, a `dev` combiner (mprocs), tests, setup, reap. Run recipes call `make` for just-needed artifacts so nothing rebuilds when fresh.

**Tech Stack:** GNU Make, `just` 1.40, `mprocs`, the existing Go + Vite stack.

**Spec:** `docs/superpowers/specs/2026-05-30-make-just-dev-stack-design.md` (authoritative).

> **Note on testing:** this is build tooling — there is no unit test to write first. Each task's red→green gate is **mechanical verification**: the file parses (`just --list`, `make -n`), builds are incremental no-ops on a second run, and an end-to-end `just spawnlet stub` + `just spawnctl` round-trip returns `ECHO: …` with no API key.

---

## Conventions
- Branch: `feat/make-just-dev-stack`. Beads `sp` task in_progress at Task 1, close after Task 3. No TodoWrite.
- Commit per task; trailer: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`. No git remote — commit only.
- **Makefile recipe bodies require a literal TAB** for indented lines (the inline `target: ; cmd` form used for stamps/dirs avoids tabs, but `bin/%`, `.make/gen`, and the `test-e2e`-free Make targets that span lines still need tabs). The `Justfile` uses space indentation (just is whitespace-flexible but must be consistent within a recipe).
- If a beads auto-export stages `.beads/issues.jsonl` and blocks anything, `git checkout HEAD -- .beads/issues.jsonl` and continue.

## File Structure
```
Makefile        REWRITE  artifacts only: build/bin pattern, gen stamp, image stamps, clean (drops test*)
Justfile        NEW      actions: spawnlet/web/dev/spawnctl, test*, setup, reap, default
mprocs.yaml     NEW      two procs: spawnlet (goose) + web
.gitignore      MODIFY   add .make/
README.md       MODIFY   "Running the slice" → just recipes + setup/mprocs prereq note
```

---

## Task 1: Makefile (artifacts) + .gitignore

**Files:** Rewrite `Makefile`; modify `.gitignore`.

- [ ] **Step 1: Rewrite `Makefile`** with EXACTLY this content (indented recipe lines under `bin/%:` and `.make/gen:` are TABs):

```makefile
# Artifacts only — incremental builds. Actions live in the Justfile.
GO_SRCS := $(shell find . -name '*.go' -not -path './web/*')

.PHONY: build images gen clean

build: bin/spawnlet bin/spawnctl          # the host-run binaries

bin/%: $(GO_SRCS) | bin
	go build -o $@ ./cmd/$*

# Proto codegen — stamp keyed on .proto sources + buf config.
gen: .make/gen
.make/gen: $(shell find proto -name '*.proto') buf.gen.yaml buf.yaml | .make
	buf generate && touch $@

# Image stamps — rebuild an image only when its build context changes.
images: .make/img-sidecar .make/img-stubagent .make/img-goose
.make/img-sidecar:   deploy/sidecar/Dockerfile   $(GO_SRCS) | .make ; docker build -t spawnery/sidecar:dev   -f $< . && touch $@
.make/img-stubagent: deploy/stubagent/Dockerfile $(GO_SRCS) | .make ; docker build -t spawnery/stubagent:dev -f $< . && touch $@
.make/img-goose:     deploy/agent/Dockerfile               | .make ; docker build -t spawnery/goose:dev     -f $< . && touch $@

bin:    ; @mkdir -p bin
.make:  ; @mkdir -p .make
clean:  ; rm -rf bin .make
```

> Why: `bin/%` rebuilds a binary when any Go source changes; `| bin` / `| .make` are order-only dir prereqs (no rebuild on dir mtime). `gen`/`images`/`build` are phony aliases pointing at real artifacts/stamps, so the phony name always "runs" but the underlying file work is incremental. `$<` is the first prereq (the Dockerfile). `gen` is in `.PHONY` to avoid colliding with the existing `gen/` codegen directory.

- [ ] **Step 2: Add `.make/` to `.gitignore`** — append the line `/.make/` (bin/ and .spawns/ are already ignored; verify with `grep -nE '^/?bin|spawns' .gitignore`). If `bin` is NOT already ignored, also append `/bin/`.

- [ ] **Step 3: Dry-run the Makefile** (no execution):

Run: `make -n build images gen`
Expected: prints the `go build`, `docker build … && touch`, and `buf generate && touch` commands with no `*** missing separator` or parse error.

- [ ] **Step 4: Verify binaries build + are incremental**

Run:
```bash
make build && ls -1 bin/        # → bin/spawnctl  bin/spawnlet
make build                      # second run
```
Expected: first run runs `go build` twice (spawnlet, spawnctl); second run prints `make: Nothing to be done for 'build'.` (incremental no-op).

- [ ] **Step 5: Verify image stamps build + are incremental** (needs Docker)

Run:
```bash
make images && ls -1 .make/     # → gen? no; img-goose img-sidecar img-stubagent
make images                     # second run
```
Expected: first run does three `docker build`s (layers likely cached → fast) and creates `.make/img-*`; second run prints `Nothing to be done for 'images'.`

- [ ] **Step 6: Confirm clean tree** — `git status --short` shows only `Makefile` + `.gitignore` modified (bin/ and .make/ ignored). Then commit:
```bash
git add Makefile .gitignore
git commit -m "build: Makefile owns incremental artifacts (bin/*, gen, image stamps)

Drops the phony test* targets (moving to the Justfile); adds .make/ stamp
dir so image rebuilds are incremental.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Justfile (actions) + mprocs.yaml

**Files:** Create `Justfile`, `mprocs.yaml`.

- [ ] **Step 1: Create `Justfile`** with EXACTLY this content (space-indented recipe bodies; the `spawnlet` recipe is ONE backslash-continued shell line):

```just
set dotenv-load := true              # auto-load .env (OPENROUTER_API_KEY) if present

repo      := justfile_directory()
addr      := "127.0.0.1:9090"
free      := "openai/gpt-oss-120b:free"
data_root := repo / ".spawns"

# list recipes
default:
    @just --list

# --- run the dev stack ---------------------------------------------------

# spawnlet, foreground. agent = goose (default) | stub
spawnlet agent="goose": (_images agent)
    @bin=spawnery/{{ if agent == "stub" { "stubagent" } else { "goose" } }}:dev; \
    AGENT_IMAGE=$bin SIDECAR_IMAGE=spawnery/sidecar:dev \
    DATA_ROOT={{data_root}} SPAWNLET_ADDR={{addr}} \
    OPENROUTER_API_KEY="${OPENROUTER_API_KEY:-unused}" \
    {{repo}}/bin/spawnlet

# build only the artifacts the chosen agent needs
_images agent="goose":
    make bin/spawnlet .make/img-sidecar .make/img-{{ if agent == "stub" { "stubagent" } else { "goose" } }}

# web UI (vite, LAN-accessible)
web:
    cd web && npm run dev -- --host

# both, in mprocs panes (one Ctrl-C)
dev:
    mprocs

# one-shot spawnctl against the running spawnlet
spawnctl prompt="What is the secret word?" model=free:
    @make bin/spawnctl
    printf '%s\n' "{{prompt}}" | {{repo}}/bin/spawnctl -addr http://{{addr}} -app {{repo}}/examples/secret-app -model {{model}}

# --- tests (actions) -----------------------------------------------------

test:
    go test ./... -count=1

test-web:
    cd web && npm test

test-e2e:
    make images
    go test -tags e2e ./... -count=1 -v

test-web-e2e:
    cd web && npm run test:e2e

# --- housekeeping --------------------------------------------------------

# install dev tooling not in the repo (mprocs, playwright browser, web deps)
setup:
    command -v mprocs >/dev/null || cargo install mprocs
    cd web && npm install && npx playwright install chromium

# reap containers the dev stack leaked (sp-8hf)
reap:
    -docker rm -f $(docker ps -aq --filter ancestor=spawnery/goose:dev --filter ancestor=spawnery/stubagent:dev --filter ancestor=spawnery/sidecar:dev) 2>/dev/null
```

> Why: `set dotenv-load := true` loads `.env` (non-fatal if absent). `_images` is a private recipe (leading `_` hides it from `--list`); `spawnlet` depends on it with the same `agent` arg. The conditional `{{ if agent == "stub" {…} else {…} }}` selects the image. The `spawnlet` body is one backslash-continued line so the `bin=…;` shell var survives into the exec. `reap`'s leading `-` ignores the error when no containers match.

- [ ] **Step 2: Create `mprocs.yaml`** with EXACTLY:
```yaml
procs:
  spawnlet: { shell: "just spawnlet" }   # goose by default
  web:      { shell: "just web" }
```

- [ ] **Step 3: Verify the Justfile parses + recipe list**

Run: `just --list`
Expected: lists `default`, `spawnlet`, `web`, `dev`, `spawnctl`, `test`, `test-web`, `test-e2e`, `test-web-e2e`, `setup`, `reap` — and does NOT list `_images` (private). No parse error.

- [ ] **Step 4: Verify the dependency graph resolves** (no execution of the agent)

Run: `just --dry-run spawnlet stub 2>&1 | head`
Expected: prints the `make bin/spawnlet .make/img-sidecar .make/img-stubagent` line then the `AGENT_IMAGE=spawnery/stubagent:dev … bin/spawnlet` line. No "unknown recipe"/parse error.

- [ ] **Step 5: End-to-end stub round-trip** (the real gate — Docker, no key)

Run:
```bash
cd /home/debian/AleCode/spawnery
pkill -f 'bin/spawnlet' 2>/dev/null; sleep 1          # clear any stale listener
just spawnlet stub &                                   # builds stub+sidecar, starts spawnlet :9090
for i in $(seq 1 60); do (exec 3<>/dev/tcp/127.0.0.1/9090) 2>/dev/null && { exec 3>&-; break; }; sleep 0.5; done
just spawnctl 'say hi'                                  # EXPECT: ECHO: say hi
pkill -f 'bin/spawnlet'; just reap                     # cleanup spawnlet + leaked containers
```
Expected: the `just spawnctl 'say hi'` output contains `ECHO: say hi`. This proves: `just spawnlet` builds the right artifacts via `make`, starts the spawnlet from the repo root, and `just spawnctl` drives it through a real spawn — all with no API key. If it hangs at the port poll, the spawnlet didn't start (read its log in the background output); if `ECHO:` is missing, debug for real — do NOT relax the check.

- [ ] **Step 6: Confirm clean tree + commit**
```bash
git status --short        # only Justfile + mprocs.yaml untracked
git add Justfile mprocs.yaml
git commit -m "build: Justfile of dev-stack actions (spawnlet/web/dev/spawnctl/test/setup/reap)

Run recipes delegate builds to make; dev runs spawnlet+web in mprocs.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: README + parity check

**Files:** Modify `README.md`.

- [ ] **Step 1: Update the "Running the slice" section** of `README.md`. Replace the hand-typed spawnlet/spawnctl command blocks with the `just` recipes. The new section should read (adapt surrounding prose to match the file's existing voice):

````markdown
## Running the slice

One-time setup (installs `just` recipes' extra tooling — `mprocs`, the Playwright
browser, web deps):

```bash
just setup        # or: cargo install mprocs && (cd web && npm install)
```

Put your OpenRouter key in a git-ignored `.env` at the repo root for the live path:

```
OPENROUTER_API_KEY=sk-or-...
```

### Dev stack

```bash
just dev          # spawnlet (goose) + web UI in mprocs panes, one Ctrl-C
# or run them separately:
just spawnlet     # goose (real LLM);  `just spawnlet stub` for the deterministic echo
just web          # vite --host on :5173
```

### Drive it from the CLI

```bash
just spawnctl 'What is the secret word?'      # against the running spawnlet, free model
```

### Tests

```bash
just test          # Go unit (hermetic)
just test-web      # web unit (vitest)
just test-e2e      # Go e2e (Docker pods + live OpenRouter; needs the key)
just test-web-e2e  # browser e2e (Playwright vs stub)
```

Leaked per-spawn containers (`sp-8hf`): `just reap`.
````

Keep the existing "Build the images" / architecture prose; only the run/test commands change. If the README still references `make test`, `make test-unit`, or `make test-e2e` elsewhere, update them to the `just` equivalents.

- [ ] **Step 2: Grep for stale references**

Run: `grep -rnE 'make (test|test-unit|test-e2e)' README.md docs/ 2>/dev/null`
Expected: no hits in README (planning docs under docs/ may legitimately mention the old commands historically — only fix README and any current "how to run" docs, not historical specs).

- [ ] **Step 3: Parity check — the moved test targets still work via just**

Run:
```bash
just test 2>&1 | tail -3
just test-web 2>&1 | tail -3
```
Expected: `just test` → Go suite passes (`ok …`); `just test-web` → vitest `2 passed`. (These replace the old `make test` / `npm test` entry points with identical behavior.)

- [ ] **Step 4: Commit**
```bash
git add README.md
git commit -m "docs: README runs the slice via just recipes + setup/mprocs prereq

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage:** §1 boundary (Make=artifacts, Just=actions) → Tasks 1,2; §2 boundary table (bins/gen/images in Make; run/test/setup/reap in Just) → Tasks 1,2; §3 Makefile (pattern rule, gen stamp, image stamps, clean) → Task 1; §4 Justfile (all recipes + mprocs.yaml) → Task 2; §5 behavior (dev=mprocs, spawnlet foreground from repo root, web --host, spawnctl free model, dotenv-load) → Task 2; §6 error handling (mprocs-missing→setup, no .env→stub works, reap for leaks) → Tasks 2,3; §7 verification (just --list, make -n, incremental no-op, stub ECHO round-trip) → Tasks 1,2; §8 migration (move make test*→just, README, .gitignore .make/) → Tasks 1,3. **No gaps.**

**Placeholder scan:** none — every file has complete content; every verification step has an exact command + expected output. The only "adapt to existing voice" note (README Step 1) still ships the full replacement block.

**Type/name consistency:** image tags `spawnery/{sidecar,stubagent,goose}:dev` and stamp names `.make/img-{sidecar,stubagent,goose}` are consistent between the Makefile (Task 1) and the Justfile's `_images`/conditionals (Task 2). `addr` = `127.0.0.1:9090` used by both `SPAWNLET_ADDR` and spawnctl's `-addr http://…`. `free` = `openai/gpt-oss-120b:free` matches the web app's model. Recipe names in the README (Task 3) match the Justfile (Task 2). `.gitignore` `.make/` (Task 1) matches the Makefile's stamp dir.

---

## Beads
One standalone task: `Make/Just dev-stack ecosystem` (dev-ergonomics; not under an existing epic). Mark in_progress at Task 1; close after Task 3 passes.
