import { useEffect, useState } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { listApps, tierLabel, type AppSummary } from "@/api/catalog";

export function Browse({ onOpen }: { onOpen: (id: string) => void }) {
  const [apps, setApps] = useState<AppSummary[]>([]);
  const [query, setQuery] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function search(q: string) {
    setLoading(true);
    setError(null);
    try {
      setApps(await listApps(q));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    void search("");
  }, []);

  return (
    <div className="flex flex-col">
      <form
        className="flex items-center gap-2 border-b border-border p-4"
        onSubmit={(e) => {
          e.preventDefault();
          void search(query);
        }}
      >
        <Input
          data-testid="market-search"
          value={query}
          placeholder="Search apps…"
          onChange={(e) => setQuery(e.target.value)}
        />
        <Button type="submit" data-testid="market-search-btn">Search</Button>
      </form>

      {loading && <p className="p-6 text-sm text-muted-foreground">Loading…</p>}
      {error && <p className="p-6 text-sm text-destructive">{error}</p>}
      {!loading && !error && apps.length === 0 && (
        <p className="p-6 text-sm text-muted-foreground">No apps found.</p>
      )}

      {!loading && !error && apps.length > 0 && (
        <div className="grid grid-cols-2 gap-4 p-6 md:grid-cols-3">
          {apps.map((a) => {
            const tl = tierLabel(a.latestTier);
            return (
              <Card
                key={a.id}
                role="button"
                tabIndex={0}
                data-testid={`app-card-${a.id}`}
                className="cursor-pointer transition-colors hover:bg-accent"
                onClick={() => onOpen(a.id)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" || e.key === " ") {
                    e.preventDefault();
                    onOpen(a.id);
                  }
                }}
              >
                <CardHeader>
                  <CardTitle className="flex items-center justify-between gap-2 text-base">
                    <span className="truncate">{a.displayName ?? a.id}</span>
                    <Badge variant={tl.variant}>{tl.label}</Badge>
                  </CardTitle>
                </CardHeader>
                <CardContent className="flex flex-col gap-2">
                  {a.summary && (
                    <p className="text-sm text-muted-foreground">{a.summary}</p>
                  )}
                  {a.tags && a.tags.length > 0 && (
                    <div className="flex flex-wrap gap-1">
                      {a.tags.map((t) => (
                        <Badge key={t} variant="secondary">{t}</Badge>
                      ))}
                    </div>
                  )}
                </CardContent>
              </Card>
            );
          })}
        </div>
      )}
    </div>
  );
}
