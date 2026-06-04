import { useEffect, useRef, useState } from "react";
import { Textarea } from "@/components/ui/textarea";

export function PromptInput({ disabled, onSend, focusKey }: { disabled: boolean; onSend: (t: string) => void; focusKey?: string | null }) {
  const [t, setT] = useState("");
  const ref = useRef<HTMLTextAreaElement>(null);
  // Focus the input whenever a spawn is (re)selected (focusKey = the active spawn id), so the user can
  // start typing immediately after picking a spawn in the sidebar.
  useEffect(() => { if (focusKey) ref.current?.focus(); }, [focusKey]);
  // Enter sends; Shift+Enter inserts a newline. The textarea is never `disabled` (a disabled element
  // is blurred by the browser, dropping focus right after Enter) — `disabled` only gates sending, so
  // you can keep typing while the agent works (sends queue server-side once connected).
  const send = () => {
    if (disabled || !t.trim()) return;
    onSend(t);
    setT("");
  };
  return (
    <div className="border-t border-border p-3">
      <Textarea
        ref={ref}
        data-testid="prompt-input"
        value={t}
        aria-busy={disabled}
        placeholder="Ask the agent…"
        className="min-h-[2.5rem] resize-none"
        onChange={(e) => setT(e.target.value)}
        onKeyDown={(e) => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); } }}
      />
    </div>
  );
}
