import { useEffect, useRef, useState } from "react";
import { listAgentImages } from "@/api/spawnlet";
import { transportFromMode, type Transport } from "@/api/sessions";

interface Choice { runnable: string; label: string; transport: Transport; }

const SHELL: Choice = { runnable: "shell", label: "Shell", transport: "mosh" };

export function AddSessionMenu({ onCreate }: { onCreate: (transport: Transport, runnable: string) => void }) {
  const [open, setOpen] = useState(false);
  const [choices, setChoices] = useState<Choice[]>([SHELL]);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    listAgentImages().then((imgs) => {
      const byId = new Map<string, Choice>();
      for (const img of imgs) {
        for (const r of img.runnables) {
          if (!byId.has(r.id)) byId.set(r.id, { runnable: r.id, label: r.label || r.id, transport: transportFromMode(r.mode) });
        }
      }
      setChoices([...byId.values(), SHELL]);
    }).catch(() => setChoices([SHELL]));
  }, []);

  // close on outside click
  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false); };
    document.addEventListener("mousedown", onDoc);
    return () => document.removeEventListener("mousedown", onDoc);
  }, [open]);

  return (
    <div ref={ref} className="relative">
      <button
        data-testid="add-session"
        aria-label="New session"
        className="rounded px-2 py-1 text-sm text-muted-foreground hover:text-foreground"
        onClick={() => setOpen((o) => !o)}
      >
        +
      </button>
      {open && (
        <div role="menu" className="absolute left-0 z-50 mt-1 min-w-40 rounded-md border border-border bg-background p-1 shadow-md">
          {choices.map((c) => (
            <button
              key={c.runnable}
              role="menuitem"
              data-testid={`new-session-${c.runnable}`}
              className="block w-full rounded px-2 py-1 text-left text-sm hover:bg-accent"
              onClick={() => { setOpen(false); onCreate(c.transport, c.runnable); }}
            >
              {c.label}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
