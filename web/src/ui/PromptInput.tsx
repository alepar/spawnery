import { useState } from "react";
export function PromptInput({ disabled, onSend }: { disabled: boolean; onSend: (t: string) => void }) {
  const [t, setT] = useState("");
  const send = () => { if (t.trim()) { onSend(t); setT(""); } };
  return (
    <div className="input">
      <textarea value={t} disabled={disabled} placeholder="Ask the agent…"
        onChange={(e) => setT(e.target.value)}
        onKeyDown={(e) => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); } }} />
      <button disabled={disabled} onClick={send}>Send</button>
    </div>
  );
}
