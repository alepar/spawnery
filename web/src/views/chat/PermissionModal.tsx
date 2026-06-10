import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import type { PermOption } from "@/acp/frames";

// isReject groups options into reject-vs-allow buckets by ACP kind (allow_once|allow_always|
// reject_once|reject_always|...), so reject buttons render de-emphasized (outline) and on the left.
function isReject(o: PermOption): boolean {
  const k = (o.kind ?? "").toLowerCase();
  return k.includes("reject") || k.includes("deny");
}

// PermissionModal renders one button per agent-supplied option (labeled by name, grouped/styled by
// kind). Clicking resolves with that option's id; dismissing resolves with "" (the node auto-denies).
export function PermissionModal(
  { title, options, onResolve }: { title: string; options: PermOption[]; onResolve: (optionId: string) => void },
) {
  const rejects = options.filter(isReject);
  const allows = options.filter((o) => !isReject(o));
  return (
    <Dialog open onOpenChange={(o) => { if (!o) onResolve(""); }}>
      <DialogContent data-testid="permission-modal">
        <DialogHeader><DialogTitle>Permission request</DialogTitle></DialogHeader>
        <p className="text-sm text-muted-foreground">
          The agent requests permission: <b className="text-foreground">{title}</b>
        </p>
        <DialogFooter>
          {rejects.map((o) => (
            <Button key={o.optionId} variant="outline" data-testid={`perm-option-${o.optionId}`}
              onClick={() => onResolve(o.optionId ?? "")}>
              {o.name ?? o.optionId}
            </Button>
          ))}
          {allows.map((o) => (
            <Button key={o.optionId} data-testid={`perm-option-${o.optionId}`}
              onClick={() => onResolve(o.optionId ?? "")}>
              {o.name ?? o.optionId}
            </Button>
          ))}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
