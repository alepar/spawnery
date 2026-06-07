import { useEffect, useRef, useState } from "react";
import { useLocation } from "wouter";
import { toast } from "sonner";
import {
  createSpawn, listSpawns, renameSpawn, suspendSpawn, resumeSpawn, deleteSpawn,
  DEV_TOKEN, type SpawnView,
} from "./api/spawnlet";
import { Conn } from "./acp/conn";
import { encodePrompt, encodePermResponse, type Frame } from "./acp/frames";
import { AppShell } from "./shell/AppShell";
import { useConnStatus } from "./shell/useConnStatus";
import { ReconnectingSocket } from "./shell/reconnectingSocket";
import { nextConnAction } from "./shell/connPolicy";
import { initialTheme, setTheme } from "./lib/theme";
import type { Item, TurnState } from "./views/chat/types";
import { reconcilePending, MAX_QUEUED } from "./lib/turn";
import { useNav } from "./nav/useNav";
import type { Nav } from "./nav/nav";

const MODEL = "deepseek/deepseek-v4-flash";

// Map a nav section to the document.title label (spawn/app resolve their dynamic name separately).
function sectionLabel(section: Nav["section"]): string {
  switch (section) {
    case "templates": return "Templates";
    case "my-apps":   return "My Apps";
    case "publish":   return "Publish";
    case "settings":  return "Settings";
    case "app":       return ""; // caller substitutes appId
    case "spawn":     return ""; // caller substitutes the spawn name
  }
}

// A per-tab client id. crypto.randomUUID() only exists in a secure context (HTTPS or localhost), so
// it's undefined on plain-HTTP LAN access (e.g. http://192.168.x.x:5173) — fall back so the app mounts.
function makeClientId(): string {
  try {
    if (typeof crypto !== "undefined" && crypto.randomUUID) return crypto.randomUUID();
  } catch { /* non-secure context */ }
  return `c-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}
const CLIENT_ID = makeClientId();

export function App() {
  const [nav, navigate] = useNav();
  const [path] = useLocation(); // raw pathname, for the one-time "/" -> "/templates" normalize
  const { conn, connecting, connected, errored, reset, waiting, reconnecting } = useConnStatus();
  const [items, setItems] = useState<Item[]>([]);
  const [turn, setTurn] = useState<TurnState>({ state: "idle", queued: 0 });
  const [perm, setPerm] = useState<{ title: string; resolve: (b: boolean) => void } | null>(null);
  const [spawns, setSpawns] = useState<SpawnView[]>([]);
  const [activeId, setActiveId] = useState<string | null>(null);

  const wsRef = useRef<ReconnectingSocket | null>(null);
  const lastSeqRef = useRef(0);
  const idRef = useRef(0);
  const genRef = useRef(0);
  const buffersRef = useRef<Map<string, Item[]>>(new Map());
  const turnsRef = useRef<Map<string, TurnState>>(new Map());
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
  };
  const closeSession = () => { teardown(); reset(); };

  const add = (it: ItemInput) => setItems((xs) => [...xs, withId(it)]);
  const appendChunk = (kind: "agent" | "thought") => (t: string) =>
    setItems((xs) => {
      const last = xs[xs.length - 1];
      if (last && last.kind === kind) return [...xs.slice(0, -1), { ...last, text: (last as { text: string }).text + t }];
      return [...xs, withId({ kind, text: t })];
    });

  const applyFrame = (f: Frame, spawnId: string) => {
    if (f.seq) lastSeqRef.current = f.seq; // advance the resume cursor on logged frames
    switch (f.kind) {
      case "reset":
        setItems([]);
        buffersRef.current.set(spawnId, []);
        lastSeqRef.current = f.fromSeq ?? 0;
        break;
      case "user":
        add({ kind: "user", text: f.text ?? "" });
        break;
      case "agent":
        appendChunk("agent")(f.text ?? "");
        break;
      case "thought":
        appendChunk("thought")(f.text ?? "");
        break;
      case "tool":
        add({ kind: "tool", title: f.title ?? "tool", status: f.status });
        break;
      case "turn": {
        const t: TurnState = { state: f.state ?? "idle", queued: f.queued ?? 0 };
        setTurn(t);
        turnsRef.current.set(spawnId, t);
        setItems((cur) => reconcilePending(cur, t.queued));
        break;
      }
      case "perm_request":
        // resolve uses the CURRENT socket at click time: if the user switched spawns first, the
        // perm_response targets the new socket and the node no-ops the unknown reqId (harmless).
        setPerm({
          title: f.title ?? "an action",
          resolve: (allow) => { setPerm(null); wsRef.current?.send(encodePermResponse(f.reqId ?? "", allow)); },
        });
        break;
    }
  };

  const openSession = (spawnId: string) => {
    const gen = ++genRef.current;
    wsRef.current?.close();
    connecting();
    const sock = new ReconnectingSocket(`ws://${location.host}/ws/session`, {
      onOpen: () => {
        if (genRef.current !== gen) return;
        // Fresh frame receiver per (re)connect (clean ndjson buffer); set it up BEFORE the bind so the
        // node's replay can't arrive before onmessage is wired. The node resumes from our cursor:
        // a partysocket reconnect keeps lastSeq (resume); a fresh open / spawn-switch has lastSeq 0.
        new Conn(sock, (m) => { if (genRef.current === gen) applyFrame(m as Frame, spawnId); });
        sock.send(JSON.stringify({ spawnId, clientId: CLIENT_ID, token: DEV_TOKEN, cursor: lastSeqRef.current }));
        connected();
      },
      onDown: () => {
        if (genRef.current !== gen) return;
        reconnecting();
      },
    });
    wsRef.current = sock;
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
          setTurn({ state: "idle", queued: 0 });
          turnsRef.current.delete(aid);
          break;
        case "open":
          // Only non-tmux spawns open an App-managed ACP ws here. tmux spawns self-manage BOTH their
          // socket and their header dot via TerminalView (onTermConn). App opens no ws for them, so
          // hasWs is always false and "open" fires every poll tick — re-asserting connecting() here
          // would clobber TerminalView's "connected" dot a few seconds after it goes green. Leave the
          // dot to TerminalView (it reports connecting on mount, connected on open, reconnecting on
          // down, and remounts when you navigate back).
          if (active?.mode !== "tmux") openSession(aid); // just became active -> connect (green)
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
    // refreshSpawns reads refs (activeIdRef/spawnsRef/connRef), not state, so it's safe to run this
    // poll once on mount; including it would re-create the timer every render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // On unmount just close the live ws — spawns persist on the node.
  useEffect(() => () => { wsRef.current?.close(); }, []);

  const spawnApp = async (appId: string, image = "", runnableId = "") => {
    // Switch the UI to the pending state SYNCHRONOUSLY, before the createSpawn round-trip: tear down
    // the previous spawn's live socket, stash + clear its transcript, and show "waiting". Otherwise
    // the old spawn stays "connected" on its live socket during the await, and a prompt sent in that
    // window goes to the OLD spawn (its echo is then dropped when the switch completes). Detach
    // activeId for the duration so the poll can't reopen the previous spawn before the new id is set.
    const prevId = activeIdRef.current;
    lastSeqRef.current = 0;
    teardown();
    setActiveId(null);
    activeIdRef.current = null;
    setItems((current) => {
      if (prevId) buffersRef.current.set(prevId, current);
      return [];
    });
    setTurn({ state: "idle", queued: 0 });
    waiting(); // grey-pulse until the node signals active; the poll then opens the ws
    try {
      const id = await createSpawn(appId, MODEL, image, runnableId); // async CP: returns immediately, status 'starting'
      buffersRef.current.set(id, []);
      setActiveId(id);
      activeIdRef.current = id;
      navigate({ section: "spawn", spawnId: id }); // URL follows; the effect's bindSpawn(id) early-returns (already bound)
      await refreshSpawns(); // sidebar shows the new spawn yellow immediately
    } catch (e: any) {
      errored();
      toast.error("Spawn failed: " + e.message);
    }
  };

  // bindSpawn binds a spawn to the live ws. It is driven by nav (the reconciliation effect) and by
  // imperative actions that already know the new id (spawnApp/onResume). It NEVER navigates or sets a
  // view — the URL is authoritative; this only reconciles the socket. Self-guards when already bound.
  const bindSpawn = (id: string) => {
    if (id === activeIdRef.current) return;
    lastSeqRef.current = 0;
    const prevId = activeIdRef.current;
    closeSession();
    setActiveId(id);
    activeIdRef.current = id;
    const sp = spawnsRef.current.find((s) => s.spawnId === id);
    // An active spawn full-replays from the node (cursor=0), so start EMPTY — seeding from the cached
    // buffer would stack the replay on top of it and double the transcript. The buffer is the view only
    // for non-active spawns (suspended/starting/etc., which don't reconnect+replay).
    const buf = sp?.status === "active" ? [] : (buffersRef.current.get(id) ?? []);
    setItems((current) => {
      if (prevId) buffersRef.current.set(prevId, current);
      return buf;
    });
    setTurn(turnsRef.current.get(id) ?? { state: "idle", queued: 0 });
    if (sp?.status === "active" && sp.mode !== "tmux") openSession(id);
    else if (sp?.status === "active" && sp.mode === "tmux") connecting(); // tmux: TerminalView's socket drives the dot from here
    else if (sp?.status === "starting") waiting();
    else if (sp?.status === "error" || sp?.status === "unreachable") errored();
    // suspended / unknown -> hidden (closeSession already reset())
  };

  // Reconciliation: the URL is authoritative for which spawn is bound. When nav points at a spawn,
  // bind it (bindSpawn self-guards when already bound). Leaving the spawn section does NOT tear down
  // the ws — the bound spawn stays live in the background (matches today: switching to Templates keeps
  // its socket open), so returning is instant.
  useEffect(() => {
    if (nav.section === "spawn" && nav.spawnId) bindSpawn(nav.spawnId);
    // bindSpawn reads refs (activeIdRef/spawnsRef), not state — mirror the poll effect's pattern.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nav]);

  // Normalize "/" -> "/templates" once on mount (replace, so it isn't a back-button trap).
  useEffect(() => {
    if (path === "/") navigate({ section: "templates" }, { replace: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Keep document.title in sync with the current section (and the active spawn's name, when shown).
  // The "app" section is owned by Detail (it sets the real human title once the app fetch resolves):
  // this effect re-runs on every 3s spawns poll, so handling "app" here would clobber Detail's title
  // back to the bare appId every tick — skip it entirely and let Detail own that page's title.
  useEffect(() => {
    if (nav.section === "app") return;
    let label: string;
    if (nav.section === "spawn") {
      const sp = spawns.find((s) => s.spawnId === nav.spawnId);
      label = sp?.name || sp?.appId || nav.spawnId;
    } else {
      label = sectionLabel(nav.section);
    }
    document.title = `Spawnery — ${label}`;
  }, [nav, spawns]);

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
      if (activeIdRef.current === id) {
        closeSession();
        setItems([]);
        setTurn({ state: "idle", queued: 0 });
        turnsRef.current.delete(id);
      }
    } catch (e: any) { toast.error("Suspend failed: " + e.message); }
    refreshSpawns();
  };
  const onResume = async (id: string) => {
    try {
      await resumeSpawn(id);
      lastSeqRef.current = 0;
      const sp = spawnsRef.current.find((s) => s.spawnId === id);
      if (activeIdRef.current === id && sp?.mode !== "tmux") openSession(id);
      // URL follows the resumed spawn. If it wasn't already bound, the nav change drives bindSpawn;
      // its status may still be "starting", so the poll opens the ws — that's fine.
      navigate({ section: "spawn", spawnId: id });
    } catch (e: any) { toast.error("Resume failed: " + e.message); }
    refreshSpawns();
  };
  const onStop = async (id: string) => {
    try { await deleteSpawn(id); } catch (e: any) { toast.error("Stop failed: " + e.message); }
    buffersRef.current.delete(id);
    turnsRef.current.delete(id);
    if (activeIdRef.current === id) {
      closeSession(); setActiveId(null); activeIdRef.current = null; setItems([]); setTurn({ state: "idle", queued: 0 });
      navigate({ section: "templates" }); // the active spawn is gone — leave its URL
    }
    // stopping a non-active spawn does not navigate (the user stays where they are)
    refreshSpawns();
  };

  const onSend = (text: string) => {
    wsRef.current?.send(encodePrompt(text));
  };

  // tmux spawns have no App-managed ACP socket; their TerminalView drives the header dot.
  const onTermConn = (s: "connecting" | "connected" | "reconnecting") => {
    if (s === "connected") connected();
    else if (s === "reconnecting") reconnecting();
    else connecting();
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
      actions={{ onSelectSpawn: (id) => navigate({ section: "spawn", spawnId: id }), onRename, onSuspend, onResume, onStop }}
      onTermConn={onTermConn}
      nav={nav}
      navigate={navigate}
    />
  );
}
