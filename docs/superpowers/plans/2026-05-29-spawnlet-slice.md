# Spawnlet Vertical Slice — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a Go "spawnlet" node service that, on a `CreateSpawn` ConnectRPC call, brings up a shared-netns pod (agent container + inference-proxy sidecar with `/app` + `/data` mounts) and transparently relays ACP bytes between a client and the agent's container stdio — proven end-to-end with a stub ACP agent (deterministic) and then with real Goose + a live OpenRouter round-trip.

**Architecture:** One Go module. The **spawnlet** exposes ConnectRPC (`CreateSpawn`/`Session`/`StopSpawn`); a **ContainerRuntime** adapter (Docker Engine API) creates a sidecar (netns owner, holds the OpenRouter key) and an agent container that joins its netns and mounts `/app` ro + `/data` rw with stdio attached. The **Relay** copies bytes both ways between the `Session` stream and the agent's demuxed stdout/stdin — the spawnlet never parses ACP. The **client** CLI contains a minimal ACP client; ACP smarts live only in the client and the agent. A **stub ACP agent** test double lets the whole pipeline be built and CI-tested with no LLM/network; swapping in **Goose** is a base-image change.

**Tech Stack:** Go 1.22, ConnectRPC (`connectrpc.com/connect`) + Buf codegen, Docker Engine API (`github.com/docker/docker/client`, `pkg/stdcopy`), newline-delimited JSON-RPC 2.0 (ACP), OpenRouter (OpenAI-compatible).

**Spec:** `docs/superpowers/specs/2026-05-29-spawnlet-slice-design.md`

---

## Conventions

- **Module path:** `spawnery`. **Go:** 1.22.
- **TDD:** every task writes the failing test first, runs it red, implements minimally, runs it green, commits.
- **Commits:** Conventional Commits; end every commit body with the Co-Authored-By trailer used in this repo.
- **Beads:** this plan's milestones are tracked as `sp-*` issues (see "Beads tracking" at the end). Mark each in_progress when starting, close when its task's tests pass. Do **not** use TodoWrite.
- **Docker tests** require a running Docker daemon; they skip cleanly when `DOCKER_HOST`/socket is unavailable via the `requireDocker(t)` helper (Task 5).

---

## File Structure

```
go.mod                                  module spawnery
buf.yaml                                buf module config
buf.gen.yaml                            codegen config (protoc-gen-go + protoc-gen-connect-go)
Makefile                                gen / build / test targets
proto/spawn/v1/spawn.proto              SpawnService + messages
gen/spawn/v1/spawn.pb.go                generated (protobuf)        [codegen]
gen/spawn/v1/spawnv1connect/spawn.connect.go  generated (connect)  [codegen]

internal/acp/codec.go                   JSON-RPC 2.0 message + ndjson framing
internal/acp/codec_test.go
internal/acp/client.go                  minimal ACP client (initialize/session.new/session.prompt)
internal/acp/client_test.go

internal/sidecar/proxy.go               OpenAI-compatible reverse proxy -> OpenRouter
internal/sidecar/proxy_test.go
cmd/sidecar/main.go                     sidecar entrypoint (reads env, serves)

internal/stubagent/agent.go             deterministic ACP test double
internal/stubagent/agent_test.go
cmd/stubagent/main.go                   stub agent entrypoint

internal/runtime/runtime.go             ContainerRuntime interface + Spec types + fake
internal/runtime/docker.go              Docker Engine API implementation
internal/runtime/docker_test.go         integration (skips without Docker)

internal/spawnlet/store.go              in-mem SpawnStore
internal/spawnlet/store_test.go
internal/spawnlet/manager.go            lifecycle: create pod / stop / lookup
internal/spawnlet/manager_test.go       (uses runtime fake)
internal/spawnlet/relay.go              Session<->stdio byte relay
internal/spawnlet/relay_test.go
internal/spawnlet/server.go             ConnectRPC SpawnService impl
internal/spawnlet/server_test.go
cmd/spawnlet/main.go                    spawnlet entrypoint (serve)

cmd/spawnctl/main.go                    slice client CLI (Connect + acp.Client loop)

deploy/sidecar/Dockerfile
deploy/stubagent/Dockerfile
deploy/agent/Dockerfile                 Goose base image (+ entry wrapper)

examples/hello-app/spawneryapp.yml
examples/hello-app/persona.md
examples/hello-app/seed/README.md
```

---

## Task 0: Project scaffold + proto codegen

**Files:** Create `go.mod`, `buf.yaml`, `buf.gen.yaml`, `Makefile`, `proto/spawn/v1/spawn.proto`; generate `gen/...`.

- [ ] **Step 1: Init the Go module and tooling**

Run:
```bash
cd /home/debian/AleCode/spawnery
go mod init spawnery
go install github.com/bufbuild/buf/cmd/buf@v1.45.0
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2
go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.16.2
```
Expected: `go.mod` created; three binaries in `$(go env GOPATH)/bin` (ensure it's on `PATH`).

- [ ] **Step 2: Write the proto**

Create `proto/spawn/v1/spawn.proto`:
```proto
syntax = "proto3";
package spawn.v1;
option go_package = "spawnery/gen/spawn/v1;spawnv1";

service SpawnService {
  rpc CreateSpawn(CreateSpawnRequest) returns (CreateSpawnResponse);
  rpc Session(stream Frame) returns (stream Frame);
  rpc StopSpawn(StopSpawnRequest) returns (StopSpawnResponse);
}

message CreateSpawnRequest {
  string app_path = 1;
  string data_path = 2;   // empty -> spawnlet allocates a fresh dir
  string model = 3;       // OpenRouter model id
}
message CreateSpawnResponse { string spawn_id = 1; }

message Frame { string spawn_id = 1; bytes data = 2; }

message StopSpawnRequest { string spawn_id = 1; }
message StopSpawnResponse {}
```

- [ ] **Step 3: Codegen config**

Create `buf.yaml`:
```yaml
version: v2
modules:
  - path: proto
lint:
  use: [STANDARD]
breaking:
  use: [FILE]
```
Create `buf.gen.yaml`:
```yaml
version: v2
plugins:
  - local: protoc-gen-go
    out: gen
    opt: paths=source_relative
  - local: protoc-gen-connect-go
    out: gen
    opt: paths=source_relative
```
Create `Makefile`:
```makefile
.PHONY: gen build test
gen:
	buf generate
build:
	go build ./...
test:
	go test ./...
```

- [ ] **Step 4: Generate and verify it compiles**

Run:
```bash
buf generate
go mod tidy
go build ./...
```
Expected: `gen/spawn/v1/spawn.pb.go` and `gen/spawn/v1/spawnv1connect/spawn.connect.go` exist; `go build ./...` succeeds (no non-generated code yet).

- [ ] **Step 5: Add .gitignore for Go and commit**

Append to `.gitignore`:
```
/bin/
*.test
*.out
```
Commit:
```bash
git add go.mod go.sum buf.yaml buf.gen.yaml Makefile proto gen .gitignore
git commit -m "feat(scaffold): go module + ConnectRPC SpawnService codegen

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 1: ACP codec (JSON-RPC 2.0 + ndjson framing)

**Files:** Create `internal/acp/codec.go`, `internal/acp/codec_test.go`.

> ACP is JSON-RPC 2.0 over stdio. We frame messages as **newline-delimited JSON** (one message per line). The byte relay is framing-agnostic; only the client + agent parse frames. *(Confirm Goose's framing in Task 12; if it uses Content-Length headers, swap the framer here — the relay is unaffected.)*

- [ ] **Step 1: Write the failing test**

Create `internal/acp/codec_test.go`:
```go
package acp

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	out := Message{JSONRPC: "2.0", ID: intptr(1), Method: "initialize"}
	if err := WriteMessage(&buf, out); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte("\n")) {
		t.Fatalf("expected newline framing, got %q", buf.String())
	}
	r := NewReader(&buf)
	got, err := r.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Method != "initialize" || got.ID == nil || *got.ID != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func intptr(i int) *int { return &i }
```

- [ ] **Step 2: Run it red**

Run: `go test ./internal/acp/ -run TestRoundTrip -v`
Expected: FAIL (undefined: Message/WriteMessage/NewReader).

- [ ] **Step 3: Implement minimally**

Create `internal/acp/codec.go`:
```go
// Package acp implements the minimal slice of the Agent Client Protocol
// (JSON-RPC 2.0 over stdio, newline-delimited) that the slice needs.
package acp

import (
	"bufio"
	"encoding/json"
	"io"
)

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

func WriteMessage(w io.Writer, m Message) error {
	if m.JSONRPC == "" {
		m.JSONRPC = "2.0"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

type Reader struct{ sc *bufio.Scanner }

func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // allow large messages
	return &Reader{sc: sc}
}

func (r *Reader) ReadMessage() (Message, error) {
	if !r.sc.Scan() {
		if err := r.sc.Err(); err != nil {
			return Message{}, err
		}
		return Message{}, io.EOF
	}
	var m Message
	if err := json.Unmarshal(r.sc.Bytes(), &m); err != nil {
		return Message{}, err
	}
	return m, nil
}
```

- [ ] **Step 4: Run it green**

Run: `go test ./internal/acp/ -run TestRoundTrip -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/acp/codec.go internal/acp/codec_test.go
git commit -m "feat(acp): JSON-RPC 2.0 message + ndjson codec

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Stub ACP agent (deterministic test double)

**Files:** Create `internal/stubagent/agent.go`, `internal/stubagent/agent_test.go`, `cmd/stubagent/main.go`.

The stub speaks just enough ACP over an `io.Reader`/`io.Writer`: answers `initialize`, `session/new` (returns a session id), and for `session/prompt` emits one streamed `session/update` echoing the prompt (optionally prefixed with a model name it read from env) then a final result. It makes **no** network calls — deterministic CI. It is configured to optionally call an OpenAI endpoint is **out of scope** for the stub (that path is exercised by Goose).

- [ ] **Step 1: Write the failing test**

Create `internal/stubagent/agent_test.go`:
```go
package stubagent

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"spawnery/internal/acp"
)

func TestStubPromptEchoes(t *testing.T) {
	in := &bytes.Buffer{}
	acp.WriteMessage(in, acp.Message{ID: ip(1), Method: "initialize"})
	acp.WriteMessage(in, acp.Message{ID: ip(2), Method: "session/new",
		Params: json.RawMessage(`{"cwd":"/data"}`)})
	acp.WriteMessage(in, acp.Message{ID: ip(3), Method: "session/prompt",
		Params: json.RawMessage(`{"text":"hello"}`)})

	out := &bytes.Buffer{}
	if err := Run(io.NopCloser(in), out); err != nil && err != io.EOF {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(out.String(), "ECHO: hello") {
		t.Fatalf("expected echoed update, got: %s", out.String())
	}
}

func ip(i int) *int { return &i }
```

- [ ] **Step 2: Run it red**

Run: `go test ./internal/stubagent/ -v`
Expected: FAIL (undefined: Run).

- [ ] **Step 3: Implement minimally**

Create `internal/stubagent/agent.go`:
```go
// Package stubagent is a deterministic ACP test double for the spawnlet slice.
package stubagent

import (
	"encoding/json"
	"io"

	"spawnery/internal/acp"
)

// Run reads ACP messages from r and writes responses to w until EOF.
func Run(r io.Reader, w io.Writer) error {
	rd := acp.NewReader(r)
	for {
		msg, err := rd.ReadMessage()
		if err != nil {
			return err
		}
		switch msg.Method {
		case "initialize":
			reply(w, msg.ID, json.RawMessage(`{"protocolVersion":"slice-0"}`))
		case "session/new":
			reply(w, msg.ID, json.RawMessage(`{"sessionId":"stub-1"}`))
		case "session/prompt":
			var p struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			// streamed update notification (no id)
			upd, _ := json.Marshal(map[string]string{"chunk": "ECHO: " + p.Text})
			acp.WriteMessage(w, acp.Message{Method: "session/update", Params: upd})
			reply(w, msg.ID, json.RawMessage(`{"stopReason":"end_turn"}`))
		}
	}
}

func reply(w io.Writer, id *int, result json.RawMessage) {
	acp.WriteMessage(w, acp.Message{ID: id, Result: result})
}
```
Create `cmd/stubagent/main.go`:
```go
package main

import (
	"os"

	"spawnery/internal/stubagent"
)

func main() {
	if err := stubagent.Run(os.Stdin, os.Stdout); err != nil {
		os.Exit(0) // EOF on stdin close is normal teardown
	}
}
```

- [ ] **Step 4: Run it green**

Run: `go test ./internal/stubagent/ -v && go build ./cmd/stubagent`
Expected: PASS; build succeeds.

- [ ] **Step 5: Commit**

```bash
git add internal/stubagent cmd/stubagent
git commit -m "feat(stubagent): deterministic ACP test double

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Inference-proxy sidecar

**Files:** Create `internal/sidecar/proxy.go`, `internal/sidecar/proxy_test.go`, `cmd/sidecar/main.go`.

Reverse proxy: listens on a local port, forwards `/v1/*` to OpenRouter (`https://openrouter.ai/api/v1`), strips any inbound auth and injects `Authorization: Bearer <key>` from config. The agent never holds the key.

- [ ] **Step 1: Write the failing test**

Create `internal/sidecar/proxy_test.go`:
```go
package sidecar

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxyInjectsKeyAndRewritesUpstream(t *testing.T) {
	var gotAuth, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	h := NewHandler(upstream.URL, "secret-key")
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if gotAuth != "Bearer secret-key" {
		t.Fatalf("auth not injected: %q", gotAuth)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path not preserved: %q", gotPath)
	}
}
```

- [ ] **Step 2: Run it red**

Run: `go test ./internal/sidecar/ -v`
Expected: FAIL (undefined: NewHandler).

- [ ] **Step 3: Implement minimally**

Create `internal/sidecar/proxy.go`:
```go
// Package sidecar is the slice's minimal OpenAI-compatible inference proxy.
package sidecar

import (
	"net/http"
	"net/http/httputil"
	"net/url"
)

// NewHandler proxies requests to upstream, injecting the bearer key.
func NewHandler(upstream, key string) http.Handler {
	target, err := url.Parse(upstream)
	if err != nil {
		panic(err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	orig := rp.Director
	rp.Director = func(r *http.Request) {
		orig(r)
		r.Host = target.Host
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Del("X-Api-Key")
	}
	return rp
}
```
Create `cmd/sidecar/main.go`:
```go
package main

import (
	"log"
	"net/http"
	"os"

	"spawnery/internal/sidecar"
)

func main() {
	upstream := getenv("SIDECAR_UPSTREAM", "https://openrouter.ai/api")
	key := os.Getenv("OPENROUTER_API_KEY")
	addr := getenv("SIDECAR_ADDR", "127.0.0.1:8080")
	if key == "" {
		log.Fatal("OPENROUTER_API_KEY required")
	}
	log.Printf("sidecar listening on %s -> %s", addr, upstream)
	log.Fatal(http.ListenAndServe(addr, sidecar.NewHandler(upstream, key)))
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 4: Run it green**

Run: `go test ./internal/sidecar/ -v && go build ./cmd/sidecar`
Expected: PASS; build succeeds.

- [ ] **Step 5: Commit**

```bash
git add internal/sidecar cmd/sidecar
git commit -m "feat(sidecar): OpenAI-compatible proxy with key injection

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: ContainerRuntime interface + fake

**Files:** Create `internal/runtime/runtime.go` (interface, Spec types, `FakeRuntime`).

Defines the swappable orchestration boundary. The fake records calls so the manager (Task 7) is unit-testable without Docker.

- [ ] **Step 1: Write the failing test**

Create `internal/runtime/runtime_test.go`:
```go
package runtime

import (
	"context"
	"testing"
)

func TestFakeRecordsStartAndStop(t *testing.T) {
	f := NewFake()
	id, err := f.StartContainer(context.Background(), ContainerSpec{Image: "img"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Started[0].Image; got != "img" {
		t.Fatalf("image not recorded: %q", got)
	}
	if err := f.StopContainer(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !f.Stopped[id] {
		t.Fatalf("stop not recorded for %s", id)
	}
}
```

- [ ] **Step 2: Run it red** — `go test ./internal/runtime/ -v` → FAIL (undefined).

- [ ] **Step 3: Implement minimally**

Create `internal/runtime/runtime.go`:
```go
// Package runtime is the spawnlet's container-orchestration boundary.
package runtime

import (
	"context"
	"fmt"
	"io"
)

type Mount struct {
	HostPath, ContainerPath string
	ReadOnly                bool
}

type ContainerSpec struct {
	Image       string
	Env         []string
	Mounts      []Mount
	NetnsOf     string // if set, join this container's network namespace
	AttachStdio bool   // attach stdin+stdout (for the agent)
}

// AttachedStream is the agent's bidirectional stdio (demuxed stdout).
type AttachedStream struct {
	Stdin  io.WriteCloser
	Stdout io.Reader
	Close  func() error
}

type ContainerRuntime interface {
	StartContainer(ctx context.Context, s ContainerSpec) (id string, err error)
	Attach(ctx context.Context, id string) (*AttachedStream, error)
	StopContainer(ctx context.Context, id string) error
}

// FakeRuntime records calls for unit tests.
type FakeRuntime struct {
	Started []ContainerSpec
	Stopped map[string]bool
	n       int
}

func NewFake() *FakeRuntime { return &FakeRuntime{Stopped: map[string]bool{}} }

func (f *FakeRuntime) StartContainer(_ context.Context, s ContainerSpec) (string, error) {
	f.n++
	id := fmt.Sprintf("fake-%d", f.n)
	f.Started = append(f.Started, s)
	return id, nil
}
func (f *FakeRuntime) Attach(_ context.Context, id string) (*AttachedStream, error) {
	pr, pw := io.Pipe()
	return &AttachedStream{Stdin: pw, Stdout: pr, Close: func() error { return pw.Close() }}, nil
}
func (f *FakeRuntime) StopContainer(_ context.Context, id string) error {
	f.Stopped[id] = true
	return nil
}
```

- [ ] **Step 4: Run it green** — `go test ./internal/runtime/ -v` → PASS.

- [ ] **Step 5: Commit**
```bash
git add internal/runtime/runtime.go internal/runtime/runtime_test.go
git commit -m "feat(runtime): ContainerRuntime interface + fake

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: Docker ContainerRuntime implementation

**Files:** Create `internal/runtime/docker.go`, `internal/runtime/docker_test.go`.

Implements the interface against the Docker Engine API. Uses `--network container:<id>` for netns sharing and `stdcopy` to demux attached stdout. The integration test runs a tiny `alpine` container and skips when Docker is unavailable.

- [ ] **Step 1: Add the Docker SDK**

Run: `go get github.com/docker/docker@v27.3.1+incompatible && go mod tidy`

- [ ] **Step 2: Write the failing integration test**

Create `internal/runtime/docker_test.go`:
```go
package runtime

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func requireDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("SKIP_DOCKER") != "" {
		t.Skip("SKIP_DOCKER set")
	}
	r, err := NewDocker()
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Ping(ctx); err != nil {
		t.Skipf("docker not pingable: %v", err)
	}
}

func TestDockerRunAndAttachEcho(t *testing.T) {
	requireDocker(t)
	r, err := NewDocker()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, err := r.StartContainer(ctx, ContainerSpec{
		Image: "alpine:3", Cmd: []string{"sh", "-c", "read x; echo got:$x"},
		AttachStdio: true,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer r.StopContainer(ctx, id)

	att, err := r.Attach(ctx, id)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer att.Close()
	att.Stdin.Write([]byte("hello\n"))

	sc := bufio.NewScanner(att.Stdout)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "got:hello") {
			return
		}
	}
	t.Fatal("did not see echoed output")
}
```
> Add `Cmd []string` to `ContainerSpec` in `runtime.go` and a `Ping(ctx)` method to the interface + fake (fake returns nil) before running.

- [ ] **Step 3: Run it red** — `go test ./internal/runtime/ -run TestDockerRunAndAttachEcho -v` → FAIL (undefined: NewDocker) or skip if no Docker.

- [ ] **Step 4: Implement**

Create `internal/runtime/docker.go`:
```go
package runtime

import (
	"context"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

type Docker struct{ cli *client.Client }

func NewDocker() (*Docker, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Docker{cli: cli}, nil
}

func (d *Docker) Ping(ctx context.Context) error {
	_, err := d.cli.Ping(ctx)
	return err
}

func (d *Docker) StartContainer(ctx context.Context, s ContainerSpec) (string, error) {
	cfg := &container.Config{
		Image:       s.Image,
		Cmd:         s.Cmd,
		Env:         s.Env,
		OpenStdin:   s.AttachStdio,
		StdinOnce:   false,
		AttachStdin: s.AttachStdio,
		Tty:         false,
	}
	host := &container.HostConfig{}
	if s.NetnsOf != "" {
		host.NetworkMode = container.NetworkMode("container:" + s.NetnsOf)
	}
	for _, m := range s.Mounts {
		host.Binds = append(host.Binds, bind(m))
	}
	created, err := d.cli.ContainerCreate(ctx, cfg, host, nil, nil, "")
	if err != nil {
		return "", err
	}
	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return "", err
	}
	return created.ID, nil
}

func bind(m Mount) string {
	b := m.HostPath + ":" + m.ContainerPath
	if m.ReadOnly {
		b += ":ro"
	}
	return b
}

func (d *Docker) Attach(ctx context.Context, id string) (*AttachedStream, error) {
	hijack, err := d.cli.ContainerAttach(ctx, id, container.AttachOptions{
		Stream: true, Stdin: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		return nil, err
	}
	// Demux multiplexed stdout into a pipe (non-TTY attach is framed).
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, io.Discard, hijack.Reader)
		pw.CloseWithError(err)
	}()
	return &AttachedStream{
		Stdin:  hijack.Conn,
		Stdout: pr,
		Close:  func() error { hijack.Close(); return nil },
	}, nil
}

func (d *Docker) StopContainer(ctx context.Context, id string) error {
	to := 0
	_ = d.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &to})
	return d.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
}
```
> Pre-pull alpine once for the test: `docker pull alpine:3`. (Image-pull-on-demand is added in Task 7's manager via a `cli.ImagePull` call; for this integration test, ensure alpine is present.)

- [ ] **Step 5: Run it green** — `go test ./internal/runtime/ -v` (PASS or SKIP if no Docker). Commit:
```bash
git add internal/runtime
git commit -m "feat(runtime): Docker implementation (netns share + stdcopy attach)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: SpawnStore

**Files:** Create `internal/spawnlet/store.go`, `internal/spawnlet/store_test.go`.

- [ ] **Step 1: Failing test** — `internal/spawnlet/store_test.go`:
```go
package spawnlet

import "testing"

func TestStorePutGetDelete(t *testing.T) {
	s := NewStore()
	s.Put(&Spawn{ID: "a", SidecarID: "s", AgentID: "g"})
	got, ok := s.Get("a")
	if !ok || got.AgentID != "g" {
		t.Fatalf("get failed: %+v ok=%v", got, ok)
	}
	s.Delete("a")
	if _, ok := s.Get("a"); ok {
		t.Fatal("expected deleted")
	}
}
```

- [ ] **Step 2: Run red** — `go test ./internal/spawnlet/ -run TestStore -v` → FAIL.

- [ ] **Step 3: Implement** — `internal/spawnlet/store.go`:
```go
package spawnlet

import "sync"

type Spawn struct {
	ID        string
	SidecarID string
	AgentID   string
	DataDir   string
	Status    string
}

type Store struct {
	mu sync.Mutex
	m  map[string]*Spawn
}

func NewStore() *Store { return &Store{m: map[string]*Spawn{}} }

func (s *Store) Put(sp *Spawn) { s.mu.Lock(); s.m[sp.ID] = sp; s.mu.Unlock() }
func (s *Store) Get(id string) (*Spawn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.m[id]
	return sp, ok
}
func (s *Store) Delete(id string) { s.mu.Lock(); delete(s.m, id); s.mu.Unlock() }
```

- [ ] **Step 4: Run green** — PASS.
- [ ] **Step 5: Commit** —
```bash
git add internal/spawnlet/store.go internal/spawnlet/store_test.go
git commit -m "feat(spawnlet): in-mem SpawnStore

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: SpawnManager (pod lifecycle)

**Files:** Create `internal/spawnlet/manager.go`, `internal/spawnlet/manager_test.go`.

Orchestrates: resolve `/data` (+ seed copy), start sidecar (netns owner, key+model env), start agent joining the sidecar netns with `/app` ro + `/data` rw + stdio, record in store. Rolls back on partial failure. Uses the runtime fake in tests.

- [ ] **Step 1: Failing test** — `internal/spawnlet/manager_test.go`:
```go
package spawnlet

import (
	"context"
	"os"
	"testing"

	"spawnery/internal/runtime"
)

func TestCreateStartsSidecarThenAgentJoiningNetns(t *testing.T) {
	f := runtime.NewFake()
	dataRoot := t.TempDir()
	m := NewManager(f, ManagerConfig{
		AgentImage: "agent", SidecarImage: "sidecar",
		OpenRouterKey: "k", DataRoot: dataRoot,
	})
	app := t.TempDir()
	os.WriteFile(app+"/spawneryapp.yml", []byte("id: test/app\n"), 0o644)

	sp, err := m.Create(context.Background(), "test-1", app, "", "anthropic/claude-3.5-sonnet")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(f.Started) != 2 {
		t.Fatalf("want 2 containers, got %d", len(f.Started))
	}
	sidecar, agent := f.Started[0], f.Started[1]
	if agent.NetnsOf != sp.SidecarID {
		t.Fatalf("agent should join sidecar netns, got %q want %q", agent.NetnsOf, sp.SidecarID)
	}
	if !hasEnv(sidecar.Env, "OPENROUTER_API_KEY=k") {
		t.Fatalf("sidecar missing key env: %v", sidecar.Env)
	}
	if !hasMountRO(agent.Mounts, "/app") || !hasMountRW(agent.Mounts, "/data") {
		t.Fatalf("agent mounts wrong: %+v", agent.Mounts)
	}
}

func hasEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
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

- [ ] **Step 2: Run red** — `go test ./internal/spawnlet/ -run TestCreate -v` → FAIL.

- [ ] **Step 3: Implement** — `internal/spawnlet/manager.go`:
```go
package spawnlet

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"spawnery/internal/runtime"
)

type ManagerConfig struct {
	AgentImage, SidecarImage, OpenRouterKey, DataRoot string
	SidecarPort                                       int // default 8080
}

type Manager struct {
	rt    runtime.ContainerRuntime
	cfg   ManagerConfig
	store *Store
}

func NewManager(rt runtime.ContainerRuntime, cfg ManagerConfig) *Manager {
	if cfg.SidecarPort == 0 {
		cfg.SidecarPort = 8080
	}
	return &Manager{rt: rt, cfg: cfg, store: NewStore()}
}

func (m *Manager) Store() *Store { return m.store }

func (m *Manager) Create(ctx context.Context, id, appPath, dataPath, model string) (*Spawn, error) {
	dataDir := dataPath
	if dataDir == "" {
		dataDir = filepath.Join(m.cfg.DataRoot, id, "data")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	copySeed(appPath, dataDir) // best-effort scaffold

	addr := fmt.Sprintf("127.0.0.1:%d", m.cfg.SidecarPort)
	sidecarID, err := m.rt.StartContainer(ctx, runtime.ContainerSpec{
		Image: m.cfg.SidecarImage,
		Env: []string{
			"OPENROUTER_API_KEY=" + m.cfg.OpenRouterKey,
			"SIDECAR_ADDR=" + addr,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("sidecar: %w", err)
	}

	agentID, err := m.rt.StartContainer(ctx, runtime.ContainerSpec{
		Image:   m.cfg.AgentImage,
		NetnsOf: sidecarID,
		Env: []string{
			"OPENAI_BASE_URL=http://" + addr + "/v1",
			"SPAWN_MODEL=" + model,
		},
		Mounts: []runtime.Mount{
			{HostPath: appPath, ContainerPath: "/app", ReadOnly: true},
			{HostPath: dataDir, ContainerPath: "/data"},
		},
		AttachStdio: true,
	})
	if err != nil {
		_ = m.rt.StopContainer(ctx, sidecarID) // rollback
		return nil, fmt.Errorf("agent: %w", err)
	}

	sp := &Spawn{ID: id, SidecarID: sidecarID, AgentID: agentID, DataDir: dataDir, Status: "ready"}
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
	m.store.Delete(id)
	return nil
}

func copySeed(appPath, dataDir string) {
	seed := filepath.Join(appPath, "seed")
	entries, err := os.ReadDir(seed)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(seed, e.Name()))
		if err == nil {
			_ = os.WriteFile(filepath.Join(dataDir, e.Name()), b, 0o644)
		}
	}
}
```

- [ ] **Step 4: Run green** — PASS.
- [ ] **Step 5: Commit** —
```bash
git add internal/spawnlet/manager.go internal/spawnlet/manager_test.go
git commit -m "feat(spawnlet): SpawnManager pod lifecycle (sidecar+agent, rollback)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 8: Relay (Session stream ↔ agent stdio)

**Files:** Create `internal/spawnlet/relay.go`, `internal/spawnlet/relay_test.go`.

Bidirectional byte copy between two abstract endpoints. Defined over small `send`/`recv` funcs so it's testable without Connect and reusable by the server.

- [ ] **Step 1: Failing test** — `internal/spawnlet/relay_test.go`:
```go
package spawnlet

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

func TestRelayCopiesBothWays(t *testing.T) {
	// client<->spawnlet modeled as channels; agent as in-memory pipes.
	toAgent := make(chan []byte, 8)
	fromAgent := make(chan []byte, 8)

	agentIn, agentInW := io.Pipe()
	agentOut := &bytes.Buffer{}
	agentOut.WriteString("hi-from-agent")

	ep := StreamEndpoint{
		Recv: func() ([]byte, error) {
			b, ok := <-toAgent
			if !ok {
				return nil, io.EOF
			}
			return b, nil
		},
		Send: func(b []byte) error { fromAgent <- append([]byte{}, b...); return nil },
	}
	stdio := AgentIO{Stdin: agentInW, Stdout: io.MultiReader(bytes.NewReader([]byte("hi-from-agent")))}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go Relay(ctx, ep, stdio)

	toAgent <- []byte("hi-from-client")
	close(toAgent)

	got := <-fromAgent
	if string(got) != "hi-from-agent" {
		t.Fatalf("agent->client got %q", got)
	}
	// drain what client sent into the agent pipe
	buf := make([]byte, 64)
	n, _ := agentIn.Read(buf)
	if string(buf[:n]) != "hi-from-client" {
		t.Fatalf("client->agent got %q", buf[:n])
	}
}
```

- [ ] **Step 2: Run red** — FAIL (undefined).

- [ ] **Step 3: Implement** — `internal/spawnlet/relay.go`:
```go
package spawnlet

import (
	"context"
	"io"
)

// StreamEndpoint abstracts the client side (e.g. a Connect bidi stream).
type StreamEndpoint struct {
	Recv func() ([]byte, error) // from client
	Send func([]byte) error     // to client
}

// AgentIO is the agent container's stdio.
type AgentIO struct {
	Stdin  io.Writer
	Stdout io.Reader
}

// Relay copies bytes both ways until either side ends.
func Relay(ctx context.Context, ep StreamEndpoint, io_ AgentIO) {
	done := make(chan struct{}, 2)
	// client -> agent
	go func() {
		for {
			b, err := ep.Recv()
			if err != nil || len(b) == 0 {
				done <- struct{}{}
				return
			}
			if _, werr := io_.Stdin.Write(b); werr != nil {
				done <- struct{}{}
				return
			}
		}
	}()
	// agent -> client
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := io_.Stdout.Read(buf)
			if n > 0 {
				if serr := ep.Send(buf[:n]); serr != nil {
					done <- struct{}{}
					return
				}
			}
			if err != nil {
				done <- struct{}{}
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
	case <-done:
	}
}
```

- [ ] **Step 4: Run green** — PASS.
- [ ] **Step 5: Commit** —
```bash
git add internal/spawnlet/relay.go internal/spawnlet/relay_test.go
git commit -m "feat(spawnlet): transparent byte relay (stream<->stdio)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 9: ConnectRPC server

**Files:** Create `internal/spawnlet/server.go`, `internal/spawnlet/server_test.go`.

Implements `SpawnService`: `CreateSpawn` → manager.Create; `Session` → attach + Relay; `StopSpawn` → manager.Stop.

- [ ] **Step 1: Failing test** — `internal/spawnlet/server_test.go`:
```go
package spawnlet

import (
	"context"
	"os"
	"testing"

	"connectrpc.com/connect"
	spawnv1 "spawnery/gen/spawn/v1"
	"spawnery/internal/runtime"
)

func TestServerCreateSpawn(t *testing.T) {
	f := runtime.NewFake()
	m := NewManager(f, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	srv := NewServer(m)

	app := t.TempDir()
	os.WriteFile(app+"/spawneryapp.yml", []byte("id: t/a\n"), 0o644)

	resp, err := srv.CreateSpawn(context.Background(), connect.NewRequest(&spawnv1.CreateSpawnRequest{
		AppPath: app, Model: "anthropic/claude-3.5-sonnet",
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.Msg.SpawnId == "" {
		t.Fatal("empty spawn id")
	}
	if _, ok := m.Store().Get(resp.Msg.SpawnId); !ok {
		t.Fatal("spawn not stored")
	}
}
```

- [ ] **Step 2: Run red** — FAIL (undefined: NewServer).

- [ ] **Step 3: Implement** — `internal/spawnlet/server.go`:
```go
package spawnlet

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"connectrpc.com/connect"
	spawnv1 "spawnery/gen/spawn/v1"
	"spawnery/gen/spawn/v1/spawnv1connect"
	"spawnery/internal/runtime"
)

type Server struct {
	spawnv1connect.UnimplementedSpawnServiceHandler
	m  *Manager
	rt runtime.ContainerRuntime
}

func NewServer(m *Manager) *Server { return &Server{m: m, rt: m.rt} }

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) CreateSpawn(ctx context.Context, req *connect.Request[spawnv1.CreateSpawnRequest]) (*connect.Response[spawnv1.CreateSpawnResponse], error) {
	id := newID()
	if _, err := s.m.Create(ctx, id, req.Msg.AppPath, req.Msg.DataPath, req.Msg.Model); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&spawnv1.CreateSpawnResponse{SpawnId: id}), nil
}

func (s *Server) StopSpawn(ctx context.Context, req *connect.Request[spawnv1.StopSpawnRequest]) (*connect.Response[spawnv1.StopSpawnResponse], error) {
	if err := s.m.Stop(ctx, req.Msg.SpawnId); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&spawnv1.StopSpawnResponse{}), nil
}

func (s *Server) Session(ctx context.Context, stream *connect.BidiStream[spawnv1.Frame, spawnv1.Frame]) error {
	// First frame binds the spawn.
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	sp, ok := s.m.Store().Get(first.SpawnId)
	if !ok {
		return connect.NewError(connect.CodeNotFound, nil)
	}
	att, err := s.rt.Attach(ctx, sp.AgentID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	defer att.Close()

	ep := StreamEndpoint{
		Recv: func() ([]byte, error) {
			f, err := stream.Receive()
			if err != nil {
				return nil, err
			}
			return f.Data, nil
		},
		Send: func(b []byte) error {
			return stream.Send(&spawnv1.Frame{SpawnId: sp.ID, Data: b})
		},
	}
	// feed the first frame's payload through, then relay.
	if len(first.Data) > 0 {
		_, _ = att.Stdin.Write(first.Data)
	}
	Relay(ctx, ep, AgentIO{Stdin: att.Stdin, Stdout: att.Stdout})
	return nil
}
```

- [ ] **Step 4: Run green** — `go test ./internal/spawnlet/ -v` → PASS.
- [ ] **Step 5: Commit** —
```bash
git add internal/spawnlet/server.go internal/spawnlet/server_test.go
git commit -m "feat(spawnlet): ConnectRPC SpawnService (create/session/stop)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 10: spawnlet entrypoint

**Files:** Create `cmd/spawnlet/main.go`.

- [ ] **Step 1: Implement** — `cmd/spawnlet/main.go`:
```go
package main

import (
	"log"
	"net/http"
	"os"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"spawnery/gen/spawn/v1/spawnv1connect"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
)

func main() {
	rt, err := runtime.NewDocker()
	if err != nil {
		log.Fatalf("docker: %v", err)
	}
	mgr := spawnlet.NewManager(rt, spawnlet.ManagerConfig{
		AgentImage:    env("AGENT_IMAGE", "spawnery/stubagent:dev"),
		SidecarImage:  env("SIDECAR_IMAGE", "spawnery/sidecar:dev"),
		OpenRouterKey: os.Getenv("OPENROUTER_API_KEY"),
		DataRoot:      env("DATA_ROOT", "/var/lib/spawnlet/spawns"),
	})
	mux := http.NewServeMux()
	mux.Handle(spawnv1connect.NewSpawnServiceHandler(spawnlet.NewServer(mgr)))

	addr := env("SPAWNLET_ADDR", "127.0.0.1:9090")
	log.Printf("spawnlet listening on %s", addr)
	// h2c so the Go client can use HTTP/2 bidi without TLS for the slice.
	log.Fatal(http.ListenAndServe(addr, h2c.NewHandler(mux, &http2.Server{})))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 2: Build** — `go get golang.org/x/net && go mod tidy && go build ./cmd/spawnlet`
Expected: builds.

- [ ] **Step 3: Commit** —
```bash
git add cmd/spawnlet go.mod go.sum
git commit -m "feat(spawnlet): h2c server entrypoint

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 11: ACP client + slice CLI

**Files:** Create `internal/acp/client.go`, `internal/acp/client_test.go`, `cmd/spawnctl/main.go`.

The client speaks ACP over an `io.Reader`/`io.Writer` (the CLI adapts the Connect stream to those). Test it directly against the stub agent via `io.Pipe`.

- [ ] **Step 1: Failing test** — `internal/acp/client_test.go`:
```go
package acp_test

import (
	"io"
	"strings"
	"testing"
	"time"

	"spawnery/internal/acp"
	"spawnery/internal/stubagent"
)

func TestClientPromptAgainstStub(t *testing.T) {
	cliR, agentW := io.Pipe() // agent -> client
	agentR, cliW := io.Pipe() // client -> agent
	go stubagent.Run(agentR, agentW)

	c := acp.NewClient(cliR, cliW)
	if err := c.Initialize(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := c.NewSession("/data"); err != nil {
		t.Fatalf("session: %v", err)
	}

	var got strings.Builder
	done := make(chan error, 1)
	go func() { done <- c.Prompt("hello", func(chunk string) { got.WriteString(chunk) }) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("prompt: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
	if !strings.Contains(got.String(), "ECHO: hello") {
		t.Fatalf("got %q", got.String())
	}
}
```

- [ ] **Step 2: Run red** — FAIL (undefined: NewClient).

- [ ] **Step 3: Implement** — `internal/acp/client.go`:
```go
package acp

import (
	"encoding/json"
	"io"
)

type Client struct {
	w   io.Writer
	r   *Reader
	nid int
}

func NewClient(r io.Reader, w io.Writer) *Client {
	return &Client{w: w, r: NewReader(r)}
}

func (c *Client) next() int { c.nid++; return c.nid }

// call sends a request and reads messages until the matching response id.
func (c *Client) call(method string, params any) (Message, error) {
	id := c.next()
	var raw json.RawMessage
	if params != nil {
		b, _ := json.Marshal(params)
		raw = b
	}
	if err := WriteMessage(c.w, Message{ID: &id, Method: method, Params: raw}); err != nil {
		return Message{}, err
	}
	for {
		m, err := c.r.ReadMessage()
		if err != nil {
			return Message{}, err
		}
		if m.ID != nil && *m.ID == id {
			return m, nil
		}
		// ignore notifications during simple calls
	}
}

func (c *Client) Initialize() error {
	_, err := c.call("initialize", map[string]string{"protocolVersion": "slice-0"})
	return err
}

func (c *Client) NewSession(cwd string) error {
	_, err := c.call("session/new", map[string]string{"cwd": cwd})
	return err
}

// Prompt sends a prompt and invokes onChunk for each streamed session/update
// until the matching response arrives.
func (c *Client) Prompt(text string, onChunk func(string)) error {
	id := c.next()
	if err := WriteMessage(c.w, Message{ID: &id, Method: "session/prompt",
		Params: mustJSON(map[string]string{"text": text})}); err != nil {
		return err
	}
	for {
		m, err := c.r.ReadMessage()
		if err != nil {
			return err
		}
		if m.Method == "session/update" {
			var u struct {
				Chunk string `json:"chunk"`
			}
			if json.Unmarshal(m.Params, &u) == nil && u.Chunk != "" {
				onChunk(u.Chunk)
			}
			continue
		}
		if m.ID != nil && *m.ID == id {
			return nil
		}
	}
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
```

- [ ] **Step 4: Run green** — PASS.

- [ ] **Step 5: Implement the CLI** — `cmd/spawnctl/main.go`:
```go
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	spawnv1 "spawnery/gen/spawn/v1"
	"spawnery/gen/spawn/v1/spawnv1connect"
	"spawnery/internal/acp"
)

func main() {
	addr := flag.String("addr", "http://127.0.0.1:9090", "spawnlet address")
	appPath := flag.String("app", "examples/hello-app", "app definition dir")
	model := flag.String("model", "anthropic/claude-3.5-sonnet", "OpenRouter model")
	flag.Parse()

	// HTTP/2 over cleartext (h2c) client for bidi streaming.
	hc := &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, a string, _ any) (any, error) {
			return (&net.Dialer{}).DialContext(ctx, network, a)
		},
	}}
	_ = hc // see note below; use h2cClient() helper
	client := spawnv1connect.NewSpawnServiceClient(h2cClient(), *addr, connect.WithGRPC())

	ctx := context.Background()
	cs, err := client.CreateSpawn(ctx, connect.NewRequest(&spawnv1.CreateSpawnRequest{
		AppPath: *appPath, Model: *model,
	}))
	if err != nil {
		log.Fatalf("createSpawn: %v", err)
	}
	id := cs.Msg.SpawnId
	fmt.Println("spawn:", id)

	stream := client.Session(ctx)
	// adapt the Connect stream to io for the acp.Client
	pr, pw := io.Pipe() // agent -> client bytes
	go func() {
		for {
			f, err := stream.Receive()
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			pw.Write(f.Data)
		}
	}()
	sendW := writerFunc(func(b []byte) (int, error) {
		return len(b), stream.Send(&spawnv1.Frame{SpawnId: id, Data: b})
	})

	c := acp.NewClient(pr, sendW)
	if err := c.Initialize(); err != nil {
		log.Fatal(err)
	}
	if err := c.NewSession("/data"); err != nil {
		log.Fatal(err)
	}

	fmt.Println("ready. type prompts:")
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		line := in.Text()
		if line == "" {
			continue
		}
		if err := c.Prompt(line, func(chunk string) { fmt.Print(chunk) }); err != nil {
			log.Fatal(err)
		}
		fmt.Println()
	}
	stream.CloseRequest()
	client.StopSpawn(ctx, connect.NewRequest(&spawnv1.StopSpawnRequest{SpawnId: id}))
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
```
> Add a small `h2cClient()` helper (and `net` import) in the same file returning an `*http.Client` with an `http2.Transport{AllowHTTP:true, DialTLSContext: plain TCP}`. Run `go build ./cmd/spawnctl` and fix imports until it compiles.

- [ ] **Step 6: Commit** —
```bash
git add internal/acp/client.go internal/acp/client_test.go cmd/spawnctl
git commit -m "feat(client): minimal ACP client + spawnctl CLI

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 12: End-to-end with the stub agent (containerized)

**Files:** Create `deploy/stubagent/Dockerfile`, `deploy/sidecar/Dockerfile`, `examples/hello-app/*`, `internal/spawnlet/e2e_test.go`.

Run the real spawnlet server + Docker runtime + the **stub agent image** + the **sidecar image** (sidecar is inert here — the stub makes no inference calls), and drive a prompt through the CLI path. Skips without Docker.

- [ ] **Step 1: Dockerfiles**

`deploy/stubagent/Dockerfile`:
```dockerfile
FROM golang:1.22 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /stubagent ./cmd/stubagent
FROM gcr.io/distroless/static
COPY --from=build /stubagent /stubagent
ENTRYPOINT ["/stubagent"]
```
`deploy/sidecar/Dockerfile`:
```dockerfile
FROM golang:1.22 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /sidecar ./cmd/sidecar
FROM gcr.io/distroless/static
COPY --from=build /sidecar /sidecar
ENTRYPOINT ["/sidecar"]
```

- [ ] **Step 2: Example app**

`examples/hello-app/spawneryapp.yml`:
```yaml
apiVersion: spawnery/v1
kind: App
id: spawnery/hello
title: Hello
agents: { support: any, requiresAcp: [prompt] }
model: { recommendedDefault: anthropic/claude-3.5-sonnet }
storage: { required: true, seed: ./seed }
visibility: open
```
`examples/hello-app/persona.md`:
```markdown
You are a friendly assistant. Keep replies short.
```
`examples/hello-app/seed/README.md`:
```markdown
# Hello data
```

- [ ] **Step 3: Build the images**

Run:
```bash
docker build -t spawnery/stubagent:dev -f deploy/stubagent/Dockerfile .
docker build -t spawnery/sidecar:dev   -f deploy/sidecar/Dockerfile .
```

- [ ] **Step 4: Write the e2e test** — `internal/spawnlet/e2e_test.go`:
```go
package spawnlet_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	spawnv1 "spawnery/gen/spawn/v1"
	"spawnery/gen/spawn/v1/spawnv1connect"
	"spawnery/internal/acp"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
)

func TestEndToEndStub(t *testing.T) {
	if os.Getenv("SKIP_DOCKER") != "" {
		t.Skip("SKIP_DOCKER")
	}
	rt, err := runtime.NewDocker()
	if err != nil || rt.Ping(context.Background()) != nil {
		t.Skip("docker unavailable")
	}
	mgr := spawnlet.NewManager(rt, spawnlet.ManagerConfig{
		AgentImage: "spawnery/stubagent:dev", SidecarImage: "spawnery/sidecar:dev",
		OpenRouterKey: "unused", DataRoot: t.TempDir(),
	})
	mux := http.NewServeMux()
	mux.Handle(spawnv1connect.NewSpawnServiceHandler(spawnlet.NewServer(mgr)))
	srv := httptest.NewServer(h2c.NewHandler(mux, &http2.Server{}))
	defer srv.Close()

	cl := spawnv1connect.NewSpawnServiceClient(srv.Client(), srv.URL, connect.WithGRPC())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&spawnv1.CreateSpawnRequest{
		AppPath: mustAbs(t, "../../examples/hello-app"), Model: "x",
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := cs.Msg.SpawnId
	defer cl.StopSpawn(ctx, connect.NewRequest(&spawnv1.StopSpawnRequest{SpawnId: id}))

	stream := cl.Session(ctx)
	pr, pw := io.Pipe()
	go func() {
		for {
			f, err := stream.Receive()
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			pw.Write(f.Data)
		}
	}()
	c := acp.NewClient(pr, writerTo(stream, id))
	if err := c.Initialize(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := c.NewSession("/data"); err != nil {
		t.Fatalf("session: %v", err)
	}
	var got strings.Builder
	if err := c.Prompt("hello", func(s string) { got.WriteString(s) }); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if !strings.Contains(got.String(), "ECHO: hello") {
		t.Fatalf("got %q", got.String())
	}
}
```
> Add small test helpers `mustAbs` and `writerTo(stream, id)` (an `io.Writer` that does `stream.Send(&Frame{SpawnId:id, Data:b})`) at the bottom of the file.

- [ ] **Step 5: Run it** — `go test ./internal/spawnlet/ -run TestEndToEndStub -v`
Expected: PASS (or SKIP without Docker).

- [ ] **Step 6: Commit** —
```bash
git add deploy examples internal/spawnlet/e2e_test.go
git commit -m "test(spawnlet): containerized end-to-end against stub agent

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 13: Real end-to-end with Goose + OpenRouter (acceptance)

**Files:** Create `deploy/agent/Dockerfile`, `deploy/agent/entrypoint.sh`; manual acceptance run.

Resolve the three Goose unknowns from the spec (§3) and prove a live round-trip. This task is **manual/acceptance** (network + real LLM), not CI.

- [ ] **Step 1: Confirm Goose specifics** (record findings as comments in the Dockerfile):
  1. ACP launch invocation (e.g. `goose acp` or equivalent headless stdio mode).
  2. How to set the **OpenAI provider with a custom base URL** to `http://127.0.0.1:8080/v1` and the model from `SPAWN_MODEL` (env vars / config file).
  3. How to set **cwd = /data** and load the persona from `/app/persona.md` (via ACP `session/new` cwd + a system-prompt/hints mechanism).

- [ ] **Step 2: Goose base image** — `deploy/agent/Dockerfile`:
```dockerfile
# Goose ACP agent base image for the spawnlet slice.
FROM debian:bookworm-slim
# Install Goose per its documented method (pin a release).
# RUN curl -fsSL https://github.com/block/goose/releases/download/<ver>/... -o /usr/local/bin/goose && chmod +x /usr/local/bin/goose
COPY deploy/agent/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
WORKDIR /data
ENTRYPOINT ["/entrypoint.sh"]
```
`deploy/agent/entrypoint.sh`:
```sh
#!/bin/sh
# Configure Goose to use our sidecar as the OpenAI-compatible endpoint,
# then launch it in ACP/stdio mode. Exact flags filled in from Step 1.
export OPENAI_BASE_URL="${OPENAI_BASE_URL:-http://127.0.0.1:8080/v1}"
export OPENAI_API_KEY="sk-unused-sidecar-injects-real-key"
exec goose acp   # <- replace with the confirmed headless ACP invocation
```

- [ ] **Step 3: Build + run acceptance**

Run:
```bash
docker build -t spawnery/goose:dev -f deploy/agent/Dockerfile .
go build -o bin/spawnlet ./cmd/spawnlet
go build -o bin/spawnctl ./cmd/spawnctl
OPENROUTER_API_KEY=<key> AGENT_IMAGE=spawnery/goose:dev SIDECAR_IMAGE=spawnery/sidecar:dev \
  DATA_ROOT=$(pwd)/.spawns bin/spawnlet &
bin/spawnctl -app examples/hello-app -model anthropic/claude-3.5-sonnet
# type: "Say hello in one sentence."
```
Expected: a real model reply streams back through the spawnlet; files the agent writes appear under `.spawns/<id>/data`.

- [ ] **Step 4: Document the run** — append a "Running the slice" section to `README.md` with the exact commands. Commit:
```bash
git add deploy/agent README.md
git commit -m "feat(agent): Goose base image + live OpenRouter acceptance run

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage:** §2 components → Tasks 2,3,5,7,8,9,11 (+stub 2); §3 Goose → Task 13; §4 API → Task 0; §5 transparent relay → Task 8 + Task 9 `Session`; §6 data flow → Tasks 9+11+13; §7 shared netns → Task 7 (`NetnsOf`) + Task 5 (`--network container:`); §8 lifecycle (data dir, seed, sidecar-then-agent, rollback, stop) → Task 7; §9 errors (Connect codes, rollback) → Tasks 7,9; §10 out-of-scope → respected (no CP/auth/E2E/storage/tiers); §11 testing → every task TDD + Tasks 12 (stub e2e) & 13 (Goose e2e). **No gaps.**

**Known follow-ups (not gaps, explicitly deferred in the spec):** ACP framing confirmation vs Goose (Task 12/13 Step 1); idle-timeout teardown; image-pull-on-demand (note in Task 5 → add `cli.ImagePull` in Task 7's manager if images aren't pre-built locally).

**Type consistency:** `ContainerSpec{Image,Cmd,Env,Mounts,NetnsOf,AttachStdio}`, `AttachedStream{Stdin,Stdout,Close}`, `Spawn{ID,SidecarID,AgentID,DataDir,Status}`, `StreamEndpoint{Recv,Send}`, `AgentIO{Stdin,Stdout}`, `acp.Message`, `acp.Client{Initialize,NewSession,Prompt}` — used consistently across Tasks 4–12.

---

## Beads tracking

Milestones (create + chain in dependency order; close each when its tests are green):
`scaffold (T0) → acp-codec (T1) → stub-agent (T2) → sidecar (T3) → runtime (T4–5) → store+manager (T6–7) → relay (T8) → server+entrypoint (T9–10) → client (T11) → e2e-stub (T12) → goose-acceptance (T13)`.
Parent: this is the build-out of **E1 runtime core (`sp-ei4`)** — link the milestone beads under it.
