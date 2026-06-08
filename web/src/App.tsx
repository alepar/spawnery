import { useEffect, useRef, useState } from "react";
import { useLocation } from "wouter";
import { toast } from "sonner";
import {
  createSpawn, listSpawns, renameSpawn, suspendSpawn, resumeSpawn, deleteSpawn,
  type SpawnView,
} from "./api/spawnlet";
import { AppShell } from "./shell/AppShell";
import { useConnStatus } from "./shell/useConnStatus";
import { useSessionStore } from "./sessions/store";
import { initialTheme, setTheme } from "./lib/theme";
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

export function App() {
  const [nav, navigate] = useNav();
  const [path] = useLocation(); // raw pathname, for the one-time "/" -> "/templates" normalize
  // useConnStatus now only feeds the spawn-LIFECYCLE fallback for the header dot (waiting while a
  // spawn is starting, error if it failed). Live per-session conn is owned by the session store and
  // supersedes this fallback once a session panel reports its socket.
  const { conn, errored, reset, waiting } = useConnStatus();
  const [spawns, setSpawns] = useState<SpawnView[]>([]);
  const [activeId, setActiveId] = useState<string | null>(null);

  // refs mirroring state so async callbacks (poll) don't read stale closures.
  const activeIdRef = useRef<string | null>(null);
  const spawnsRef = useRef<SpawnView[]>([]);

  // The header dot is fed by the active SESSION's conn (from the store), falling back to the
  // spawn-lifecycle hint when no session has reported yet (starting/error).
  const sessionConn = useSessionStore((s) => (s.activeId ? (s.conn[s.activeId] ?? null) : null));
  const headerConn = nav.section === "spawn" ? (sessionConn ?? conn) : null;

  useEffect(() => { setTheme(initialTheme()); }, []);
  useEffect(() => { activeIdRef.current = activeId; }, [activeId]);
  useEffect(() => { spawnsRef.current = spawns; }, [spawns]);

  // refreshSpawns fetches the ledger and reconciles the active spawn's header LIFECYCLE hint off its
  // status. It no longer opens/teardowns any socket — SpawnTabs (keyed on activeId) owns the live
  // per-session sockets, and the store's session conn drives the header dot once a session mounts.
  const refreshSpawns = async (): Promise<SpawnView[]> => {
    let list: SpawnView[];
    try { list = await listSpawns(); }
    catch { return spawnsRef.current; }
    setSpawns(list);
    const aid = activeIdRef.current;
    if (aid) {
      const active = list.find((s) => s.spawnId === aid);
      if (active?.status === "starting") waiting();
      else if (active?.status === "error" || active?.status === "unreachable") errored();
      else reset(); // active/suspended: the store's session conn drives the header dot
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
    // refreshSpawns reads refs (activeIdRef/spawnsRef), not state, so it's safe to run this poll once
    // on mount; including it would re-create the timer every render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const spawnApp = async (appId: string, image = "", runnableId = "") => {
    // Detach the previous spawn synchronously so the keyed SpawnTabs unmounts (tearing down its
    // sockets) before the new spawn arrives. The poll can't reopen the previous spawn while activeId
    // is null.
    setActiveId(null);
    activeIdRef.current = null;
    waiting(); // grey-pulse until the node signals active; SpawnTabs then opens the session sockets
    try {
      const id = await createSpawn(appId, MODEL, image, runnableId); // async CP: returns immediately, status 'starting'
      setActiveId(id);
      activeIdRef.current = id;
      navigate({ section: "spawn", spawnId: id }); // URL follows; the effect's bindSpawn(id) early-returns (already bound)
      await refreshSpawns(); // sidebar shows the new spawn yellow immediately
    } catch (e: any) {
      errored();
      toast.error("Spawn failed: " + e.message);
    }
  };

  // bindSpawn binds a spawn as the active one. It is driven by nav (the reconciliation effect) and by
  // imperative actions that already know the new id (spawnApp/onResume). It NEVER navigates or sets a
  // view — the URL is authoritative; this only reconciles which spawn is active. SpawnTabs (keyed on
  // activeId) owns the per-session sockets, so this only sets activeId + the lifecycle header hint.
  const bindSpawn = (id: string) => {
    if (id === activeIdRef.current) return;
    setActiveId(id);
    activeIdRef.current = id;
    const sp = spawnsRef.current.find((s) => s.spawnId === id);
    if (sp?.status === "starting") waiting();
    else if (sp?.status === "error" || sp?.status === "unreachable") errored();
    else reset(); // active/suspended/unknown -> the store's session conn drives the dot
  };

  // Reconciliation: the URL is authoritative for which spawn is bound. When nav points at a spawn,
  // bind it (bindSpawn self-guards when already bound). Leaving the spawn section does NOT tear down
  // SpawnTabs — the bound spawn stays live in the background (matches today: switching to Templates
  // keeps its socket open), so returning is instant.
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
    // Keep the spawn selected (unlike onStop) — the user stays on its now-suspended view. SpawnTabs'
    // poll reconciles its roster; the lifecycle hint follows the next refresh.
    try { await suspendSpawn(id); } catch (e: any) { toast.error("Suspend failed: " + e.message); }
    refreshSpawns();
  };
  const onResume = async (id: string) => {
    try {
      await resumeSpawn(id);
      // URL follows the resumed spawn; the nav change drives bindSpawn (its status may still be
      // "starting", which the poll resolves). SpawnTabs reopens its sockets on (re)mount.
      navigate({ section: "spawn", spawnId: id });
    } catch (e: any) { toast.error("Resume failed: " + e.message); }
    refreshSpawns();
  };
  const onStop = async (id: string) => {
    try { await deleteSpawn(id); } catch (e: any) { toast.error("Stop failed: " + e.message); }
    if (activeIdRef.current === id) {
      setActiveId(null); activeIdRef.current = null; reset();
      navigate({ section: "templates" }); // the active spawn is gone — leave its URL
    }
    // stopping a non-active spawn does not navigate (the user stays where they are)
    refreshSpawns();
  };

  return (
    <AppShell
      headerConn={headerConn}
      onSpawnApp={spawnApp}
      spawns={spawns}
      activeId={activeId}
      actions={{ onSelectSpawn: (id) => navigate({ section: "spawn", spawnId: id }), onRename, onSuspend, onResume, onStop }}
      nav={nav}
      navigate={navigate}
    />
  );
}
