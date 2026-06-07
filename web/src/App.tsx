import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import {
  createSpawn, listSpawns, renameSpawn, suspendSpawn, resumeSpawn, deleteSpawn,
  DEV_TOKEN, type SpawnView,
} from "./api/spawnlet";
import { Conn } from "./acp/conn";
import { encodePrompt, encodePermResponse, type Frame, type Command } from "./acp/frames";
import { AppShell } from "./shell/AppShell";
import { useConnStatus } from "./shell/useConnStatus";
import { ReconnectingSocket } from "./shell/reconnectingSocket";
import { nextConnAction } from "./shell/connPolicy";
import { initialTheme, setTheme } from "./lib/theme";
import type { Item, TurnState, PermPrompt } from "./views/chat/types";
import { reconcilePending, MAX_QUEUED } from "./lib/turn";
import { upsertTool as upsertToolItems } from "./lib/toolChip";
import { upsertPlan as upsertPlanItems } from "./lib/plan";

const MODEL = "deepseek/deepseek-v4-flash";

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
  const { conn, connecting, connected, errored, reset, waiting, reconnecting } = useConnStatus();
  const [items, setItems] = useState<Item[]>([]);
  const [turn, setTurn] = useState<TurnState>({ state: "idle", queued: 0 });
  const [perm, setPerm] = useState<PermPrompt | null>(null);
  const [commands, setCommands] = useState<Command[]>([]); // advertised slash commands for the active spawn (cat E)
  const [spawns, setSpawns] = useState<SpawnView[]>([]);
  const [activeId, setActiveId] = useState<string | null>(null);

  const wsRef = useRef<ReconnectingSocket | null>(null);
  const lastSeqRef = useRef(0);
  const idRef = useRef(0);
  const genRef = useRef(0);
  const buffersRef = useRef<Map<string, Item[]>>(new Map());
  const turnsRef = useRef<Map<string, TurnState>>(new Map());
  const commandsRef = useRef<Map<string, Command[]>>(new Map()); // per-spawn command set (cat E)
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
  // upsertTool reconciles tool_call / tool_call_update frames into a single chip keyed by toolId: the
  // creation adds the chip; later updates merge status/content/raw in place (so one tool = one chip).
  const upsertTool = (f: Extract<Frame, { kind: "tool" }>) =>
    setItems((xs) => upsertToolItems(xs, f, () => idRef.current++));
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
        upsertTool(f);
        break;
      case "plan":
        // Replace-in-place: each plan frame carries the WHOLE current plan, so swap the single plan
        // item's entries (see lib/plan.ts). No plan ever -> no plan item -> nothing renders.
        setItems((xs) => upsertPlanItems(xs, f.plan ?? [], () => idRef.current++));
        break;
      case "commands": {
        // Replace-in-place: the frame carries the WHOLE current command set. Stash it per-spawn (so a
        // later spawn-switch restores it) and surface it to the active prompt box's `/`-menu. No
        // commands frame ever (e.g. goose) -> the set stays [] -> the menu is inert (cat E).
        const cmds = f.cmds ?? [];
        commandsRef.current.set(spawnId, cmds);
        setCommands(cmds);
        break;
      }
      case "turn": {
        const t: TurnState = { state: f.state ?? "idle", queued: f.queued ?? 0, reason: f.reason, error: f.error, usage: f.usage };
        setTurn(t);
        turnsRef.current.set(spawnId, t);
        setItems((cur) => reconcilePending(cur, t.queued));
        break;
      }
      case "perm_request":
        // resolve uses the CURRENT socket at click time: if the user switched spawns first, the
        // perm_response targets the new socket and the node no-ops the unknown reqId (harmless).
        // optionId is the agent option the user picked (cat H); "" (dismiss) lets the node auto-deny.
        setPerm({
          title: f.title ?? "an action",
          options: f.options ?? [],
          resolve: (optionId) => { setPerm(null); wsRef.current?.send(encodePermResponse(f.reqId ?? "", optionId)); },
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
          setCommands([]);
          turnsRef.current.delete(aid);
          commandsRef.current.delete(aid);
          break;
        case "open":
          if (active?.mode !== "tmux") openSession(aid); // just became active -> connect (green); tmux spawns self-manage via TerminalView
          else connecting(); // tmux: show pending until TerminalView's socket reports connected (onTermConn)
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
    setCommands([]); // new spawn: its command set arrives via the next `commands` frame
    waiting(); // grey-pulse until the node signals active; the poll then opens the ws
    try {
      const id = await createSpawn(appId, MODEL, image, runnableId); // async CP: returns immediately, status 'starting'
      buffersRef.current.set(id, []);
      setActiveId(id);
      activeIdRef.current = id;
      await refreshSpawns(); // sidebar shows the new spawn yellow immediately
    } catch (e: any) {
      errored();
      toast.error("Spawn failed: " + e.message);
    }
  };

  const selectSpawn = (id: string) => {
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
    // An active spawn replays from cursor 0, so its `commands` frame re-arrives; seed from the cache
    // meanwhile (and it's the only source for a non-active, non-replaying spawn).
    setCommands(commandsRef.current.get(id) ?? []);
    if (sp?.status === "active" && sp.mode !== "tmux") openSession(id);
    else if (sp?.status === "active" && sp.mode === "tmux") connecting(); // tmux: TerminalView's socket drives the dot from here
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
      commandsRef.current.delete(id);
      // keep the spawn selected (unlike onStop) — the user stays on its now-empty suspended view.
      if (activeIdRef.current === id) {
        closeSession();
        setItems([]);
        setTurn({ state: "idle", queued: 0 });
        setCommands([]);
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
    } catch (e: any) { toast.error("Resume failed: " + e.message); }
    refreshSpawns();
  };
  const onStop = async (id: string) => {
    try { await deleteSpawn(id); } catch (e: any) { toast.error("Stop failed: " + e.message); }
    buffersRef.current.delete(id);
    turnsRef.current.delete(id);
    commandsRef.current.delete(id);
    if (activeIdRef.current === id) { closeSession(); setActiveId(null); activeIdRef.current = null; setItems([]); setTurn({ state: "idle", queued: 0 }); setCommands([]); }
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
      commands={commands}
      onSpawnApp={spawnApp}
      spawns={spawns}
      activeId={activeId}
      actions={{ onSelectSpawn: selectSpawn, onRename, onSuspend, onResume, onStop }}
      onTermConn={onTermConn}
    />
  );
}
