// Build-time endpoint configuration (web-epic W1, sp-2ckv.6).
//
// In the canonical (release) build the CP/AS origins are baked-in constants — the CSP's
// connect-src is pinned to exactly these, so the bundle physically cannot talk anywhere
// else. In dev (`just web`) they are unset: HTTP goes same-origin through the vite proxy
// and WS derives from location.origin, preserving today's behavior unchanged.

export const CP_ORIGIN: string = import.meta.env.VITE_CP_ORIGIN ?? "";
export const AS_ORIGIN: string = import.meta.env.VITE_AS_ORIGIN ?? "";

/** CP HTTP URL for a path; relative (vite-proxied) when no CP origin is configured. */
export function cpHttpUrl(path: string): string {
  return CP_ORIGIN + path;
}

/** AS HTTP URL for a path; relative when no AS origin is configured (dev proxy). */
export function asHttpUrl(path: string): string {
  return AS_ORIGIN + path;
}

/**
 * CP WebSocket URL for a path. Always dials the CONFIGURED CP origin (never bare
 * location.host — that breaks the moment the SPA leaves the CP origin) and derives the
 * ws/wss scheme from the http/https scheme rather than hardcoding ws://.
 */
export function cpWsUrl(path: string): string {
  const base = CP_ORIGIN || window.location.origin;
  return base.replace(/^http/, "ws") + path;
}
