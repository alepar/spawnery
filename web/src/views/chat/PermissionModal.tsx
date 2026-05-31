import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";

export function PermissionModal({ title, onResolve }: { title: string; onResolve: (allow: boolean) => void }) {
  return (
    <Dialog open onOpenChange={(o) => { if (!o) onResolve(false); }}>
      <DialogContent data-testid="permission-modal">
        <DialogHeader><DialogTitle>Permission request</DialogTitle></DialogHeader>
        <p className="text-sm text-muted-foreground">
          The agent requests permission: <b className="text-foreground">{title}</b>
        </p>
        <DialogFooter>
          <Button variant="outline" onClick={() => onResolve(false)}>Deny</Button>
          <Button onClick={() => onResolve(true)}>Allow</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
