// StopButton renders a turn-interrupt control (cat J) shown ONLY while a turn is in flight (busy).
// Clicking it sends the upward cancel frame; the agent then ends the turn with the `cancelled` reason
// (cat G), which flips the turn to idle and hides the button again. Idle turns render nothing — there is
// no turn to cancel.
export function StopButton({ busy, onCancel }: { busy: boolean; onCancel: () => void }) {
  if (!busy) return null;
  return (
    <div className="flex justify-center px-4 pt-2">
      <button
        type="button"
        data-testid="stop-button"
        aria-label="Stop the current turn"
        onClick={onCancel}
        className="rounded border border-border bg-background px-3 py-1 text-xs text-foreground hover:bg-accent"
      >
        ⏹ Stop
      </button>
    </div>
  );
}
