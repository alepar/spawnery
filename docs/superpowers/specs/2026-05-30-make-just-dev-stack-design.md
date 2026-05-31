# Make / Just Dev-Stack Ecosystem — Design

**Status:** Approved in brainstorming — pending written-spec review
**Date:** 2026-05-30
**Context:** Developer ergonomics. Today the repo has a single `Makefile` mixing artifact builds
(`build`, `gen`, `images`) with phony actions (`test`, `test-unit`, `test-e2e`). Running the dev
stack (spawnlet + web UI) and driving `spawnctl` against it is hand-typed multi-flag commands
(see the manual e2e walkthrough). This splits build vs. run cleanly and gives one-word recipes.

---

## 1. Goal & boundary

Two tools, one responsibility each:

- **`Makefile` — artifacts only (incremental).** Every target produces a file and rebuilds only when
  its inputs change: the Go binaries, proto codegen, and Docker image stamps.
- **`Justfile` — actions only (phony).** Every recipe *does* something: run spawnlet / web / spawnctl,
  the `dev` combiner, tests, setup, cleanup. Run recipes **delegate builds to `make`** so artifacts are
  always current without ever rebuilding what's fresh.

**In scope:** the two files + a committed `mprocs.yaml`; documenting `mprocs` as a dev prerequisite.
**Out of scope:** CI wiring (CI can call the same `just`/`make` targets later), production build/serve,
container orchestration beyond the existing per-spawn Docker pods.

---

## 2. Make-vs-Just boundary

| Concern | Lives in | Why |
|---|---|---|
| `bin/spawnlet`, `bin/spawnctl` (+ any `cmd/<name>` via pattern rule) | **Make** | file artifact, incremental on Go sources |
| Proto codegen (`buf generate`) | **Make** | artifact (`gen/`), stamp keyed on `.proto` + buf config |
| Docker images (sidecar / stubagent / goose) | **Make** | artifact; **stamp files** make rebuilds incremental |
| run spawnlet / web / spawnctl, `dev` combiner | **Just** | phony action |
| `test`, `test-web`, `test-e2e`, `test-web-e2e` | **Just** | phony action (moved out of the Makefile) |
| `setup`, `reap`, `default` (list) | **Just** | phony action |

Just recipes that need a binary or image call `make <artifact>` first; `make` no-ops when current.

---

## 3. `Makefile` (artifacts)

```makefile
GO_SRCS := $(shell find . -name '*.go' -not -path './web/*')

.PHONY: build images clean
build: bin/spawnlet bin/spawnctl          # the host-run binaries

bin/%: $(GO_SRCS)                          # pattern rule for any cmd/<name>
	go build -o $@ ./cmd/$*

# Proto codegen — stamp keyed on the .proto sources + buf config.
gen: .make/gen
.make/gen: $(shell find proto -name '*.proto') buf.gen.yaml buf.yaml | .make
	buf generate && touch $@

# Image stamps — rebuild an image only when its build context changes.
images: .make/img-sidecar .make/img-stubagent .make/img-goose
.make/img-sidecar:   deploy/sidecar/Dockerfile   $(GO_SRCS) | .make ; docker build -t spawnery/sidecar:dev   -f $< . && touch $@
.make/img-stubagent: deploy/stubagent/Dockerfile $(GO_SRCS) | .make ; docker build -t spawnery/stubagent:dev -f $< . && touch $@
.make/img-goose:     deploy/agent/Dockerfile               | .make ; docker build -t spawnery/goose:dev     -f $< . && touch $@

.make: ; @mkdir -p .make
clean: ; rm -rf bin .make
```

Notes:
- `bin/%` depends on **all** Go sources (coarse but correct: any change rebuilds the affected binaries;
  cross-package deps make finer tracking brittle). `build` pre-declares the two host-run binaries;
  the pattern rule still builds `bin/sidecar`/`bin/stubagent` on demand if ever needed.
- The `sidecar`/`stubagent` image stamps depend on `$(GO_SRCS)` because their Dockerfiles compile our
  code; the `goose` image depends only on its Dockerfile (upstream binary, no local Go).
- Confirmed paths: `proto/spawn/v1/spawn.proto`, `buf.gen.yaml` + `buf.yaml` at repo root, codegen to
  `gen/`. Dockerfiles: `deploy/sidecar/Dockerfile`, `deploy/stubagent/Dockerfile`, `deploy/agent/Dockerfile`.
- `.make/` is git-ignored (build metadata).

---

## 4. `Justfile` (actions)

```just
set dotenv-load                      # auto-load .env (OPENROUTER_API_KEY) if present

repo      := justfile_directory()
addr      := "127.0.0.1:9090"
free      := "openai/gpt-oss-120b:free"
data_root := repo / ".spawns"

default: ; @just --list

# --- run the dev stack ---------------------------------------------------

# spawnlet, foreground. agent = goose (default) | stub
spawnlet agent="goose": (_images agent)
    @bin=spawnery/{{ if agent == "stub" { "stubagent" } else { "goose" } }}:dev; \
     AGENT_IMAGE=$bin SIDECAR_IMAGE=spawnery/sidecar:dev \
     DATA_ROOT={{data_root}} SPAWNLET_ADDR={{addr}} \
     OPENROUTER_API_KEY="${OPENROUTER_API_KEY:-unused}" \
     {{repo}}/bin/spawnlet

_images agent="goose":
    make bin/spawnlet .make/img-sidecar .make/img-{{ if agent == "stub" { "stubagent" } else { "goose" } }}

# web UI (vite, LAN-accessible)
web: ; cd web && npm run dev -- --host

# both, in mprocs panes (one Ctrl-C)
dev: ; mprocs

# one-shot spawnctl against the running spawnlet
spawnctl prompt="What is the secret word?" model=free:
    @make bin/spawnctl
    printf '%s\n' "{{prompt}}" | {{repo}}/bin/spawnctl -addr http://{{addr}} -app {{repo}}/examples/secret-app -model {{model}}

# --- tests (actions) -----------------------------------------------------
test:          ; go test ./... -count=1
test-web:      ; cd web && npm test
test-e2e:      ; make images && go test -tags e2e ./... -count=1 -v
test-web-e2e:  ; cd web && npm run test:e2e

# --- housekeeping --------------------------------------------------------
# install dev tooling not in the repo (mprocs, playwright browser, web deps)
setup:
    command -v mprocs >/dev/null || cargo install mprocs
    cd web && npm install && npx playwright install chromium

# reap containers the dev stack leaked (sp-8hf)
reap:
    -docker rm -f $(docker ps -aq --filter ancestor=spawnery/goose:dev --filter ancestor=spawnery/stubagent:dev --filter ancestor=spawnery/sidecar:dev) 2>/dev/null
```

`mprocs.yaml` (committed):
```yaml
procs:
  spawnlet: { shell: "just spawnlet" }   # goose by default
  web:      { shell: "just web" }
```

---

## 5. Behavior / data flow

- **`just dev`** → `mprocs` reads `mprocs.yaml`, launches `just spawnlet` (goose) + `just web` in two
  labeled panes (scrollback, per-pane restart, single Ctrl-C). Each pane's recipe `make`s its artifacts
  first, so the goose image stamp makes the build a near-instant no-op once built.
- **`just spawnlet [agent]`** → builds the binary + the chosen agent image (+ sidecar) via `make`, then
  execs the spawnlet in the **foreground from the repo root** (so the relative `examples/secret-app`
  the web app sends resolves), listening on `127.0.0.1:9090` (the Vite proxy target).
- **`just web`** → `vite --host` on `:5173`, LAN-accessible; proxies `/ws` + `/spawn.v1.*` to the spawnlet.
- **`just spawnctl ['prompt'] [model]`** → builds `bin/spawnctl`, pipes the prompt to it against the
  running spawnlet, `secret-app`, free model by default (spawnctl's own default is a paid Claude model).
- **`set dotenv-load`** loads `OPENROUTER_API_KEY` from `.env` for the goose path; the stub path falls
  back to `unused`, needing no key.

---

## 6. Error handling / edge cases

- **`mprocs` not installed** → `just dev` fails with `mprocs: command not found`; `just setup` installs it.
  Documented as a prerequisite in the README. (`cargo install mprocs`, or a prebuilt binary.)
- **No `.env`** → `set dotenv-load` is non-fatal when the file is absent (just `:= true` default loads
  only if present); the goose path then runs with `OPENROUTER_API_KEY=unused` and inference fails loudly
  at the agent — expected, since goose needs a key. The stub path works regardless.
- **Stale `:9090` listener** (a previous spawnlet still bound) → the new spawnlet fails to bind and exits
  loudly; the user kills the old one (or `mprocs` shows the crashed pane to restart).
- **Container leak on exit** (`sp-8hf`): neither `just dev`'s Ctrl-C nor the foreground recipes tear down
  the per-spawn Docker containers. `just reap` removes stragglers by ancestor image.
- **`make` coarse rebuilds**: editing any Go file rebuilds the host binaries and the
  sidecar/stubagent image stamps on next run — acceptable for a dev loop; the goose image is unaffected.

---

## 7. Testing / verification

This is build tooling, so verification is mechanical:
- `just --list` parses the Justfile (all recipes enumerated) and `make -n build images gen` dry-runs the
  Makefile without errors.
- `make build` produces `bin/spawnlet` + `bin/spawnctl`; a second `make build` is a no-op (incremental).
- `make images` builds the three stamps; a second `make images` is a no-op.
- `just spawnlet stub` boots the deterministic stack; `just spawnctl 'say hi'` returns `ECHO: say hi`
  end-to-end (no key) — proves the run + delegation path.
- `just test` + `just test-web` stay green (parity with the old `make test` / `npm test`).
- A `just dev` smoke (goose) with the real `.env` shows both panes healthy and the web secret-word flow
  — the same manual e2e, now one command.

---

## 8. Migration / housekeeping

- The Makefile's `test`, `test-unit`, `test-e2e` targets **move to the Justfile** (as `test`, `test-e2e`);
  `images`/`build`/`gen` stay in Make (now stamp-based). Update any docs/CI references from
  `make test*` → `just test*`.
- README: replace the hand-typed run commands in "Running the slice" with the `just` recipes; add a
  one-line `just setup` + `mprocs` prerequisite note.
- `.gitignore`: add `.make/` (build stamps); `bin/` and `.spawns/` are already ignored.

---

## Appendix — decision log

| # | Decision | Choice |
|---|---|---|
| M.1 | Run model | **Separate recipes + a `just dev` combiner** (`just spawnlet`, `just web`, `just dev`) |
| M.2 | `dev` combiner | **mprocs** (labeled panes, per-proc restart, one Ctrl-C); `mprocs.yaml` committed |
| M.3 | Default agent image | **goose** (real inference); `just spawnlet stub` overrides |
| M.4 | Make/Just split | **Make = artifacts** (bin/*, gen, image stamps); **Just = actions** (run/dev/test/setup/reap) |
| M.5 | Build delegation | Just run recipes call `make` for just-needed artifacts; incremental no-op when fresh |
| M.6 | spawnctl model | pin **free** `openai/gpt-oss-120b:free` (spawnctl's own default is paid Claude) |
