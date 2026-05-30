import { useState } from "react";
export function Thoughts({ text }: { text: string }) {
  const [open, setOpen] = useState(false);
  if (!text) return null;
  return (
    <div className="thoughts">
      <button onClick={() => setOpen((v) => !v)}>{open ? "▾" : "▸"} thinking</button>
      {open && <pre>{text}</pre>}
    </div>
  );
}
