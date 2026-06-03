import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import {
  createSpawn, listSpawns, renameSpawn, suspendSpawn, resumeSpawn, deleteSpawn,
  DEV_TOKEN, type SpawnView,
} from "./api/spawnlet";
import { Client, historyToItems } from "./acp/client";
import { AppShell } from "./shell/AppShell";
import { initialTheme, setTheme } from "./lib/theme";
import type { Item } from "./views/chat/types";

const MODEL = "openai/gpt-oss-120b:free";

export function App() {
  const [status, setStatus] = useState("");
  const [items, setItems] = useState<Item[]>([]);
  const [busy, setBusy] = useState(false);
  const [perm, setPerm] = useState<{ title: string; resolve: (b: boolean) => void } | null>(null);
  const [spawns, setSpawns] = useState<SpawnView[]>([]);
  const [activeId, setActiveId] = useState<string | null>(null);

  const clientRef = useRef<Client | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const idRef = useRef(0);
  const genRef = useRef(0);
  const buffersRef = useRef<Map<string, Item[]>>(new Map());
  // refs mirroring state so async callbacks (poll, ws onopen, onHistory) don't read stale closures.
  const activeIdRef = useRef<string | null>(null);
  const itemsRef = useRef<Item[]>([]);
  const spawnsRef = useRef<SpawnView[]>([]);

  useEffect(() => { setTheme(initialTheme()); }, []);
  useEffect(() => { activeIdRef.current = activeId; }, [activeId]);
  useEffect(() => { itemsRef.current = items; }, [items]);
  useEffect(() => { spawnsRef.current = spawns; }, [spawns]);

  // Distributive Omit so each Item variant keeps its own fields (plain Omit<union,"id"> collapses them).
  type ItemInput = Item extends infer T ? (T extends { id: number } ? Omit<T, "id"> : never) : never;
  const withId = (it: ItemInput): Item => ({ ...it, id: idRef.current++ } as Item);

  const refreshSpawns = async () => {
    try {
      const list = await listSpawns();
      setSpawns(list);
      if (activeIdRef.current && !list.some((s) => s.spawnId === activeIdRef.current)) {
        // active spawn disappeared from the ledger (stopped) — tear the live session down.
        genRef.current++;
        wsRef.current?.close();
        wsRef.current = null; clientRef.current = null;
        setActiveId(null); setItems([]); setStatus("");
      }
    } catch { /* transient; keep the last list */ }
  };

  useEffect(() => {
    refreshSpawns();
    const t = setInterval(refreshSpawns, 3000);
    return () => clearInterval(t);
  }, []);

  // On unmount just close the live ws — spawns persist on the node.
  useEffect(() => () => { wsRef.current?.close(); }, []);

  const closeSession = () => {
    genRef.current++;
    wsRef.current?.close();
    wsRef.current = null;
    clientRef.current = null;
  };

  const openSession = (spawnId: string) => {
    const gen = ++genRef.current;
    wsRef.current?.close();
    setBusy(true); setStatus("starting…");
    const ws = new WebSocket(`ws://${location.host}/ws/session`);
    ws.binaryType = "arraybuffer";
    wsRef.current = ws;
    ws.onopen = async () => {
      ws.send(JSON.stringify({ spawnId, token: DEV_TOKEN }));
      const c = new Client(ws as any);
      clientRef.current = c;
      // CRI-lane adapter replays the transcript here; Docker lane never fires this (buffer is used).
      c.onHistory = (h) => {
        if (genRef.current !== gen) return;
        const its = historyToItems(h).map(withId);
        buffersRef.current.set(spawnId, its);
        if (activeIdRef.current === spawnId) setItems(its);
      };
      try {
        await c.initialize();
        await c.newSession("/app");
      } catch (e: any) {
        if (genRef.current !== gen) return;
        setStatus("error: " + e.message); setBusy(false);
        return;
      }
      if (genRef.current !== gen) return;
      setStatus("ready"); setBusy(false);
    };
    ws.onerror = () => { if (genRef.current !== gen) return; setStatus("connection error"); toast.error("Connection error"); };
    ws.onclose = () => { if (genRef.current !== gen) return; setStatus("session ended"); };
  };

  const spawnApp = async (appId: string) => {
    setBusy(true); setStatus("starting…");
    try {
      const id = await createSpawn(appId, MODEL);
      const prevId = activeIdRef.current;
      buffersRef.current.set(id, []);
      setActiveId(id);
      activeIdRef.current = id; // make active immediately for the onHistory guard
      // Stash the OUTGOING spawn's transcript before clearing for the new one (mirrors selectSpawn),
      // so spawning a 2nd instance while the 1st had messages doesn't lose the 1st's transcript.
      setItems((current) => {
        if (prevId && prevId !== id) buffersRef.current.set(prevId, current);
        return [];
      });
      await refreshSpawns();
      openSession(id);
    } catch (e: any) {
      setStatus("error: " + e.message); setBusy(false);
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
    // Atomically stash the OUTGOING spawn's last-committed transcript and load the incoming one.
    // Using the functional updater (vs itemsRef, which lags by one render) avoids losing a chunk if
    // the user switches mid-stream. Writing a ref inside the updater is idempotent under StrictMode's
    // double-invoke (same value both times), so it's safe here.
    setItems((current) => {
      if (prevId) buffersRef.current.set(prevId, current);
      return buf;
    });
    const sp = spawnsRef.current.find((s) => s.spawnId === id);
    if (sp?.status === "active") openSession(id);
    else setStatus(sp?.status ?? "");
  };

  const onRename = async (id: string, name: string) => {
    setSpawns((xs) => xs.map((s) => (s.spawnId === id ? { ...s, name } : s))); // optimistic
    try { await renameSpawn(id, name); } catch (e: any) { toast.error("Rename failed: " + e.message); }
    refreshSpawns();
  };
  const onSuspend = async (id: string) => {
    try {
      await suspendSpawn(id);
      if (activeIdRef.current === id) { closeSession(); setStatus("suspended"); }
    } catch (e: any) { toast.error("Suspend failed: " + e.message); }
    refreshSpawns();
  };
  const onResume = async (id: string) => {
    try {
      await resumeSpawn(id);
      // openSession even though the ledger may still read 'suspending' — the backend transitions the
      // spawn to active synchronously before it accepts the ws, so the handshake succeeds.
      if (activeIdRef.current === id) openSession(id);
    } catch (e: any) { toast.error("Resume failed: " + e.message); }
    refreshSpawns();
  };
  const onStop = async (id: string) => {
    try { await deleteSpawn(id); } catch (e: any) { toast.error("Stop failed: " + e.message); }
    buffersRef.current.delete(id);
    if (activeIdRef.current === id) { closeSession(); setActiveId(null); activeIdRef.current = null; setItems([]); setStatus(""); }
    refreshSpawns();
  };

  const add = (it: ItemInput) => setItems((xs) => [...xs, withId(it)]);
  const appendChunk = (kind: "agent" | "thought") => (t: string) =>
    setItems((xs) => {
      const last = xs[xs.length - 1];
      if (last && last.kind === kind) return [...xs.slice(0, -1), { ...last, text: (last as { text: string }).text + t }];
      return [...xs, withId({ kind, text: t })];
    });

  const onSend = async (text: string) => {
    if (!clientRef.current) return;
    add({ kind: "user", text });
    setBusy(true);
    try {
      await clientRef.current.prompt(text, {
        onText: appendChunk("agent"),
        onThought: appendChunk("thought"),
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
    <AppShell
      status={status}
      items={items}
      busy={busy}
      onSend={onSend}
      perm={perm}
      onSpawnApp={spawnApp}
      spawns={spawns}
      activeId={activeId}
      actions={{ onSelectSpawn: selectSpawn, onRename, onSuspend, onResume, onStop }}
    />
  );
}
