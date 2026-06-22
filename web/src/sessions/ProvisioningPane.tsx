import { cn } from "@/lib/utils";
import type { SpawnView } from "@/api/spawnlet";

export function ProvisioningPane({ spawn }: { spawn: SpawnView }) {
  if (spawn.status === "error") {
    return (
      <div data-testid="provisioning-pane" data-state="error" className="mx-auto max-w-[70ch] px-4 py-6">
        <div className="rounded-md border border-red-500/40 bg-muted/40 p-4">
          <div className="mb-1 text-sm font-semibold text-red-500">Provisioning failed</div>
          {spawn.errorStep && (
            <div data-testid="provisioning-error-step" className="mb-2 text-sm text-muted-foreground">
              Failed at: <span className="font-medium text-foreground">{spawn.errorStep}</span>
            </div>
          )}
          {spawn.errorDetail && (
            <details open>
              <summary className="cursor-pointer text-xs uppercase tracking-wide text-muted-foreground">Error detail</summary>
              <pre data-testid="provisioning-error-detail" className="mt-2 max-h-64 overflow-auto whitespace-pre-wrap rounded bg-background p-2 font-mono text-xs text-foreground">
                {spawn.errorDetail}
              </pre>
            </details>
          )}
        </div>
      </div>
    );
  }
  const total = spawn.provisionTotal ?? 0;
  const cur = spawn.provisionStep ?? 0;
  const doneCount = Math.max(0, cur - 1);
  const shownStep = Math.min(cur, total);
  return (
    <div data-testid="provisioning-pane" data-state="starting" className="mx-auto max-w-[70ch] px-4 py-6">
      <div className="rounded-md border border-border bg-muted/40 p-4">
        <div className="mb-2 text-sm font-semibold">Provisioning…</div>
        {total > 0 && (
          <div data-testid="provisioning-steps" className="mb-3 flex gap-1">
            {Array.from({ length: Math.min(total, 50) }, (_, i) => {
              const state = i < doneCount ? "done" : i === doneCount ? "running" : "pending";
              return <span key={i} data-testid="provisioning-step" data-state={state}
                className={cn("h-1.5 flex-1 rounded-full",
                  state === "done" && "bg-green-500",
                  state === "running" && "bg-amber-500 animate-pulse",
                  state === "pending" && "bg-zinc-400/40")} />;
            })}
          </div>
        )}
        <div data-testid="provisioning-label" className="text-sm text-muted-foreground">
          {total > 0 ? `Step ${shownStep} of ${total}` : "Starting…"}
          {spawn.provisionStepLabel ? `: ${spawn.provisionStepLabel}` : ""}
        </div>
      </div>
    </div>
  );
}
