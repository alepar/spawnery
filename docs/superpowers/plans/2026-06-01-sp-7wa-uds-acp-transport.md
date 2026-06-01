# UDS ACP Transport (sp-7wa) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the agent's Docker stdio-`Attach` with an in-pod Unix-domain-socket ACP bridge, applied to the Docker/runc path first — the keystone that de-risks the transport before any CRI work.

**Architecture:** A small `acpadapter` binary becomes the agent container's entrypoint: it starts the real agent (`goose acp`), listens on the abstract socket `@spawnlet-acp`, and bridges the current client connection to the agent's persistent stdin/stdout. The node enters the pod's network namespace (`setns`) and dials that socket via a new `runtime.AttachACP(netnsPath)`; the `Manager` records the sidecar's netns path on the spawn, and `server.go`/`ws.go` attach through it instead of `rt.Attach`.

**Tech Stack:** Go 1.25, Docker SDK, abstract Unix sockets, `golang.org/x/sys/unix` (`setns`), the existing `runtime.AttachedStream` + `spawnlet.Relay` plumbing.

**Scope:** Node + agent images only. No CP, proto, manifest, or web changes. Commits use `--no-verify` (the `.beads` export hook dirties commits). Spec: `docs/superpowers/specs/2026-06-01-runsc-cri-pod-backend-design.md` §3.3 + slice 1 in §5.

**Environment note:** This dev sandbox has **no Docker, no root, no iptables**. Hermetic Go unit tests run here (`go test ./...`). Anything needing Docker (`docker build`, image-based e2e) or `setns` (root) is **host-gated** — its verification step says so explicitly and is run on the privileged node host.

---

## File Structure

**New files:**
- `deploy/agent/acpadapter/bridge.go` — the connection-hub + stdout-pump + `serve` loop (pure, hermetically testable).
- `deploy/agent/acpadapter/bridge_test.go` — hermetic bridge tests (fake echo agent, reconnect, half-close).
- `deploy/agent/acpadapter/main.go` — process wiring: start the agent subprocess, listen on `@spawnlet-acp`, exit when the agent exits.
- `deploy/agent/acpadapter/main_test.go` — hermetic smoke test running the built binary against a stub agent.
- `internal/runtime/attach_acp_linux.go` — `AttachACP(ctx, netnsPath)` + `dialInNetns`/`dialOnce` (setns + dial + retry).
- `internal/runtime/attach_acp_test.go` — hermetic test of the `dialOnce` error path (no root).
- `internal/runtime/attach_acp_e2e_test.go` — host-gated (`//go:build acp_e2e`) real setns+dial round-trip.

**Modified files:**
- `deploy/agent/Dockerfile` — add a Go builder stage that builds `acpadapter`; copy it in.
- `deploy/agent/entrypoint.sh` — final line execs the adapter wrapping `goose acp`.
- `deploy/stubagent/Dockerfile` — build + entrypoint through the adapter (keeps host-gated e2e valid).
- `Makefile:20` — add `$(GO_SRCS)` as a prereq of `.make/img-goose` (the image now embeds Go source).
- `internal/spawnlet/store.go` — `Spawn.NetnsPath` field.
- `internal/spawnlet/manager.go` — compute the sidecar netns path, store it on the spawn.
- `internal/spawnlet/server.go` — replace the `rt` field with an injectable `attach` func defaulting to `AttachACP`.
- `internal/spawnlet/ws.go` — attach via `s.attach`.
- `internal/spawnlet/ws_test.go` — inject a fake echo attach so the hermetic WS test stays green.
- `ISOLATION.md`, `deployment.md` — document the UDS transport + the node's `CAP_SYS_ADMIN` (setns) requirement.

---

## Task 1: acpadapter bridge core

The bridge is the testable heart of the adapter. **Design rationale (do not simplify to two `io.Copy`s):** the agent's stdout reader must be a *single persistent* goroutine that writes to whichever client is currently connected. A naive `io.Copy(conn, agentStdout)` per connection **deadlocks** — when the agent is idle it blocks on `agentStdout.Read`, and a client disconnect (detected on the *other* direction) cannot interrupt that blocked read, so `serve` never loops to accept the next client.

**Files:**
- Create: `deploy/agent/acpadapter/bridge.go`
- Test: `deploy/agent/acpadapter/bridge_test.go`

- [ ] **Step 1: Write the failing test**

```go
// deploy/agent/acpadapter/bridge_test.go
package main

import (
	"bufio"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// fakeAgent returns (toAgent, fromAgent) wired as an echo: bytes written to
// toAgent come back on fromAgent. Models a persistent agent process stdio.
func fakeAgent() (io.Writer, io.Reader) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go io.Copy(outW, inR) // echo stdin -> stdout, lives for the whole test
	return inW, outR
}

func dialAndRoundtrip(t *testing.T, path, line string) {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, line); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != line {
		t.Fatalf("got %q want %q", got, line)
	}
}

func TestServeBridgesAndPersistsAcrossReconnect(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "acp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	toAgent, fromAgent := fakeAgent()
	go serve(ln, toAgent, fromAgent)

	// First client.
	dialAndRoundtrip(t, sock, "hello\n")
	// Reconnect: a NEW client must reach the SAME persistent agent.
	dialAndRoundtrip(t, sock, "world\n")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./deploy/agent/acpadapter/ -run TestServeBridges -v`
Expected: FAIL — `undefined: serve`.

- [ ] **Step 3: Write minimal implementation**

```go
// deploy/agent/acpadapter/bridge.go
package main

import (
	"io"
	"net"
	"sync"
)

// connHub holds the currently-attached client connection (at most one). The
// stdout pump writes agent output to whichever client is current; output
// produced while no client is attached is dropped (attach/detach semantics).
type connHub struct {
	mu  sync.Mutex
	cur net.Conn
}

func (h *connHub) set(c net.Conn) { h.mu.Lock(); h.cur = c; h.mu.Unlock() }

func (h *connHub) clear(c net.Conn) {
	h.mu.Lock()
	if h.cur == c {
		h.cur = nil
	}
	h.mu.Unlock()
}

func (h *connHub) write(p []byte) {
	h.mu.Lock()
	c := h.cur
	h.mu.Unlock()
	if c != nil {
		_, _ = c.Write(p) // a dead conn's Write returns fast; no lock held here
	}
}

// pump is the single persistent reader of the agent's stdout.
func pump(fromAgent io.Reader, hub *connHub) {
	buf := make([]byte, 32*1024)
	for {
		n, err := fromAgent.Read(buf)
		if n > 0 {
			hub.write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// serve accepts one client at a time and bridges it to the long-lived agent
// stdio. The agent persists across client disconnects, so a reconnecting client
// resumes the same session. It returns only when the listener is closed.
func serve(ln net.Listener, toAgent io.Writer, fromAgent io.Reader) error {
	hub := &connHub{}
	go pump(fromAgent, hub)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		hub.set(conn)
		_, _ = io.Copy(toAgent, conn) // client -> agent stdin; returns on client EOF/close
		hub.clear(conn)
		_ = conn.Close()
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./deploy/agent/acpadapter/ -run TestServeBridges -race -v`
Expected: PASS (both round-trips succeed; `-race` clean).

- [ ] **Step 5: Add the half-close test**

Half-close on the client side (the node's `AttachedStream.Close` uses `CloseWrite`) must stop forwarding stdin while agent→client output can still flow.

```go
// append to deploy/agent/acpadapter/bridge_test.go
func TestServeClientHalfCloseStopsStdinNotStdout(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "acp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	toAgent, fromAgent := fakeAgent()
	go serve(ln, toAgent, fromAgent)

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "ping\n"); err != nil {
		t.Fatal(err)
	}
	// Half-close the write side; the echo of "ping\n" must still arrive.
	if err := c.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read after half-close: %v", err)
	}
	if got != "ping\n" {
		t.Fatalf("got %q want %q", got, "ping\n")
	}
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./deploy/agent/acpadapter/ -race -v`
Expected: PASS for both tests.

- [ ] **Step 7: Commit**

```bash
git add deploy/agent/acpadapter/bridge.go deploy/agent/acpadapter/bridge_test.go
git commit --no-verify -m "feat(acpadapter): UDS<->agent stdio bridge core (sp-7wa)"
```

---

## Task 2: acpadapter main (process wiring)

Wires `main()`: start the agent subprocess, listen on the abstract socket (overridable via `ACP_SOCKET` for tests), and exit when the agent exits.

**Files:**
- Create: `deploy/agent/acpadapter/main.go`
- Test: `deploy/agent/acpadapter/main_test.go`

- [ ] **Step 1: Write the implementation**

```go
// deploy/agent/acpadapter/main.go
package main

import (
	"log"
	"net"
	"os"
	"os/exec"
)

// acpadapter starts the agent given by its args (e.g. `goose acp`), listens on
// the abstract socket @spawnlet-acp (or $ACP_SOCKET), and bridges the current
// client connection to the agent's stdio. Exits when the agent exits.
func main() {
	log.SetPrefix("acpadapter: ")
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal("usage: acpadapter <agent-cmd> [args...]")
	}
	sock := os.Getenv("ACP_SOCKET")
	if sock == "" {
		sock = "@spawnlet-acp" // leading @ = Linux abstract namespace
	}

	agent := exec.Command(os.Args[1], os.Args[2:]...)
	agent.Stderr = os.Stderr
	toAgent, err := agent.StdinPipe()
	if err != nil {
		log.Fatalf("agent stdin: %v", err)
	}
	fromAgent, err := agent.StdoutPipe()
	if err != nil {
		log.Fatalf("agent stdout: %v", err)
	}
	if err := agent.Start(); err != nil {
		log.Fatalf("start agent: %v", err)
	}

	ln, err := net.Listen("unix", sock)
	if err != nil {
		log.Fatalf("listen %s: %v", sock, err)
	}

	// The spawn is over when the agent exits.
	go func() {
		werr := agent.Wait()
		log.Printf("agent exited: %v", werr)
		_ = ln.Close()
		os.Exit(0)
	}()

	if err := serve(ln, toAgent, fromAgent); err != nil {
		log.Printf("serve ended: %v", err)
	}
}
```

- [ ] **Step 2: Write the smoke test**

Runs the *built* binary with a stub agent (`cat`, which echoes stdin→stdout) over a temp-path socket, and asserts a round-trip. `cat` exits on stdin EOF; one connection is sufficient for this smoke.

```go
// deploy/agent/acpadapter/main_test.go
package main

import (
	"bufio"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestAdapterBinaryBridgesToStubAgent(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "acpadapter")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	sock := filepath.Join(dir, "acp.sock")

	cmd := exec.Command(bin, "cat")
	cmd.Env = append(cmd.Environ(), "ACP_SOCKET="+sock)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start adapter: %v", err)
	}
	defer cmd.Process.Kill()

	// Wait for the socket to appear.
	var c net.Conn
	for i := 0; i < 100; i++ {
		var err error
		if c, err = net.Dial("unix", sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if c == nil {
		t.Fatal("adapter never bound the socket")
	}
	defer c.Close()

	if _, err := io.WriteString(c, "echo-me\n"); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "echo-me\n" {
		t.Fatalf("got %q want %q", got, "echo-me\n")
	}
}
```

- [ ] **Step 3: Run the test**

Run: `go test ./deploy/agent/acpadapter/ -run TestAdapterBinary -v`
Expected: PASS (`go` toolchain present in this sandbox; `cat` present).

- [ ] **Step 4: Vet + build the package**

Run: `go vet ./deploy/agent/acpadapter/ && go build ./deploy/agent/acpadapter/`
Expected: no output, exit 0.

- [ ] **Step 5: Commit**

```bash
git add deploy/agent/acpadapter/main.go deploy/agent/acpadapter/main_test.go
git commit --no-verify -m "feat(acpadapter): process wiring + abstract socket (sp-7wa)"
```

---

## Task 3: goose image runs through the adapter

The goose image fetches a prebuilt binary; add a Go builder stage for `acpadapter` and exec it from the entrypoint. The `.make/img-goose` rule must depend on Go sources now that the image embeds them.

**Files:**
- Modify: `deploy/agent/Dockerfile`
- Modify: `deploy/agent/entrypoint.sh:19`
- Modify: `Makefile:20`

- [ ] **Step 1: Add the adapter build stage + copy to the goose Dockerfile**

In `deploy/agent/Dockerfile`, after the `FROM debian:bookworm-slim AS fetch` stage and **before** the final `FROM debian:bookworm-slim`, insert a Go builder stage:

```dockerfile
FROM golang:1.25 AS gobuild
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /acpadapter ./deploy/agent/acpadapter
```

Then in the final stage, after `COPY --from=fetch /usr/local/bin/goose /usr/local/bin/goose`, add:

```dockerfile
COPY --from=gobuild /acpadapter /usr/local/bin/acpadapter
```

- [ ] **Step 2: Switch the entrypoint to the adapter**

In `deploy/agent/entrypoint.sh`, replace the final line:

```sh
exec goose acp
```

with:

```sh
exec /usr/local/bin/acpadapter goose acp
```

- [ ] **Step 3: Make the goose image rebuild on Go source changes**

In `Makefile`, line 20, change:

```makefile
.make/img-goose:     deploy/agent/Dockerfile               | .make ; docker build -t spawnery/goose:dev     -f $< . && touch $@
```

to:

```makefile
.make/img-goose:     deploy/agent/Dockerfile     $(GO_SRCS) | .make ; docker build -t spawnery/goose:dev     -f $< . && touch $@
```

- [ ] **Step 4: Verify the adapter still builds for the image target**

Run: `go build -o /tmp/acpadapter ./deploy/agent/acpadapter`
Expected: exit 0 (this is exactly what the Docker `gobuild` stage runs).

- [ ] **Step 5: Host-gated image build verification**

> **Run on the privileged node host (needs Docker) — NOT in this sandbox.**

Run: `make .make/img-goose`
Expected: image `spawnery/goose:dev` builds; `docker run --rm --entrypoint /usr/local/bin/acpadapter spawnery/goose:dev 2>&1 | head -1` prints the usage line `acpadapter: usage: acpadapter <agent-cmd> [args...]`.

- [ ] **Step 6: Commit**

```bash
git add deploy/agent/Dockerfile deploy/agent/entrypoint.sh Makefile
git commit --no-verify -m "feat(goose-image): run goose acp behind the UDS adapter (sp-7wa)"
```

---

## Task 4: stubagent image runs through the adapter

The host-gated e2e tests use the stubagent (an echo agent). After the node switches to dialing `@spawnlet-acp`, the stubagent must also expose that socket, or those e2e tests break. The stubagent Dockerfile already has a Go build stage.

**Files:**
- Modify: `deploy/stubagent/Dockerfile`

- [ ] **Step 1: Build the adapter and route the entrypoint through it**

Replace the contents of `deploy/stubagent/Dockerfile` with:

```dockerfile
FROM golang:1.25 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /stubagent ./cmd/stubagent
RUN CGO_ENABLED=0 go build -o /acpadapter ./deploy/agent/acpadapter
FROM gcr.io/distroless/static
COPY --from=build /stubagent /stubagent
COPY --from=build /acpadapter /acpadapter
ENTRYPOINT ["/acpadapter", "/stubagent"]
```

- [ ] **Step 2: Verify both binaries build**

Run: `go build -o /tmp/stubagent ./cmd/stubagent && go build -o /tmp/acpadapter ./deploy/agent/acpadapter`
Expected: exit 0.

- [ ] **Step 3: Host-gated image build verification**

> **Run on the privileged node host (needs Docker) — NOT in this sandbox.**

Run: `make .make/img-stubagent`
Expected: image `spawnery/stubagent:dev` builds.

- [ ] **Step 4: Commit**

```bash
git add deploy/stubagent/Dockerfile
git commit --no-verify -m "feat(stubagent-image): run stub agent behind the UDS adapter (sp-7wa)"
```

---

## Task 5: runtime.AttachACP (node side)

Enters the pod netns and dials the agent's abstract socket. Lives in a `_linux.go` file (implicit linux build constraint — `setns` is Linux-only; the node is Linux-only). Retries the dial briefly because the adapter may bind the socket slightly after the agent container starts.

**Files:**
- Create: `internal/runtime/attach_acp_linux.go`
- Test: `internal/runtime/attach_acp_test.go`
- Test (host-gated): `internal/runtime/attach_acp_e2e_test.go`

- [ ] **Step 1: Ensure the setns dependency is a direct module requirement**

Run: `go get golang.org/x/sys/unix`
Expected: `go.mod` lists `golang.org/x/sys` as a direct requirement (it is currently indirect).

- [ ] **Step 2: Write the failing test (dialOnce error path — hermetic, no root)**

`dialOnce` against a real netns path without `CAP_SYS_ADMIN` fails at `setns` (or at `open` for a bogus path); either way it must return an error promptly without entering the retry loop.

```go
// internal/runtime/attach_acp_test.go
package runtime

import (
	"strings"
	"testing"
)

func TestDialOnceFailsWithoutPrivilege(t *testing.T) {
	// Dialing into our own netns still requires CAP_SYS_ADMIN for setns; under
	// the unprivileged test runner this must error (not hang, not panic).
	_, err := dialOnce("/proc/self/ns/net", "@spawnery-acp-test-nonexistent")
	if err == nil {
		t.Fatal("expected error dialing without privilege / missing socket")
	}
	if strings.Contains(err.Error(), "panic") {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestDialOnceBogusNetnsPath(t *testing.T) {
	_, err := dialOnce("/proc/nonexistent-pid/ns/net", "@x")
	if err == nil {
		t.Fatal("expected error opening bogus netns path")
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/runtime/ -run TestDialOnce -v`
Expected: FAIL — `undefined: dialOnce`.

- [ ] **Step 4: Write the implementation**

```go
// internal/runtime/attach_acp_linux.go
package runtime

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

// acpSocket is the abstract Unix socket the in-container acpadapter listens on.
const acpSocket = "@spawnlet-acp"

// AttachACP enters the pod network namespace at netnsPath (e.g.
// /proc/<sidecar-pid>/ns/net) and dials the agent's abstract ACP socket,
// returning a bidirectional stream whose Stdin/Stdout are the same connection.
// Requires CAP_SYS_ADMIN (setns). Used for both the Docker and CRI backends.
func AttachACP(ctx context.Context, netnsPath string) (*AttachedStream, error) {
	conn, err := dialInNetns(netnsPath, acpSocket)
	if err != nil {
		return nil, err
	}
	return &AttachedStream{
		Stdin:  conn,
		Stdout: conn,
		Close:  conn.Close,
	}, nil
}

// dialInNetns retries the dial for a few seconds: right after the agent
// container starts, the adapter may not have bound the socket yet.
func dialInNetns(netnsPath, sock string) (net.Conn, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := dialOnce(netnsPath, sock)
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("dial ACP socket in %s: %w", netnsPath, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// dialOnce locks the OS thread, switches its network namespace to netnsPath,
// dials the socket, then restores the thread's namespace. The returned conn's
// fd is valid regardless of namespace once connected.
func dialOnce(netnsPath, sock string) (net.Conn, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	orig, err := os.Open("/proc/thread-self/ns/net")
	if err != nil {
		return nil, fmt.Errorf("open current netns: %w", err)
	}
	defer orig.Close()

	target, err := os.Open(netnsPath)
	if err != nil {
		return nil, fmt.Errorf("open target netns %s: %w", netnsPath, err)
	}
	defer target.Close()

	if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
		return nil, fmt.Errorf("setns %s: %w", netnsPath, err)
	}
	// Restore this thread's original namespace no matter what.
	defer unix.Setns(int(orig.Fd()), unix.CLONE_NEWNET)

	return net.Dial("unix", sock)
}
```

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/runtime/ -run TestDialOnce -race -v`
Expected: PASS (both tests error out as asserted).

- [ ] **Step 6: Write the host-gated e2e test**

> Real round-trip: build the adapter, run it with `cat` listening on `@spawnlet-acp` in the current netns, then `AttachACP(ctx, "/proc/self/ns/net")` and echo. Needs root (setns). Build-tagged so it never runs in CI/this sandbox.

```go
// internal/runtime/attach_acp_e2e_test.go
//go:build acp_e2e

package runtime_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"spawnery/internal/runtime"
)

func TestAttachACPRoundtrip(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "acpadapter")
	if out, err := exec.Command("go", "build", "-o", bin, "../../deploy/agent/acpadapter").CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "cat") // default ACP_SOCKET=@spawnlet-acp
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start adapter: %v", err)
	}
	defer cmd.Process.Kill()
	time.Sleep(300 * time.Millisecond) // let it bind

	att, err := runtime.AttachACP(context.Background(), "/proc/self/ns/net")
	if err != nil {
		t.Fatalf("AttachACP: %v", err)
	}
	defer att.Close()

	if _, err := io.WriteString(att.Stdin, "secret-word\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("secret-word\n"))
	if _, err := io.ReadFull(att.Stdout, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "secret-word\n" {
		t.Fatalf("got %q", buf)
	}
}
```

- [ ] **Step 7: Verify the e2e test compiles (does not run here)**

Run: `go vet -tags acp_e2e ./internal/runtime/`
Expected: exit 0 (compiles; the test itself runs only on a privileged host via the tag).

- [ ] **Step 8: Commit**

```bash
git add internal/runtime/attach_acp_linux.go internal/runtime/attach_acp_test.go internal/runtime/attach_acp_e2e_test.go go.mod go.sum
git commit --no-verify -m "feat(runtime): AttachACP — setns into pod netns + dial @spawnlet-acp (sp-7wa)"
```

---

## Task 6: Record the netns path on the spawn

The `Manager` must capture the sidecar's netns path at create time so `AttachACP` can find the pod netns later. Mirror the existing `FloorIP` capture.

**Files:**
- Modify: `internal/spawnlet/store.go:5-12`
- Modify: `internal/spawnlet/manager.go` (after sidecar start, ~line 113)
- Test: `internal/spawnlet/manager_test.go`

- [ ] **Step 1: Add the field**

In `internal/spawnlet/store.go`, add `NetnsPath` to `Spawn`:

```go
type Spawn struct {
	ID        string
	SidecarID string
	AgentID   string
	MountDirs []string // host dirs backing this spawn's mounts (for Finalize)
	FloorIP   string   // pod bridge IP the egress floor was applied for (for Remove on Stop)
	NetnsPath string   // /proc/<sidecar-pid>/ns/net — the pod netns, for AttachACP
	Status    string
}
```

- [ ] **Step 2: Write the failing test**

```go
// append to internal/spawnlet/manager_test.go
func TestCreateRecordsNetnsPath(t *testing.T) {
	f := runtime.NewFake() // FakeRuntime.ContainerPID returns 4242
	m := NewManager(f, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	sp, err := m.Create(context.Background(), "n-1", writeApp(t), "x")
	if err != nil {
		t.Fatal(err)
	}
	if sp.NetnsPath != "/proc/4242/ns/net" {
		t.Fatalf("NetnsPath = %q, want /proc/4242/ns/net", sp.NetnsPath)
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/spawnlet/ -run TestCreateRecordsNetnsPath -v`
Expected: FAIL — `sp.NetnsPath` is empty.

- [ ] **Step 4: Compute the netns path in `Manager.Create`**

In `internal/spawnlet/manager.go`, immediately after the sidecar `StartContainer` block succeeds (after the `if err != nil { ... }` that handles the sidecar start, i.e. just before the `var floorIP string` line at ~115), insert:

```go
	sidecarPID, perr := m.rt.ContainerPID(ctx, sidecarID)
	if perr != nil {
		_ = m.rt.StopContainer(ctx, sidecarID)
		finalizeAll()
		return nil, fmt.Errorf("sidecar pid: %w", perr)
	}
	netnsPath := fmt.Sprintf("/proc/%d/ns/net", sidecarPID)
```

Then add `NetnsPath: netnsPath` to the `Spawn` literal at the end of `Create` (~line 151):

```go
	sp := &Spawn{ID: id, SidecarID: sidecarID, AgentID: agentID, MountDirs: mountDirs, FloorIP: floorIP, NetnsPath: netnsPath, Status: "ready"}
```

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/spawnlet/ -run TestCreateRecordsNetnsPath -race -v`
Expected: PASS.

- [ ] **Step 6: Run the package tests (regression — fail-closed path still stops the sidecar)**

Run: `go test ./internal/spawnlet/ -race`
Expected: PASS (the new `ContainerPID` call sits before the floor; `manager_egress_test.go`'s fail-closed assertions still hold because `FakeRuntime.ContainerPID` succeeds).

- [ ] **Step 7: Commit**

```bash
git add internal/spawnlet/store.go internal/spawnlet/manager.go internal/spawnlet/manager_test.go
git commit --no-verify -m "feat(spawnlet): record pod netns path on the spawn (sp-7wa)"
```

---

## Task 7: Attach via AttachACP in the relay paths

Replace `s.rt.Attach(ctx, sp.AgentID)` in both relay handlers with an injectable `attach` func defaulting to `runtime.AttachACP(ctx, sp.NetnsPath)`. The injection keeps the hermetic WS test (which relies on the fake runtime's echoing `Attach`) green.

**Files:**
- Modify: `internal/spawnlet/server.go`
- Modify: `internal/spawnlet/ws.go:40`
- Modify: `internal/spawnlet/ws_test.go`

- [ ] **Step 1: Replace the `rt` field with an injectable `attach` func**

In `internal/spawnlet/server.go`, change the struct and constructor:

```go
type Server struct {
	spawnv1connect.UnimplementedSpawnServiceHandler
	m      *Manager
	attach func(ctx context.Context, sp *Spawn) (*runtime.AttachedStream, error)
}

func NewServer(m *Manager) *Server {
	return &Server{
		m: m,
		attach: func(ctx context.Context, sp *Spawn) (*runtime.AttachedStream, error) {
			return runtime.AttachACP(ctx, sp.NetnsPath)
		},
	}
}
```

In `Server.Session`, change the attach line (was `att, err := s.rt.Attach(ctx, sp.AgentID)`):

```go
	att, err := s.attach(ctx, sp)
```

- [ ] **Step 2: Update the WS handler**

In `internal/spawnlet/ws.go`, change line 40 from `att, err := s.rt.Attach(ctx, sp.AgentID)` to:

```go
	att, err := s.attach(ctx, sp)
```

- [ ] **Step 3: Keep the hermetic WS test green by injecting the fake echo attach**

In `internal/spawnlet/ws_test.go`, after `srv := NewServer(m)`, add (using the `f` FakeRuntime already in the test, whose `Attach` echoes stdin→stdout):

```go
	srv.attach = func(ctx context.Context, sp *Spawn) (*runtime.AttachedStream, error) {
		return f.Attach(ctx, sp.AgentID)
	}
```

- [ ] **Step 4: Run the spawnlet package tests**

Run: `go test ./internal/spawnlet/ -race -v -run 'TestWSRelayEchoesViaFake|TestServer'`
Expected: PASS — the WS echo test passes via the injected fake; server tests unaffected.

- [ ] **Step 5: Build the whole module**

Run: `go build ./... && go vet ./internal/spawnlet/ ./internal/runtime/`
Expected: exit 0 (no stale `s.rt` reference; `runtime` import still used in `server.go`).

- [ ] **Step 6: Full hermetic test sweep**

Run: `go test ./... -race`
Expected: PASS across the module (the build-tagged `e2e`/`egress_e2e`/`acp_e2e` suites are excluded without their tags).

- [ ] **Step 7: Commit**

```bash
git add internal/spawnlet/server.go internal/spawnlet/ws.go internal/spawnlet/ws_test.go
git commit --no-verify -m "feat(spawnlet): relay attaches via AttachACP UDS transport (sp-7wa)"
```

---

## Task 8: Document the transport + privilege change

The node now needs `CAP_SYS_ADMIN` (setns) to attach — a real operational change. Record it.

**Files:**
- Modify: `ISOLATION.md` (§3.4 container isolation / §4 knobs)
- Modify: `deployment.md` (§2.2 node host prereqs)

- [ ] **Step 1: Note the UDS transport + setns requirement in ISOLATION.md**

In `ISOLATION.md`, under §3.4 (Container isolation), add a bullet:

```markdown
- **ACP transport (UDS):** the agent's ACP stream rides an in-pod **abstract Unix socket**
  (`@spawnlet-acp`), bridged to `goose acp`'s stdio by a small in-container adapter. The node reaches
  it by entering the pod's network namespace (`setns`) and dialing the socket — so the spawnlet
  process needs **`CAP_SYS_ADMIN`** (in practice: root, already required for the egress floor on cloud
  nodes). This replaces the Docker stdio-attach and is identical across the Docker and CRI backends.
```

- [ ] **Step 2: Note the prereq in deployment.md**

In `deployment.md` §2.2 (Node host), add to the bullet list:

```markdown
- The spawnlet needs **`CAP_SYS_ADMIN`** (root) to `setns` into each pod's network namespace for the
  ACP socket bridge — on cloud nodes this is already required by the egress floor; on an unprivileged
  self-hosted dev node it is now required even with the floor disabled.
```

- [ ] **Step 3: Commit**

```bash
git add ISOLATION.md deployment.md
git commit --no-verify -m "docs: UDS ACP transport + setns/CAP_SYS_ADMIN node requirement (sp-7wa)"
```

---

## Self-Review

**1. Spec coverage (spec §3.3 + slice 1):**
- In-container adapter listening on `@spawnlet-acp`, bridging to `goose acp` stdio, exits on goose exit → Tasks 1–3. ✓
- `runtime.AttachACP(ctx, netnsPath)` with locked-thread setns + restore + dial → Task 5. ✓
- Half-close semantics → Task 1 Step 5 (adapter side) + `AttachedStream` carries the `*net.UnixConn` whose `CloseWrite` the relay can use; `Close` closes the conn. ✓
- Docker backend exposes the sidecar netns path; manager threads it → Task 6. ✓
- `ws.go` + `server.go` switch from `rt.Attach` to `AttachACP` → Task 7. ✓
- Unified across backends: the adapter + `AttachACP` are backend-agnostic; the Docker path uses them now, the CRI path (slice 3) reuses the same `AttachACP` with the pod netns path. ✓
- Host-gated real verification (no docker/root here) → Tasks 3/4 Step 5, Task 5 Steps 6–7. ✓

**2. Placeholder scan:** No TBD/TODO; every code step contains complete code; every run step has an exact command + expected result.

**3. Type consistency:** `serve(ln net.Listener, toAgent io.Writer, fromAgent io.Reader) error`, `connHub`/`pump` used consistently in Tasks 1–2. `AttachACP(ctx context.Context, netnsPath string) (*AttachedStream, error)` and `dialOnce(netnsPath, sock string)` consistent across Task 5 and the Task 7 attach func. `Spawn.NetnsPath` set in Task 6, read in Task 7. `Server.attach func(ctx, sp *Spawn) (*runtime.AttachedStream, error)` consistent across server.go/ws.go/ws_test.go. ✓

**4. Known consequence (flag at handoff):** the node now requires `CAP_SYS_ADMIN`/root for attach (Task 8). On cloud this is already true (egress floor); on an unprivileged self-hosted dev node it is newly required. Captured in docs; surface to the user — an alternative (a host-path-bind-mounted socket dialed without setns) would avoid the root-for-attach requirement but deviates from the approved abstract-socket design.
