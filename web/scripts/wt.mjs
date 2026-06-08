// Pure Windows-Terminal-scheme -> xterm ITheme mapper.
//
// SINGLE SOURCE OF TRUTH for the mapping. Authored in plain JS (ESM) so the codegen script
// (`gen-term-themes.mjs`) can import it with a bare `node scripts/gen-term-themes.mjs` — no
// build step, no TS loader. The TS side (`src/term/wt.ts`) re-exports this with proper types,
// and vitest exercises the mapper through that typed surface, so there is exactly one
// implementation and it is unit-tested.

const HEX = /^#[0-9a-fA-F]{6}$/;

// Windows-Terminal field -> ITheme field. The two renames that matter: WT's `purple`/`brightPurple`
// are xterm's `magenta`/`brightMagenta`. Everything else maps by identical name.
const ANSI = {
  black: "black",
  red: "red",
  green: "green",
  yellow: "yellow",
  blue: "blue",
  purple: "magenta",
  cyan: "cyan",
  white: "white",
  brightBlack: "brightBlack",
  brightRed: "brightRed",
  brightGreen: "brightGreen",
  brightYellow: "brightYellow",
  brightBlue: "brightBlue",
  brightPurple: "brightMagenta",
  brightCyan: "brightCyan",
  brightWhite: "brightWhite",
};

/**
 * @param {Record<string, unknown>} wt Parsed Windows-Terminal color scheme.
 * @param {string} field Source field name (for error messages).
 * @returns {string} A validated `#rrggbb` hex string.
 */
function hex(wt, field) {
  const v = wt[field];
  if (typeof v !== "string" || !HEX.test(v)) {
    throw new Error(`wtToITheme: field "${field}" must be a #rrggbb hex, got ${JSON.stringify(v)}`);
  }
  return v;
}

/**
 * Map a Windows-Terminal color scheme to an xterm.js ITheme.
 * Throws if any required field is missing or not a valid `#rrggbb` hex.
 *
 * @param {Record<string, unknown>} wt
 * @returns {Record<string, string>}
 */
export function wtToITheme(wt) {
  if (wt == null || typeof wt !== "object") {
    throw new Error("wtToITheme: expected an object");
  }
  const background = hex(wt, "background");
  const selectionBackground = hex(wt, "selectionBackground");

  /** @type {Record<string, string>} */
  const theme = {
    background,
    foreground: hex(wt, "foreground"),
    cursor: hex(wt, "cursorColor"),
    cursorAccent: background,
    selectionBackground,
    // Omit selectionForeground (let xterm invert); keep inactive selection visible.
    selectionInactiveBackground: selectionBackground,
  };
  for (const [src, dst] of Object.entries(ANSI)) {
    theme[dst] = hex(wt, src);
  }
  return theme;
}
