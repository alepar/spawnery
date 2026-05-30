# Per-Mount Data Backends Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the single rw `/data` mount with N **named** data mounts inside the read-only `/app` tree (`/app/<path>`, cwd=`/app`), each seeded from an app seed dir and realized by a per-mount `StorageBackend`; implement the **scratch** backend (seed-then-nuke) and wire it through the manager + teardown.

**Architecture:** A new `internal/manifest` package parses `spawneryapp.yml` `storage.mounts`. A new `internal/storage` package defines `Backend{Prepare,Finalize}` + a `Scratch` impl. The `Manager` parses the manifest, calls `backend.Prepare` per mount → binds each host dir rw at `/app/<path>` (with `/app` ro), and on `Stop` calls `backend.Finalize` per mount (scratch nukes). The agent's cwd becomes `/app`, so Goose reads `/app/AGENTS.md` in place (the entrypoint copy is removed).

**Tech Stack:** Go 1.25, `gopkg.in/yaml.v3` (new dep), existing ConnectRPC + Docker SDK + ACP.

**Spec:** `docs/superpowers/specs/2026-05-29-data-mounts-design.md` (authoritative).

---

## Conventions
- Branch: `feat/per-mount-mounts`. Beads prefix `sp`; mark the milestone in_progress when starting, close when its tests pass. No TodoWrite.
- TDD: failing test first, see red, implement minimally, see green, commit. Co-Authored-By trailer on every commit: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- **Unit suite** (`go test ./...`) must stay green + Docker/key-free. **E2e suite** (`go test -tags e2e ./...`) covers the Docker/live tests and fails loudly (no skips). Run e2e with the key: `set -a; . ./.env; set +a`.
- No git remote — commit only.

## File Structure
```
internal/manifest/manifest.go        NEW  parse spawneryapp.yml storage.mounts
internal/manifest/manifest_test.go   NEW
internal/storage/storage.go          NEW  Backend interface + Scratch backend + copyDirFiles
internal/storage/storage_test.go     NEW
internal/spawnlet/store.go           MOD  Spawn.DataDir -> Spawn.MountDirs []string
internal/spawnlet/manager.go         MOD  parse manifest; per-mount Prepare->bind /app/<path>; Stop Finalizes; drop copySeed
internal/spawnlet/manager_test.go    MOD  new mount assertions + a Stop-finalizes test
internal/spawnlet/server.go          MOD  CreateSpawn calls m.Create(ctx,id,appPath,model) (drop dataPath)
internal/spawnlet/server_test.go     MOD  temp app dir gets a valid spawneryapp.yml
cmd/spawnctl/main.go                 MOD  NewSession("/data") -> ("/app")
deploy/agent/Dockerfile              MOD  WORKDIR /data -> /app
deploy/agent/entrypoint.sh           MOD  remove the AGENTS.md copy line
examples/secret-app/spawneryapp.yml  MOD  storage.mounts: [{name:main,path:data,seed:seed}]
examples/secret-app/AGENTS.md        MOD  instruction -> read data/README.md
internal/spawnlet/e2e_test.go        MOD  NewSession("/app")
internal/spawnlet/e2e_goose_test.go  MOD  NewSession("/app")
```

---

## Task 1: `internal/manifest` — parse `spawneryapp.yml` storage.mounts

**Files:** Create `internal/manifest/manifest.go`, `internal/manifest/manifest_test.go`.

- [ ] **Step 1: Add the YAML dep**

Run: `go get gopkg.in/yaml.v3@v3.0.1 && go mod tidy`
Expected: `gopkg.in/yaml.v3` in `go.mod`.

- [ ] **Step 2: Write the failing test** — `internal/manifest/manifest_test.go`:
```go
package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseMounts(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "spawneryapp.yml"), []byte(`
apiVersion: spawnery/v1
kind: App
id: spawnery/secret
storage:
  mounts:
    - name: main
      path: data
      seed: seed
`), 0o644)

	m, err := Parse(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Storage.Mounts) != 1 {
		t.Fatalf("want 1 mount, got %d", len(m.Storage.Mounts))
	}
	got := m.Storage.Mounts[0]
	if got.Name != "main" || got.Path != "data" || got.Seed != "seed" {
		t.Fatalf("mount mismatch: %+v", got)
	}
}

func TestParseNoStorage(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "spawneryapp.yml"), []byte("id: spawnery/zork\n"), 0o644)
	m, err := Parse(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Storage.Mounts) != 0 {
		t.Fatalf("want 0 mounts, got %d", len(m.Storage.Mounts))
	}
}
```

- [ ] **Step 3: Run red** — `go test ./internal/manifest/ -v` → FAIL (undefined: Parse).

- [ ] **Step 4: Implement** — `internal/manifest/manifest.go`:
```go
// Package manifest parses an App's spawneryapp.yml.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Mount is one named data mount the app declares.
type Mount struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"` // relative to /app
	Seed string `yaml:"seed"` // relative to /app
}

type Storage struct {
	Mounts []Mount `yaml:"mounts"`
}

type Manifest struct {
	ID      string  `yaml:"id"`
	Storage Storage `yaml:"storage"`
}

// Parse reads <appPath>/spawneryapp.yml.
func Parse(appPath string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(appPath, "spawneryapp.yml"))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}
```

- [ ] **Step 5: Run green** — `go test ./internal/manifest/ -v` → PASS.
- [ ] **Step 6: Commit**
```bash
git add internal/manifest go.mod go.sum
git commit -m "feat(manifest): parse spawneryapp.yml storage.mounts

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: `internal/storage` — Backend interface + scratch

**Files:** Create `internal/storage/storage.go`, `internal/storage/storage_test.go`.

- [ ] **Step 1: Write the failing test** — `internal/storage/storage_test.go`:
```go
package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestScratchPrepareSeedsAndFinalizeNukes(t *testing.T) {
	root := t.TempDir()
	seed := t.TempDir()
	os.WriteFile(filepath.Join(seed, "README.md"), []byte("secret"), 0o644)

	s := NewScratch(root)
	hostDir, err := s.Prepare(context.Background(), "spawn1", "main", seed)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// seeded file present in the prepared dir
	b, err := os.ReadFile(filepath.Join(hostDir, "README.md"))
	if err != nil || string(b) != "secret" {
		t.Fatalf("seed not copied: %q err=%v", b, err)
	}
	// finalize removes the dir
	if err := s.Finalize(context.Background(), hostDir); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if _, err := os.Stat(hostDir); !os.IsNotExist(err) {
		t.Fatalf("expected hostDir removed, stat err=%v", err)
	}
}

func TestScratchPrepareMissingSeedIsEmpty(t *testing.T) {
	s := NewScratch(t.TempDir())
	hostDir, err := s.Prepare(context.Background(), "spawn1", "main", "/no/such/seed")
	if err != nil {
		t.Fatalf("prepare with missing seed should succeed (empty mount): %v", err)
	}
	entries, _ := os.ReadDir(hostDir)
	if len(entries) != 0 {
		t.Fatalf("want empty mount, got %d entries", len(entries))
	}
}
```

- [ ] **Step 2: Run red** — `go test ./internal/storage/ -v` → FAIL (undefined).

- [ ] **Step 3: Implement** — `internal/storage/storage.go`:
```go
// Package storage realizes a spawn's data mounts via pluggable backends.
package storage

import (
	"context"
	"os"
	"path/filepath"
)

// Backend prepares and finalizes the host directory backing one data mount.
type Backend interface {
	// Prepare returns a host dir to bind read-write at the mount path.
	// seedDir is the absolute host path of the app's seed dir (may be missing).
	Prepare(ctx context.Context, spawnID, mountName, seedDir string) (hostDir string, err error)
	// Finalize runs at teardown.
	Finalize(ctx context.Context, hostDir string) error
}

// Scratch is an ephemeral backend: seed a fresh dir on Prepare, nuke it on Finalize.
type Scratch struct{ Root string }

func NewScratch(root string) *Scratch { return &Scratch{Root: root} }

func (s *Scratch) Prepare(_ context.Context, spawnID, mountName, seedDir string) (string, error) {
	hostDir := filepath.Join(s.Root, spawnID, mountName)
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		return "", err
	}
	if err := copyDirFiles(seedDir, hostDir); err != nil {
		return "", err
	}
	return hostDir, nil
}

func (s *Scratch) Finalize(_ context.Context, hostDir string) error {
	return os.RemoveAll(hostDir)
}

// copyDirFiles copies top-level regular files from srcDir into dstDir.
// A missing srcDir yields an empty mount (no error).
func copyDirFiles(srcDir, dstDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(srcDir, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dstDir, e.Name()), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run green** — `go test ./internal/storage/ -v` → PASS.
- [ ] **Step 5: Commit**
```bash
git add internal/storage
git commit -m "feat(storage): Backend interface + scratch (seed-then-nuke)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Manager + Store rewrite (named mounts inside /app, finalize on stop)

**Files:** Modify `internal/spawnlet/store.go`, `internal/spawnlet/manager.go`, `internal/spawnlet/manager_test.go`.

- [ ] **Step 1: Change the Spawn struct** — in `internal/spawnlet/store.go`, replace the `Spawn` struct:
```go
type Spawn struct {
	ID        string
	SidecarID string
	AgentID   string
	MountDirs []string // host dirs backing this spawn's mounts (for Finalize)
	Status    string
}
```
(Leave `Store`/`Put`/`Get`/`Delete` unchanged.)

- [ ] **Step 2: Rewrite the manager test** — replace `internal/spawnlet/manager_test.go`:
```go
package spawnlet

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"spawnery/internal/runtime"
)

func writeApp(t *testing.T) string {
	t.Helper()
	app := t.TempDir()
	os.WriteFile(filepath.Join(app, "spawneryapp.yml"), []byte(`
id: spawnery/secret
storage:
  mounts:
    - name: main
      path: data
      seed: seed
`), 0o644)
	os.MkdirAll(filepath.Join(app, "seed"), 0o755)
	os.WriteFile(filepath.Join(app, "seed", "README.md"), []byte("QUOKKA-4417"), 0o644)
	return app
}

func TestCreateMountsAppRoAndNamedDataRw(t *testing.T) {
	f := runtime.NewFake()
	m := NewManager(f, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	app := writeApp(t)

	sp, err := m.Create(context.Background(), "test-1", app, "model-x")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(f.Started) != 2 {
		t.Fatalf("want 2 containers, got %d", len(f.Started))
	}
	agent := f.Started[1]
	if agent.NetnsOf != sp.SidecarID {
		t.Fatalf("agent should join sidecar netns")
	}
	if !hasMountRO(agent.Mounts, "/app") {
		t.Fatalf("/app should be ro: %+v", agent.Mounts)
	}
	if !hasMountRW(agent.Mounts, "/app/data") {
		t.Fatalf("/app/data should be rw: %+v", agent.Mounts)
	}
	// the rw mount's host dir was seeded
	if len(sp.MountDirs) != 1 {
		t.Fatalf("want 1 mount dir, got %d", len(sp.MountDirs))
	}
	b, err := os.ReadFile(filepath.Join(sp.MountDirs[0], "README.md"))
	if err != nil || string(b) != "QUOKKA-4417" {
		t.Fatalf("mount not seeded: %q err=%v", b, err)
	}
}

func TestStopFinalizesMounts(t *testing.T) {
	f := runtime.NewFake()
	m := NewManager(f, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	sp, err := m.Create(context.Background(), "test-2", writeApp(t), "model-x")
	if err != nil {
		t.Fatal(err)
	}
	dir := sp.MountDirs[0]
	if err := m.Stop(context.Background(), "test-2"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("scratch mount should be nuked on stop, stat err=%v", err)
	}
}

func hasMountRO(ms []runtime.Mount, cp string) bool {
	for _, m := range ms {
		if m.ContainerPath == cp && m.ReadOnly {
			return true
		}
	}
	return false
}
func hasMountRW(ms []runtime.Mount, cp string) bool {
	for _, m := range ms {
		if m.ContainerPath == cp && !m.ReadOnly {
			return true
		}
	}
	return false
}
```

- [ ] **Step 3: Run red** — `go test ./internal/spawnlet/ -run 'TestCreateMounts|TestStopFinalizes' -v` → FAIL (Create signature/behavior mismatch).

- [ ] **Step 4: Rewrite the manager** — replace `internal/spawnlet/manager.go`:
```go
package spawnlet

import (
	"context"
	"fmt"
	"path/filepath"

	"spawnery/internal/manifest"
	"spawnery/internal/runtime"
	"spawnery/internal/storage"
)

type ManagerConfig struct {
	AgentImage, SidecarImage, OpenRouterKey, DataRoot string
	SidecarPort                                       int // default 8080
}

type Manager struct {
	rt      runtime.ContainerRuntime
	cfg     ManagerConfig
	store   *Store
	backend storage.Backend
}

func NewManager(rt runtime.ContainerRuntime, cfg ManagerConfig) *Manager {
	if cfg.SidecarPort == 0 {
		cfg.SidecarPort = 8080
	}
	return &Manager{rt: rt, cfg: cfg, store: NewStore(), backend: storage.NewScratch(cfg.DataRoot)}
}

func (m *Manager) Store() *Store { return m.store }

func (m *Manager) Create(ctx context.Context, id, appPath, model string) (*Spawn, error) {
	mf, err := manifest.Parse(appPath)
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}

	// /app is read-only; each declared mount is a rw overlay at /app/<path>,
	// backed (slice: scratch) by a host dir seeded from /app/<seed>.
	mounts := []runtime.Mount{{HostPath: appPath, ContainerPath: "/app", ReadOnly: true}}
	var mountDirs []string
	finalizeAll := func() {
		for _, d := range mountDirs {
			_ = m.backend.Finalize(ctx, d)
		}
	}
	for _, mt := range mf.Storage.Mounts {
		seedDir := filepath.Join(appPath, mt.Seed)
		hostDir, err := m.backend.Prepare(ctx, id, mt.Name, seedDir)
		if err != nil {
			finalizeAll()
			return nil, fmt.Errorf("prepare mount %q: %w", mt.Name, err)
		}
		mountDirs = append(mountDirs, hostDir)
		mounts = append(mounts, runtime.Mount{HostPath: hostDir, ContainerPath: "/app/" + mt.Path})
	}

	addr := fmt.Sprintf("127.0.0.1:%d", m.cfg.SidecarPort)
	sidecarID, err := m.rt.StartContainer(ctx, runtime.ContainerSpec{
		Image: m.cfg.SidecarImage,
		Env: []string{
			"OPENROUTER_API_KEY=" + m.cfg.OpenRouterKey,
			"SIDECAR_ADDR=" + addr,
		},
	})
	if err != nil {
		finalizeAll()
		return nil, fmt.Errorf("sidecar: %w", err)
	}

	agentID, err := m.rt.StartContainer(ctx, runtime.ContainerSpec{
		Image:   m.cfg.AgentImage,
		NetnsOf: sidecarID,
		Env: []string{
			"OPENAI_BASE_URL=http://" + addr + "/v1",
			"SPAWN_MODEL=" + model,
		},
		Mounts:      mounts,
		AttachStdio: true,
	})
	if err != nil {
		_ = m.rt.StopContainer(ctx, sidecarID)
		finalizeAll()
		return nil, fmt.Errorf("agent: %w", err)
	}

	sp := &Spawn{ID: id, SidecarID: sidecarID, AgentID: agentID, MountDirs: mountDirs, Status: "ready"}
	m.store.Put(sp)
	return sp, nil
}

func (m *Manager) Stop(ctx context.Context, id string) error {
	sp, ok := m.store.Get(id)
	if !ok {
		return fmt.Errorf("unknown spawn %s", id)
	}
	_ = m.rt.StopContainer(ctx, sp.AgentID)
	_ = m.rt.StopContainer(ctx, sp.SidecarID)
	for _, d := range sp.MountDirs {
		_ = m.backend.Finalize(ctx, d)
	}
	m.store.Delete(id)
	return nil
}
```
(The old `copySeed` function is deleted.)

- [ ] **Step 5: Run green** — `go test ./internal/spawnlet/ -run 'TestCreateMounts|TestStopFinalizes|TestStore|TestRelay' -v` → PASS.
- [ ] **Step 6: Commit**
```bash
git add internal/spawnlet/store.go internal/spawnlet/manager.go internal/spawnlet/manager_test.go
git commit -m "feat(spawnlet): named per-mount backends inside /app; finalize on stop

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: Server — drop `dataPath`, fix server_test

**Files:** Modify `internal/spawnlet/server.go`, `internal/spawnlet/server_test.go`.

- [ ] **Step 1: Update server_test** — in `internal/spawnlet/server_test.go`, the temp app dir must now contain a valid `spawneryapp.yml`. Replace the app-dir setup so the test writes one:
```go
	app := t.TempDir()
	os.WriteFile(app+"/spawneryapp.yml", []byte("id: t/a\nstorage:\n  mounts: []\n"), 0o644)
```
(keep the rest of `TestServerCreateSpawn`; ensure `os` is imported).

- [ ] **Step 2: Run red** — `go test ./internal/spawnlet/ -run TestServerCreateSpawn -v`
Expected: FAIL (server still calls `m.Create(ctx, id, AppPath, DataPath, Model)` — wrong arity).

- [ ] **Step 3: Update server.go** — in `internal/spawnlet/server.go`, the `CreateSpawn` method, change the `m.Create` call to drop the data-path arg:
```go
	if _, err := s.m.Create(ctx, id, req.Msg.AppPath, req.Msg.Model); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
```
(`req.Msg.DataPath` is now ignored; the proto field stays but is unused in the slice.)

- [ ] **Step 4: Run green** — `go test ./internal/spawnlet/ -v` → all PASS. Also `go build ./...`.
- [ ] **Step 5: Commit**
```bash
git add internal/spawnlet/server.go internal/spawnlet/server_test.go
git commit -m "refactor(server): CreateSpawn drops unused dataPath (manifest drives mounts)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: cwd `/app` — client + image WORKDIR + entrypoint

**Files:** Modify `cmd/spawnctl/main.go`, `deploy/agent/Dockerfile`, `deploy/agent/entrypoint.sh`.

- [ ] **Step 1: spawnctl cwd** — in `cmd/spawnctl/main.go`, change the session cwd:
```go
	if err := c.NewSession("/app"); err != nil {
		log.Fatal(err)
	}
```

- [ ] **Step 2: Goose image WORKDIR** — in `deploy/agent/Dockerfile`, change `WORKDIR /data` to `WORKDIR /app`.

- [ ] **Step 3: Remove the AGENTS.md copy** — in `deploy/agent/entrypoint.sh`, delete the line that copies `/app/AGENTS.md` into `/data` (and any reference to `/data`). The relevant block (`[ -f /app/AGENTS.md ] && cp /app/AGENTS.md /data/AGENTS.md ...`) is removed entirely — Goose reads `/app/AGENTS.md` in place because cwd is `/app`.

- [ ] **Step 4: Build check** — `go build ./...` (the Dockerfile/entrypoint are rebuilt in Task 7). Expected: OK.
- [ ] **Step 5: Commit**
```bash
git add cmd/spawnctl/main.go deploy/agent/Dockerfile deploy/agent/entrypoint.sh
git commit -m "feat(agent): cwd=/app; read AGENTS.md in place (drop the /data copy)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: secret-app under the new model

**Files:** Modify `examples/secret-app/spawneryapp.yml`, `examples/secret-app/AGENTS.md`. (`seed/README.md` keeps `The secret word is: QUOKKA-4417`.)

- [ ] **Step 1: Manifest** — `examples/secret-app/spawneryapp.yml`:
```yaml
apiVersion: spawnery/v1
kind: App
id: spawnery/secret
title: Secret
agents: { support: any, requiresAcp: [prompt] }
model: { recommendedDefault: anthropic/claude-3.5-sonnet }
storage:
  mounts:
    - name: main
      path: data
      seed: seed
visibility: open
```

- [ ] **Step 2: AGENTS.md** — `examples/secret-app/AGENTS.md`:
```markdown
When the user asks for the secret word, read the file `data/README.md` in your
working directory, find the secret word it contains, and reply with exactly that
word and nothing else.
```

- [ ] **Step 3: Commit**
```bash
git add examples/secret-app
git commit -m "feat(secret-app): declare a named scratch mount; read data/README.md

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: e2e — cwd `/app`, rebuild images, run (stub + Goose); verify ro cwd

**Files:** Modify `internal/spawnlet/e2e_test.go`, `internal/spawnlet/e2e_goose_test.go`.

- [ ] **Step 1: Stub e2e cwd** — in `internal/spawnlet/e2e_test.go`, change `c.NewSession("/data")` to `c.NewSession("/app")`. (The stub echoes; the assertion `ECHO: hello` is unaffected.)

- [ ] **Step 2: Goose e2e cwd** — in `internal/spawnlet/e2e_goose_test.go`, change `c.NewSession("/data")` to `c.NewSession("/app")`. (Prompt + `QUOKKA-4417` assertion unchanged.)

- [ ] **Step 3: Rebuild images** (entrypoint/Dockerfile/secret-app changed):
```bash
docker build -t spawnery/stubagent:dev -f deploy/stubagent/Dockerfile .
docker build -t spawnery/sidecar:dev   -f deploy/sidecar/Dockerfile .
docker build -t spawnery/goose:dev     -f deploy/agent/Dockerfile .
```

- [ ] **Step 4: Unit suite (Docker/key-free)** — `go test ./... -count=1`
Expected: all unit packages green (manifest, storage, spawnlet, acp, runtime, sidecar, stubagent), no Docker.

- [ ] **Step 5: E2e suite (live)** — `set -a; . ./.env; set +a` then
`go test -tags e2e ./... -count=1 -v -timeout 300s`
Expected: `TestEndToEndStub` (now via a scratch `/app/data` mount, cwd `/app`) and `TestEndToEndGooseSecret` (Goose reads `/app/AGENTS.md` in place → reads `/app/data/README.md` → recites `QUOKKA-4417`) both PASS, plus `TestDockerRunAndAttachEcho`.
**Empirical check:** confirm Goose runs fine with a read-only cwd `/app` (it should only read `data/README.md` and write nothing to cwd). If Goose errors trying to write into `/app`, inspect `docker logs` of the goose container; if it needs a writable cwd for its own state, set `HOME`/`GOOSE_*` to a writable path in the entrypoint (do NOT make `/app` writable) and note it.

- [ ] **Step 6: Commit**
```bash
git add internal/spawnlet/e2e_test.go internal/spawnlet/e2e_goose_test.go
git commit -m "test(e2e): cwd=/app; secret recited from /app/data via scratch mount

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage** (`2026-05-29-data-mounts-design.md`):
- §2 mount model (/app ro, cwd=/app, rw overlays) → Tasks 3 (mounts) + 5 (cwd) + 7 (verify).
- §3 manifest `storage.mounts {name,path,seed}` → Task 1 + Task 6.
- §4 spawn.yml backend binding → slice defaults to scratch (Task 3); per-name binding is full-design (not built — design-only, acceptable per §8).
- §5 `StorageBackend{Prepare,Finalize}` + scratch → Task 2.
- §6 lifecycle (Prepare per mount, bind, Finalize on stop) → Task 3.
- §7 teardown (StopSpawn finalizes mounts; e2e uses StopSpawn) → Task 3 + existing e2e StopSpawn defers.
- §8 slice scope (scratch only, drop /data + copySeed + AGENTS.md copy) → Tasks 3 + 5.
- secret-app + e2e → Tasks 6 + 7.
**No gaps** (spawn.yml name-binding is explicitly design-only for the slice).

**Placeholder scan:** none — every step has exact code/commands.

**Type consistency:** `manifest.Mount{Name,Path,Seed}`, `manifest.Parse(appPath)`, `storage.Backend{Prepare(ctx,spawnID,mountName,seedDir)->string; Finalize(ctx,hostDir)}`, `storage.NewScratch(root)`, `Spawn{...,MountDirs []string}`, `Manager.Create(ctx,id,appPath,model)`, `Manager.Stop` — used consistently across Tasks 1–7. `runtime.Mount{HostPath,ContainerPath,ReadOnly}` unchanged.

---

## Beads
One milestone: `Per-mount data backends (named mounts inside /app, scratch)` under the spawnlet work. Mark in_progress at Task 1; close after Task 7's suites are green.
