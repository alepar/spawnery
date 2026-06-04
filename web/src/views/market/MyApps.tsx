import { useEffect, useState } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Switch } from "@/components/ui/switch";
import { listMyApps, setAppListing, tierLabel, type AppSummary } from "@/api/catalog";

export function MyApps() {
  const [apps, setApps] = useState<AppSummary[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function load() {
    setLoading(true);
    setError(null);
    try {
      setApps(await listMyApps());
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    void load();
  }, []);

  async function toggle(app: AppSummary) {
    try {
      await setAppListing(app.id, !app.listed);
      await load();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }

  return (
    <div className="flex flex-col">
      {loading && <p className="p-6 text-sm text-muted-foreground">Loading…</p>}
      {error && <p className="p-6 text-sm text-destructive">{error}</p>}
      {!loading && !error && apps.length === 0 && (
        <p className="p-6 text-sm text-muted-foreground">
          You haven't published any apps yet.
        </p>
      )}

      {!loading && !error && apps.length > 0 && (
        <div className="grid grid-cols-1 gap-4 p-6 md:grid-cols-2">
          {apps.map((a) => {
            const tl = tierLabel(a.latestTier);
            return (
              <Card key={a.id} data-testid={`myapp-${a.id}`}>
                <CardHeader>
                  <CardTitle className="flex items-center justify-between gap-2 text-base">
                    <span className="truncate">{a.displayName ?? a.id}</span>
                    <Badge variant={tl.variant}>{tl.label}</Badge>
                  </CardTitle>
                </CardHeader>
                <CardContent className="flex items-center justify-between gap-2">
                  <span className="text-sm text-muted-foreground">
                    {a.listed ? "Listed" : "Unlisted"}
                  </span>
                  <Switch
                    data-testid={`listing-toggle-${a.id}`}
                    checked={!!a.listed}
                    aria-label={a.listed ? "Take down" : "List"}
                    onCheckedChange={() => void toggle(a)}
                  />
                </CardContent>
              </Card>
            );
          })}
        </div>
      )}
    </div>
  );
}
