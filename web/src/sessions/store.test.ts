import { describe, it, expect, beforeEach } from "vitest";
import { useSessionStore, reduceFrame, type AcpRuntime, type SessionMeta } from "./store";
import type { Frame } from "@/acp/frames";

const meta = (id: string, over: Partial<SessionMeta> = {}): SessionMeta => ({
  sessionId: id, transport: "acp", runnable: "goose-acp", label: "goose-acp", status: "active", pinned: id === "0", ...over,
});
const emptyRt: AcpRuntime = {
  items: [], turn: { state: "idle", queued: 0 }, perm: null, mode: null, commands: [], nextId: 0, lastSeq: 0,
};

beforeEach(() => useSessionStore.getState().bindSpawn("__reset__"));

describe("reduceFrame (pure)", () => {
  it("appends a user item and assigns ids", () => {
    const rt = reduceFrame(emptyRt, { kind: "user", text: "hi" } as Frame);
    expect(rt.items).toEqual([{ id: 0, kind: "user", text: "hi" }]);
    expect(rt.nextId).toBe(1);
  });
  it("coalesces consecutive agent chunks", () => {
    let rt = reduceFrame(emptyRt, { kind: "agent", text: "a" } as Frame);
    rt = reduceFrame(rt, { kind: "agent", text: "b" } as Frame);
    expect(rt.items).toEqual([{ id: 0, kind: "agent", text: "ab" }]);
  });
  it("updates turn and advances lastSeq off f.seq", () => {
    const rt = reduceFrame(emptyRt, { kind: "turn", state: "busy", queued: 2, seq: 5 } as Frame);
    expect(rt.turn).toEqual({ state: "busy", queued: 2 });
    expect(rt.lastSeq).toBe(5);
  });
  it("captures a perm_request as {title,reqId,options}", () => {
    const rt = reduceFrame(emptyRt, { kind: "perm_request", title: "delete?", reqId: "r1" } as Frame);
    expect(rt.perm).toEqual({ title: "delete?", reqId: "r1", options: [] });
  });
  it("captures the agent's kinded perm options (cat H)", () => {
    const options = [{ optionId: "allow", name: "Allow", kind: "allow_once" }];
    const rt = reduceFrame(emptyRt, { kind: "perm_request", title: "delete?", reqId: "r1", options } as Frame);
    expect(rt.perm).toEqual({ title: "delete?", reqId: "r1", options });
  });
  it("reset clears items and resets the cursor to fromSeq", () => {
    let rt = reduceFrame(emptyRt, { kind: "user", text: "x" } as Frame);
    rt = reduceFrame(rt, { kind: "reset", fromSeq: 3 } as Frame);
    expect(rt.items).toEqual([]);
    expect(rt.lastSeq).toBe(3);
  });

  // --- ACP enrichment cases restored by sp-x8y4 (semantics from the pre-tabs App.applyFrame) ---

  it("plan: replace-in-place — one plan item, later frames swap entries keeping id/position", () => {
    let rt = reduceFrame(emptyRt, { kind: "user", text: "go" } as Frame);
    rt = reduceFrame(rt, { kind: "plan", plan: [{ content: "step 1", status: "pending" }] } as Frame);
    rt = reduceFrame(rt, { kind: "agent", text: "working" } as Frame);
    rt = reduceFrame(rt, {
      kind: "plan",
      plan: [{ content: "step 1", status: "completed" }, { content: "step 2", status: "in_progress" }],
    } as Frame);
    const plans = rt.items.filter((it) => it.kind === "plan");
    expect(plans).toHaveLength(1); // never stacks
    expect(rt.items[1]).toEqual({
      id: 1, // same id + position as the first plan frame's item
      kind: "plan",
      entries: [{ content: "step 1", status: "completed" }, { content: "step 2", status: "in_progress" }],
    });
  });

  it("plan: empty frame before any plan is a no-op (graceful absence)", () => {
    const rt = reduceFrame(emptyRt, { kind: "plan", plan: [] } as Frame);
    expect(rt.items).toEqual([]);
  });

  it("commands: wholesale replace; a frame without cmds clears the set", () => {
    let rt = reduceFrame(emptyRt, { kind: "commands", cmds: [{ name: "init" }, { name: "compact" }] } as Frame);
    expect(rt.commands).toEqual([{ name: "init" }, { name: "compact" }]);
    rt = reduceFrame(rt, { kind: "commands", cmds: [{ name: "web" }] } as Frame);
    expect(rt.commands).toEqual([{ name: "web" }]); // replaced, not merged
    rt = reduceFrame(rt, { kind: "commands" } as Frame);
    expect(rt.commands).toEqual([]);
  });

  it("mode: adopts the session/new advertisement (current + available)", () => {
    const payload = { current: "build", available: [{ id: "build", name: "Build" }, { id: "plan", name: "Plan" }] };
    const rt = reduceFrame(emptyRt, { kind: "mode", mode: payload } as Frame);
    expect(rt.mode).toEqual(payload);
  });

  it("mode: a current-only update keeps the prior available set (mergeMode semantics)", () => {
    const available = [{ id: "build", name: "Build" }, { id: "plan", name: "Plan" }];
    let rt = reduceFrame(emptyRt, { kind: "mode", mode: { current: "build", available } } as Frame);
    rt = reduceFrame(rt, { kind: "mode", mode: { current: "plan" } } as Frame);
    expect(rt.mode).toEqual({ current: "plan", available });
  });

  it("turn: idle carries reason/error/usage into TurnState", () => {
    const usage = { input: 10, output: 20, total: 30, cost: 0.01 };
    const error = { code: 429, message: "rate limited" };
    const rt = reduceFrame(emptyRt, { kind: "turn", state: "idle", queued: 0, reason: "max_tokens", error, usage } as Frame);
    expect(rt.turn).toEqual({ state: "idle", queued: 0, reason: "max_tokens", error, usage });
  });

  it("turn: a busy frame clears prior reason/error/usage", () => {
    let rt = reduceFrame(emptyRt, {
      kind: "turn", state: "idle", queued: 0, reason: "cancelled", usage: { total: 5 },
    } as Frame);
    rt = reduceFrame(rt, { kind: "turn", state: "busy", queued: 1 } as Frame);
    expect(rt.turn).toEqual({ state: "busy", queued: 1 });
    expect(rt.turn.reason).toBeUndefined();
    expect(rt.turn.error).toBeUndefined();
    expect(rt.turn.usage).toBeUndefined();
  });

  it("tool: frames with the same toolId upsert one chip, merging status/content/diff/raw payloads", () => {
    let rt = reduceFrame(emptyRt, {
      kind: "tool", toolId: "t1", title: "write file", status: "in_progress", tool: { rawInput: { path: "a.txt" } },
    } as Frame);
    rt = reduceFrame(rt, {
      kind: "tool", toolId: "t1", status: "completed",
      tool: { content: [{ type: "text", text: "done" }], diff: { path: "a.txt", newText: "x" }, rawOutput: { ok: true } },
    } as Frame);
    expect(rt.items).toEqual([{
      id: 0,
      kind: "tool",
      toolId: "t1",
      title: "write file", // status-only update keeps prior title
      status: "completed",
      content: [{ type: "text", text: "done" }],
      diff: { path: "a.txt", newText: "x" },
      rawInput: { path: "a.txt" },
      rawOutput: { ok: true },
    }]);
  });

  it("tool: frames without a toolId keep append behavior (one chip each)", () => {
    let rt = reduceFrame(emptyRt, { kind: "tool", title: "shell", status: "in_progress" } as Frame);
    rt = reduceFrame(rt, { kind: "tool", title: "shell", status: "completed" } as Frame);
    expect(rt.items).toHaveLength(2);
    expect(rt.items.map((it) => it.id)).toEqual([0, 1]);
  });
});

describe("session store", () => {
  it("bindSpawn resets and switching spawns clears state", () => {
    const s = useSessionStore.getState();
    s.bindSpawn("s1");
    s.reconcileRoster([meta("0")]);
    expect(useSessionStore.getState().sessions).toHaveLength(1);
    s.bindSpawn("s2");
    expect(useSessionStore.getState().spawnId).toBe("s2");
    expect(useSessionStore.getState().sessions).toEqual([]);
    expect(useSessionStore.getState().activeId).toBeNull();
  });

  it("reconcileRoster inits acp runtime for new acp sessions and defaults active to the first", () => {
    const s = useSessionStore.getState();
    s.bindSpawn("s1");
    s.reconcileRoster([meta("0"), meta("1", { transport: "mosh", runnable: "shell", label: "shell", pinned: false })]);
    const st = useSessionStore.getState();
    expect(st.activeId).toBe("0");
    expect(st.acp["0"]).toBeTruthy();   // acp session gets runtime
    expect(st.acp["1"]).toBeUndefined(); // mosh session does not
  });

  it("reconcileRoster prunes removed sessions and reassigns active if the active tab vanished", () => {
    const s = useSessionStore.getState();
    s.bindSpawn("s1");
    s.reconcileRoster([meta("0"), meta("1")]);
    s.setActive("1");
    s.reconcileRoster([meta("0")]); // session 1 closed
    const st = useSessionStore.getState();
    expect(st.sessions.map((m) => m.sessionId)).toEqual(["0"]);
    expect(st.acp["1"]).toBeUndefined();
    expect(st.conn["1"]).toBeUndefined();
    expect(st.activeId).toBe("0");
  });

  // Recreate cycle (spec 2026-06-08 sidebar-lifecycle §3 amendment): RecreateSpawn drops the
  // spawn's session mirror, so the roster poll sees an empty list (stale runtimes wiped); the
  // recreated container then re-registers session ids, which must start with FRESH runtimes —
  // the dead container's transcript must not survive even when the session id is reused.
  it("recreate cycle: empty roster wipes stale runtimes; repopulated roster starts fresh", () => {
    const s = useSessionStore.getState();
    s.bindSpawn("s1");
    s.reconcileRoster([meta("0")]);
    s.applyFrame("0", { kind: "agent", text: "from the dead container" } as Frame);
    expect(useSessionStore.getState().acp["0"].items).toHaveLength(1);
    s.reconcileRoster([]); // mirror dropped with the route -> ListSessions returns empty
    expect(useSessionStore.getState().acp["0"]).toBeUndefined();
    s.reconcileRoster([meta("0")]); // recreated container registers session 0 again
    expect(useSessionStore.getState().acp["0"].items).toEqual([]);
  });

  it("applyFrame routes to the right session and clearPerm clears it", () => {
    const s = useSessionStore.getState();
    s.bindSpawn("s1");
    s.reconcileRoster([meta("0"), meta("1")]);
    s.applyFrame("1", { kind: "user", text: "yo" } as Frame);
    expect(useSessionStore.getState().acp["1"].items).toEqual([{ id: 0, kind: "user", text: "yo" }]);
    expect(useSessionStore.getState().acp["0"].items).toEqual([]);
    s.applyFrame("1", { kind: "perm_request", title: "t", reqId: "r" } as Frame);
    expect(useSessionStore.getState().acp["1"].perm).toEqual({ title: "t", reqId: "r", options: [] });
    s.clearPerm("1");
    expect(useSessionStore.getState().acp["1"].perm).toBeNull();
  });

  it("setConn records per-session conn (used by both acp + mosh panels)", () => {
    const s = useSessionStore.getState();
    s.bindSpawn("s1");
    s.reconcileRoster([meta("0")]);
    s.setConn("0", "connected");
    expect(useSessionStore.getState().conn["0"]).toBe("connected");
  });
});
