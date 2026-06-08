import { describe, it, expect, vi, beforeEach } from "vitest";

const unaryMock = vi.fn();
vi.mock("./connect", () => ({ unary: (...a: unknown[]) => unaryMock(...a), DEV_TOKEN: "dev-token" }));

import {
  transportFromMode, transportToProto, transportFromProto,
  listSessions, createSession, closeSession,
} from "./sessions";

beforeEach(() => unaryMock.mockReset());

describe("transport mapping", () => {
  it("maps mode->transport (tmux=>mosh, else acp)", () => {
    expect(transportFromMode("tmux")).toBe("mosh");
    expect(transportFromMode("acp")).toBe("acp");
    expect(transportFromMode("")).toBe("acp");
  });
  it("maps transport<->proto enum NAME", () => {
    expect(transportToProto("mosh")).toBe("SESSION_TRANSPORT_MOSH");
    expect(transportToProto("acp")).toBe("SESSION_TRANSPORT_ACP");
    expect(transportFromProto("SESSION_TRANSPORT_MOSH")).toBe("mosh");
    expect(transportFromProto("SESSION_TRANSPORT_ACP")).toBe("acp");
    expect(transportFromProto(undefined)).toBe("acp");
  });
});

describe("session RPC wrappers", () => {
  it("listSessions maps the proto roster", async () => {
    unaryMock.mockResolvedValue({ sessions: [
      { sessionId: "0", transport: "SESSION_TRANSPORT_ACP", runnable: "goose-acp", status: "active", pinned: true },
      { sessionId: "1", transport: "SESSION_TRANSPORT_MOSH", runnable: "shell", status: "active", pinned: false },
    ] });
    const got = await listSessions("s1");
    expect(unaryMock).toHaveBeenCalledWith("ListSessions", { spawnId: "s1" });
    expect(got).toEqual([
      { sessionId: "0", transport: "acp", runnable: "goose-acp", status: "active", pinned: true },
      { sessionId: "1", transport: "mosh", runnable: "shell", status: "active", pinned: false },
    ]);
  });
  it("createSession sends the proto transport NAME + runnable", async () => {
    unaryMock.mockResolvedValue({});
    await createSession("s1", "mosh", "shell");
    expect(unaryMock).toHaveBeenCalledWith("CreateSession", { spawnId: "s1", transport: "SESSION_TRANSPORT_MOSH", runnable: "shell" });
  });
  it("closeSession sends spawnId+sessionId", async () => {
    unaryMock.mockResolvedValue({});
    await closeSession("s1", "1");
    expect(unaryMock).toHaveBeenCalledWith("CloseSession", { spawnId: "s1", sessionId: "1" });
  });
});
