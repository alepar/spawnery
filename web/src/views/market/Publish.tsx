import { useState } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { toast } from "sonner";
import { registerAppVersion, type AppManifest, type ManifestMount } from "@/api/catalog";

interface MountRow {
  name: string;
  path: string;
  seed: string;
}

export function Publish({ onPublished }: { onPublished?: () => void }) {
  const [id, setId] = useState("");
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [tagsCsv, setTagsCsv] = useState("");
  const [version, setVersion] = useState("");
  const [ref, setRef] = useState("");
  const [mounts, setMounts] = useState<MountRow[]>([
    { name: "main", path: "data", seed: "seed" },
  ]);
  const [submitting, setSubmitting] = useState(false);

  function updateMount(i: number, patch: Partial<MountRow>) {
    setMounts((rows) => rows.map((r, idx) => (idx === i ? { ...r, ...patch } : r)));
  }
  function addMount() {
    setMounts((rows) => [...rows, { name: "", path: "", seed: "" }]);
  }
  function removeMount(i: number) {
    setMounts((rows) => rows.filter((_, idx) => idx !== i));
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setSubmitting(true);
    try {
      const manifestMounts: ManifestMount[] = mounts.map((m) => ({
        name: m.name,
        path: m.path,
        seed: m.seed || undefined,
      }));
      const manifest: AppManifest = {
        apiVersion: "spawnery/v1",
        id,
        title,
        description: description || undefined,
        tags: tagsCsv
          .split(",")
          .map((s) => s.trim())
          .filter(Boolean),
        visibility: "open",
        mounts: manifestMounts,
      };
      const res = await registerAppVersion({ manifest, version, ref });
      toast.success(`Published ${res.appId}@${res.version}`);
      onPublished?.();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div className="flex flex-col p-6">
      <form
        data-testid="publish-form"
        className="flex flex-col gap-4"
        onSubmit={(e) => void handleSubmit(e)}
      >
        <Card>
          <CardHeader>
            <CardTitle className="text-base">App details</CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <label className="flex flex-col gap-1 text-sm">
              <span className="font-medium">ID</span>
              <Input
                data-testid="publish-id"
                value={id}
                placeholder="owner/app"
                onChange={(e) => setId(e.target.value)}
              />
            </label>
            <label className="flex flex-col gap-1 text-sm">
              <span className="font-medium">Title</span>
              <Input
                data-testid="publish-title"
                value={title}
                onChange={(e) => setTitle(e.target.value)}
              />
            </label>
            <label className="flex flex-col gap-1 text-sm">
              <span className="font-medium">Description</span>
              <Input
                data-testid="publish-description"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
              />
            </label>
            <label className="flex flex-col gap-1 text-sm">
              <span className="font-medium">Tags (comma-separated)</span>
              <Input
                data-testid="publish-tags"
                value={tagsCsv}
                placeholder="wiki, notes"
                onChange={(e) => setTagsCsv(e.target.value)}
              />
            </label>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-base">Version</CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <label className="flex flex-col gap-1 text-sm">
              <span className="font-medium">Version</span>
              <Input
                data-testid="publish-version"
                value={version}
                placeholder="1.0.0"
                onChange={(e) => setVersion(e.target.value)}
              />
            </label>
            <label className="flex flex-col gap-1 text-sm">
              <span className="font-medium">Ref</span>
              <Input
                data-testid="publish-ref"
                value={ref}
                placeholder="owner/app@sha"
                onChange={(e) => setRef(e.target.value)}
              />
            </label>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="flex items-center justify-between gap-2 text-base">
              <span>Mounts</span>
              <Button
                type="button"
                variant="outline"
                size="sm"
                data-testid="publish-mount-add"
                onClick={addMount}
              >
                Add mount
              </Button>
            </CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            {mounts.map((m, i) => (
              <div key={i} className="flex items-end gap-2">
                <label className="flex flex-1 flex-col gap-1 text-sm">
                  <span className="font-medium">Name</span>
                  <Input
                    data-testid={`publish-mount-name-${i}`}
                    value={m.name}
                    onChange={(e) => updateMount(i, { name: e.target.value })}
                  />
                </label>
                <label className="flex flex-1 flex-col gap-1 text-sm">
                  <span className="font-medium">Path</span>
                  <Input
                    data-testid={`publish-mount-path-${i}`}
                    value={m.path}
                    onChange={(e) => updateMount(i, { path: e.target.value })}
                  />
                </label>
                <label className="flex flex-1 flex-col gap-1 text-sm">
                  <span className="font-medium">Seed</span>
                  <Input
                    data-testid={`publish-mount-seed-${i}`}
                    value={m.seed}
                    onChange={(e) => updateMount(i, { seed: e.target.value })}
                  />
                </label>
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  data-testid={`publish-mount-remove-${i}`}
                  disabled={mounts.length <= 1}
                  onClick={() => removeMount(i)}
                >
                  Remove
                </Button>
              </div>
            ))}
          </CardContent>
        </Card>

        <Button type="submit" data-testid="publish-submit" disabled={submitting}>
          {submitting ? "Publishing…" : "Publish"}
        </Button>
      </form>
    </div>
  );
}
