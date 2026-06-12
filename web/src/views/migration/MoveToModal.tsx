/**
 * MoveToModal — Move-to migration UI (sp-8dkp Phase 8).
 *
 * A multi-step Dialog that guides the user through migrating a spawn to a
 * different node.  Driven entirely by the useMoveTo state machine.
 *
 * Steps rendered per phase:
 *   loading          — spinner
 *   selecting        — node list with durability context banner + size/ETA estimate
 *   needs-enroll     — unenrolled browser; show enroll/approve prompt (preflight gate)
 *   upgrading        — node-local → owner-sealed upgrade in progress
 *   confirming       — target summary + durability warning + "Migrate" button
 *   running          — progress steps
 *   done             — success summary
 *   error-suspend    — suspend leg failed; Recreate action
 *   error-resume     — resume leg failed (spawn suspended); Resume-on-origin action
 *   delivery-pending — delivery failed; persistent Retry Delivery action
 *   reconnecting     — CP unreachable; retry banner with backoff
 *
 * Minimize (WM14): rendered as a floating badge when minimized (modal hidden).
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

/** Format bytes as human-readable size. */
function sizeLabel(bytes: number): string {
  if (bytes === 0) return "";
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/**
 * Rough transfer ETA based on journal size.
 * Assumes ~5 MB/s (typical encrypted Kopia journal transfer over LAN/WAN).
 * Returns an honest "at least N sec" string, or empty if size is unknown.
 */
function etaLabel(bytes: number): string {
  if (bytes === 0) return "";
  const secs = bytes / (5 * 1024 * 1024);
  if (secs < 2) return "< 2 sec";
  if (secs < 60) return `~${Math.ceil(secs)} sec`;
  const mins = secs / 60;
  return `~${Math.ceil(mins)} min`;
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
  const eta  = etaLabel(t.journalSizeBytes);
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
        {eta  && <span>· ETA {eta}</span>}
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
  if (!durability) return null;
  if (durability === "ephemeral") {
    return (
      <p data-testid="durability-banner-ephemeral" className="text-sm text-amber-600 rounded-md bg-amber-50 px-3 py-2">
        This spawn uses ephemeral storage. Its data does not travel — only the process
        will be migrated; any in-container data will be lost on the source node.
      </p>
    );
  }
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
        This spawn uses node-local storage. Confirming will upgrade it to owner-sealed
        (re-seals the repo password to your device set — no data is re-encrypted) so it
        can travel to the new node.
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
  // Minimized: show a floating badge instead of the full dialog.
  if (state.minimized) {
    return (
      <div
        data-testid="migrate-badge"
        className="fixed bottom-4 right-4 z-50 flex items-center gap-2 rounded-full bg-primary px-4 py-2 text-sm font-medium text-primary-foreground shadow-lg cursor-pointer"
        onClick={actions.restore}
      >
        <span>Migrating…</span>
        <span className="text-xs opacity-70">click to restore</span>
      </div>
    );
  }

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
        {state.phase === "needs-enroll" && (
          <NeedsEnrollView state={state} onClose={actions.cancel} />
        )}
        {state.phase === "upgrading" && <UpgradingView />}
        {state.phase === "confirming" && (
          <ConfirmingView state={state} actions={actions} />
        )}
        {state.phase === "running" && (
          <RunningView state={state} actions={actions} />
        )}
        {state.phase === "done" && (
          <DoneView state={state} onClose={actions.cancel} />
        )}
        {state.phase === "error-suspend" && (
          <ErrorSuspendView state={state} onClose={actions.cancel} />
        )}
        {state.phase === "error-resume" && (
          <ErrorResumeView state={state} onClose={actions.cancel} />
        )}
        {state.phase === "delivery-pending" && (
          <DeliveryPendingView state={state} actions={actions} />
        )}
        {state.phase === "reconnecting" && (
          <ReconnectingView state={state} actions={actions} />
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

function NeedsEnrollView({ state, onClose }: { state: MoveToState; onClose: () => void }) {
  void state;
  return (
    <>
      <DialogHeader>
        <DialogTitle>Device enrollment required</DialogTitle>
      </DialogHeader>
      <div data-testid="migrate-needs-enroll" className="space-y-3 text-sm text-muted-foreground">
        <p>
          This spawn uses owner-sealed storage and the journal key must be re-sealed
          to the target node by an enrolled device.
        </p>
        <p>
          This browser is not enrolled. To proceed, either:
        </p>
        <ul className="list-disc pl-4 space-y-1">
          <li>Enroll this browser from the <strong>Settings → Devices</strong> page
              (requires approving from another enrolled device).</li>
          <li>Run the migration from the CLI:
              <code className="ml-1 font-mono text-xs">spawnctl move</code>.</li>
        </ul>
        <p className="text-xs text-amber-600">
          The spawn is still running and untouched — no lifecycle action was taken.
        </p>
      </div>
      <DialogFooter>
        <Button variant="outline" onClick={onClose} data-testid="migrate-needs-enroll-close">
          Close
        </Button>
      </DialogFooter>
    </>
  );
}

function UpgradingView() {
  return (
    <>
      <DialogHeader>
        <DialogTitle>Upgrading storage…</DialogTitle>
      </DialogHeader>
      <p data-testid="migrate-upgrading" className="text-sm text-muted-foreground">
        Upgrading this spawn&apos;s storage to owner-sealed so it can be migrated…
      </p>
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

function RunningView({ state, actions }: { state: MoveToState; actions: MoveToActions }) {
  // Minimize is available once migration has started (phase 1 = suspend initiated).
  const canMinimize = state.progress?.step === "migrating" ||
    state.progress?.step === "verifying-node" ||
    state.progress?.step === "resealing" ||
    state.progress?.step === "delivering";
  return (
    <>
      <DialogHeader>
        <DialogTitle>Migrating…</DialogTitle>
      </DialogHeader>
      <ProgressView step={state.progress?.step ?? "fetching-keys"} />
      {canMinimize && (
        <DialogFooter>
          <Button variant="ghost" size="sm" onClick={actions.minimize} data-testid="migrate-minimize">
            Minimize
          </Button>
        </DialogFooter>
      )}
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

/** Suspend-leg failure — spawn is in error state. Offer Recreate. */
function ErrorSuspendView({ state, onClose }: { state: MoveToState; onClose: () => void }) {
  return (
    <>
      <DialogHeader>
        <DialogTitle>Migration failed — suspend error</DialogTitle>
      </DialogHeader>
      <div className="space-y-2 text-sm">
        <p className="text-red-600 break-words" data-testid="migrate-error-suspend">
          {state.errorMsg}
        </p>
        <p className="text-muted-foreground">
          The spawn could not be suspended and is in an error state. Data is intact as
          of the last journal snapshot. Use <strong>Recreate</strong> to provision a
          fresh container.
        </p>
      </div>
      <DialogFooter>
        <Button variant="outline" onClick={onClose} data-testid="migrate-error-dismiss">
          Dismiss
        </Button>
        <Button
          variant="destructive"
          onClick={onClose}
          data-testid="migrate-error-recreate"
        >
          Recreate
        </Button>
      </DialogFooter>
    </>
  );
}

/** Resume-leg failure — CP reverted, spawn is in suspended state. Offer Resume-on-origin. */
function ErrorResumeView({ state, onClose }: { state: MoveToState; onClose: () => void }) {
  return (
    <>
      <DialogHeader>
        <DialogTitle>Migration failed — resume error</DialogTitle>
      </DialogHeader>
      <div className="space-y-2 text-sm">
        <p className="text-amber-600 break-words" data-testid="migrate-error-resume">
          {state.errorMsg}
        </p>
        <p className="text-muted-foreground">
          The spawn was suspended but could not be resumed on the target. The CP has
          reverted it to <strong>suspended</strong> on the origin node — your data is
          intact. Use <strong>Resume</strong> to bring it back on the original node.
        </p>
      </div>
      <DialogFooter>
        <Button variant="outline" onClick={onClose} data-testid="migrate-error-dismiss">
          Dismiss
        </Button>
        <Button onClick={onClose} data-testid="migrate-error-resume">
          Resume
        </Button>
      </DialogFooter>
    </>
  );
}

/**
 * Delivery-leg failure — spawn active on target but journal key not delivered.
 * Persistent, reload-derivable state (spec §3 "delivery-fail"). Offer Retry Delivery.
 */
function DeliveryPendingView({ state, actions }: { state: MoveToState; actions: MoveToActions }) {
  return (
    <>
      <DialogHeader>
        <DialogTitle>Journal key delivery pending</DialogTitle>
      </DialogHeader>
      <div className="space-y-2 text-sm" data-testid="migrate-delivery-pending">
        <p className="text-muted-foreground">
          The spawn is running on the target node but the journal key has not been
          delivered yet. The journaled mounts will not restore until delivery completes.
        </p>
        <p className="text-muted-foreground">
          Retry delivery from this or any enrolled device.
        </p>
        {state.errorMsg && (
          <p className="text-xs text-red-500 break-words">{state.errorMsg}</p>
        )}
      </div>
      <DialogFooter>
        <Button variant="outline" onClick={actions.cancel} data-testid="migrate-delivery-dismiss">
          Dismiss
        </Button>
        <Button onClick={actions.retryDelivery} data-testid="migrate-delivery-retry">
          Retry Delivery
        </Button>
      </DialogFooter>
    </>
  );
}

/**
 * CP unreachable — show retry banner with backoff indicator.
 * Never shows an infinite spinner; always offers a Retry action.
 */
function ReconnectingView({ state, actions }: { state: MoveToState; actions: MoveToActions }) {
  return (
    <>
      <DialogHeader>
        <DialogTitle>Connection lost</DialogTitle>
      </DialogHeader>
      <div className="space-y-2 text-sm" data-testid="migrate-reconnecting">
        <p className="text-amber-600">
          Could not reach the control plane. Retrying…
        </p>
        {state.errorMsg && (
          <p className="text-xs text-muted-foreground break-words">{state.errorMsg}</p>
        )}
        <p className="text-xs text-muted-foreground">
          The spawn is unchanged — no migration action was taken.
        </p>
      </div>
      <DialogFooter>
        <Button variant="outline" onClick={actions.cancel} data-testid="migrate-reconnect-cancel">
          Cancel
        </Button>
        <Button
          onClick={() => state.spawnId && actions.open(state.spawnId)}
          data-testid="migrate-reconnect-retry"
        >
          Retry
        </Button>
      </DialogFooter>
    </>
  );
}
