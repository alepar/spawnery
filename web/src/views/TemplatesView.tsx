import { useState } from "react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { Browse } from "./market/Browse";
import { Detail } from "./market/Detail";
import { MyApps } from "./market/MyApps";
import { Publish } from "./market/Publish";

type Tab = "browse" | "detail" | "mine" | "publish";

export function TemplatesView({ onSpawn }: { onSpawn?: (appId: string, image?: string, runnableId?: string) => void } = {}) {
  const [tab, setTab] = useState<Tab>("browse");
  const [selectedId, setSelectedId] = useState<string | null>(null);

  const tabBtn = (t: Tab, label: string) => (
    <Button
      key={t}
      variant={tab === t ? "secondary" : "ghost"}
      size="sm"
      data-testid={`templates-tab-${t}`}
      className={cn(tab === t && "font-semibold")}
      onClick={() => setTab(t)}
    >
      {label}
    </Button>
  );

  return (
    <div className="flex flex-col" data-testid="templates">
      <div className="flex items-center gap-1 border-b border-border p-2">
        {tabBtn("browse", "Browse")}
        {tabBtn("mine", "My Apps")}
        {tabBtn("publish", "Publish")}
      </div>

      {tab === "browse" && (
        <Browse
          onOpen={(id) => {
            setSelectedId(id);
            setTab("detail");
          }}
        />
      )}

      {tab === "detail" && selectedId && (
        <Detail id={selectedId} onBack={() => setTab("browse")} onSpawn={onSpawn} />
      )}

      {tab === "mine" && <MyApps />}

      {tab === "publish" && <Publish onPublished={() => setTab("mine")} />}
    </div>
  );
}
