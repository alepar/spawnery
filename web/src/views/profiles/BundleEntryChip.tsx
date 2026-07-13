import { useState } from "react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { reingestBundle } from "@/api/bundles";
import { connectErrorMessage, type ProfileEntry } from "@/api/profiles";
import { BundleDiffDialog } from "./BundleDiffDialog";

export interface BundleEntryChipProps {
  entry: ProfileEntry;
  onRemove: (entryId: string) => void;
  /** Performs the actual RepinProfileBundle call (the caller holds profileId/expectedVersion)
   * and refreshes the profile on success. Rejections are toasted by BundleDiffDialog. */
  onRepin: (entryId: string, versionId: string, diffToken: string) => Promise<void>;
}

/**
 * Profile-entry chip for a BUNDLE_REF entry (sp-mwco.1.9 D4): member count, pinned version, an
 * "Update available" badge (latestSeq > pinnedSeq, computed client-side), read-only
 * excluded/renamed overrides, Remove, and Check for updates. No target toggles (D6) — a
 * BUNDLE_REF entry's targets edit would be remove+re-add, which silently re-pins to LATEST and
 * defeats the diff gate; overrides are likewise set at attach and shown read-only here.
 */
export function BundleEntryChip({ entry, onRemove, onRepin }: BundleEntryChipProps) {
  const [checking, setChecking] = useState(false);
  const [diffOpen, setDiffOpen] = useState(false);
  const [toVersionId, setToVersionId] = useState<string | null>(null);

  const pinnedSeq = entry.pinnedSeq ?? 0;
  const latestSeq = entry.latestSeq ?? 0;
  const updateAvailable = latestSeq > pinnedSeq;
  const excluded = entry.excludedSubdirs ?? [];
  const renames = entry.memberRenames ?? {};
  const hasOverrides = excluded.length > 0 || Object.keys(renames).length > 0;

  const handleCheckUpdates = async () => {
    if (!entry.bundleId) return;
    setChecking(true);
    try {
      const res = await reingestBundle(entry.bundleId);
      const upToDate = res.notModified || (!res.changed && latestSeq <= pinnedSeq);
      if (upToDate) {
        toast.success("Bundle is up to date");
        return;
      }
      setToVersionId(res.versionId);
      setDiffOpen(true);
    } catch (e: unknown) {
      toast.error("Check for updates failed: " + connectErrorMessage(e));
    } finally {
      setChecking(false);
    }
  };

  return (
    <div
      data-testid={`bundle-chip-${entry.entryId}`}
      className="flex flex-col gap-1 rounded border border-border p-3"
    >
      <div className="flex items-center justify-between gap-2">
        <div className="flex flex-wrap items-center gap-2">
          <span className="rounded bg-muted px-1.5 py-0.5 text-xs font-medium">Bundle</span>
          <span className="text-sm font-medium">{entry.name}</span>
          <span className="text-xs text-muted-foreground">{entry.memberCount ?? 0} members</span>
          <span className="text-xs text-muted-foreground">v{pinnedSeq}</span>
          {updateAvailable && (
            <span className="rounded bg-amber-100 px-1.5 py-0.5 text-xs font-medium text-amber-800 dark:bg-amber-900/40 dark:text-amber-300">
              Update available
            </span>
          )}
        </div>
        <div className="flex gap-2">
          <Button
            data-testid={`bundle-chip-check-updates-${entry.entryId}`}
            size="sm"
            variant="outline"
            disabled={checking}
            onClick={handleCheckUpdates}
          >
            {checking ? "Checking…" : "Check for updates"}
          </Button>
          <Button
            data-testid={`bundle-chip-remove-${entry.entryId}`}
            size="sm"
            variant="ghost"
            onClick={() => onRemove(entry.entryId)}
          >
            Remove
          </Button>
        </div>
      </div>

      {hasOverrides && (
        <div className="text-xs text-muted-foreground">
          {excluded.length > 0 && <div>Excluded: {excluded.join(", ")}</div>}
          {Object.keys(renames).length > 0 && (
            <div>Renamed: {Object.entries(renames).map(([from, to]) => `${from} → ${to}`).join(", ")}</div>
          )}
        </div>
      )}

      <div className="text-xs italic text-muted-foreground">per-agent targets: all</div>

      {entry.bundleId && toVersionId && (
        <BundleDiffDialog
          open={diffOpen}
          onOpenChange={setDiffOpen}
          bundleId={entry.bundleId}
          fromVersionId={entry.versionId ?? ""}
          toVersionId={toVersionId}
          onRepin={async (versionId, diffToken) => {
            await onRepin(entry.entryId, versionId, diffToken);
            setDiffOpen(false);
          }}
        />
      )}
    </div>
  );
}
