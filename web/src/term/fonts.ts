// Self-hosted monospace terminal fonts: side-effect CSS imports + a registry.
//
// This is the ONE place fonts are wired. The @fontsource side-effect imports
// below bundle woff2 locally (served by Vite — no CDN) and ship each font's
// license inside node_modules/@fontsource/<font>/LICENSE. Hack is not on
// @fontsource, so it is vendored under web/public/fonts/hack/ (unmodified
// upstream woff2 + LICENSE) and declared via @font-face in ./fonts.css.
//
// Fonts ship UNMODIFIED (no subsetting) to avoid OFL Reserved-Font-Name rename
// obligations and to keep full glyph coverage — see the design spec §7.

// @fontsource — regular (400) + bold (700) for each family.
import "@fontsource/fira-code/400.css";
import "@fontsource/fira-code/700.css";
import "@fontsource/jetbrains-mono/400.css";
import "@fontsource/jetbrains-mono/700.css";
import "@fontsource/source-code-pro/400.css";
import "@fontsource/source-code-pro/700.css";
import "@fontsource/cascadia-code/400.css";
import "@fontsource/cascadia-code/700.css";
import "@fontsource/ibm-plex-mono/400.css";
import "@fontsource/ibm-plex-mono/700.css";

// Vendored fonts (@font-face for Hack).
import "./fonts.css";

export interface TermFont {
  /** Stable kebab id, persisted in settings. */
  id: string;
  /** Human-readable name shown in the picker. */
  label: string;
  /** CSS font-family value to assign to the terminal. */
  stack: string;
}

const MONO_FALLBACK = "ui-monospace, monospace";

/**
 * Curated monospace fonts for the terminal. The first entry is the System
 * default (no font file). Each subsequent entry's family name matches the
 * @font-face / @fontsource declaration so the browser can resolve it.
 */
export const TERM_FONTS: TermFont[] = [
  {
    id: "system",
    label: "System monospace",
    stack: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
  },
  { id: "fira-code", label: "Fira Code", stack: `"Fira Code", ${MONO_FALLBACK}` },
  { id: "jetbrains-mono", label: "JetBrains Mono", stack: `"JetBrains Mono", ${MONO_FALLBACK}` },
  { id: "source-code-pro", label: "Source Code Pro", stack: `"Source Code Pro", ${MONO_FALLBACK}` },
  { id: "cascadia-code", label: "Cascadia Code", stack: `"Cascadia Code", ${MONO_FALLBACK}` },
  { id: "ibm-plex-mono", label: "IBM Plex Mono", stack: `"IBM Plex Mono", ${MONO_FALLBACK}` },
  { id: "hack", label: "Hack", stack: `"Hack", ${MONO_FALLBACK}` },
];

const SYSTEM_FONT = TERM_FONTS[0];

/** Look up a font by id, falling back to the System default for unknown ids. */
export function fontById(id: string): TermFont {
  return TERM_FONTS.find((f) => f.id === id) ?? SYSTEM_FONT;
}
