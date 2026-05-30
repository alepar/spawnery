export function ToolCallChip({ title, status }: { title: string; status?: string }) {
  return <div className="chip">🔧 {title}{status ? ` — ${status}` : ""}</div>;
}
