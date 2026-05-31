import { Badge } from "@/components/ui/badge";

export function ToolCallChip({ title, status }: { title: string; status?: string }) {
  return (
    <div className="mx-auto max-w-[70ch] px-4 py-1" data-role="tool">
      <Badge variant="secondary" className="font-mono text-xs">
        🔧 {title}{status ? ` — ${status}` : ""}
      </Badge>
    </div>
  );
}
