import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleTrigger, CollapsibleContent } from "@/components/ui/collapsible";
import type { BundleSummary, BundleVersion, BundleMember } from "@/api/bundles";

export interface BundleCardProps {
  summary: BundleSummary;
  versions: BundleVersion[];
  members: BundleMember[];
  onAttach: (bundleId: string) => void;
  onCheckUpdates: (bundleId: string) => void;
  checkingUpdates?: boolean;
}

/**
 * One catalog card per bundle (sp-mwco.1.9 D3): provenance (source url/ref/subdir + latest
 * source commit, spec §4.7), an expandable member list, Attach, and Check for updates.
 */
export function BundleCard({ summary, versions, members, onAttach, onCheckUpdates, checkingUpdates }: BundleCardProps) {
  const [open, setOpen] = useState(false);
  const latest = versions.find((v) => v.versionId === summary.latestVersionId) ?? versions[versions.length - 1];

  return (
    <div data-testid={`bundle-card-${summary.bundleId}`} className="flex flex-col gap-2 rounded border border-border p-3">
      <div className="flex items-center justify-between gap-2">
        <div className="text-sm font-medium">{summary.name}</div>
        <div className="flex gap-2">
          <Button
            data-testid={`bundle-card-check-updates-${summary.bundleId}`}
            size="sm"
            variant="outline"
            disabled={checkingUpdates}
            onClick={() => onCheckUpdates(summary.bundleId)}
          >
            {checkingUpdates ? "Checking…" : "Check for updates"}
          </Button>
          <Button
            data-testid={`bundle-card-attach-${summary.bundleId}`}
            size="sm"
            onClick={() => onAttach(summary.bundleId)}
          >
            Attach
          </Button>
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground">
        {summary.sourceUrl && (
          <a href={summary.sourceUrl} target="_blank" rel="noopener noreferrer" className="underline">
            {summary.sourceUrl.replace(/^https?:\/\//, "")}
          </a>
        )}
        {summary.sourceRef && <span>ref {summary.sourceRef}</span>}
        {summary.sourceSubdir && <span>subdir {summary.sourceSubdir}</span>}
        {latest?.sourceCommit && (
          <span title={latest.sourceCommit}>commit {latest.sourceCommit.slice(0, 12)}</span>
        )}
      </div>

      <Collapsible open={open} onOpenChange={setOpen}>
        <CollapsibleTrigger asChild>
          <button type="button" className="w-fit text-left text-xs text-muted-foreground underline">
            {summary.memberCount} members
          </button>
        </CollapsibleTrigger>
        <CollapsibleContent>
          <div className="mt-1 flex flex-col gap-1">
            {members.map((m) => (
              <div key={m.catalogId} className="flex items-center justify-between gap-2 text-xs">
                <span className="font-medium">{m.name}</span>
                <span className="text-muted-foreground">{m.sourceSubdir}</span>
                {m.sha256 && (
                  <span title={m.sha256} className="text-muted-foreground">{m.sha256.slice(0, 8)}</span>
                )}
              </div>
            ))}
          </div>
        </CollapsibleContent>
      </Collapsible>
    </div>
  );
}
