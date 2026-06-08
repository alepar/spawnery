import { describe, it, expect, beforeEach } from "vitest";
import { useSessionStore, reduceFrame, type AcpRuntime, type SessionMeta } from "./store";
import type { Frame } from "@/acp/frames";

const meta = (id: string, over: Partial<SessionMeta> = {}): SessionMeta => ({
  sessionId: id, transport: "acp", runnable: "goose-acp", label: "goose-acp", status: "active", pinned: id === "0", ...over,
});
const emptyRt: AcpRuntime = { items: [], turn: { state: "idle", queued: 0 }, perm: null, nextId: 0, lastSeq: 0 };

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
