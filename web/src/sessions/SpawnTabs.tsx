import { useEffect, useRef } from "react";
import { toast } from "sonner";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { ConnStatus } from "@/shell/ConnStatus";
import { AcpSessionPanel } from "./AcpSessionPanel";
import { TerminalView } from "@/views/TerminalView";
import { AddSessionMenu } from "./AddSessionMenu";
import { useSessionStore, type SessionMeta } from "./store";
import { listSessions, createSession, closeSession, type Transport, type SessionDescriptor } from "@/api/sessions";

function toMeta(d: SessionDescriptor): SessionMeta {
  return { ...d, label: d.runnable || "session" };
}

export function SpawnTabs({ spawnId }: { spawnId: string }) {
  const sessions = useSessionStore((s) => s.sessions);
  const activeId = useSessionStore((s) => s.activeId);
  const connMap = useSessionStore((s) => s.conn);
  const bindSpawn = useSessionStore((s) => s.bindSpawn);
  const reconcileRoster = useSessionStore((s) => s.reconcileRoster);
  const setActive = useSessionStore((s) => s.setActive);
  const setConn = useSessionStore((s) => s.setConn);
  const knownIds = useRef<Set<string>>(new Set());

  // Bind the store to this spawn (resets tabs/sockets when spawnId changes).
  useEffect(() => { bindSpawn(spawnId); }, [spawnId, bindSpawn]);

  // Poll ListSessions (no server push — mirrors App's listSpawns poll). refresh() is also called
  // imperatively right after create/close so the new/closed tab appears without waiting a tick.
  const refresh = async (): Promise<SessionMeta[]> => {
    let metas: SessionMeta[];
    try { metas = (await listSessions(spawnId)).map(toMeta); }
    catch { return useSessionStore.getState().sessions; }
    if (useSessionStore.getState().spawnId !== spawnId) return metas; // stale (spawn switched mid-flight)
    reconcileRoster(metas);
    knownIds.current = new Set(metas.map((m) => m.sessionId));
    return metas;
  };

  useEffect(() => {
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    const tick = async () => { await refresh(); if (!stopped) timer = setTimeout(tick, 3000); };
    tick();
    return () => { stopped = true; clearTimeout(timer); };
    // refresh reads spawnId from closure + store via getState; re-run only when spawnId changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [spawnId]);

  const onCreate = async (transport: Transport, runnable: string) => {
    const before = new Set(useSessionStore.getState().sessions.map((m) => m.sessionId));
    try {
      await createSession(spawnId, transport, runnable); // empty response; learn id from the roster
      const after = await refresh();
      const added = after.find((m) => !before.has(m.sessionId)); // node-allocated id
      if (added) setActive(added.sessionId);
    } catch (e: any) { toast.error("New session failed: " + e.message); }
  };

  const onClose = async (sessionId: string) => {
    try {
      await closeSession(spawnId, sessionId);
      if (useSessionStore.getState().activeId === sessionId) setActive("0");
      await refresh();
    } catch (e: any) { toast.error("Close session failed: " + e.message); }
  };

  const active = activeId ?? sessions[0]?.sessionId ?? "";

  return (
    <div className="flex h-full flex-col">
      <Tabs value={active} onValueChange={setActive}>
        <TabsList>
          {sessions.map((s) => {
            const c = connMap[s.sessionId] ?? null;
            return (
              <span key={s.sessionId} className="flex items-center" data-testid={`tab-${s.sessionId}`}>
                <TabsTrigger value={s.sessionId}>
                  {c && <span className="mr-1"><ConnStatus conn={c} /></span>}
                  <span>{s.label}</span>
                </TabsTrigger>
                {!s.pinned && (
                  <button
                    aria-label={`Close ${s.label}`}
                    data-testid={`close-${s.sessionId}`}
                    className="ml-0.5 rounded px-1 text-muted-foreground hover:text-foreground"
                    onClick={() => onClose(s.sessionId)}
                  >
                    ×
                  </button>
                )}
              </span>
            );
          })}
          <AddSessionMenu onCreate={onCreate} />
        </TabsList>
      </Tabs>
      {/* Keep-alive host: every panel stays mounted; inactive ones are display:none. */}
      <div className="relative flex-1 overflow-hidden">
        {sessions.map((s) => {
          const isActive = s.sessionId === active;
          return (
            <div
              key={s.sessionId}
              data-testid={`panel-${s.sessionId}`}
              className="absolute inset-0"
              style={{ display: isActive ? "block" : "none" }}
            >
              {s.transport === "acp"
                ? <AcpSessionPanel spawnId={spawnId} sessionId={s.sessionId} active={isActive} />
                : <TerminalView spawnId={spawnId} sessionId={s.sessionId} active={isActive} onConn={(st) => setConn(s.sessionId, st)} />}
            </div>
          );
        })}
      </div>
    </div>
  );
}
