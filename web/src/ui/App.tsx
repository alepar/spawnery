import { useEffect, useRef, useState } from "react";
import { createSpawn, stopSpawn } from "../api/spawnlet";
import { Client } from "../acp/client";
import { ChatLog, type Item } from "./ChatLog";
import { PromptInput } from "./PromptInput";
import { PermissionModal } from "./PermissionModal";
import "./app.css";

const APP_PATH = "examples/secret-app";
const MODEL = "openai/gpt-oss-120b:free";

export function App() {
  const [status, setStatus] = useState("starting…");
  const [items, setItems] = useState<Item[]>([]);
  const [busy, setBusy] = useState(true);
  const [perm, setPerm] = useState<{ title: string; resolve: (b: boolean) => void } | null>(null);
  const clientRef = useRef<Client | null>(null);
  const spawnRef = useRef<string>("");
  const wsRef = useRef<WebSocket | null>(null);

  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        const id = await createSpawn(APP_PATH, MODEL);
        if (!alive) { stopSpawn(id); return; }
        spawnRef.current = id;
        const ws = new WebSocket(`ws://${location.host}/ws/session`);
        ws.binaryType = "arraybuffer";
        wsRef.current = ws;
        ws.onopen = async () => {
          ws.send(JSON.stringify({ spawnId: id }));
          const c = new Client(ws as any);
          clientRef.current = c;
          await c.initialize();
          await c.newSession("/app");
          if (alive) { setStatus("ready"); setBusy(false); }
        };
        ws.onerror = () => alive && setStatus("connection error");
        ws.onclose = () => alive && setStatus("session ended");
      } catch (e: any) {
        if (alive) setStatus("error: " + e.message);
      }
    })();
    return () => {
      alive = false;
      wsRef.current?.close();
      if (spawnRef.current) stopSpawn(spawnRef.current);
    };
  }, []);

  const add = (it: Item) => setItems((xs) => [...xs, it]);
  // Streamed chunks arrive one-per-frame; coalesce consecutive chunks of the
  // same kind into a single block (so a streamed thought/message renders as one
  // bubble, not one per word). A different-kind item between runs closes the block.
  const appendChunk = (kind: "agent" | "thought") => (t: string) =>
    setItems((xs) => {
      const last = xs[xs.length - 1];
      if (last && last.kind === kind) return [...xs.slice(0, -1), { kind, text: last.text + t }];
      return [...xs, { kind, text: t }];
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
    <div className="app">
      <header>Spawnery — secret-app <span className="status">{status}</span></header>
      <ChatLog items={items} />
      <PromptInput disabled={busy} onSend={onSend} />
      {perm && <PermissionModal title={perm.title} onResolve={perm.resolve} />}
    </div>
  );
}
