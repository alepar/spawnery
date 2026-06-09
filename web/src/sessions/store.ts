import { create } from "zustand";
import type { Item, TurnState } from "@/views/chat/types";
import type { Command, Frame, ModePayload, PermOption } from "@/acp/frames";
import type { Transport } from "@/api/sessions";
import type { ConnState } from "@/shell/useConnStatus";
import { reconcilePending } from "@/lib/turn";
import { upsertTool } from "@/lib/toolChip";
import { upsertPlan } from "@/lib/plan";
import { mergeMode } from "@/lib/mode";

export interface SessionMeta {
  sessionId: string;
  transport: Transport;
  runnable: string;
  label: string;
  status: string;
  pinned: boolean;
}

// Per-acp-session transcript state. Mosh sessions have no runtime here (xterm owns their bytes);
// they only contribute a `conn` entry.
export interface AcpRuntime {
  items: Item[];
  turn: TurnState;
  // options are the agent's kinded permission choices (cat H); reqId targets the perm_response.
  perm: { title: string; reqId: string; options: PermOption[] } | null;
  mode: ModePayload | null; // current + available modes (cat F); null until the agent advertises
  commands: Command[]; // agent slash commands (cat E); [] for agents that never advertise
  nextId: number;
  lastSeq: number; // resume cursor for reconnects
}

const EMPTY_RT: AcpRuntime = {
  items: [], turn: { state: "idle", queued: 0 }, perm: null, mode: null, commands: [], nextId: 0, lastSeq: 0,
};

// Client→node control frames are encoded and sent UP the socket; the store only ever decodes
// node→client traffic, so these kinds can never reach reduceFrame.
type ClientToNodeKind = "prompt" | "perm_response" | "cancel" | "set_mode";

// Pure: fold one ACP frame into a session's runtime (mirrors the old App.applyFrame, per session).
export function reduceFrame(rt: AcpRuntime, f: Frame): AcpRuntime {
  let { items, turn, perm, mode, commands, nextId, lastSeq } = rt;
  if (f.seq) lastSeq = f.seq;
  type ItemInput = Item extends infer T ? (T extends { id: number } ? Omit<T, "id"> : never) : never;
  const withId = (it: ItemInput): Item => ({ ...it, id: nextId++ }) as Item;
  const makeId = () => nextId++;
  const appendChunk = (kind: "agent" | "thought", t: string) => {
    const last = items[items.length - 1];
    if (last && last.kind === kind) {
      items = [...items.slice(0, -1), { ...last, text: (last as { text: string }).text + t }];
    } else {
      items = [...items, withId({ kind, text: t } as ItemInput)];
    }
  };
  switch (f.kind) {
    case "reset": items = []; lastSeq = f.fromSeq ?? 0; break;
    case "user": items = [...items, withId({ kind: "user", text: f.text ?? "" } as ItemInput)]; break;
    case "agent": appendChunk("agent", f.text ?? ""); break;
    case "thought": appendChunk("thought", f.text ?? ""); break;
    // One chip per toolId: later frames merge title/status/content/diff/rawInput/rawOutput in place;
    // frames without a toolId keep append behavior (see lib/toolChip.ts).
    case "tool": items = upsertTool(items, f, makeId); break;
    // Replace-in-place: each plan frame carries the WHOLE current plan, so swap the single plan
    // item's entries (see lib/plan.ts). No plan ever -> no plan item -> nothing renders.
    case "plan": items = upsertPlan(items, f.plan ?? [], makeId); break;
    // Wholesale replace: the frame carries the WHOLE current command set (cat E).
    case "commands": commands = f.cmds ?? []; break;
    // session/new advertises current + available; current_mode_update carries only current —
    // mergeMode keeps the prior available set when a frame omits it (cat F, lib/mode.ts).
    case "mode": mode = mergeMode(mode, f.mode); break;
    // reason/error/usage ride the idle frame (cats D+G); a busy frame carries none, so the
    // wholesale TurnState replace naturally clears them at turn start.
    case "turn":
      turn = { state: f.state ?? "idle", queued: f.queued ?? 0, reason: f.reason, error: f.error, usage: f.usage };
      items = reconcilePending(items, turn.queued);
      break;
    case "perm_request": perm = { title: f.title ?? "an action", reqId: f.reqId ?? "", options: f.options ?? [] }; break;
    default: {
      // Exhaustiveness guard: every server→client kind must have a case above, so the only kinds
      // that may fall through are the client→node control frames (which the store never sees —
      // they go up the socket, not down). Dropping a server→client case makes this a compile
      // error instead of a silent feature drop — exactly how the tabs migration regressed sp-ufz.
      const _unhandled: never = f.kind as Exclude<(typeof f)["kind"], ClientToNodeKind>;
      void _unhandled;
      break;
    }
  }
  return { items, turn, perm, mode, commands, nextId, lastSeq };
}

interface SessionStore {
  spawnId: string | null;
  sessions: SessionMeta[];
  activeId: string | null;
  acp: Record<string, AcpRuntime>;
  conn: Record<string, ConnState | null>;

  bindSpawn(spawnId: string): void;
  reconcileRoster(list: SessionMeta[]): void;
  setActive(sessionId: string): void;
  applyFrame(sessionId: string, f: Frame): void;
  setConn(sessionId: string, c: ConnState | null): void;
  clearPerm(sessionId: string): void;
}

export const useSessionStore = create<SessionStore>((set, get) => ({
  spawnId: null,
  sessions: [],
  activeId: null,
  acp: {},
  conn: {},

  // Switch the store to a new spawn: a different spawn means a fresh tab set + fresh sockets
  // (panels remount, keyed on spawnId). Idempotent for the same spawn.
  bindSpawn: (spawnId) => {
    if (get().spawnId === spawnId) return;
    set({ spawnId, sessions: [], activeId: null, acp: {}, conn: {} });
  },

  reconcileRoster: (list) => set((s) => {
    const ids = new Set(list.map((m) => m.sessionId));
    const acp = { ...s.acp };
    const conn = { ...s.conn };
    for (const m of list) {
      if (m.transport === "acp" && !acp[m.sessionId]) acp[m.sessionId] = EMPTY_RT;
    }
    for (const id of Object.keys(acp)) if (!ids.has(id)) delete acp[id];
    for (const id of Object.keys(conn)) if (!ids.has(id)) delete conn[id];
    const activeId = s.activeId && ids.has(s.activeId) ? s.activeId : (list[0]?.sessionId ?? null);
    return { sessions: list, acp, conn, activeId };
  }),

  setActive: (sessionId) => set({ activeId: sessionId }),

  applyFrame: (sessionId, f) => set((s) => {
    const rt = s.acp[sessionId] ?? EMPTY_RT;
    return { acp: { ...s.acp, [sessionId]: reduceFrame(rt, f) } };
  }),

  setConn: (sessionId, c) => set((s) => ({ conn: { ...s.conn, [sessionId]: c } })),

  clearPerm: (sessionId) => set((s) => {
    const rt = s.acp[sessionId];
    if (!rt) return {};
    return { acp: { ...s.acp, [sessionId]: { ...rt, perm: null } } };
  }),
}));
