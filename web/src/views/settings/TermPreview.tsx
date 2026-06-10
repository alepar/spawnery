// A non-connected live xterm preview for the terminal settings pane. Mirrors
// TerminalView's xterm lifecycle minus the socket: it renders a fixed sample
// buffer and re-applies appearance settings on every change. See design spec §5.
import { useEffect, useRef } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { useTermSettings, applyToTerminal } from "@/term/settings";
import { fontById } from "@/term/fonts";

// Deterministic sample: a prompt line, a coloured `ls`-style line, a small code
// snippet, then the 16 ANSI swatches (normal 30-37, bright 90-97) and a bold sample.
const SAMPLE = [
  "\x1b[32muser@spawnery\x1b[0m:\x1b[34m~/project\x1b[0m$ ls --color",
  "\x1b[34mdist\x1b[0m  \x1b[34msrc\x1b[0m  \x1b[32mbuild.sh\x1b[0m  README.md  \x1b[36mlink\x1b[0m -> target",
  "\x1b[35mconst\x1b[0m greet = (\x1b[33mname\x1b[0m) => \x1b[32m`Hello, ${name}!`\x1b[0m;",
  "",
  // 16 ANSI swatches: normal then bright.
  [0, 1, 2, 3, 4, 5, 6, 7].map((i) => `\x1b[4${i}m  \x1b[0m`).join("") +
    [0, 1, 2, 3, 4, 5, 6, 7].map((i) => `\x1b[10${i}m  \x1b[0m`).join(""),
  "\x1b[1mBold\x1b[0m \x1b[3mitalic\x1b[0m \x1b[4munderline\x1b[0m \x1b[7minverse\x1b[0m",
].join("\r\n");

/** The primary family token from a font stack (the part document.fonts wants). */
function primaryFamily(stack: string): string {
  const first = stack.split(",")[0].trim();
  return first.replace(/^["']|["']$/g, "");
}

export function TermPreview() {
  const hostRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const { settings, appDark } = useTermSettings();

  // Create the terminal once, write the deterministic sample buffer, then dispose
  // on unmount. Appearance is applied by the settings-keyed effect below.
  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;

    const term = new Terminal({ convertEol: false, cursorBlink: false, scrollback: 0 });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host);
    fit.fit();
    term.write(SAMPLE);

    termRef.current = term;
    fitRef.current = fit;

    const ro = new ResizeObserver(() => fit.fit());
    ro.observe(host);

    return () => {
      ro.disconnect();
      term.dispose();
      termRef.current = null;
      fitRef.current = null;
    };
  }, []);

  // Re-apply theme/font/size on every settings (or app light/dark) change. Font
  // metrics must settle before measuring, so load the font, then fit + refresh.
  useEffect(() => {
    const term = termRef.current;
    const fit = fitRef.current;
    if (!term || !fit) return;

    applyToTerminal(term, settings, appDark);

    const family = primaryFamily(fontById(settings.fontFamily).stack);
    let cancelled = false;
    document.fonts
      .load(`${settings.fontSize}px "${family}"`)
      .catch(() => {})
      .finally(() => {
        if (cancelled || !termRef.current) return;
        fit.fit();
        term.refresh(0, term.rows - 1);
      });

    return () => { cancelled = true; };
  }, [settings, appDark]);

  return (
    <div
      data-testid="term-preview"
      ref={hostRef}
      className="h-48 w-full overflow-hidden rounded-md border border-border bg-black p-1"
    />
  );
}
