import { useState } from "react";
import { Textarea } from "@/components/ui/textarea";
import { Button } from "@/components/ui/button";

export function PromptInput({ disabled, onSend }: { disabled: boolean; onSend: (t: string) => void }) {
  const [t, setT] = useState("");
  // Guarded so it never fires while busy. The textarea itself is NOT disabled: a disabled element is
  // blurred by the browser, which would drop focus right after Enter. Keeping it enabled retains focus
  // (and lets you type the next message while the agent responds); the Send button carries the busy state.
  const send = () => {
    if (disabled || !t.trim()) return;
    onSend(t);
    setT("");
  };
  return (
    <div className="flex items-end gap-2 border-t border-border p-3">
      <Textarea
        data-testid="prompt-input"
        value={t}
        aria-busy={disabled}
        placeholder="Ask the agent…"
        className="min-h-[2.5rem] resize-none"
        onChange={(e) => setT(e.target.value)}
        onKeyDown={(e) => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); } }}
      />
      <Button data-testid="prompt-send" disabled={disabled} onClick={send}>Send</Button>
    </div>
  );
}
