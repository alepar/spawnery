import { useEffect, useRef, useState } from "react";
import { recoverStaleFlowMarker } from "./github/flow";
import { useLocation } from "wouter";
import { toast } from "sonner";
import {
  createSpawn, listSpawns, renameSpawn, suspendSpawn, resumeSpawn, recreateSpawn, deleteSpawn,
  type SpawnView,
} from "./api/spawnlet";
import { AppShell } from "./shell/AppShell";
import { useConnStatus } from "./shell/useConnStatus";
import { initialTheme, setTheme } from "./lib/theme";
import { useNav } from "./nav/useNav";
import type { Nav } from "./nav/nav";
import { useSessionStore, authEnabled } from "./auth/session";
import { LoginView } from "./views/LoginView";
import { useMoveTo } from "./views/migration/useMoveTo";
import { MoveToModal } from "./views/migration/MoveToModal";
import { SetModelModal } from "./views/model/SetModelModal";
import { useForkSpawn } from "./views/fork/useForkSpawn";
import { ForkSpawnModal } from "./views/fork/ForkSpawnModal";

const MODEL = "deepseek/deepseek-v4-flash";

// Map a nav section to the document.title label (spawn/app resolve their dynamic name separately).
function sectionLabel(section: Nav["section"]): string {
  switch (section) {
    case "templates": return "Templates";
    case "my-apps":   return "My Apps";
    case "publish":   return "Publish";
    case "settings":  return "Settings";
    case "profiles":  return "Profiles";
    case "app":       return ""; // caller substitutes appId
    case "spawn":     return ""; // caller substitutes the spawn name
  }
}

/** AppRoot gates on auth status, rendering LoginView when not authed (login wall). */
export function App() {
  const status = useSessionStore((s) => s.status);
  // bootstrap() carries any AS callback error code into the store so it survives
  // the destructive parseCallback (which strips the URL/sessionStorage state).
  const callbackErrorCode = useSessionStore((s) => s.callbackErrorCode);

  // Reap a stranded GitHub-link flow marker if bootstrap settled to a non-authed (failure) state:
  // the Settings GitHub panel will not mount on the login wall (spec §6.2 Recovery).
  useEffect(() => { recoverStaleFlowMarker(status); }, [status]);

  // Login wall: show LoginView when auth is enabled and user is not authed.
  if (authEnabled() && status !== "authed") {
    return <LoginView errorCode={callbackErrorCode ?? undefined} />;
  }

  return <AppMain />;
}

function AppMain() {
  const [nav, navigate] = useNav();
  const [path] = useLocation(); // raw pathname, for the one-time "/" -> "/templates" normalize
  // useConnStatus tracks the spawn-LIFECYCLE hint (waiting while a spawn is starting, error if it
  // failed). It no longer drives a header dot — per-session connection state is shown by each tab's
  // own ConnStatus dot, and spawn-lifecycle status is shown by the Sidebar spawn dots. We keep the
  // setters for their side effects on the lifecycle reconciliation effects below.
  const { errored, reset, waiting } = useConnStatus();
  const [spawns, setSpawns] = useState<SpawnView[]>([]);
  const [activeId, setActiveId] = useState<string | null>(null);
  // The spawn whose "Set model…" modal is open (from the sidebar kebab), or null when closed.
  const [modelSpawnId, setModelSpawnId] = useState<string | null>(null);
  const moveTo = useMoveTo();
  const forkSpawn = useForkSpawn();

  // refs mirroring state so async callbacks (poll) don't read stale closures.
  const activeIdRef = useRef<string | null>(null);
  const spawnsRef = useRef<SpawnView[]>([]);
  const moveToPhaseRef = useRef(moveTo.state.phase);
  const forkSpawnPhaseRef = useRef(forkSpawn.state.phase);
  const lastAutoOpenedForkRef = useRef<string | null>(null);
  const forkAutoNavigatePathRef = useRef<string | null>(null);

  useEffect(() => { setTheme(initialTheme()); }, []);
  useEffect(() => { activeIdRef.current = activeId; }, [activeId]);
  useEffect(() => { spawnsRef.current = spawns; }, [spawns]);
  useEffect(() => { moveToPhaseRef.current = moveTo.state.phase; }, [moveTo.state.phase]);
  useEffect(() => { forkSpawnPhaseRef.current = forkSpawn.state.phase; }, [forkSpawn.state.phase]);
  useEffect(() => {
    const forkSpawnId = forkSpawn.state.result?.forkSpawnId;
    if (forkSpawn.state.phase !== "done" || !forkSpawnId || lastAutoOpenedForkRef.current === forkSpawnId) return;
    lastAutoOpenedForkRef.current = forkSpawnId;
    const startPath = forkAutoNavigatePathRef.current;
    forkAutoNavigatePathRef.current = null;
    if (startPath && path === startPath) {
      navigate({ section: "spawn", spawnId: forkSpawnId });
    }
  }, [forkSpawn.state.phase, forkSpawn.state.result?.forkSpawnId, navigate, path]);

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
    // Delivery-pending reconstruction (spec §3): if a spawn reports delivery pending
    // and the modal is not already open for it, auto-open in delivery-pending state.
    for (const sp of list) {
      const anyPendingModalOpen = moveToPhaseRef.current !== "idle" || forkSpawnPhaseRef.current !== "idle";
      if (!sp.journalKeyDeliveryPending || anyPendingModalOpen) continue;
      if (sp.parentSpawnId) {
        forkSpawn.openDeliveryPending(sp.spawnId);
        break; // open at most one at a time
      } else {
        moveTo.openDeliveryPending(sp.spawnId);
        break; // open at most one at a time
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
    // refreshSpawns reads refs (activeIdRef/spawnsRef), not state, so it's safe to run this poll once
    // on mount; including it would re-create the timer every render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const spawnApp = async (appId: string, image = "", runnableId = "", profileId = "") => {
    // Detach the previous spawn synchronously so the keyed SpawnTabs unmounts (tearing down its
    // sockets) before the new spawn arrives. The poll can't reopen the previous spawn while activeId
    // is null.
    setActiveId(null);
    activeIdRef.current = null;
    waiting(); // grey-pulse until the node signals active; SpawnTabs then opens the session sockets
    try {
      const id = await createSpawn(appId, MODEL, image, runnableId, profileId); // async CP: returns immediately, status 'starting'
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
  const onRecreate = async (id: string) => {
    try {
      await recreateSpawn(id);
      // URL follows the recreated spawn (a no-op if it's already selected — no rebind needed: the
      // recreate drops the spawn's session mirror, so SpawnTabs' ListSessions roster poll wipes the
      // stale runtimes and repopulates fresh transcripts on its own).
      navigate({ section: "spawn", spawnId: id });
    } catch (e: any) { toast.error("Recreate failed: " + e.message); }
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
    <>
      <AppShell
        onSpawnApp={spawnApp}
        spawns={spawns}
        activeId={activeId}
        actions={{
          onSelectSpawn: (id) => navigate({ section: "spawn", spawnId: id }),
          onRename,
          onSuspend,
          onResume,
          onRecreate,
          onStop,
          onMoveTo: (id) => moveTo.open(id),
          onFork: (id) => {
            forkAutoNavigatePathRef.current = path;
            forkSpawn.open(id);
          },
          onSetModel: (id) => setModelSpawnId(id),
        }}
        nav={nav}
        navigate={navigate}
      />
      <MoveToModal state={moveTo.state} actions={moveTo} />
      <ForkSpawnModal
        state={forkSpawn.state}
        actions={{
          ...forkSpawn,
          openFork: (id) => navigate({ section: "spawn", spawnId: id }),
        }}
      />
      {/* Live spawn lookup so the modal's model/modelApplied stay fresh across the poll. */}
      <SetModelModal
        spawn={modelSpawnId ? spawns.find((s) => s.spawnId === modelSpawnId) ?? null : null}
        onClose={() => setModelSpawnId(null)}
      />
    </>
  );
}
