# CRI Thin Byte-Bridge Adapter Implementation Plan (sp-j5b)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Strip the in-pod `acpadapter` (CRI/runsc lane) to a transparent byte-bridge with a bounded gap buffer, so the long-lived node pump owns all brokering/history/fan-out — making the CRI lane behave identically to the Docker lane.

**Architecture:** The adapter keeps goose alive as a subprocess, accepts one node connection at a time over `@spawnlet-acp`, and copies bytes raw both ways. Goose stdout produced while no node is attached is buffered (bounded, oldest-whole-line eviction) and flushed on reconnect. The adapter no longer parses ACP or synthesizes frames; the node pump does. The now-orphaned `internal/transcript` package is deleted.

**Tech Stack:** Go 1.26, `net` (abstract UNIX socket), `bufio`/`io`, hermetic `go test`.

**Source spec:** `docs/superpowers/specs/2026-06-03-cri-thin-adapter-pump-design.md`

**Workflow constraints:** Local-only repo, NO git remote, NO push. Commit with `git commit --no-verify` (the `.beads` export hook dirties commits). Do NOT touch `.beads/` via `git checkout`.

---

## Required Reading (do this first)

Before any edits, READ these in full — the plan reproduces their relevant shapes but you must confirm them:
- `deploy/agent/acpadapter/bridge.go` — current broker bridge (`connHub`, `pump`, `recordingCopy`, `serve`).
- `deploy/agent/acpadapter/bridge_test.go` — current tests + helpers (`fakeAgent`, `dialAndRoundtrip`, `scriptedAgent`).
- `deploy/agent/acpadapter/main.go` — calls `serve(ln, toAgent, fromAgent)`; this signature MUST stay unchanged.
- `internal/transcript/recorder.go` + `recorder_test.go` — the package being deleted.

Confirm with `grep -rn "internal/transcript" --include=*.go .` that the ONLY non-test importer is `deploy/agent/acpadapter/bridge.go`. If anything else imports it, STOP and report — the deletion in Task 2 assumes the adapter is the sole consumer.

---

## File Structure

| File | Responsibility | Change |
|------|----------------|--------|
| `deploy/agent/acpadapter/bridge.go` | Transparent byte-bridge: `connHub` (cur conn + bounded gap buffer), `pump` (stdout→hub), `serve` (accept→attach→copy→detach) | Rewrite |
| `deploy/agent/acpadapter/bridge_test.go` | Hermetic tests: raw-bytes bridge, reconnect persistence, no broker frames, gap-buffer flush, ring eviction | Rewrite |
| `deploy/agent/acpadapter/main.go` | Process entry: start agent, listen, `serve` | Unchanged (verify only) |
| `internal/transcript/` | (deleted) | Delete package + test |

No node-side changes: the pump already reaches the CRI lane via `mgr.Attach → AttachACP`. `deploy/agent/entrypoint.sh` is unchanged (`exec acpadapter goose acp`).

---

## Task 1: Rewrite the adapter to a thin byte-bridge

**Files:**
- Rewrite: `deploy/agent/acpadapter/bridge.go`
- Rewrite: `deploy/agent/acpadapter/bridge_test.go`

This is a same-package (`package main`) rewrite. The tests are white-box (they construct `connHub` directly and exercise `serve` over a real socket). Write the tests first (TDD), watch them fail against the current broker bridge, then replace `bridge.go`.

- [ ] **Step 1: Replace `bridge_test.go` with the new tests**

Overwrite `deploy/agent/acpadapter/bridge_test.go` with EXACTLY:

```go
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
	go func() { _, _ = io.Copy(outW, inR) }() // echo stdin -> stdout, lives for the whole test
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

// The adapter bridges raw bytes both ways and keeps the agent alive across a reconnect, so a NEW
// client reaches the SAME persistent agent.
func TestServeBridgesAndPersistsAcrossReconnect(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "acp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	toAgent, fromAgent := fakeAgent()
	go serve(ln, toAgent, fromAgent)

	dialAndRoundtrip(t, sock, "hello\n")
	dialAndRoundtrip(t, sock, "world\n")
}

// The thin adapter must NOT parse ACP or inject broker frames (spawn/turn, spawn/history). A
// session/prompt sent through is echoed back verbatim with nothing prepended or appended.
func TestServeDoesNotInjectBrokerFrames(t *testing.T) {
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
	prompt := `{"method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"hi"}]}}` + "\n"
	if _, err := io.WriteString(c, prompt); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	r := bufio.NewReader(c)
	got, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != prompt {
		t.Fatalf("first line got %q want the echoed prompt verbatim (no broker frame)", got)
	}
	// No second line should arrive — a broker would have injected a spawn/turn frame.
	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if extra, err := r.ReadString('\n'); err == nil {
		t.Fatalf("unexpected extra frame after the echo: %q", extra)
	}
}

// Goose output produced while no node is attached is buffered and flushed, IN ORDER, to the next
// connection — and live output after attach follows strictly behind the flushed buffer.
func TestConnHubBuffersWhileDetachedAndFlushesOnAttach(t *testing.T) {
	h := &connHub{}
	h.write([]byte("a\n")) // no conn attached -> buffered
	h.write([]byte("b\n"))

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	done := make(chan struct{})
	go func() {
		h.attach(server) // flushes "a\n","b\n" to server (blocks until read; net.Pipe is synchronous)
		h.write([]byte("c\n"))
		close(done)
	}()

	r := bufio.NewReader(client)
	for _, want := range []string{"a\n", "b\n", "c\n"} {
		_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
		got, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if got != want {
			t.Fatalf("got %q want %q (order must be buffered-then-live)", got, want)
		}
	}
	<-done
}

// Past the byte cap the gap buffer evicts OLDEST WHOLE LINES, never delivering a torn line.
func TestConnHubEvictsOldestWholeLinesPastCap(t *testing.T) {
	h := &connHub{}
	mkLine := func(c byte) []byte {
		b := make([]byte, 100*1024) // 102400 bytes; 12 of these = 1,228,800 > maxBufBytes (1,048,576)
		for i := range b {
			b[i] = c
		}
		b[len(b)-1] = '\n'
		return b
	}
	for i := 0; i < 12; i++ {
		h.write(mkLine(byte('A' + i)))
	}

	h.mu.Lock()
	n, count := h.n, len(h.buf)
	first := h.buf[0][0]
	bufCopy := append([][]byte(nil), h.buf...)
	h.mu.Unlock()

	if n > maxBufBytes {
		t.Fatalf("buffer holds %d bytes, exceeds cap %d", n, maxBufBytes)
	}
	// 1,228,800 -> evict A -> 1,126,400 (still > cap) -> evict B -> 1,024,000 (<= cap). 10 lines, first 'C'.
	if count != 10 {
		t.Fatalf("want 10 buffered lines after eviction, got %d", count)
	}
	if first != 'C' {
		t.Fatalf("oldest KEPT line should start with 'C' (A,B evicted), got %q", first)
	}
	for i, b := range bufCopy {
		if len(b) == 0 || b[len(b)-1] != '\n' {
			t.Fatalf("buffered line %d is torn (no trailing newline)", i)
		}
	}
}
```

- [ ] **Step 2: Run the new tests against the OLD bridge to confirm they fail**

Run: `cd /home/debian/AleCode/spawnery && go test ./deploy/agent/acpadapter/ 2>&1 | head -40`

Expected: FAIL — the build first fails because the deleted helper `scriptedAgent` and the removed tests no longer exist while old `bridge.go` is unchanged; even once it builds, `TestServeDoesNotInjectBrokerFrames` fails (the old broker injects a `spawn/turn` frame, so the assertion "no extra frame" trips) and `connHub` has no `write/attach/n/buf` fields matching the new tests (compile error). A compile failure here is an acceptable "red" — it proves the tests bind to the new shape.

- [ ] **Step 3: Replace `bridge.go` with the thin byte-bridge**

Overwrite `deploy/agent/acpadapter/bridge.go` with EXACTLY:

```go
package main

import (
	"bufio"
	"io"
	"net"
	"sync"
)

// maxBufBytes bounds the in-pod gap buffer. Goose stdout produced while no node is attached is held
// here and flushed to the next connection. Past the cap the OLDEST WHOLE LINES are evicted, so a
// wedged or absent node can never OOM the pod and a partial/torn JSON line is never delivered.
const maxBufBytes = 1 << 20 // 1 MiB

// connHub holds the at-most-one attached node connection plus a bounded buffer of goose stdout lines
// produced while nothing is attached. A single mutex serializes the live-vs-buffer decision in
// write() against the swap+flush in attach(), so byte order is preserved with no interleaving.
type connHub struct {
	mu  sync.Mutex
	cur net.Conn
	buf [][]byte // whole ndjson lines held while cur == nil, FIFO
	n   int      // total bytes currently in buf
}

// write sends one ndjson line to the attached node connection, or buffers it if none is attached.
func (h *connHub) write(line []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cur != nil {
		_, _ = h.cur.Write(line) // a dead conn's Write returns fast; detach() swaps cur to nil
		return
	}
	b := append([]byte(nil), line...)
	h.buf = append(h.buf, b)
	h.n += len(b)
	for h.n > maxBufBytes && len(h.buf) > 1 { // evict oldest whole lines; never drop the only line
		h.n -= len(h.buf[0])
		h.buf = h.buf[1:]
	}
}

// attach makes c the current connection and flushes the gap buffer to it FIRST, in order, then
// clears it. Returns the displaced connection (if any) for the caller to close.
//
// LOCK TRADEOFF: the flush runs while holding h.mu. That is deliberate. Holding the lock across
// "set cur; flush buf; clear buf" guarantees strict ordering — any concurrent write() is forced
// either before the flush (it appended to buf, so it gets flushed) or after it (cur != nil, so it
// goes live, strictly behind the flushed bytes). No live line slips in front of the buffer and
// nothing interleaves. The cost is head-of-line blocking: while the flush runs, write() cannot take
// the lock, so the stdout pump stops draining goose and a slow reattaching node briefly stalls the
// agent's stdout. We accept that because the flush is bounded (<= maxBufBytes) and goes to a local
// abstract UDS only on reconnect — tiny and rare. The off-lock alternative (snapshot under lock,
// write outside it, queue concurrent writes behind a flushing flag) removes the stall at a
// complexity cost not worth it here.
func (h *connHub) attach(c net.Conn) net.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	prev := h.cur
	h.cur = c
	for _, line := range h.buf {
		_, _ = c.Write(line)
	}
	h.buf = nil
	h.n = 0
	if prev == c {
		return nil
	}
	return prev
}

// detach clears the current connection if it is still c (so subsequent stdout buffers) and closes c.
func (h *connHub) detach(c net.Conn) {
	h.mu.Lock()
	if h.cur == c {
		h.cur = nil
	}
	h.mu.Unlock()
	_ = c.Close()
}

// pump is the single persistent reader of the agent's stdout. It forwards each ndjson line to the
// attached node connection (or the gap buffer). It does NOT parse or record — the node pump owns all
// brokering, history, and fan-out.
func pump(fromAgent io.Reader, hub *connHub) {
	br := bufio.NewReaderSize(fromAgent, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			hub.write(line)
		}
		if err != nil {
			return
		}
	}
}

// serve accepts one node connection at a time and bridges it to the long-lived agent stdio: goose
// stdout flows to the connection (pump), the connection flows to goose stdin (io.Copy). The agent
// persists across connection drops; output produced during a gap is buffered and flushed on the next
// attach. It returns only when the listener is closed.
func serve(ln net.Listener, toAgent io.Writer, fromAgent io.Reader) error {
	hub := &connHub{}
	go pump(fromAgent, hub)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		if prev := hub.attach(conn); prev != nil {
			_ = prev.Close()
		}
		_, _ = io.Copy(toAgent, conn) // node -> goose stdin; returns when the node closes its conn
		hub.detach(conn)
	}
}
```

- [ ] **Step 4: Run the adapter tests and confirm they pass**

Run: `cd /home/debian/AleCode/spawnery && go test ./deploy/agent/acpadapter/ -v 2>&1 | tail -30`

Expected: PASS — `TestServeBridgesAndPersistsAcrossReconnect`, `TestServeDoesNotInjectBrokerFrames`, `TestConnHubBuffersWhileDetachedAndFlushesOnAttach`, `TestConnHubEvictsOldestWholeLinesPastCap` all ok.

- [ ] **Step 5: Run with the race detector**

Run: `cd /home/debian/AleCode/spawnery && go test -race ./deploy/agent/acpadapter/ 2>&1 | tail -15`

Expected: PASS, no data-race reports (the `connHub` flush test exercises concurrent `attach`/`write`).

- [ ] **Step 6: Commit**

```bash
cd /home/debian/AleCode/spawnery
git add deploy/agent/acpadapter/bridge.go deploy/agent/acpadapter/bridge_test.go
git commit --no-verify -m "feat(acpadapter): thin byte-bridge + gap buffer; pump owns brokering [sp-j5b]"
```

---

## Task 2: Delete the orphaned `internal/transcript` package

The adapter was its only consumer (verified in Required Reading). Removing the broker logic from `bridge.go` orphans the whole package.

**Files:**
- Delete: `internal/transcript/recorder.go`
- Delete: `internal/transcript/recorder_test.go`

- [ ] **Step 1: Re-confirm there are no remaining importers**

Run: `cd /home/debian/AleCode/spawnery && grep -rn "internal/transcript" --include=*.go .`

Expected: NO output (Task 1 removed the adapter's import). If anything prints, STOP and report — do not delete a package still in use.

- [ ] **Step 2: Delete the package**

```bash
cd /home/debian/AleCode/spawnery
git rm internal/transcript/recorder.go internal/transcript/recorder_test.go
```

(If the directory still contains other files, STOP and report — the plan assumed only these two.)

- [ ] **Step 3: Build the whole module**

Run: `cd /home/debian/AleCode/spawnery && go build ./... 2>&1 | head -20`

Expected: clean build, no output. A "package transcript is not in std" or "no Go files" error means a dangling import remains — fix it before continuing.

- [ ] **Step 4: Run the full test suite**

Run: `cd /home/debian/AleCode/spawnery && go test ./... 2>&1 | tail -30`

Expected: all packages `ok` or `no test files`. No `internal/transcript` line should appear. Host-gated suites (build-tagged e2e/egress) are skipped as usual.

- [ ] **Step 5: Commit**

```bash
cd /home/debian/AleCode/spawnery
git commit --no-verify -m "chore: delete orphaned internal/transcript (pump owns history now) [sp-j5b]"
```

---

## Task 3: Confirm the half-close test and `scriptedAgent` helper are gone

The rewrite in Task 1 already dropped `TestServeClientHalfCloseStopsStdinNotStdout` and `TestServeReplaysHistoryOnReconnect`. The `scriptedAgent` helper those used must not linger as dead code.

**Files:**
- Verify: `deploy/agent/acpadapter/bridge_test.go`, `deploy/agent/acpadapter/main_test.go`

- [ ] **Step 1: Search for orphaned helpers/tests**

Run: `cd /home/debian/AleCode/spawnery && grep -rn "scriptedAgent\|HalfClose\|ReplaysHistory\|spawn/history\|spawn/turn" deploy/agent/acpadapter/`

Expected: NO output. If `scriptedAgent` is still defined (e.g. in `main_test.go`) but now unused, delete its definition. If it is still referenced by a surviving test in `main_test.go`, leave it and note that in your report.

- [ ] **Step 2: Vet the package**

Run: `cd /home/debian/AleCode/spawnery && go vet ./deploy/agent/acpadapter/ 2>&1 | head`

Expected: no output (no unused-helper or other vet complaints).

- [ ] **Step 3: Commit any cleanup (only if Step 1 found something)**

```bash
cd /home/debian/AleCode/spawnery
git add deploy/agent/acpadapter/
git commit --no-verify -m "chore(acpadapter): drop orphaned scriptedAgent test helper [sp-j5b]"
```

If Step 1 was clean, skip this commit and note "no orphaned helpers" in your report.

---

## Done criteria

- `deploy/agent/acpadapter/bridge.go` is a transparent byte-bridge: no ACP parsing, no `spawn/turn`/`spawn/history` frames, no `transcript` import.
- The gap buffer is bounded at `maxBufBytes` with oldest-whole-line eviction; `connHub.attach` carries the lock-tradeoff comment.
- `internal/transcript` is deleted; `go build ./...` and `go test ./...` are green.
- No node-side, `main.go`, or `entrypoint.sh` changes.
