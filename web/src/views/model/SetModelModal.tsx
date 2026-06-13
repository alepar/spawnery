import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { setSpawnModel, type SpawnView } from "@/api/spawnlet";

// SetModelModal switches the model of an already-running spawn. Opened from the spawn's kebab
// context menu ("Set model…"), mirroring the Move-to modal. It shows the spawn's CURRENT model —
// authoritative, from the spawn RECORD via the ListSpawns poll (NOT the agent) — and lets the user
// switch it to any free-form OpenRouter id. Submitting calls SpawnService.SetSpawnModel and
// optimistically marks the switch pending. The "pending" badge is shown while the record reports
// model_applied=false OR while an optimistic submit hasn't been reflected by the poll; it clears
// once the record shows the new id applied. No new polling channel — `model`/`modelApplied` arrive
// via the same poll the rest of the UI uses (the live `spawn` prop re-renders this on each tick).
export function SetModelModal({ spawn, onClose }: {
  // The live SpawnView for the spawn being edited, or null when the modal is closed. Passing the
  // live object (looked up from App's poll) keeps `model`/`modelApplied` fresh while open.
  spawn: SpawnView | null;
  onClose: () => void;
}) {
  return (
    <Dialog open={spawn !== null} onOpenChange={(o) => { if (!o) onClose(); }}>
      <DialogContent data-testid="set-model-modal" className="max-w-md">
        {spawn && <SetModelBody key={spawn.spawnId} spawn={spawn} onClose={onClose} />}
      </DialogContent>
    </Dialog>
  );
}

function SetModelBody({ spawn, onClose }: { spawn: SpawnView; onClose: () => void }) {
  const { spawnId, model, modelApplied } = spawn;
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
    <>
      <DialogHeader>
        <DialogTitle>Set model</DialogTitle>
      </DialogHeader>
      <div className="space-y-3">
        <div className="flex items-center gap-2 text-sm text-muted-foreground">
          <span>Current:</span>
          <span data-testid="set-model-current" className="font-mono text-foreground break-all">
            {model || "—"}
          </span>
          {pending && (
            <span data-testid="model-pending" className="rounded bg-amber-500/20 px-1.5 py-0.5 text-xs text-amber-600">
              pending
            </span>
          )}
        </div>
        <input
          id="model-input"
          aria-label="Spawn model"
          autoFocus
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => { if (e.key === "Enter") submit(); }}
          placeholder="openrouter/model-id"
          disabled={busy}
          className="w-full rounded border border-border bg-background px-2 py-1 text-sm text-foreground disabled:opacity-50"
        />
      </div>
      <DialogFooter>
        <Button variant="outline" onClick={onClose} data-testid="set-model-cancel">
          Cancel
        </Button>
        <Button
          aria-label="Set model"
          onClick={submit}
          disabled={busy || !draft.trim() || draft.trim() === model}
          data-testid="set-model-submit"
        >
          Set
        </Button>
      </DialogFooter>
    </>
  );
}
