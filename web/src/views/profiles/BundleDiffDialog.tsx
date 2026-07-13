import { useEffect, useState } from "react";
import { toast } from "sonner";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription, DialogFooter, DialogClose,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { getBundleDiff, type GetBundleDiffResult, type MemberDiff } from "@/api/bundles";
import { lineDiff } from "@/lib/lineDiff";
import { connectErrorMessage } from "@/api/profiles";

export interface BundleDiffDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  bundleId: string;
  fromVersionId: string;
  toVersionId: string;
  onRepin: (toVersionId: string, diffToken: string) => Promise<void>;
}

const CHANGE_LABEL: Record<string, string> = {
  ADDED: "Added",
  REMOVED: "Removed",
  CHANGED: "Changed",
  CHANGE_UNSPECIFIED: "Unspecified",
};

/**
 * The diff view + gated Re-pin button (sp-mwco.1.9 D5, spec §4.9). Re-pin is impossible without
 * viewing the diff: it stays disabled until GetBundleDiff has loaded, and stays disabled when the
 * response's diffToken is empty — that's the server telling us the gate is unsatisfiable (a
 * missing/expired token; see sp-mwco.1.13 for the server-side fix to the underlying dead-end).
 * The client re-pins ONLY with the token GetBundleDiff returned — never a token it minted itself.
 */
export function BundleDiffDialog({
  open, onOpenChange, bundleId, fromVersionId, toVersionId, onRepin,
}: BundleDiffDialogProps) {
  const [loading, setLoading] = useState(false);
  const [diff, setDiff] = useState<GetBundleDiffResult | null>(null);
  const [repinning, setRepinning] = useState(false);

  useEffect(() => {
    if (!open) return;
    setDiff(null);
    setLoading(true);
    getBundleDiff(bundleId, fromVersionId, toVersionId)
      .then(setDiff)
      .catch((e: unknown) => toast.error("Failed to load diff: " + connectErrorMessage(e)))
      .finally(() => setLoading(false));
  }, [open, bundleId, fromVersionId, toVersionId]);

  const handleRepin = async () => {
    if (!diff || !diff.diffToken) return;
    setRepinning(true);
    try {
      await onRepin(toVersionId, diff.diffToken);
    } catch (e: unknown) {
      toast.error(connectErrorMessage(e));
    } finally {
      setRepinning(false);
    }
  };

  const repinDisabled = loading || !diff || diff.diffToken === "" || repinning;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent data-testid="bundle-diff-dialog" className="max-h-[80vh] overflow-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Review changes before re-pin</DialogTitle>
          <DialogDescription>
            Re-pinning is gated on viewing this diff — it is the supply-chain check for the update.
          </DialogDescription>
        </DialogHeader>

        {loading && <div className="text-sm text-muted-foreground">Loading diff…</div>}

        {diff && (
          <div className="flex flex-col gap-3">
            <div data-testid="bundle-diff-commits" className="text-xs text-muted-foreground">
              {diff.fromCommit.slice(0, 12)} → {diff.toCommit.slice(0, 12)}
            </div>

            {diff.diffToken === "" && (
              <div className="rounded bg-destructive/10 px-2 py-1 text-xs text-destructive">
                No live diff token for this version pair — the update window may have expired, or
                the bundle moved on before this diff was viewed. Re-pin is unavailable; close this
                and click "Check for updates again" to retry.
              </div>
            )}

            {diff.members.length === 0 && (
              <div className="text-xs text-muted-foreground">No member changes.</div>
            )}

            {diff.members.map((m) => <MemberDiffPanel key={m.sourceSubdir} member={m} />)}
          </div>
        )}

        <DialogFooter>
          <DialogClose asChild>
            <Button variant="outline" type="button">Cancel</Button>
          </DialogClose>
          <Button data-testid="bundle-diff-repin" disabled={repinDisabled} onClick={handleRepin}>
            {repinning ? "Re-pinning…" : "Re-pin"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function MemberDiffPanel({ member }: { member: MemberDiff }) {
  const hasBody = (member.oldSkillMd ?? "") !== "" || (member.newSkillMd ?? "") !== "";
  const showBody = hasBody && !member.bodyUnavailable;
  const parts = showBody ? lineDiff(member.oldSkillMd ?? "", member.newSkillMd ?? "") : [];

  return (
    <div data-testid={`bundle-diff-member-${member.sourceSubdir}`} className="rounded border border-border p-2">
      <div className="flex items-center gap-2">
        <span className="rounded bg-muted px-1.5 py-0.5 text-xs font-medium">
          {CHANGE_LABEL[member.change] ?? member.change}
        </span>
        <span className="text-sm font-medium">{member.name}</span>
        <span className="text-xs text-muted-foreground">{member.sourceSubdir}</span>
      </div>

      {member.bodyUnavailable && (
        <div className="mt-1 text-xs text-muted-foreground">Body unavailable (source object gone).</div>
      )}
      {(member.oldTruncated || member.newTruncated) && (
        <div className="mt-1 text-xs text-muted-foreground">
          {member.oldTruncated && member.newTruncated
            ? "Both versions truncated"
            : member.oldTruncated ? "Old version truncated" : "New version truncated"} at the size cap.
        </div>
      )}

      {showBody && (
        <pre className="mt-1 overflow-x-auto whitespace-pre-wrap rounded bg-muted/50 p-1 font-mono text-xs">
          {parts.map((p, i) => (
            <div
              key={i}
              className={
                p.type === "add" ? "bg-green-100 text-green-900 dark:bg-green-900/40 dark:text-green-300"
                : p.type === "del" ? "bg-red-100 text-red-900 dark:bg-red-900/40 dark:text-red-300"
                : p.type === "elided" ? "italic text-muted-foreground"
                : ""
              }
            >
              {p.type === "add" ? "+ " : p.type === "del" ? "- " : "  "}{p.text}
            </div>
          ))}
        </pre>
      )}
    </div>
  );
}
