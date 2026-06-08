import type { ITheme } from "@xterm/xterm";

/** Windows-Terminal color-scheme shape (the fields this app's curated corpus uses). */
export interface WtScheme {
  name?: string;
  background: string;
  foreground: string;
  cursorColor: string;
  selectionBackground: string;
  black: string;
  red: string;
  green: string;
  yellow: string;
  blue: string;
  purple: string;
  cyan: string;
  white: string;
  brightBlack: string;
  brightRed: string;
  brightGreen: string;
  brightYellow: string;
  brightBlue: string;
  brightPurple: string;
  brightCyan: string;
  brightWhite: string;
}

/**
 * Map a Windows-Terminal color scheme to an xterm.js ITheme.
 * Throws if any required field is missing or not a valid `#rrggbb` hex.
 */
export function wtToITheme(wt: unknown): ITheme;
