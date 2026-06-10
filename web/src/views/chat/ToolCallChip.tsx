import { useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import type { ContentBlock, Diff } from "@/acp/frames";

// DiffView renders a file edit as a simple line-based removed/added view: the old text as "-" lines
// followed by the new text as "+" lines. A whole-block replace is shown honestly (no LCS minimization),
// which matches opencode's edit semantics (a targeted oldString -> newString swap).
function DiffView({ diff }: { diff: Diff }) {
  const removed = diff.oldText ? diff.oldText.split("\n") : [];
  const added = diff.newText ? diff.newText.split("\n") : [];
  return (
    <div data-testid="tool-diff">
      {diff.path ? (
        <div
          data-testid="tool-diff-path"
          className="mb-0.5 uppercase tracking-wide text-[10px] text-muted-foreground"
        >
          {diff.path}
        </div>
      ) : null}
      <pre className="overflow-x-auto whitespace-pre rounded bg-background/60 p-1">
        {removed.map((line, i) => (
          <div
            key={`r${i}`}
            data-testid="diff-removed"
            className="text-red-600 dark:text-red-400"
          >
            - {line}
          </div>
        ))}
        {added.map((line, i) => (
          <div
            key={`a${i}`}
            data-testid="diff-added"
            className="text-green-700 dark:text-green-400"
          >
            + {line}
          </div>
        ))}
      </pre>
    </div>
  );
}

// stringifyRaw renders a parsed-JSON raw value for the "raw" view (objects pretty-printed; a plain
// string left as-is so a string output isn't shown with surrounding quotes).
function stringifyRaw(v: unknown): string {
  if (v === undefined || v === null) return "";
  if (typeof v === "string") return v;
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}

export function ToolCallChip({
  title,
  status,
  content,
  diff,
  rawInput,
  rawOutput,
}: {
  title: string;
  status?: string;
  content?: ContentBlock[];
  diff?: Diff;
  rawInput?: unknown;
  rawOutput?: unknown;
}) {
  const [open, setOpen] = useState(false);
  const resultText = (content ?? [])
    .map((b) => b.text ?? "")
    .filter(Boolean)
    .join("\n");
  const hasDiff = !!diff && !!(diff.path || diff.oldText || diff.newText);
  const rawIn = stringifyRaw(rawInput);
  const rawOut = stringifyRaw(rawOutput);
  const hasDetail = resultText !== "" || hasDiff || rawIn !== "" || rawOut !== "";

  const chip = (
    <Badge variant="secondary" className="font-mono text-xs">
      🔧 {title}
      {status ? ` — ${status}` : ""}
      {hasDetail ? <span className="ml-1 opacity-60">{open ? "▾" : "▸"}</span> : null}
    </Badge>
  );

  if (!hasDetail) {
    return (
      <div className="mx-auto max-w-[70ch] px-4 py-1" data-role="tool">
        {chip}
      </div>
    );
  }

  return (
    <div className="mx-auto max-w-[70ch] px-4 py-1" data-role="tool">
      <Collapsible open={open} onOpenChange={setOpen}>
        <CollapsibleTrigger
          data-testid="tool-toggle"
          className="cursor-pointer text-left focus:outline-none"
        >
          {chip}
        </CollapsibleTrigger>
        <CollapsibleContent
          data-testid="tool-detail"
          className="mt-1 space-y-2 rounded-md border bg-muted/40 p-2 font-mono text-xs"
        >
          {resultText !== "" && (
            <pre data-testid="tool-result" className="whitespace-pre-wrap break-words">
              {resultText}
            </pre>
          )}
          {hasDiff && diff ? <DiffView diff={diff} /> : null}
          {rawIn !== "" && (
            <div>
              <div className="mb-0.5 uppercase tracking-wide text-[10px] text-muted-foreground">raw input</div>
              <pre data-testid="tool-raw-input" className="whitespace-pre-wrap break-words">
                {rawIn}
              </pre>
            </div>
          )}
          {rawOut !== "" && (
            <div>
              <div className="mb-0.5 uppercase tracking-wide text-[10px] text-muted-foreground">raw output</div>
              <pre data-testid="tool-raw-output" className="whitespace-pre-wrap break-words">
                {rawOut}
              </pre>
            </div>
          )}
        </CollapsibleContent>
      </Collapsible>
    </div>
  );
}
