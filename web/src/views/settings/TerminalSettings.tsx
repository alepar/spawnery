// Terminal appearance pane: theme (follow app light/dark or fixed), font family
// and size, plus a live xterm preview. Writes through useTermSettings().update.
// See the design spec §5.
import { useTermSettings } from "@/term/settings";
import { TERM_THEME_NAMES } from "@/term/themes.gen";
import { TERM_FONTS } from "@/term/fonts";
import { Switch } from "@/components/ui/switch";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import { TermPreview } from "./TermPreview";

const FONT_SIZE_MIN = 8;
const FONT_SIZE_MAX = 24;

const SELECT_CLASS =
  "h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-xs outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 dark:bg-input/30";

function clampFontSize(n: number): number {
  if (Number.isNaN(n)) return FONT_SIZE_MIN;
  return Math.min(FONT_SIZE_MAX, Math.max(FONT_SIZE_MIN, Math.round(n)));
}

function SchemeOptions() {
  return (
    <>
      {TERM_THEME_NAMES.map((name) => (
        <option key={name} value={name}>{name}</option>
      ))}
    </>
  );
}

export function TerminalSettings() {
  const { settings, update } = useTermSettings();

  return (
    <div className="space-y-5" data-testid="terminal-settings">
      <Field
        label="Match app light/dark"
        hint="Pick a light and a dark scheme; the terminal follows the app theme."
        inline
      >
        <Switch
          data-testid="term-follow"
          checked={settings.follow}
          onCheckedChange={(v) => update({ follow: v })}
        />
      </Field>

      {settings.follow ? (
        <>
          <Field label="Light scheme">
            <select
              data-testid="term-light"
              aria-label="Light scheme"
              className={SELECT_CLASS}
              value={settings.light}
              onChange={(e) => update({ light: e.target.value })}
            >
              <SchemeOptions />
            </select>
          </Field>
          <Field label="Dark scheme">
            <select
              data-testid="term-dark"
              aria-label="Dark scheme"
              className={SELECT_CLASS}
              value={settings.dark}
              onChange={(e) => update({ dark: e.target.value })}
            >
              <SchemeOptions />
            </select>
          </Field>
        </>
      ) : (
        <Field label="Theme">
          <select
            data-testid="term-fixed"
            aria-label="Theme"
            className={SELECT_CLASS}
            value={settings.fixed}
            onChange={(e) => update({ fixed: e.target.value })}
          >
            <SchemeOptions />
          </select>
        </Field>
      )}

      <Field label="Font family">
        <select
          data-testid="term-font"
          aria-label="Font family"
          className={SELECT_CLASS}
          value={settings.fontFamily}
          onChange={(e) => update({ fontFamily: e.target.value })}
        >
          {TERM_FONTS.map((f) => (
            <option key={f.id} value={f.id}>{f.label}</option>
          ))}
        </select>
      </Field>

      <Field label="Font size">
        <Input
          data-testid="term-fontsize"
          type="number"
          aria-label="Font size"
          min={FONT_SIZE_MIN}
          max={FONT_SIZE_MAX}
          value={settings.fontSize}
          onChange={(e) => update({ fontSize: clampFontSize(Number(e.target.value)) })}
        />
      </Field>

      <div className="space-y-1.5">
        <div className="text-sm font-medium">Preview</div>
        <TermPreview />
      </div>
    </div>
  );
}

function Field({
  label,
  hint,
  inline,
  children,
}: {
  label: string;
  hint?: string;
  inline?: boolean;
  children: React.ReactNode;
}) {
  return (
    <div className={cn(inline ? "flex items-center justify-between gap-4" : "space-y-1.5")}>
      <div>
        <div className="text-sm font-medium">{label}</div>
        {hint && <div className="text-xs text-muted-foreground">{hint}</div>}
      </div>
      {children}
    </div>
  );
}
