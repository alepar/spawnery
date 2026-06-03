import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import {
  createSpawn, listSpawns, renameSpawn, suspendSpawn, resumeSpawn, deleteSpawn,
  DEV_TOKEN, type SpawnView,
} from "./api/spawnlet";
import { Client, historyToItems } from "./acp/client";
import { AppShell } from "./shell/AppShell";
import { useConnStatus } from "./shell/useConnStatus";
import { nextConnAction } from "./shell/connPolicy";
import { initialTheme, setTheme } from "./lib/theme";
import type { Item } from "./views/chat/types";
import { reconcilePending, MAX_QUEUED } from "./lib/turn";

const MODEL = "deepseek/deepseek-v4-flash";

export function App() {
  const { conn, connecting, connected, errored, closed, reset, waiting } = useConnStatus();
  const [items, setItems] = useState<Item[]>([]);
  const [turn, setTurn] = useState<{ state: "busy" | "idle"; queued: number }>({ state: "idle", queued: 0 });
  const [perm, setPerm] = useState<{ title: string; resolve: (b: boolean) => void } | null>(null);
  const [spawns, setSpawns] = useState<SpawnView[]>([]);
  const [activeId, setActiveId] = useState<string | null>(null);

  const clientRef = useRef<Client | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const idRef = useRef(0);
  const genRef = useRef(0);
  const buffersRef = useRef<Map<string, Item[]>>(new Map());
  const turnsRef = useRef<Map<string, { state: "busy" | "idle"; queued: number }>>(new Map());
  // refs mirroring state so async callbacks (poll, ws onopen, onHistory) don't read stale closures.
  const activeIdRef = useRef<string | null>(null);
  const spawnsRef = useRef<SpawnView[]>([]);
  const connRef = useRef(conn);

  useEffect(() => { setTheme(initialTheme()); }, []);
  useEffect(() => { activeIdRef.current = activeId; }, [activeId]);
  useEffect(() => { spawnsRef.current = spawns; }, [spawns]);
  useEffect(() => { connRef.current = conn; }, [conn]);

  // Distributive Omit so each Item variant keeps its own fields (plain Omit<union,"id"> collapses them).
  type ItemInput = Item extends infer T ? (T extends { id: number } ? Omit<T, "id"> : never) : never;
  const withId = (it: ItemInput): Item => ({ ...it, id: idRef.current++ } as Item);

  // teardown closes the live ws but leaves the header state to the caller (the error case must show
  // red, not the null that closeSession's reset() would set).
  const teardown = () => {
    genRef.current++;
    wsRef.current?.close();
    wsRef.current = null;
    clientRef.current = null;
  };
  const closeSession = () => { teardown(); reset(); };

  const openSession = (spawnId: string) => {
    const gen = ++genRef.current;
    wsRef.current?.close();
    connecting();
    const ws = new WebSocket(`ws://${location.host}/ws/session`);
    ws.binaryType = "arraybuffer";
    wsRef.current = ws;
    ws.onopen = async () => {
      ws.send(JSON.stringify({ spawnId, token: DEV_TOKEN }));
      const c = new Client(ws as any);
      clientRef.current = c;
      // spawn/history replay: the node relay (Docker lane) or the in-pod adapter (CRI lane) sends it on
      // (re)connect, so the transcript is restored even after a browser reload wipes the in-memory buffer.
      c.onHistory = (h) => {
        if (genRef.current !== gen) return;
        const its = historyToItems(h).map(withId);
        buffersRef.current.set(spawnId, its);
        if (activeIdRef.current === spawnId) setItems(its);
      };
      c.onTurn = (t) => {
        if (genRef.current !== gen) return;
        turnsRef.current.set(spawnId, t);
        if (activeIdRef.current === spawnId) {
          setTurn(t);
          setItems((cur) => reconcilePending(cur, t.queued));
        }
      };
      try {
        await c.initialize();
        await c.newSession("/app");
      } catch (e: any) {
        if (genRef.current !== gen) return;
        errored();
        return;
      }
      if (genRef.current !== gen) return;
      connected();
    };
    ws.onerror = () => { if (genRef.current !== gen) return; errored(); toast.error("Connection error"); };
    ws.onclose = () => { if (genRef.current !== gen) return; closed(); };
  };

  // refreshSpawns fetches the ledger, reconciles the active spawn's header/WS off its status (via the
  // pure nextConnAction policy), and returns the fetched list (so the poll can pick its cadence).
  const refreshSpawns = async (): Promise<SpawnView[]> => {
    let list: SpawnView[];
    try { list = await listSpawns(); }
    catch { return spawnsRef.current; }
    setSpawns(list);
    const aid = activeIdRef.current;
    if (aid) {
      const active = list.find((s) => s.spawnId === aid);
      switch (nextConnAction(active?.status, !!wsRef.current, connRef.current)) {
        case "drop":
          closeSession();
          setActiveId(null); activeIdRef.current = null; setItems([]);
          break;
        case "open":
          openSession(aid); // just became active -> connect (green)
          break;
        case "error":
          teardown(); errored(); // failed to start -> red, stays in the sidebar
          break;
        case "waiting":
          waiting(); // still starting -> grey pulse
          break;
        case "none":
          break;
      }
    }
    return list;
  };

  // Poll the ledger: 1s while any spawn is 'starting' (snappy green/connect), else 3s.
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout>;
    let stopped = false;
    const tick = async () => {
      const list = await refreshSpawns();
      if (stopped) return;
      const fast = list.some((s) => s.status === "starting");
      timer = setTimeout(tick, fast ? 1000 : 3000);
    };
    tick();
    return () => { stopped = true; clearTimeout(timer); };
  }, []);

  // On unmount just close the live ws — spawns persist on the node.
  useEffect(() => () => { wsRef.current?.close(); }, []);

  const spawnApp = async (appId: string) => {
    try {
      const id = await createSpawn(appId, MODEL); // async CP: returns immediately, status 'starting'
      const prevId = activeIdRef.current;
      teardown(); // close any current live session before switching to the new (starting) spawn
      buffersRef.current.set(id, []);
      setActiveId(id);
      activeIdRef.current = id;
      setItems((current) => {
        if (prevId && prevId !== id) buffersRef.current.set(prevId, current);
        return [];
      });
      setTurn({ state: "idle", queued: 0 });
      waiting(); // grey-pulse until the node signals active; the poll then opens the ws
      await refreshSpawns(); // sidebar shows the new spawn yellow immediately
    } catch (e: any) {
      errored();
      toast.error("Spawn failed: " + e.message);
    }
  };

  const selectSpawn = (id: string) => {
    if (id === activeIdRef.current) return;
    const prevId = activeIdRef.current;
    closeSession();
    setActiveId(id);
    activeIdRef.current = id;
    const buf = buffersRef.current.get(id) ?? [];
    setItems((current) => {
      if (prevId) buffersRef.current.set(prevId, current);
      return buf;
    });
    setTurn(turnsRef.current.get(id) ?? { state: "idle", queued: 0 });
    const sp = spawnsRef.current.find((s) => s.spawnId === id);
    if (sp?.status === "active") openSession(id);
    else if (sp?.status === "starting") waiting();
    else if (sp?.status === "error" || sp?.status === "unreachable") errored();
    // suspended / unknown -> hidden (closeSession already reset())
  };

  const onRename = async (id: string, name: string) => {
    setSpawns((xs) => xs.map((s) => (s.spawnId === id ? { ...s, name } : s))); // optimistic
    try { await renameSpawn(id, name); } catch (e: any) { toast.error("Rename failed: " + e.message); }
    refreshSpawns();
  };
  const onSuspend = async (id: string) => {
    try {
      await suspendSpawn(id);
      buffersRef.current.delete(id); // resumed spawns start fresh — drop the stale cached transcript
      // keep the spawn selected (unlike onStop) — the user stays on its now-empty suspended view.
      if (activeIdRef.current === id) { closeSession(); setItems([]); }
    } catch (e: any) { toast.error("Suspend failed: " + e.message); }
    refreshSpawns();
  };
  const onResume = async (id: string) => {
    try {
      await resumeSpawn(id);
      if (activeIdRef.current === id) openSession(id);
    } catch (e: any) { toast.error("Resume failed: " + e.message); }
    refreshSpawns();
  };
  const onStop = async (id: string) => {
    try { await deleteSpawn(id); } catch (e: any) { toast.error("Stop failed: " + e.message); }
    buffersRef.current.delete(id);
    if (activeIdRef.current === id) { closeSession(); setActiveId(null); activeIdRef.current = null; setItems([]); setTurn({ state: "idle", queued: 0 }); turnsRef.current.delete(id); }
    refreshSpawns();
  };

  const add = (it: ItemInput) => setItems((xs) => [...xs, withId(it)]);
  const appendChunk = (kind: "agent" | "thought") => (t: string) =>
    setItems((xs) => {
      const last = xs[xs.length - 1];
      if (last && last.kind === kind) return [...xs.slice(0, -1), { ...last, text: (last as { text: string }).text + t }];
      return [...xs, withId({ kind, text: t })];
    });

  const onSend = (text: string) => {
    const c = clientRef.current;
    if (!c) return;
    // Optimistic: if the agent is already working (or prompts are queued), this one will queue too —
    // render it pending. The broker's spawn/turn reconciles the exact pending set as the queue drains.
    const willQueue = turn.state === "busy" || turn.queued > 0;
    add({ kind: "user", text, pending: willQueue });
    // Fire-and-forget: turn-state drives the UI, not this promise. It may resolve much later (queued)
    // or never (disconnect/switch) — that's fine, we no longer gate on it.
    void c.prompt(text, {
      onText: appendChunk("agent"),
      onThought: appendChunk("thought"),
      onToolCall: (tc) => add({ kind: "tool", title: tc.title, status: tc.status }),
      onToolUpdate: (tc) => add({ kind: "tool", title: "tool", status: tc.status }),
      requestPermission: (req) =>
        new Promise<boolean>((resolve) =>
          setPerm({ title: req?.options?.[0]?.name ?? "an action", resolve: (b) => { setPerm(null); resolve(b); } })),
    }).catch(() => {});
  };

  return (
    <AppShell
      conn={conn}
      items={items}
      turn={turn}
      canSend={conn === "connected" && turn.queued < MAX_QUEUED}
      onSend={onSend}
      perm={perm}
      onSpawnApp={spawnApp}
      spawns={spawns}
      activeId={activeId}
      actions={{ onSelectSpawn: selectSpawn, onRename, onSuspend, onResume, onStop }}
    />
  );
}
