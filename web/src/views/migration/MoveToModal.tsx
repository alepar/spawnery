/**
 * MoveToModal — Move-to migration UI (sp-8dkp Phase 8).
 *
 * A multi-step Dialog that guides the user through migrating a spawn to a
 * different node.  Driven entirely by the useMoveTo state machine.
 *
 * Steps rendered per phase:
 *   loading    — spinner
 *   selecting  — node list with durability context banner
 *   confirming — target summary + durability warning + "Migrate" button
 *   running    — progress steps (fetching-keys / migrating / verifying-node /
 *                                resealing / delivering / done)
 *   done       — success summary
 *   error      — error text + dismiss button
 */

import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import type { MoveToState, MoveToActions } from "./useMoveTo";
import type { MigrationTarget } from "@/api/migration";

// ── Helpers ───────────────────────────────────────────────────────────────────

function sizeLabel(bytes: number): string {
  if (bytes === 0) return "";
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

function classLabel(cls: string): string {
  return cls === "self-hosted" ? "self-hosted" : cls;
}

// ── Target row ────────────────────────────────────────────────────────────────

function TargetRow({
  t,
  onSelect,
}: {
  t: MigrationTarget;
  onSelect: () => void;
}) {
  const size = sizeLabel(t.journalSizeBytes);
  return (
    <button
      data-testid={`migrate-target-${t.nodeId}`}
      onClick={onSelect}
      disabled={!t.online}
      className={[
        "w-full rounded-md border border-border px-3 py-2 text-left text-sm",
        t.online
          ? "hover:bg-accent cursor-pointer"
          : "opacity-40 cursor-not-allowed",
      ].join(" ")}
    >
      <div className="flex items-center justify-between gap-2">
        <span className="font-medium">{t.nodeId}</span>
        <span className="text-xs text-muted-foreground">{classLabel(t.class)}</span>
      </div>
      <div className="flex items-center gap-2 mt-0.5 text-xs text-muted-foreground">
        {t.online ? (
          <span className="text-green-500">online</span>
        ) : (
          <span className="text-zinc-400">offline</span>
        )}
        {size && <span>· journal {size}</span>}
      </div>
    </button>
  );
}

// ── Progress step indicator ───────────────────────────────────────────────────

const STEP_LABELS: Record<string, string> = {
  "fetching-keys":    "Fetching journal key ciphertext…",
  "migrating":        "Migrating spawn (suspend source → resume on target)…",
  "verifying-node":   "Verifying target node certificate…",
  "resealing":        "Re-sealing journal key to target node…",
  "delivering":       "Delivering sealed key to target…",
  "done":             "Migration complete.",
};

function ProgressView({ step }: { step: string }) {
  const steps = ["fetching-keys", "migrating", "verifying-node", "resealing", "delivering"];
  const current = steps.indexOf(step);
  return (
    <div data-testid="migrate-progress" className="space-y-2">
      {steps.map((s, i) => (
        <div
          key={s}
          data-testid={`migrate-step-${s}`}
          className={[
            "flex items-center gap-2 text-sm",
            i < current ? "text-muted-foreground" :
            i === current ? "text-foreground font-medium" :
            "text-muted-foreground/40",
          ].join(" ")}
        >
          <span className="w-3 text-center">
            {i < current ? "✓" : i === current ? "▶" : "○"}
          </span>
          <span>{STEP_LABELS[s] ?? s}</span>
        </div>
      ))}
    </div>
  );
}

// ── Durability banner ─────────────────────────────────────────────────────────

function DurabilityBanner({ durability }: { durability: string | null }) {
  if (!durability || durability === "ephemeral") return null;
  if (durability === "owner-sealed") {
    return (
      <p data-testid="durability-banner-owner-sealed" className="text-sm text-muted-foreground rounded-md bg-muted/40 px-3 py-2">
        This spawn has journaled mounts. The journal key will be re-sealed to the
        target node so your data restores automatically.
      </p>
    );
  }
  if (durability === "node-local") {
    return (
      <p data-testid="durability-banner-node-local" className="text-sm text-amber-600 rounded-md bg-amber-50 px-3 py-2">
        This spawn has node-local journaled mounts that are not yet owner-sealed.
        Migration will proceed, but the journal data will not be restored on the
        target. Run <code className="font-mono text-xs">spawnctl upgrade-sealed</code> first
        to enable full data portability.
      </p>
    );
  }
  return null;
}

// ── Main modal ────────────────────────────────────────────────────────────────

export function MoveToModal({
  state,
  actions,
}: {
  state: MoveToState;
  actions: MoveToActions;
}) {
  const open = state.phase !== "idle";

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => { if (!o) actions.cancel(); }}
    >
      <DialogContent data-testid="move-to-modal" className="max-w-md">
        {state.phase === "loading" && <LoadingView />}
        {state.phase === "selecting" && (
          <SelectingView state={state} actions={actions} />
        )}
        {state.phase === "confirming" && (
          <ConfirmingView state={state} actions={actions} />
        )}
        {state.phase === "running" && (
          <RunningView state={state} />
        )}
        {state.phase === "done" && (
          <DoneView state={state} onClose={actions.cancel} />
        )}
        {state.phase === "error" && (
          <ErrorView state={state} onClose={actions.cancel} />
        )}
      </DialogContent>
    </Dialog>
  );
}

// ── Sub-views ─────────────────────────────────────────────────────────────────

function LoadingView() {
  return (
    <>
      <DialogHeader>
        <DialogTitle>Move to…</DialogTitle>
      </DialogHeader>
      <p data-testid="migrate-loading" className="text-sm text-muted-foreground">
        Loading migration targets…
      </p>
    </>
  );
}

function SelectingView({ state, actions }: { state: MoveToState; actions: MoveToActions }) {
  return (
    <>
      <DialogHeader>
        <DialogTitle>Move to…</DialogTitle>
      </DialogHeader>
      <div className="space-y-3">
        <DurabilityBanner durability={state.durability} />
        {state.targets.length === 0 ? (
          <p data-testid="migrate-no-targets" className="text-sm text-muted-foreground">
            No other nodes are available.
          </p>
        ) : (
          <div data-testid="migrate-target-list" className="space-y-1.5">
            {state.targets.map((t) => (
              <TargetRow key={t.nodeId} t={t} onSelect={() => actions.select(t)} />
            ))}
          </div>
        )}
      </div>
      <DialogFooter>
        <Button variant="outline" onClick={actions.cancel} data-testid="migrate-cancel">
          Cancel
        </Button>
      </DialogFooter>
    </>
  );
}

function ConfirmingView({ state, actions }: { state: MoveToState; actions: MoveToActions }) {
  const t = state.selectedTarget;
  if (!t) return null;
  return (
    <>
      <DialogHeader>
        <DialogTitle>Migrate to {t.nodeId}?</DialogTitle>
      </DialogHeader>
      <div className="space-y-3">
        <p className="text-sm text-muted-foreground">
          The spawn will be suspended on its current node and resumed on{" "}
          <strong className="text-foreground">{t.nodeId}</strong> ({classLabel(t.class)}).
        </p>
        <DurabilityBanner durability={state.durability} />
      </div>
      <DialogFooter>
        <Button
          variant="outline"
          onClick={() => actions.open(state.spawnId!)}
          data-testid="migrate-back"
        >
          Back
        </Button>
        <Button onClick={actions.confirm} data-testid="migrate-confirm">
          Migrate
        </Button>
      </DialogFooter>
    </>
  );
}

function RunningView({ state }: { state: MoveToState }) {
  return (
    <>
      <DialogHeader>
        <DialogTitle>Migrating…</DialogTitle>
      </DialogHeader>
      <ProgressView step={state.progress?.step ?? "fetching-keys"} />
    </>
  );
}

function DoneView({ state, onClose }: { state: MoveToState; onClose: () => void }) {
  const res = state.result;
  return (
    <>
      <DialogHeader>
        <DialogTitle>Migration complete</DialogTitle>
      </DialogHeader>
      <div data-testid="migrate-done" className="space-y-2 text-sm">
        <p>
          Spawn is now running on{" "}
          <strong className="font-medium">{res?.resolvedNodeId}</strong>.
        </p>
        {res && res.journalKeysDelivered > 0 && (
          <p className="text-muted-foreground">
            {res.journalKeysDelivered} journal key{res.journalKeysDelivered > 1 ? "s" : ""} delivered
            — journaled mounts will restore automatically.
          </p>
        )}
        {res && res.journalKeysDelivered === 0 && (
          <p className="text-muted-foreground">No journal mounts to restore.</p>
        )}
      </div>
      <DialogFooter>
        <Button onClick={onClose} data-testid="migrate-done-close">
          Close
        </Button>
      </DialogFooter>
    </>
  );
}

function ErrorView({ state, onClose }: { state: MoveToState; onClose: () => void }) {
  return (
    <>
      <DialogHeader>
        <DialogTitle>Migration failed</DialogTitle>
      </DialogHeader>
      <p data-testid="migrate-error" className="text-sm text-red-600 break-words">
        {state.errorMsg}
      </p>
      <DialogFooter>
        <Button variant="outline" onClick={onClose} data-testid="migrate-error-close">
          Dismiss
        </Button>
      </DialogFooter>
    </>
  );
}
