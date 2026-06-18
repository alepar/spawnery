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
import { listAgentImages, type AgentImageView, type CreateMountBinding } from "@/api/spawnlet";
import { ProfileSelect } from "@/views/profiles/ProfileSelect";

export function Detail({
  id,
  onBack,
  onSpawn,
}: {
  id: string;
  onBack: () => void;
  onSpawn?: (appId: string, image?: string, runnableId?: string, profileId?: string, mounts?: CreateMountBinding[]) => void;
}) {
  const [app, setApp] = useState<AppSummary | null>(null);
  const [versions, setVersions] = useState<AppVersionSummary[]>([]);
  const [manifest, setManifest] = useState<AppManifest | undefined>(undefined);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [images, setImages] = useState<AgentImageView[]>([]);
  const [imageIdx, setImageIdx] = useState(0);
  const [runnableId, setRunnableId] = useState("");
  const [profileId, setProfileId] = useState("");
  const [repoInputs, setRepoInputs] = useState<Record<string, { ownerRepo: string; create: boolean }>>({});

  useEffect(() => {
    listAgentImages().then((imgs) => {
      const withRunnables = imgs.filter((i) => i.runnables.length > 0);
      setImages(withRunnables);
      if (withRunnables.length > 0) setRunnableId(withRunnables[0].runnables[0].id);
    }).catch(() => {});
  }, []);

  const selImage = images[imageIdx];

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

  // GitHub mount slots derived from manifest.
  const githubSlots = (manifest?.mounts ?? []).filter((m) => m.github);
  const slotValue = (n: string) => repoInputs[n] ?? { ownerRepo: "", create: false };
  const ownerRepoOk = (s: string) => /^[^/\s]+\/[^/\s]+$/.test(s.trim());
  const githubReady = githubSlots.every((s) => ownerRepoOk(slotValue(s.name).ownerRepo));

  const buildMounts = (): CreateMountBinding[] =>
    githubSlots.map((s) => {
      const v = slotValue(s.name);
      return { name: s.name, backendUri: `github:${v.ownerRepo.trim()}`, createIfMissing: v.create };
    });

  // Detail is the sole writer of document.title for the app section: it sets the bare id until the
  // fetch resolves, then the real human title. App's title effect deliberately skips the "app"
  // section (so its 3s spawn poll can't clobber this); on leaving the section App retakes the title.
  useEffect(() => {
    document.title = `Spawnery — ${title}`;
  }, [title]);

  return (
    <div className="flex flex-col gap-4 p-6">
      <div className="flex items-center justify-between gap-2">
        <Button variant="outline" size="sm" data-testid="detail-back" onClick={onBack}>
          ← Back
        </Button>
        <div className="flex items-center gap-2">
          {images.length > 0 && (
            <div className="flex gap-2" data-testid="agent-selector">
              <select
                data-testid="image-select"
                aria-label="Agent image"
                value={imageIdx}
                onChange={(e) => {
                  const idx = Number(e.target.value);
                  setImageIdx(idx);
                  setRunnableId(images[idx].runnables[0]?.id ?? "");
                }}
              >
                {images.map((i, idx) => (
                  <option key={i.image} value={idx}>{i.image}</option>
                ))}
              </select>
              <select
                data-testid="runnable-select"
                aria-label="Runnable"
                value={runnableId}
                onChange={(e) => setRunnableId(e.target.value)}
              >
                {selImage?.runnables.map((r) => (
                  <option key={r.id} value={r.id}>{r.label}</option>
                ))}
              </select>
            </div>
          )}
          <ProfileSelect value={profileId} onChange={setProfileId} />
          <Button
            data-testid="spawn-btn"
            disabled={githubSlots.length > 0 && !githubReady}
            onClick={() => onSpawn?.(id, selImage?.image ?? "", runnableId, profileId, buildMounts())}
          >
            Spawn
          </Button>
        </div>
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

          {githubSlots.length > 0 && (
            <Card>
              <CardHeader>
                <CardTitle className="text-base">GitHub repository</CardTitle>
              </CardHeader>
              <CardContent className="flex flex-col gap-3">
                {githubSlots.map((s) => {
                  const v = slotValue(s.name);
                  const invalid = v.ownerRepo !== "" && !ownerRepoOk(v.ownerRepo);
                  return (
                    <div key={s.name} className="flex flex-col gap-1">
                      <label className="text-sm font-medium">{s.name}</label>
                      <input
                        data-testid={`github-mount-${s.name}`}
                        className="border border-border rounded px-2 py-1 text-sm"
                        placeholder="owner/repo"
                        value={v.ownerRepo}
                        onChange={(e) =>
                          setRepoInputs((prev) => ({
                            ...prev,
                            [s.name]: { ...slotValue(s.name), ownerRepo: e.target.value },
                          }))
                        }
                      />
                      {invalid && (
                        <p className="text-xs text-destructive">Enter as owner/repo</p>
                      )}
                      <label className="flex items-center gap-2 text-sm">
                        <input
                          type="checkbox"
                          data-testid={`github-create-${s.name}`}
                          checked={v.create}
                          onChange={(e) =>
                            setRepoInputs((prev) => ({
                              ...prev,
                              [s.name]: { ...slotValue(s.name), create: e.target.checked },
                            }))
                          }
                        />
                        Create if it doesn't exist
                      </label>
                    </div>
                  );
                })}
              </CardContent>
            </Card>
          )}

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
