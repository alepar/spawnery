import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { Browse } from "./market/Browse";
import { Detail } from "./market/Detail";
import { MyApps } from "./market/MyApps";
import { Publish } from "./market/Publish";
import type { Nav } from "@/nav/nav";

type Tab = "browse" | "detail" | "mine" | "publish";

// Derive the active tab from nav: app -> the detail pane, my-apps/publish -> their tabs, everything
// else (templates) -> browse. The detail tab has no button; it's reached by opening an app card.
function navToTab(section: Nav["section"]): Tab {
  switch (section) {
    case "app":      return "detail";
    case "my-apps":  return "mine";
    case "publish":  return "publish";
    default:         return "browse";
  }
}

export function TemplatesView({ nav, navigate, onSpawn }: {
  nav: Nav;
  navigate: (nav: Nav) => void;
  onSpawn?: (appId: string, image?: string, runnableId?: string, profileId?: string) => void;
}) {
  const tab = navToTab(nav.section);

  const tabBtn = (t: Tab, label: string, target: Nav) => (
    <Button
      key={t}
      variant={tab === t ? "secondary" : "ghost"}
      size="sm"
      data-testid={`templates-tab-${t}`}
      className={cn(tab === t && "font-semibold")}
      onClick={() => navigate(target)}
    >
      {label}
    </Button>
  );

  return (
    <div className="flex flex-col" data-testid="templates">
      <div className="flex items-center gap-1 border-b border-border p-2">
        {tabBtn("browse", "Browse", { section: "templates" })}
        {tabBtn("mine", "My Apps", { section: "my-apps" })}
        {tabBtn("publish", "Publish", { section: "publish" })}
      </div>

      {tab === "browse" && (
        <Browse onOpen={(id) => navigate({ section: "app", appId: id })} />
      )}

      {nav.section === "app" && (
        <Detail id={nav.appId} onBack={() => navigate({ section: "templates" })} onSpawn={onSpawn} />
      )}

      {tab === "mine" && <MyApps />}

      {tab === "publish" && <Publish onPublished={() => navigate({ section: "my-apps" })} />}
    </div>
  );
}
