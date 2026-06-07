import { useEffect, useMemo, useRef, useState } from "react";
import { Textarea } from "@/components/ui/textarea";
import type { Command } from "@/acp/frames";

// matchSlash returns the command token being typed when the box holds a single `/command` at the very
// start (slash as the first char, no whitespace yet). It returns null otherwise, so the menu stays
// closed for normal prose, a mid-text slash, or once the user types a space after the command name.
function matchSlash(value: string): string | null {
  const m = /^\/([^\s]*)$/.exec(value);
  return m ? m[1] : null;
}

export function PromptInput({ disabled, onSend, focusKey, commands = [] }: {
  disabled: boolean;
  onSend: (t: string) => void;
  focusKey?: string | null;
  commands?: Command[];
}) {
  const [t, setT] = useState("");
  const [hi, setHi] = useState(0); // highlighted menu index
  const ref = useRef<HTMLTextAreaElement>(null);
  // Focus the input whenever a spawn is (re)selected (focusKey = the active spawn id), so the user can
  // start typing immediately after picking a spawn in the sidebar.
  useEffect(() => { if (focusKey) ref.current?.focus(); }, [focusKey]);

  // The `/`-autocomplete menu: open only while the box holds a `/prefix` token AND there are matching
  // commands. With no commands frame (e.g. goose) the list is empty -> the menu never opens (inert).
  const token = matchSlash(t);
  const matches = useMemo(() => {
    if (token === null) return [];
    const p = token.toLowerCase();
    return commands.filter((c) => (c.name ?? "").toLowerCase().startsWith(p));
  }, [token, commands]);
  const menuOpen = matches.length > 0;
  // Keep the highlight in range as the filtered set shrinks/grows.
  useEffect(() => { setHi(0); }, [token]);

  // pick inserts the chosen command as `/name ` (trailing space closes the menu and readies arguments),
  // then refocuses the box so typing continues seamlessly.
  const pick = (c: Command) => {
    setT(`/${c.name ?? ""} `);
    setHi(0);
    ref.current?.focus();
  };

  // Enter sends; Shift+Enter inserts a newline. The textarea is never `disabled` (a disabled element
  // is blurred by the browser, dropping focus right after Enter) — `disabled` only gates sending, so
  // you can keep typing while the agent works (sends queue server-side once connected).
  const send = () => {
    if (disabled || !t.trim()) return;
    onSend(t);
    setT("");
  };
  const onKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (menuOpen) {
      // While the command menu is open, the arrow keys move the highlight and Enter/Tab accept it
      // (instead of sending) so a partially-typed command can be completed in place.
      if (e.key === "ArrowDown") { e.preventDefault(); setHi((i) => Math.min(i + 1, matches.length - 1)); return; }
      if (e.key === "ArrowUp") { e.preventDefault(); setHi((i) => Math.max(i - 1, 0)); return; }
      if (e.key === "Enter" || e.key === "Tab") { e.preventDefault(); pick(matches[hi] ?? matches[0]); return; }
      if (e.key === "Escape") { e.preventDefault(); setT(""); return; }
    }
    if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); }
  };

  return (
    <div className="relative border-t border-border p-3">
      {menuOpen && (
        <ul
          data-testid="command-menu"
          role="listbox"
          className="absolute bottom-full left-3 right-3 mb-1 max-h-60 overflow-auto rounded-md border border-border bg-popover shadow-md"
        >
          {matches.map((c, i) => (
            <li key={c.name}>
              <button
                type="button"
                data-testid="command-option"
                role="option"
                aria-selected={i === hi}
                // onMouseDown (not onClick) so the textarea keeps focus through the selection.
                onMouseDown={(e) => { e.preventDefault(); pick(c); }}
                onMouseEnter={() => setHi(i)}
                className={`flex w-full flex-col items-start px-3 py-1.5 text-left ${i === hi ? "bg-accent" : ""}`}
              >
                <span className="font-medium">/{c.name}</span>
                {c.description && <span className="text-muted-foreground text-11-regular">{c.description}</span>}
              </button>
            </li>
          ))}
        </ul>
      )}
      <Textarea
        ref={ref}
        data-testid="prompt-input"
        value={t}
        aria-busy={disabled}
        placeholder="Ask the agent…"
        className="min-h-[2.5rem] resize-none"
        onChange={(e) => setT(e.target.value)}
        onKeyDown={onKeyDown}
      />
    </div>
  );
}
