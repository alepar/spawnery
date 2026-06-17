/**
 * GitHub-link flow state that lives in the SPA across the top-level OAuth navigation.
 *
 * The only thing we persist is the non-secret `flow_id` marker (a server-issued, single-use flow
 * handle — NOT a credential): the redemption secret is the HttpOnly `as_gh_completer` cookie that
 * the AS sets on the callback and the browser auto-attaches to /github/link/redeem. JS never reads
 * the cookie.
 */

export const GH_FLOW_MARKER_KEY = "spawnery-gh-link-flow";

export function setFlowMarker(flowId: string): void {
  try { sessionStorage.setItem(GH_FLOW_MARKER_KEY, flowId); } catch { /* private-mode */ }
}
export function getFlowMarker(): string | null {
  try { return sessionStorage.getItem(GH_FLOW_MARKER_KEY); } catch { return null; }
}
export function clearFlowMarker(): void {
  try { sessionStorage.removeItem(GH_FLOW_MARKER_KEY); } catch { /* ignore */ }
}

/** parseLinkError extracts the AS callback ?error=<code> (set by redirectLinkError on user-denial). */
export function parseLinkError(search: string): string | null {
  try { return new URLSearchParams(search).get("error"); } catch { return null; }
}

/**
 * recoverStaleFlowMarker clears a stranded marker when bootstrap settles to a non-authed (failure)
 * state. The GitHub panel only mounts when authed, so on the login wall the marker would otherwise be
 * stranded (spec §6.2 Recovery). 'loading' is still in-flight → no-op. Returns true iff it cleared one.
 */
export function recoverStaleFlowMarker(status: string): boolean {
  if (status === "authed" || status === "loading") return false;
  if (getFlowMarker() === null) return false;
  clearFlowMarker();
  return true;
}

/** linkErrorMessage maps an AS callback error code to a user-facing message. */
export function linkErrorMessage(code: string): string {
  switch (code) {
    case "access_denied":
      return "You declined the GitHub authorization. Try linking again.";
    default:
      return `GitHub authorization failed (${code}). Try linking again.`;
  }
}
