import { useState } from "react";
import { Textarea } from "@/components/ui/textarea";
import { Button } from "@/components/ui/button";

export function PromptInput({ disabled, onSend }: { disabled: boolean; onSend: (t: string) => void }) {
  const [t, setT] = useState("");
  const send = () => { if (t.trim()) { onSend(t); setT(""); } };
  return (
    <div className="flex items-end gap-2 border-t border-border p-3">
      <Textarea
        data-testid="prompt-input"
        value={t}
        disabled={disabled}
        placeholder="Ask the agent…"
        className="min-h-[2.5rem] resize-none"
        onChange={(e) => setT(e.target.value)}
        onKeyDown={(e) => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); } }}
      />
      <Button data-testid="prompt-send" disabled={disabled} onClick={send}>Send</Button>
    </div>
  );
}
