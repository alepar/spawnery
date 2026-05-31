import { useState } from "react";
import { Switch } from "@/components/ui/switch";
import { setTheme } from "@/lib/theme";

export function SettingsView() {
  const [dark, setDark] = useState(() => document.documentElement.classList.contains("dark"));
  return (
    <div className="max-w-md space-y-6 p-6" data-testid="settings">
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
    </div>
  );
}
