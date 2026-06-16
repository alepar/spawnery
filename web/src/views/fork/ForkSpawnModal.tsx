import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { DeliveryPendingPanel, TargetList } from "@/views/migration/MoveToModal";
import type { ForkSpawnActions, ForkSpawnState } from "./useForkSpawn";

export type ForkSpawnModalActions = ForkSpawnActions & {
  openFork: (forkSpawnId: string) => void;
};

export function ForkSpawnModal({
  state,
  actions,
}: {
  state: ForkSpawnState;
  actions: ForkSpawnModalActions;
}) {
  const open = state.phase !== "idle";
  return (
    <Dialog open={open} onOpenChange={(o) => { if (!o) actions.cancel(); }}>
      <DialogContent data-testid="fork-modal" className="max-w-md">
        {state.phase === "loading" && <LoadingView />}
        {state.phase === "selecting" && <SelectingView state={state} actions={actions} />}
        {state.phase === "confirming" && <ConfirmingView state={state} actions={actions} />}
        {state.phase === "running" && <RunningView state={state} />}
        {state.phase === "done" && <DoneView state={state} actions={actions} />}
        {state.phase === "delivery-pending" && <DeliveryPendingView state={state} actions={actions} />}
        {state.phase === "needs-enroll" && <NeedsEnrollView actions={actions} />}
        {state.phase === "reconnecting" && <ReconnectingView state={state} actions={actions} />}
      </DialogContent>
    </Dialog>
  );
}

function targetLabel(state: ForkSpawnState): string {
  const t = state.selectedTarget;
  if (!t) return "target";
  if (t.isCurrent) return "same node";
  if (t.class === "cloud") return "cloud";
  return t.nodeId;
}

function ForkNameInput({ state, actions }: { state: ForkSpawnState; actions: ForkSpawnModalActions }) {
  return (
    <input
      data-testid="fork-name-input"
      value={state.name}
      onChange={(e) => actions.setName(e.target.value)}
      placeholder="Fork name"
      className="w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
    />
  );
}

function LoadingView() {
  return (
    <>
      <DialogHeader>
        <DialogTitle>Fork…</DialogTitle>
      </DialogHeader>
      <p className="text-sm text-muted-foreground">Loading targets…</p>
    </>
  );
}

function SelectingView({ state, actions }: { state: ForkSpawnState; actions: ForkSpawnModalActions }) {
  return (
    <>
      <DialogHeader>
        <DialogTitle>Fork…</DialogTitle>
      </DialogHeader>
      <div className="space-y-3">
        <ForkNameInput state={state} actions={actions} />
        <TargetList
          targets={state.targets}
          selectedTarget={state.selectedTarget}
          testId="fork-target-list"
          targetTestIdPrefix="fork-target"
          onSelect={actions.select}
        />
      </div>
      <DialogFooter>
        <Button variant="outline" onClick={actions.cancel}>Cancel</Button>
        <Button
          disabled={!state.selectedTarget}
          onClick={() => state.selectedTarget && actions.select(state.selectedTarget)}
        >
          Continue
        </Button>
      </DialogFooter>
    </>
  );
}

function ConfirmingView({ state, actions }: { state: ForkSpawnState; actions: ForkSpawnModalActions }) {
  return (
    <>
      <DialogHeader>
        <DialogTitle>Fork to {targetLabel(state)}?</DialogTitle>
      </DialogHeader>
      <div className="space-y-3">
        <ForkNameInput state={state} actions={actions} />
        <p className="text-sm text-muted-foreground">The source stays active.</p>
      </div>
      <DialogFooter>
        <Button variant="outline" onClick={() => state.sourceSpawnId && actions.open(state.sourceSpawnId)}>
          Back
        </Button>
        <Button onClick={actions.confirm} data-testid="fork-confirm">
          Fork
        </Button>
      </DialogFooter>
    </>
  );
}

function RunningView({ state }: { state: ForkSpawnState }) {
  const label = state.progress === "verifying-node" || state.progress === "resealing" || state.progress === "delivering"
    ? "Seeding…"
    : "Forking…";
  return (
    <>
      <DialogHeader>
        <DialogTitle>{label}</DialogTitle>
      </DialogHeader>
      <p data-testid="fork-running" className="text-sm text-muted-foreground">
        {label}
      </p>
    </>
  );
}

function DoneView({ state, actions }: { state: ForkSpawnState; actions: ForkSpawnModalActions }) {
  const forkSpawnId = state.result?.forkSpawnId ?? state.forkSpawnId ?? "";
  return (
    <>
      <DialogHeader>
        <DialogTitle>Fork ready</DialogTitle>
      </DialogHeader>
      <div data-testid="fork-done" className="space-y-2 text-sm">
        <p className="text-muted-foreground">
          Fork is running on {state.result?.resolvedNodeId || "the selected node"}.
        </p>
      </div>
      <DialogFooter>
        <Button variant="outline" onClick={actions.cancel}>Close</Button>
        <Button data-testid="fork-open" onClick={() => forkSpawnId && actions.openFork(forkSpawnId)}>
          Open fork
        </Button>
      </DialogFooter>
    </>
  );
}

function DeliveryPendingView({ state, actions }: { state: ForkSpawnState; actions: ForkSpawnModalActions }) {
  return (
    <DeliveryPendingPanel
      title="Fork delivery pending"
      body={[
        "The fork exists, but journaled mounts are waiting for key delivery.",
        "Retry delivery from this or any enrolled device.",
      ]}
      errorMsg={state.errorMsg}
      containerTestId="fork-delivery-pending"
      dismissTestId="fork-delivery-dismiss"
      retryTestId="fork-delivery-retry"
      dismissLabel="Dismiss"
      retryLabel="Retry Delivery"
      onDismiss={actions.cancel}
      onRetry={actions.retryDelivery}
    />
  );
}

function NeedsEnrollView({ actions }: { actions: ForkSpawnModalActions }) {
  return (
    <>
      <DialogHeader>
        <DialogTitle>Device enrollment required</DialogTitle>
      </DialogHeader>
      <p className="text-sm text-muted-foreground">
        This fork needs journal key delivery from an enrolled device.
      </p>
      <DialogFooter>
        <Button onClick={actions.cancel}>Close</Button>
      </DialogFooter>
    </>
  );
}

function ReconnectingView({ state, actions }: { state: ForkSpawnState; actions: ForkSpawnModalActions }) {
  return (
    <>
      <DialogHeader>
        <DialogTitle>Connection lost</DialogTitle>
      </DialogHeader>
      <div className="space-y-2 text-sm">
        <p className="text-amber-600">Could not reach the control plane.</p>
        {state.errorMsg && <p className="text-xs text-muted-foreground break-words">{state.errorMsg}</p>}
      </div>
      <DialogFooter>
        <Button variant="outline" onClick={actions.cancel}>Cancel</Button>
        {state.sourceSpawnId && <Button onClick={() => actions.open(state.sourceSpawnId!)}>Retry</Button>}
      </DialogFooter>
    </>
  );
}
