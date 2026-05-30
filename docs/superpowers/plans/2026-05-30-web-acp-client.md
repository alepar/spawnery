# Web ACP Client (demo) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A React+TS SPA that talks directly to the spawnlet — hardcoded `CreateSpawn` for `secret-app`, a WebSocket session reusing the existing transparent `Relay`, and a demo-rich ACP chat UI (chat + tool-calls + thoughts + permission modal).

**Architecture:** Add a `GET /ws/session` WebSocket endpoint to the spawnlet that reuses the existing `Relay` (transparent bytes; browser does ACP ndjson framing). The browser is the ACP client: a small `acp/` TS module (Conn over WS + Client) drives `initialize`/`session/new`/`session/prompt` and renders streamed `session/update`s; `CreateSpawn`/`StopSpawn` go via plain `fetch` (Connect-JSON unary) against the existing handler. Vite dev-proxies both to the spawnlet (one origin, no CORS).

**Tech Stack:** Go 1.25 + `github.com/coder/websocket`; React 18 + TypeScript + Vite + Vitest.

**Spec:** `docs/superpowers/specs/2026-05-30-web-acp-client-design.md` (authoritative).

---

## Conventions
- Branch: `feat/web-acp-client`. Beads prefix `sp`; mark the milestone in_progress at Task 1, close after Task 8. No TodoWrite.
- TDD where tractable (Go WS endpoint, `acp/` TS module). React UI: complete code + manual e2e. Co-Authored-By trailer on every commit: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- **Go unit suite** (`go test ./...`) stays Docker/key-free. The **WS Go e2e** is `//go:build e2e` and fails loudly. **TS unit** (`cd web && npm test`) is hermetic.
- No git remote — commit only.

## File Structure
```
internal/spawnlet/ws.go            NEW  Server.HandleWS — WS<->Relay bridge
internal/spawnlet/ws_test.go       NEW  Docker-free WS relay test (fake runtime loopback)
internal/spawnlet/ws_e2e_test.go   NEW  //go:build e2e — real stub agent over WS
internal/spawnlet/manager.go       MOD  resolve relative appPath -> absolute (browser can't send abs)
cmd/spawnlet/main.go               MOD  mux.HandleFunc("/ws/session", srv.HandleWS)

web/package.json                   NEW
web/tsconfig.json                  NEW
web/vite.config.ts                 NEW  React plugin + dev proxy (/spawn.v1.SpawnService, /ws)
web/index.html                     NEW
web/src/main.tsx                   NEW
web/src/api/spawnlet.ts            NEW  createSpawn/stopSpawn (fetch Connect-JSON)
web/src/acp/types.ts               NEW  JSON-RPC + session/update types
web/src/acp/conn.ts                NEW  Conn over WebSocket (ndjson framing)
web/src/acp/conn.test.ts           NEW
web/src/acp/client.ts              NEW  Client: initialize/newSession/prompt/permission
web/src/acp/client.test.ts         NEW
web/src/ui/App.tsx                 NEW  spawn-on-mount, WS, chat wiring
web/src/ui/ChatLog.tsx             NEW
web/src/ui/ToolCallChip.tsx        NEW
web/src/ui/Thoughts.tsx            NEW
web/src/ui/PermissionModal.tsx     NEW
web/src/ui/PromptInput.tsx         NEW
web/src/ui/app.css                 NEW
```

---

## Task 1: Spawnlet WS endpoint + relative-appPath + Docker-free test

**Files:** Create `internal/spawnlet/ws.go`, `internal/spawnlet/ws_test.go`; modify `internal/spawnlet/manager.go`, `cmd/spawnlet/main.go`.

- [ ] **Step 1: Add the WS dep**

Run: `go get github.com/coder/websocket@v1.8.12 && go mod tidy`

- [ ] **Step 2: Resolve relative appPath** — in `internal/spawnlet/manager.go`, at the very top of `Create` (before `manifest.Parse`), add:
```go
	if abs, err := filepath.Abs(appPath); err == nil {
		appPath = abs
	}
```
(`path/filepath` is already imported.) This lets the browser send `examples/secret-app` (resolved against the spawnlet's CWD).

- [ ] **Step 3: Write the failing WS test** — `internal/spawnlet/ws_test.go`:
```go
package spawnlet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"spawnery/internal/runtime"
)

func TestWSRelayEchoesViaFake(t *testing.T) {
	f := runtime.NewFake()
	m := NewManager(f, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	srv := NewServer(m)
	sp, err := m.Create(context.Background(), "ws-1", writeApp(t), "x") // writeApp from manager_test.go
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws/session", srv.HandleWS)
	hs := httptest.NewServer(mux)
	defer hs.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+hs.URL[len("http"):]+"/ws/session", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	// bind, then send a frame; the fake runtime's Attach loops stdin->stdout, so it echoes.
	conn.Write(ctx, websocket.MessageText, []byte(`{"spawnId":"`+sp.ID+`"}`))
	conn.Write(ctx, websocket.MessageBinary, []byte("hello\n"))
	_, got, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello\n" {
		t.Fatalf("want echo, got %q", got)
	}
}
```

- [ ] **Step 4: Run red** — `go test ./internal/spawnlet/ -run TestWSRelayEchoes -v` → FAIL (undefined: HandleWS).

- [ ] **Step 5: Implement** — `internal/spawnlet/ws.go`:
```go
package spawnlet

import (
	"encoding/json"
	"net/http"

	"github.com/coder/websocket"
)

// HandleWS bridges a browser WebSocket to a spawn's agent stdio via the
// transparent Relay. First message: {"spawnId":"..."} (text); then raw ACP bytes
// in both directions. Same byte relay as the ConnectRPC Session, different transport.
func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"}, // dev only; tighten when CP/auth lands
	})
	if err != nil {
		return
	}
	conn.SetReadLimit(16 * 1024 * 1024)
	ctx := r.Context()
	defer conn.CloseNow()

	_, first, err := conn.Read(ctx)
	if err != nil {
		return
	}
	var bind struct {
		SpawnID string `json:"spawnId"`
	}
	if err := json.Unmarshal(first, &bind); err != nil {
		conn.Close(websocket.StatusUnsupportedData, "bad bind frame")
		return
	}
	sp, ok := s.m.Store().Get(bind.SpawnID)
	if !ok {
		conn.Close(websocket.StatusPolicyViolation, "unknown spawn")
		return
	}
	att, err := s.rt.Attach(ctx, sp.AgentID)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "attach failed")
		return
	}
	defer att.Close()

	ep := StreamEndpoint{
		Recv: func() ([]byte, error) {
			_, b, err := conn.Read(ctx)
			return b, err
		},
		Send: func(b []byte) error {
			return conn.Write(ctx, websocket.MessageBinary, b)
		},
	}
	Relay(ctx, ep, AgentIO{Stdin: att.Stdin, Stdout: att.Stdout})
	conn.Close(websocket.StatusNormalClosure, "")
}
```
> `Server` already has `m *Manager` and `rt runtime.ContainerRuntime` (see `server.go`).

- [ ] **Step 6: Mount it** — in `cmd/spawnlet/main.go`, after the connect handler is registered on `mux` and before wrapping in `h2c`, add:
```go
	mux.HandleFunc("/ws/session", srv.HandleWS)
```
(Use the existing `srv`/`mux` variable names; if the server isn't held in a variable, assign `srv := spawnlet.NewServer(mgr)` and pass it to both the connect handler and this line.)

- [ ] **Step 7: Run green** — `go test ./internal/spawnlet/ -v` (all pass) and `go build ./...`.
- [ ] **Step 8: Commit**
```bash
git add internal/spawnlet/ws.go internal/spawnlet/ws_test.go internal/spawnlet/manager.go cmd/spawnlet/main.go go.mod go.sum
git commit -m "feat(spawnlet): WebSocket session endpoint reusing the transparent Relay

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: WS e2e through the real stub agent

**Files:** Create `internal/spawnlet/ws_e2e_test.go`.

- [ ] **Step 1: Write the e2e test** — `internal/spawnlet/ws_e2e_test.go`:
```go
//go:build e2e

package spawnlet_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	spawnv1 "spawnery/gen/spawn/v1"
	"spawnery/gen/spawn/v1/spawnv1connect"
	"spawnery/internal/acp"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
)

// wsIO adapts a websocket.Conn to io.Reader/io.Writer for acp.Client.
type wsIO struct {
	ctx  context.Context
	conn *websocket.Conn
	buf  []byte
}

func (a *wsIO) Read(p []byte) (int, error) {
	for len(a.buf) == 0 {
		_, b, err := a.conn.Read(a.ctx)
		if err != nil {
			return 0, err
		}
		a.buf = b
	}
	n := copy(p, a.buf)
	a.buf = a.buf[n:]
	return n, nil
}
func (a *wsIO) Write(p []byte) (int, error) {
	if err := a.conn.Write(a.ctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func TestWSEndToEndStub(t *testing.T) {
	rt, err := runtime.NewDocker()
	if err != nil || rt.Ping(context.Background()) != nil {
		t.Fatal("docker required for e2e")
	}
	mgr := spawnlet.NewManager(rt, spawnlet.ManagerConfig{
		AgentImage: "spawnery/stubagent:dev", SidecarImage: "spawnery/sidecar:dev",
		OpenRouterKey: "unused", DataRoot: t.TempDir(),
	})
	srv := spawnlet.NewServer(mgr)
	mux := http.NewServeMux()
	mux.Handle(spawnv1connect.NewSpawnServiceHandler(srv))
	mux.HandleFunc("/ws/session", srv.HandleWS)
	hs := httptest.NewServer(h2c.NewHandler(mux, &http2.Server{}))
	defer hs.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hc := &http.Client{}
	cl := spawnv1connect.NewSpawnServiceClient(hs.Client(), hs.URL) // Connect protocol, unary
	cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&spawnv1.CreateSpawnRequest{
		AppPath: mustAbsWS(t, "../../examples/secret-app"), Model: "x",
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := cs.Msg.SpawnId
	defer cl.StopSpawn(ctx, connect.NewRequest(&spawnv1.StopSpawnRequest{SpawnId: id}))
	_ = hc

	conn, _, err := websocket.Dial(ctx, "ws"+hs.URL[len("http"):]+"/ws/session", nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 * 1024 * 1024)
	conn.Write(ctx, websocket.MessageText, []byte(`{"spawnId":"`+id+`"}`))

	c := acp.NewClient(&wsIO{ctx: ctx, conn: conn}, &wsIO{ctx: ctx, conn: conn})
	if err := c.Initialize(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := c.NewSession("/app"); err != nil {
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

func mustAbsWS(t *testing.T, rel string) string {
	t.Helper()
	abs, err := io.WriteString // placeholder to force import; replaced below
	_ = abs
	_ = err
	return mustAbs(t, rel) // reuse helper from e2e_test.go (same _test package)
}
```
> Remove the `io` placeholder lines — the test reuses `mustAbs` from `e2e_test.go` (same `spawnlet_test` package, both `//go:build e2e`). Keep the `wsIO` adapter. Two `wsIO` instances share one `conn` (one reads, one writes — safe; coder/websocket allows concurrent read+write). If the stub uses `/app` cwd it ignores it and just echoes.

- [ ] **Step 2: Build images if needed + run**
```bash
docker build -t spawnery/stubagent:dev -f deploy/stubagent/Dockerfile .
docker build -t spawnery/sidecar:dev   -f deploy/sidecar/Dockerfile .
go test -tags e2e ./internal/spawnlet/ -run TestWSEndToEndStub -v -timeout 120s
```
Expected: PASS (the stub echoes `ECHO: hello` back through the WS relay).

- [ ] **Step 3: Commit**
```bash
git add internal/spawnlet/ws_e2e_test.go
git commit -m "test(spawnlet): WS e2e — stub agent round-trip over WebSocket

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: web/ scaffold (Vite + React + TS + Vitest + proxy)

**Files:** Create `web/package.json`, `web/tsconfig.json`, `web/vite.config.ts`, `web/index.html`, `web/src/main.tsx`, `web/src/ui/App.tsx` (stub).

- [ ] **Step 1: package.json** — `web/package.json`:
```json
{
  "name": "spawnery-web",
  "private": true,
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "tsc -b && vite build",
    "test": "vitest run"
  },
  "dependencies": {
    "react": "^18.3.1",
    "react-dom": "^18.3.1"
  },
  "devDependencies": {
    "@types/react": "^18.3.12",
    "@types/react-dom": "^18.3.1",
    "@vitejs/plugin-react": "^4.3.4",
    "typescript": "^5.6.3",
    "vite": "^5.4.11",
    "vitest": "^2.1.8"
  }
}
```

- [ ] **Step 2: configs** — `web/tsconfig.json`:
```json
{
  "compilerOptions": {
    "target": "ES2020", "useDefineForClassFields": true, "lib": ["ES2020", "DOM", "DOM.Iterable"],
    "module": "ESNext", "skipLibCheck": true, "moduleResolution": "bundler",
    "strict": true, "jsx": "react-jsx", "noEmit": true, "esModuleInterop": true
  },
  "include": ["src"]
}
```
`web/vite.config.ts`:
```ts
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/spawn.v1.SpawnService": { target: "http://127.0.0.1:9090", changeOrigin: true },
      "/ws": { target: "http://127.0.0.1:9090", ws: true, changeOrigin: true },
    },
  },
});
```
`web/index.html`:
```html
<!doctype html>
<html><head><meta charset="utf-8"><title>Spawnery</title></head>
<body><div id="root"></div><script type="module" src="/src/main.tsx"></script></body></html>
```

- [ ] **Step 3: entry + stub App** — `web/src/main.tsx`:
```tsx
import { createRoot } from "react-dom/client";
import { App } from "./ui/App";
createRoot(document.getElementById("root")!).render(<App />);
```
`web/src/ui/App.tsx` (temporary stub, replaced in Task 7):
```tsx
export function App() {
  return <div>Spawnery web — scaffolding</div>;
}
```

- [ ] **Step 4: Install + verify**
```bash
cd web && npm install && npm run build && npm test || true
```
Expected: `npm install` succeeds, `vite build` produces `dist/`. (`npm test` finds no tests yet — that's fine; it'll pass with 0 tests or print "no test files".)

- [ ] **Step 5: Commit**
```bash
git add web/package.json web/package-lock.json web/tsconfig.json web/vite.config.ts web/index.html web/src/main.tsx web/src/ui/App.tsx
git commit -m "feat(web): Vite+React+TS scaffold with spawnlet dev proxy

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: acp/ — types + Conn (WS ndjson framing)

**Files:** Create `web/src/acp/types.ts`, `web/src/acp/conn.ts`, `web/src/acp/conn.test.ts`.

- [ ] **Step 1: Failing test** — `web/src/acp/conn.test.ts`:
```ts
import { describe, it, expect } from "vitest";
import { Conn } from "./conn";
import type { Message } from "./types";

// Minimal WebSocket-like fake; we drive onmessage manually.
class FakeWS {
  binaryType = "arraybuffer";
  onmessage: ((ev: { data: any }) => void) | null = null;
  sent: Uint8Array[] = [];
  send(data: Uint8Array) { this.sent.push(data); }
  feed(s: string) { this.onmessage?.({ data: new TextEncoder().encode(s) }); }
}

describe("Conn", () => {
  it("splits ndjson across chunk boundaries", () => {
    const ws = new FakeWS();
    const got: Message[] = [];
    const c = new Conn(ws as any, (m) => got.push(m));
    ws.feed('{"jsonrpc":"2.0","id":1,"result":{}}\n{"method":"sessio');
    ws.feed('n/update","params":{"x":1}}\n');
    expect(got.length).toBe(2);
    expect(got[0].id).toBe(1);
    expect(got[1].method).toBe("session/update");
    c.send({ id: 2, method: "ping" });
    const sentText = new TextDecoder().decode(ws.sent[0]);
    expect(sentText).toBe('{"jsonrpc":"2.0","id":2,"method":"ping"}\n');
  });
});
```

- [ ] **Step 2: Run red** — `cd web && npx vitest run src/acp/conn.test.ts` → FAIL (no Conn).

- [ ] **Step 3: Implement** — `web/src/acp/types.ts`:
```ts
export interface RpcError { code: number; message: string; data?: unknown }

export interface Message {
  jsonrpc?: string;
  id?: number;
  method?: string;
  params?: any;
  result?: any;
  error?: RpcError;
}

// session/update notification: { sessionId, update: { sessionUpdate, ... } }
export interface SessionUpdate {
  sessionId: string;
  update: {
    sessionUpdate: string; // agent_message_chunk | agent_thought_chunk | tool_call | tool_call_update
    content?: { type: string; text?: string };
    toolCallId?: string;
    title?: string;
    status?: string;
  };
}
```
`web/src/acp/conn.ts`:
```ts
import type { Message } from "./types";

// WebSocketLike is the subset of WebSocket we use (so tests can fake it).
export interface WebSocketLike {
  binaryType: string;
  onmessage: ((ev: { data: any }) => void) | null;
  send(data: Uint8Array): void;
}

// Conn frames ACP JSON-RPC messages over a WebSocket as newline-delimited JSON.
// Incoming binary/text chunks are buffered and split on "\n"; outgoing messages
// are json+"\n" sent as one binary frame each.
export class Conn {
  private buf = "";
  private dec = new TextDecoder();
  private enc = new TextEncoder();

  constructor(private ws: WebSocketLike, private onMessage: (m: Message) => void) {
    ws.binaryType = "arraybuffer";
    ws.onmessage = (ev) => this.onData(ev.data);
  }

  private onData(data: any) {
    const text =
      typeof data === "string" ? data : this.dec.decode(new Uint8Array(data as ArrayBuffer));
    this.buf += text;
    let i: number;
    while ((i = this.buf.indexOf("\n")) >= 0) {
      const line = this.buf.slice(0, i);
      this.buf = this.buf.slice(i + 1);
      if (line.trim()) this.onMessage(JSON.parse(line) as Message);
    }
  }

  send(m: Message) {
    const obj = { jsonrpc: "2.0", ...m };
    this.ws.send(this.enc.encode(JSON.stringify(obj) + "\n"));
  }
}
```

- [ ] **Step 4: Run green** — `cd web && npx vitest run src/acp/conn.test.ts` → PASS.
- [ ] **Step 5: Commit**
```bash
git add web/src/acp/types.ts web/src/acp/conn.ts web/src/acp/conn.test.ts
git commit -m "feat(web/acp): JSON-RPC types + Conn (WS ndjson framing)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: acp/ — Client (initialize / newSession / prompt / permission)

**Files:** Create `web/src/acp/client.ts`, `web/src/acp/client.test.ts`.

- [ ] **Step 1: Failing test** — `web/src/acp/client.test.ts`:
```ts
import { describe, it, expect } from "vitest";
import { Client } from "./client";
import type { WebSocketLike } from "./conn";

// A fake WS that lets the test capture sent messages and inject incoming ones.
class FakeWS implements WebSocketLike {
  binaryType = "arraybuffer";
  onmessage: ((ev: { data: any }) => void) | null = null;
  sent: any[] = [];
  send(data: Uint8Array) {
    this.sent.push(JSON.parse(new TextDecoder().decode(data).trim()));
  }
  inject(m: any) {
    this.onmessage?.({ data: new TextEncoder().encode(JSON.stringify(m) + "\n") });
  }
}

describe("Client", () => {
  it("resolves a call when the matching response arrives", async () => {
    const ws = new FakeWS();
    const c = new Client(ws);
    const p = c.initialize();
    expect(ws.sent[0].method).toBe("initialize");
    ws.inject({ id: ws.sent[0].id, result: {} });
    await p;
  });

  it("streams session/update chunks and resolves prompt", async () => {
    const ws = new FakeWS();
    const c = new Client(ws);
    ws.inject({ id: (c as any).nid + 1, result: { sessionId: "s1" } }); // not yet; do via newSession
    // newSession
    const ns = c.newSession("/app");
    ws.inject({ id: ws.sent.at(-1).id, result: { sessionId: "s1" } });
    await ns;

    const chunks: string[] = [];
    const pr = c.prompt("hi", { onText: (t) => chunks.push(t) });
    const promptId = ws.sent.at(-1).id;
    ws.inject({ method: "session/update", params: { sessionId: "s1", update: { sessionUpdate: "agent_message_chunk", content: { type: "text", text: "ECHO: hi" } } } });
    ws.inject({ id: promptId, result: { stopReason: "end_turn" } });
    await pr;
    expect(chunks.join("")).toContain("ECHO: hi");
  });

  it("answers a permission request via the handler", async () => {
    const ws = new FakeWS();
    const c = new Client(ws);
    const ns = c.newSession("/app");
    ws.inject({ id: ws.sent.at(-1).id, result: { sessionId: "s1" } });
    await ns;

    let asked = false;
    const pr = c.prompt("go", { requestPermission: async () => { asked = true; return true; } });
    const promptId = ws.sent.at(-1).id;
    ws.inject({ id: 999, method: "session/request_permission", params: { sessionId: "s1", options: [{ optionId: "allow", name: "Allow", kind: "allow_once" }] } });
    // the client should have responded to id 999
    await new Promise((r) => setTimeout(r, 0));
    const resp = ws.sent.find((m) => m.id === 999);
    expect(asked).toBe(true);
    expect(resp.result.outcome.outcome).toBe("selected");
    ws.inject({ id: promptId, result: { stopReason: "end_turn" } });
    await pr;
  });
});
```

- [ ] **Step 2: Run red** — `cd web && npx vitest run src/acp/client.test.ts` → FAIL.

- [ ] **Step 3: Implement** — `web/src/acp/client.ts`:
```ts
import { Conn, type WebSocketLike } from "./conn";
import type { Message, SessionUpdate } from "./types";

export interface PromptHandlers {
  onText?: (t: string) => void;
  onThought?: (t: string) => void;
  onToolCall?: (tc: { id: string; title: string; status?: string }) => void;
  onToolUpdate?: (tc: { id: string; status?: string }) => void;
  // return true to allow, false to deny
  requestPermission?: (req: any) => Promise<boolean>;
}

export class Client {
  private conn: Conn;
  private nid = 0;
  private sessionId = "";
  private pending = new Map<number, (m: Message) => void>();
  private handlers: PromptHandlers = {};

  constructor(ws: WebSocketLike) {
    this.conn = new Conn(ws, (m) => this.route(m));
  }

  private next() { return ++this.nid; }

  private call(method: string, params: any): Promise<Message> {
    const id = this.next();
    return new Promise((resolve, reject) => {
      this.pending.set(id, (m) => {
        if (m.error) reject(new Error(`acp ${method}: ${m.error.code} ${m.error.message}`));
        else resolve(m);
      });
      this.conn.send({ id, method, params });
    });
  }

  private route(m: Message) {
    if (m.method === "session/update") {
      this.dispatchUpdate(m.params as SessionUpdate);
      return;
    }
    if (m.method === "session/request_permission" && m.id != null) {
      this.handlePermission(m);
      return;
    }
    if (m.id != null && this.pending.has(m.id)) {
      const r = this.pending.get(m.id)!;
      this.pending.delete(m.id);
      r(m);
    }
  }

  private dispatchUpdate(p: SessionUpdate) {
    const u = p.update;
    switch (u.sessionUpdate) {
      case "agent_message_chunk":
        if (u.content?.text) this.handlers.onText?.(u.content.text);
        break;
      case "agent_thought_chunk":
        if (u.content?.text) this.handlers.onThought?.(u.content.text);
        break;
      case "tool_call":
        this.handlers.onToolCall?.({ id: u.toolCallId ?? "", title: u.title ?? "tool", status: u.status });
        break;
      case "tool_call_update":
        this.handlers.onToolUpdate?.({ id: u.toolCallId ?? "", status: u.status });
        break;
    }
  }

  private async handlePermission(m: Message) {
    const allow = this.handlers.requestPermission ? await this.handlers.requestPermission(m.params) : true;
    const options: Array<{ optionId: string; kind?: string }> = m.params?.options ?? [];
    // pick an allow-ish option for allow, a reject-ish one for deny; fall back to first option.
    const pick = (want: string[]) =>
      options.find((o) => want.some((w) => (o.kind ?? "").includes(w)))?.optionId ?? options[0]?.optionId ?? "";
    const outcome = allow
      ? { outcome: "selected", optionId: pick(["allow"]) }
      : { outcome: "selected", optionId: pick(["reject", "deny"]) };
    this.conn.send({ id: m.id, result: { outcome } });
  }
  // NOTE: confirm the exact session/request_permission response shape against the ACP spec
  // during the live Goose run; the secret-word demo may not trigger a permission request at all.

  async initialize(): Promise<void> {
    await this.call("initialize", { protocolVersion: 1, clientCapabilities: {} });
  }

  async newSession(cwd: string): Promise<void> {
    const m = await this.call("session/new", { cwd, mcpServers: [] });
    this.sessionId = m.result?.sessionId ?? "";
  }

  async prompt(text: string, handlers: PromptHandlers): Promise<void> {
    this.handlers = handlers;
    await this.call("session/prompt", { sessionId: this.sessionId, prompt: [{ type: "text", text }] });
  }
}
```

- [ ] **Step 4: Run green** — `cd web && npx vitest run` → all PASS.
- [ ] **Step 5: Commit**
```bash
git add web/src/acp/client.ts web/src/acp/client.test.ts
git commit -m "feat(web/acp): ACP Client (initialize/newSession/prompt + permission)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: api/ — spawnlet unary client

**Files:** Create `web/src/api/spawnlet.ts`.

- [ ] **Step 1: Implement** — `web/src/api/spawnlet.ts`:
```ts
// Calls the spawnlet's ConnectRPC unary methods via plain fetch (Connect JSON).
// Proto field names are camelCase in Connect JSON. The Vite dev proxy maps
// /spawn.v1.SpawnService/* to the spawnlet, so these are same-origin (no CORS).

async function unary<T>(method: string, body: unknown): Promise<T> {
  const res = await fetch(`/spawn.v1.SpawnService/${method}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "Connect-Protocol-Version": "1" },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    throw new Error(`${method} failed: ${res.status} ${await res.text()}`);
  }
  return (await res.json()) as T;
}

export async function createSpawn(appPath: string, model: string): Promise<string> {
  const r = await unary<{ spawnId: string }>("CreateSpawn", { appPath, model });
  return r.spawnId;
}

export async function stopSpawn(spawnId: string): Promise<void> {
  await unary<Record<string, never>>("StopSpawn", { spawnId });
}
```

- [ ] **Step 2: Typecheck** — `cd web && npx tsc -b` → no errors.
- [ ] **Step 3: Commit**
```bash
git add web/src/api/spawnlet.ts
git commit -m "feat(web/api): spawnlet createSpawn/stopSpawn via Connect-JSON fetch

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: ui/ — chat components + App wiring

**Files:** Create `web/src/ui/ChatLog.tsx`, `ToolCallChip.tsx`, `Thoughts.tsx`, `PermissionModal.tsx`, `PromptInput.tsx`, `app.css`; replace `web/src/ui/App.tsx`.

- [ ] **Step 1: Leaf components**

`web/src/ui/ToolCallChip.tsx`:
```tsx
export function ToolCallChip({ title, status }: { title: string; status?: string }) {
  return <div className="chip">🔧 {title}{status ? ` — ${status}` : ""}</div>;
}
```
`web/src/ui/Thoughts.tsx`:
```tsx
import { useState } from "react";
export function Thoughts({ text }: { text: string }) {
  const [open, setOpen] = useState(false);
  if (!text) return null;
  return (
    <div className="thoughts">
      <button onClick={() => setOpen((v) => !v)}>{open ? "▾" : "▸"} thinking</button>
      {open && <pre>{text}</pre>}
    </div>
  );
}
```
`web/src/ui/PromptInput.tsx`:
```tsx
import { useState } from "react";
export function PromptInput({ disabled, onSend }: { disabled: boolean; onSend: (t: string) => void }) {
  const [t, setT] = useState("");
  const send = () => { if (t.trim()) { onSend(t); setT(""); } };
  return (
    <div className="input">
      <textarea value={t} disabled={disabled} placeholder="Ask the agent…"
        onChange={(e) => setT(e.target.value)}
        onKeyDown={(e) => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); } }} />
      <button disabled={disabled} onClick={send}>Send</button>
    </div>
  );
}
```
`web/src/ui/PermissionModal.tsx`:
```tsx
export function PermissionModal({ title, onResolve }: { title: string; onResolve: (allow: boolean) => void }) {
  return (
    <div className="modal-backdrop">
      <div className="modal">
        <p>The agent requests permission: <b>{title}</b></p>
        <button onClick={() => onResolve(true)}>Allow</button>
        <button onClick={() => onResolve(false)}>Deny</button>
      </div>
    </div>
  );
}
```

- [ ] **Step 2: ChatLog** — `web/src/ui/ChatLog.tsx`:
```tsx
import { ToolCallChip } from "./ToolCallChip";
import { Thoughts } from "./Thoughts";

export type Item =
  | { kind: "user"; text: string }
  | { kind: "agent"; text: string }
  | { kind: "tool"; title: string; status?: string }
  | { kind: "thought"; text: string };

export function ChatLog({ items }: { items: Item[] }) {
  return (
    <div className="log">
      {items.map((it, i) => {
        if (it.kind === "tool") return <ToolCallChip key={i} title={it.title} status={it.status} />;
        if (it.kind === "thought") return <Thoughts key={i} text={it.text} />;
        return <div key={i} className={`bubble ${it.kind}`}>{it.text}</div>;
      })}
    </div>
  );
}
```

- [ ] **Step 3: App wiring** — replace `web/src/ui/App.tsx`:
```tsx
import { useEffect, useRef, useState } from "react";
import { createSpawn, stopSpawn } from "../api/spawnlet";
import { Client } from "../acp/client";
import { ChatLog, type Item } from "./ChatLog";
import { PromptInput } from "./PromptInput";
import { PermissionModal } from "./PermissionModal";
import "./app.css";

const APP_PATH = "examples/secret-app";
const MODEL = "openai/gpt-oss-120b:free";

export function App() {
  const [status, setStatus] = useState("starting…");
  const [items, setItems] = useState<Item[]>([]);
  const [busy, setBusy] = useState(true);
  const [perm, setPerm] = useState<{ title: string; resolve: (b: boolean) => void } | null>(null);
  const clientRef = useRef<Client | null>(null);
  const spawnRef = useRef<string>("");
  const wsRef = useRef<WebSocket | null>(null);

  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        const id = await createSpawn(APP_PATH, MODEL);
        if (!alive) { stopSpawn(id); return; }
        spawnRef.current = id;
        const ws = new WebSocket(`ws://${location.host}/ws/session`);
        ws.binaryType = "arraybuffer";
        wsRef.current = ws;
        ws.onopen = async () => {
          ws.send(JSON.stringify({ spawnId: id }));
          const c = new Client(ws as any);
          clientRef.current = c;
          await c.initialize();
          await c.newSession("/app");
          if (alive) { setStatus("ready"); setBusy(false); }
        };
        ws.onerror = () => alive && setStatus("connection error");
        ws.onclose = () => alive && setStatus("session ended");
      } catch (e: any) {
        if (alive) setStatus("error: " + e.message);
      }
    })();
    return () => {
      alive = false;
      wsRef.current?.close();
      if (spawnRef.current) stopSpawn(spawnRef.current);
    };
  }, []);

  const add = (it: Item) => setItems((xs) => [...xs, it]);
  const appendAgent = (t: string) =>
    setItems((xs) => {
      const last = xs[xs.length - 1];
      if (last && last.kind === "agent") return [...xs.slice(0, -1), { kind: "agent", text: last.text + t }];
      return [...xs, { kind: "agent", text: t }];
    });

  const onSend = async (text: string) => {
    if (!clientRef.current) return;
    add({ kind: "user", text });
    setBusy(true);
    try {
      await clientRef.current.prompt(text, {
        onText: appendAgent,
        onThought: (t) => add({ kind: "thought", text: t }),
        onToolCall: (tc) => add({ kind: "tool", title: tc.title, status: tc.status }),
        onToolUpdate: (tc) => add({ kind: "tool", title: "tool", status: tc.status }),
        requestPermission: (req) =>
          new Promise<boolean>((resolve) =>
            setPerm({ title: req?.options?.[0]?.name ?? "an action", resolve: (b) => { setPerm(null); resolve(b); } })),
      });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="app">
      <header>Spawnery — secret-app <span className="status">{status}</span></header>
      <ChatLog items={items} />
      <PromptInput disabled={busy} onSend={onSend} />
      {perm && <PermissionModal title={perm.title} onResolve={perm.resolve} />}
    </div>
  );
}
```

- [ ] **Step 4: Minimal CSS** — `web/src/ui/app.css`:
```css
.app { max-width: 760px; margin: 0 auto; font-family: system-ui, sans-serif; }
header { padding: 8px; border-bottom: 1px solid #ddd; display: flex; justify-content: space-between; }
.status { color: #888; font-size: 12px; }
.log { padding: 8px; display: flex; flex-direction: column; gap: 6px; min-height: 300px; }
.bubble { padding: 8px 10px; border-radius: 8px; max-width: 80%; white-space: pre-wrap; }
.bubble.user { align-self: flex-end; background: #dceefe; }
.bubble.agent { align-self: flex-start; background: #f1f1f1; }
.chip { align-self: flex-start; font-size: 12px; color: #555; background: #fff7e6; border: 1px solid #ffe0a3; border-radius: 12px; padding: 2px 8px; }
.thoughts { font-size: 12px; color: #777; }
.thoughts pre { white-space: pre-wrap; background: #fafafa; padding: 6px; }
.input { display: flex; gap: 6px; padding: 8px; border-top: 1px solid #ddd; }
.input textarea { flex: 1; height: 48px; }
.modal-backdrop { position: fixed; inset: 0; background: rgba(0,0,0,.3); display: grid; place-items: center; }
.modal { background: #fff; padding: 16px; border-radius: 8px; }
.modal button { margin: 0 6px; }
```

- [ ] **Step 5: Build check** — `cd web && npx tsc -b && npm run build` → no errors; `dist/` produced.
- [ ] **Step 6: Commit**
```bash
git add web/src/ui web/src/api
git commit -m "feat(web/ui): chat UI wired to createSpawn + ACP-over-WS

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 8: Manual live e2e (the secret-word flow in the browser)

**Files:** none (run + document); append a "Web client" section to `README.md`.

- [ ] **Step 1: Build images (if stale)**
```bash
docker build -t spawnery/goose:dev   -f deploy/agent/Dockerfile .
docker build -t spawnery/sidecar:dev -f deploy/sidecar/Dockerfile .
```

- [ ] **Step 2: Run spawnlet from the repo root** (so the relative `examples/secret-app` resolves):
```bash
go build -o bin/spawnlet ./cmd/spawnlet
set -a; . ./.env; set +a    # OPENROUTER_API_KEY
AGENT_IMAGE=spawnery/goose:dev SIDECAR_IMAGE=spawnery/sidecar:dev \
  DATA_ROOT=$(pwd)/.spawns bin/spawnlet &
```

- [ ] **Step 3: Run the web dev server**
```bash
cd web && npm run dev
# open the printed URL (http://localhost:5173)
```
Expected in the browser: status goes `starting… → ready`; type **"What is the secret word?"** → you see a **🔧 tool-call chip** (reading `data/README.md`), maybe a collapsible **thinking** block, then the agent bubble **"QUOKKA-4417"**. Closing the tab calls `StopSpawn` (the containers are torn down).

- [ ] **Step 4: Document** — add a short "## Web client (demo)" section to `README.md` with the two run commands above and the expected result. Commit:
```bash
git add README.md
git commit -m "docs: web client (demo) run instructions

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage:** §1 goal (direct-to-spawnlet, hardcoded spawn, chat) → Tasks 1,6,7; §2 ACP scope (message/tool/thought/permission, no fs/terminal) → Tasks 5,7 (initialize advertises `clientCapabilities:{}`); §3a api → Task 6; §3b acp → Tasks 4,5; §3c ui → Task 7; §4 spawnlet WS endpoint reusing Relay → Task 1; §5 vite proxy → Task 3; §6 data flow → Task 7 + Task 8 (manual); §7 error handling → Task 7 (status banners, createSpawn catch); §8 testing (acp unit, Go WS e2e, manual) → Tasks 4,5,2,8. **No gaps.** The relative-appPath resolution (Task 1 Step 2) is an added necessity (browser can't send absolute paths) — noted.

**Placeholder scan:** the only "to confirm" is the ACP `session/request_permission` response shape (Task 5) — flagged as a live-run confirmation, with a working best-effort implementation + a note that the secret-word demo may not trigger it. Not a placeholder in the no-code sense; the code is complete. The `wsIO`/`mustAbsWS` placeholder lines in Task 2 are explicitly called out to delete.

**Type consistency:** `Message`, `SessionUpdate`, `Conn(ws, onMessage)`, `WebSocketLike`, `Client(ws)` with `initialize`/`newSession`/`prompt`/`PromptHandlers{onText,onThought,onToolCall,onToolUpdate,requestPermission}`, `createSpawn(appPath,model)→spawnId`, `stopSpawn(spawnId)`, `Item` union, `Server.HandleWS`, `StreamEndpoint{Recv,Send}`, `AgentIO{Stdin,Stdout}` — consistent across tasks.

---

## Beads
One milestone: `Web ACP client (demo) — spawnlet WS endpoint + React chat`. Mark in_progress at Task 1; close after Task 8. Part of E6 web client (`sp-95v`).
