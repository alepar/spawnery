import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import { setSpawnModel } from "@/api/spawnlet";

// ModelControl shows the spawn's CURRENT model — authoritative, from the spawn RECORD via the
// ListSpawns poll (NOT the agent) — and lets the user switch it to any free-form OpenRouter id.
// Submitting calls SpawnService.SetSpawnModel and optimistically marks the switch pending. The
// "pending" badge is shown while the record reports model_applied=false OR while an optimistic
// submit hasn't been reflected by the poll; it clears once the record shows the new id applied.
// No new polling channel — `model`/`modelApplied` arrive via the same poll the rest of the UI uses.
export function ModelControl({ spawnId, model, modelApplied }: {
  spawnId: string;
  model: string;
  modelApplied: boolean;
}) {
  const [draft, setDraft] = useState(model);
  const [optimistic, setOptimistic] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const lastModel = useRef(model);

  // Re-sync the input only when the RECORD's model actually changes (our submit landed, or another
  // client switched it) — never on a plain poll tick, so typing isn't clobbered every few seconds.
  useEffect(() => {
    if (model !== lastModel.current) { lastModel.current = model; setDraft(model); }
  }, [model]);

  // Drop the optimistic flag once the record reflects the submitted id.
  useEffect(() => {
    if (optimistic !== null && model === optimistic) setOptimistic(null);
  }, [model, optimistic]);

  const pending = optimistic !== null || !modelApplied;

  const submit = async () => {
    const next = draft.trim();
    if (busy || !next || next === model) return;
    setOptimistic(next);
    setBusy(true);
    try {
      await setSpawnModel(spawnId, next);
    } catch (e: any) {
      setOptimistic(null);
      toast.error("Set model failed: " + e.message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div data-testid="model-control" className="flex items-center gap-2 px-4 pt-2 text-xs text-muted-foreground">
      <label htmlFor="model-input" className="select-none">Model</label>
      <input
        id="model-input"
        aria-label="Spawn model"
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => { if (e.key === "Enter") submit(); }}
        placeholder="openrouter/model-id"
        disabled={busy}
        className="min-w-0 flex-1 rounded border border-border bg-background px-2 py-1 text-xs text-foreground disabled:opacity-50"
      />
      <button
        type="button"
        aria-label="Set model"
        onClick={submit}
        disabled={busy}
        className="rounded border border-border px-2 py-1 text-xs hover:text-foreground disabled:opacity-50"
      >
        Set
      </button>
      {pending && (
        <span data-testid="model-pending" className="rounded bg-amber-500/20 px-1.5 py-0.5 text-amber-600">
          pending
        </span>
      )}
    </div>
  );
}
