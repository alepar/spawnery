import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import { createSpawn, stopSpawn, DEV_TOKEN } from "./api/spawnlet";
import { Client } from "./acp/client";
import { AppShell } from "./shell/AppShell";
import { initialTheme, setTheme } from "./lib/theme";
import type { Item } from "./views/chat/types";

const APP_ID = "secret-app";
const MODEL = "openai/gpt-oss-120b:free";

export function App() {
  const [status, setStatus] = useState("starting…");
  const [items, setItems] = useState<Item[]>([]);
  const [busy, setBusy] = useState(true);
  const [perm, setPerm] = useState<{ title: string; resolve: (b: boolean) => void } | null>(null);
  const clientRef = useRef<Client | null>(null);
  const spawnRef = useRef<string>("");
  const wsRef = useRef<WebSocket | null>(null);
  const idRef = useRef(0);
  const genRef = useRef(0);

  useEffect(() => { setTheme(initialTheme()); }, []);

  const spawnApp = async (appId: string, model: string) => {
    const gen = ++genRef.current;
    wsRef.current?.close();
    if (spawnRef.current) stopSpawn(spawnRef.current);
    spawnRef.current = "";
    setItems([]);
    setBusy(true);
    setStatus("starting…");
    try {
      const id = await createSpawn(appId, model);
      if (genRef.current !== gen) { stopSpawn(id); return; }
      spawnRef.current = id;
      const ws = new WebSocket(`ws://${location.host}/ws/session`);
      ws.binaryType = "arraybuffer";
      wsRef.current = ws;
      ws.onopen = async () => {
        ws.send(JSON.stringify({ spawnId: id, token: DEV_TOKEN }));
        const c = new Client(ws as any);
        clientRef.current = c;
        await c.initialize();
        await c.newSession("/app");
        if (genRef.current !== gen) return;
        setStatus("ready"); setBusy(false);
      };
      ws.onerror = () => { if (genRef.current !== gen) return; setStatus("connection error"); toast.error("Connection error"); };
      ws.onclose = () => { if (genRef.current !== gen) return; setStatus("session ended"); };
    } catch (e: any) {
      if (genRef.current !== gen) return;
      setStatus("error: " + e.message); toast.error("Spawn failed: " + e.message);
    }
  };

  useEffect(() => {
    spawnApp(APP_ID, MODEL);
    return () => { wsRef.current?.close(); if (spawnRef.current) stopSpawn(spawnRef.current); };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  type ItemInput = Item extends infer T ? (T extends { id: number } ? Omit<T, "id"> : never) : never;
  const add = (it: ItemInput) =>
    setItems((xs) => [...xs, { ...it, id: idRef.current++ } as Item]);

  // Streamed chunks arrive one-per-frame; coalesce consecutive chunks of the same kind
  // into a single block (so a streamed thought/message renders as one bubble). The id is
  // kept stable across appends so the virtualized row memoizes correctly.
  const appendChunk = (kind: "agent" | "thought") => (t: string) =>
    setItems((xs) => {
      const last = xs[xs.length - 1];
      if (last && last.kind === kind) return [...xs.slice(0, -1), { ...last, text: last.text + t }];
      return [...xs, { id: idRef.current++, kind, text: t } as Item];
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

  return <AppShell status={status} items={items} busy={busy} onSend={onSend} perm={perm} onSpawnApp={(appId) => spawnApp(appId, MODEL)} />;
}
