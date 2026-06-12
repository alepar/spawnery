import { useState } from "react";
import { Switch } from "@/components/ui/switch";
import { setTheme } from "@/lib/theme";
import { cn } from "@/lib/utils";
import { TerminalSettings } from "./settings/TerminalSettings";
import { DeviceManagement } from "./settings/DeviceManagement";

type Tab = "general" | "terminal" | "devices";

export function SettingsView() {
  const [dark, setDark] = useState(() => document.documentElement.classList.contains("dark"));
  const [tab, setTab] = useState<Tab>("general");

  return (
    <div className="max-w-md space-y-6 p-6" data-testid="settings">
      <div className="flex gap-1 border-b border-border" role="tablist">
        <TabButton
          id="settings-tab-general"
          active={tab === "general"}
          onClick={() => setTab("general")}
        >
          General
        </TabButton>
        <TabButton
          id="settings-tab-terminal"
          active={tab === "terminal"}
          onClick={() => setTab("terminal")}
        >
          Terminal
        </TabButton>
        <TabButton
          id="settings-tab-devices"
          active={tab === "devices"}
          onClick={() => setTab("devices")}
        >
          Devices
        </TabButton>
      </div>

      {tab === "general" && (
        <div className="flex items-center justify-between">
          <div>
            <div className="text-sm font-medium">Dark mode</div>
            <div className="text-xs text-muted-foreground">Persisted across reloads.</div>
          </div>
          <Switch
            data-testid="theme-toggle"
            checked={dark}
            onCheckedChange={(v) => { setDark(v); setTheme(v ? "dark" : "light"); }}
          />
        </div>
      )}

      {tab === "terminal" && <TerminalSettings />}

      {tab === "devices" && <DeviceManagement />}
    </div>
  );
}

function TabButton({
  id,
  active,
  onClick,
  children,
}: {
  id: string;
  active: boolean;
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <button
      type="button"
      role="tab"
      data-testid={id}
      aria-selected={active}
      onClick={onClick}
      className={cn(
        "-mb-px border-b-2 px-3 py-2 text-sm font-medium transition-colors",
        active
          ? "border-primary text-foreground"
          : "border-transparent text-muted-foreground hover:text-foreground",
      )}
    >
      {children}
    </button>
  );
}
