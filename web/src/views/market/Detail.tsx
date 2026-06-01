import { useEffect, useState } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import {
  getApp,
  tierLabel,
  type AppSummary,
  type AppVersionSummary,
  type AppManifest,
} from "@/api/catalog";

export function Detail({
  id,
  onBack,
  onSpawn,
}: {
  id: string;
  onBack: () => void;
  onSpawn?: (appId: string) => void;
}) {
  const [app, setApp] = useState<AppSummary | null>(null);
  const [versions, setVersions] = useState<AppVersionSummary[]>([]);
  const [manifest, setManifest] = useState<AppManifest | undefined>(undefined);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    getApp(id)
      .then((r) => {
        if (cancelled) return;
        setApp(r.app);
        setVersions(r.versions);
        setManifest(r.manifest);
      })
      .catch((e) => {
        if (cancelled) return;
        setError(e instanceof Error ? e.message : String(e));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [id]);

  const title = app?.displayName ?? manifest?.title ?? id;
  const tl = tierLabel(app?.latestTier);

  return (
    <div className="flex flex-col gap-4 p-6">
      <div className="flex items-center justify-between gap-2">
        <Button variant="outline" size="sm" data-testid="detail-back" onClick={onBack}>
          ← Back
        </Button>
        <Button data-testid="spawn-btn" onClick={() => onSpawn?.(id)}>
          Spawn
        </Button>
      </div>

      {loading && <p className="text-sm text-muted-foreground">Loading…</p>}
      {error && <p className="text-sm text-destructive">{error}</p>}

      {!loading && !error && (
        <>
          <div className="flex flex-col gap-2">
            <div className="flex items-center gap-2">
              <h2 className="text-xl font-semibold">{title}</h2>
              <Badge variant={tl.variant}>{tl.label}</Badge>
            </div>
            <p className="text-sm text-muted-foreground">{id}</p>
            {manifest?.description && (
              <p className="text-sm text-muted-foreground">{manifest.description}</p>
            )}
          </div>

          {manifest && (
            <Card>
              <CardHeader>
                <CardTitle className="text-base">Manifest</CardTitle>
              </CardHeader>
              <CardContent className="flex flex-col gap-1 text-sm text-muted-foreground">
                {manifest.model?.recommendedDefault && (
                  <p>
                    <span className="font-medium text-foreground">Model: </span>
                    {manifest.model.recommendedDefault}
                  </p>
                )}
                {manifest.tools && manifest.tools.length > 0 && (
                  <p>
                    <span className="font-medium text-foreground">Tools: </span>
                    {manifest.tools.join(", ")}
                  </p>
                )}
                {manifest.agents?.support && manifest.agents.support.length > 0 && (
                  <p>
                    <span className="font-medium text-foreground">Agents: </span>
                    {manifest.agents.support.join(", ")}
                  </p>
                )}
                {manifest.persona && (
                  <p>
                    <span className="font-medium text-foreground">Persona: </span>
                    {manifest.persona}
                  </p>
                )}
              </CardContent>
            </Card>
          )}

          <Card>
            <CardHeader>
              <CardTitle className="text-base">Versions</CardTitle>
            </CardHeader>
            <CardContent className="flex flex-col gap-2">
              {versions.length === 0 && (
                <p className="text-sm text-muted-foreground">No versions.</p>
              )}
              {versions.map((v) => {
                const vtl = tierLabel(v.tier);
                return (
                  <div
                    key={v.version}
                    data-testid={`version-${v.version}`}
                    className="flex items-center justify-between gap-2 text-sm"
                  >
                    <span className="font-medium">{v.version}</span>
                    <div className="flex items-center gap-2">
                      <Badge variant={vtl.variant}>{vtl.label}</Badge>
                      {v.createdAt && (
                        <span className="text-muted-foreground">{v.createdAt}</span>
                      )}
                    </div>
                  </div>
                );
              })}
            </CardContent>
          </Card>
        </>
      )}
    </div>
  );
}
