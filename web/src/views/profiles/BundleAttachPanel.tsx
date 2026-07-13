import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import type { BundleMember } from "@/api/bundles";

// A rename must be a single clean path segment — the server rejects anything else too
// (sp-mwco.1.9 D2/D4); this is a fail-early client-side check, not the source of truth.
const CLEAN_SEGMENT = /^[A-Za-z0-9._-]+$/;

export interface BundleAttachOverrides {
  excludedSubdirs: string[];
  memberRenames: Record<string, string>;
}

export interface BundleAttachPanelProps {
  bundleId: string;
  name: string;
  members: BundleMember[];
  skippedEntries: string[];
  warnings: string[];
  /** IngestSkillFromURLResponse.changed / ReingestBundleResponse.changed. Undefined = unknown (n/a). */
  changed?: boolean;
  attaching?: boolean;
  onAttach: (overrides: BundleAttachOverrides) => void;
}

/**
 * Ingest/attach result panel (sp-mwco.1.9 D2): shows discovered members, skipped entries
 * (symlinks etc.), and warnings from an ingest or bundle listing, and lets the user pick
 * per-member exclude/rename before an explicit "Attach to profile" — an ingest that discovers
 * 40 skills must not silently attach 40.
 */
export function BundleAttachPanel({
  bundleId, name, members, skippedEntries, warnings, changed, attaching, onAttach,
}: BundleAttachPanelProps) {
  const [excluded, setExcluded] = useState<Set<string>>(new Set());
  const [renames, setRenames] = useState<Record<string, string>>({});

  const toggleExclude = (subdir: string) => {
    setExcluded((prev) => {
      const next = new Set(prev);
      if (next.has(subdir)) next.delete(subdir); else next.add(subdir);
      return next;
    });
  };

  const setRename = (subdir: string, value: string) => {
    setRenames((prev) => ({ ...prev, [subdir]: value }));
  };

  const activeRenames = Object.entries(renames).filter(([, v]) => v.trim() !== "");
  const invalidRename = activeRenames.some(([, v]) => !CLEAN_SEGMENT.test(v.trim()));
  const allExcluded = members.length > 0 && excluded.size === members.length;
  const disabled = !!attaching || allExcluded || invalidRename || members.length === 0;

  const handleAttach = () => {
    if (disabled) return;
    const memberRenames: Record<string, string> = {};
    for (const [subdir, value] of activeRenames) memberRenames[subdir] = value.trim();
    onAttach({ excludedSubdirs: Array.from(excluded), memberRenames });
  };

  return (
    <div data-testid={`bundle-attach-panel-${bundleId}`} className="flex flex-col gap-3">
      <div className="text-sm font-semibold">{name}</div>

      {changed === false && (
        <div className="rounded bg-muted px-2 py-1 text-xs text-muted-foreground">
          Already ingested; no new version was cut.
        </div>
      )}

      {skippedEntries.length > 0 && (
        <div data-testid="bundle-attach-skipped" className="flex flex-col gap-0.5">
          <div className="text-xs font-medium text-muted-foreground">Skipped entries</div>
          <ul className="list-inside list-disc text-xs text-muted-foreground">
            {skippedEntries.map((s) => <li key={s}>skipped: {s}</li>)}
          </ul>
        </div>
      )}

      {warnings.length > 0 && (
        <div data-testid="bundle-attach-warnings" className="flex flex-col gap-0.5">
          <div className="text-xs font-medium text-amber-600 dark:text-amber-400">Warnings</div>
          <ul className="list-inside list-disc text-xs text-amber-600 dark:text-amber-400">
            {warnings.map((w) => <li key={w}>{w}</li>)}
          </ul>
        </div>
      )}

      <div className="flex flex-col gap-2">
        {members.map((m) => (
          <div key={m.catalogId} className="flex items-center gap-2 rounded border border-border p-2">
            <input
              type="checkbox"
              data-testid={`member-exclude-${m.catalogId}`}
              aria-label={`Exclude ${m.name}`}
              checked={excluded.has(m.sourceSubdir)}
              onChange={() => toggleExclude(m.sourceSubdir)}
            />
            <div className="flex-1">
              <div className="text-sm font-medium">{m.name}</div>
              <div className="text-xs text-muted-foreground">{m.sourceSubdir}</div>
            </div>
            <Input
              data-testid={`member-rename-${m.catalogId}`}
              placeholder="rename (optional)"
              className="w-40"
              value={renames[m.sourceSubdir] ?? ""}
              onChange={(e) => setRename(m.sourceSubdir, e.target.value)}
            />
          </div>
        ))}
      </div>

      {invalidRename && (
        <div className="text-xs text-destructive">
          A rename must be a single path segment (no slashes) — e.g. "my-skill", not "skills/my-skill".
        </div>
      )}
      {allExcluded && (
        <div className="text-xs text-destructive">At least one member must remain attached.</div>
      )}

      <Button
        data-testid="bundle-attach-submit"
        size="sm"
        disabled={disabled}
        onClick={handleAttach}
      >
        {attaching ? "Attaching…" : "Attach to profile"}
      </Button>
    </div>
  );
}
