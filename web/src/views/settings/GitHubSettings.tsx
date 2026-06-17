/**
 * Settings → GitHub panel (spec r2 §6.2).
 *
 * Reads GET /github/links and renders status (linked @login vN / relink_required / Link GitHub) with
 * Relink / Revoke. Link/Relink: POST start (client_kind=web) → store the non-secret {flow_id} marker
 * → top-level navigate to authorize_url. On return: AFTER bootstrap()/silent-refresh completes
 * (status === 'authed'), if the marker is present → POST redeem with credentials:'include' (the
 * HttpOnly completer cookie rides cross-origin under corsCredentialed) → on 409 identity_change show
 * the @old→@new modal then re-redeem confirm_switch=true → clear the marker → refresh. A callback
 * ?error= is surfaced and the marker cleared. The login-wall (bootstrap-failure) recovery is in App.
 */
import { useState, useEffect, useCallback, useRef } from "react";
import { useSessionStore } from "@/auth/session";
import {
  startGithubLink, redeemGithubLink, listGithubLinks, revokeGithubLink,
  type GithubLinkMeta,
} from "@/api/githubLink";
import {
  getFlowMarker, setFlowMarker, clearFlowMarker, parseLinkError, linkErrorMessage,
} from "@/github/flow";

interface GitHubSettingsProps {
  /** Top-level navigation to the authorize URL (injectable for tests). */
  navigateTop?: (url: string) => void;
  /** Current URL search string (injectable for tests). */
  getSearch?: () => string;
  /** Strip the callback ?error= from the address bar (injectable for tests). */
  stripSearch?: () => void;
}

export function GitHubSettings({
  navigateTop = (url) => { window.location.assign(url); },
  getSearch = () => window.location.search,
  stripSearch = () => { window.history.replaceState(null, "", window.location.pathname); },
}: GitHubSettingsProps = {}) {
  const status = useSessionStore((s) => s.status);
  const [links, setLinks] = useState<GithubLinkMeta[] | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [identity, setIdentity] = useState<{ oldLogin: string; newLogin: string; flowId: string } | null>(null);
  const resumedRef = useRef(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setLinks(await listGithubLinks());
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load GitHub link");
      setLinks(null);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void refresh(); }, [refresh]);

  // Surface a callback ?error= once on mount (AS redirectLinkError lands the SPA with ?error=<code>).
  useEffect(() => {
    const code = parseLinkError(getSearch());
    if (code) {
      setError(linkErrorMessage(code));
      clearFlowMarker();
      stripSearch();
    }
    // run once on mount
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const doRedeem = useCallback(async (flowId: string, confirmSwitch: boolean) => {
    setBusy(true);
    setError(null);
    try {
      const res = await redeemGithubLink(flowId, confirmSwitch);
      switch (res.kind) {
        case "linked":
          clearFlowMarker();
          setIdentity(null);
          await refresh();
          break;
        case "identity-change":
          // Keep the marker: the confirm modal re-redeems with confirm_switch=true.
          setIdentity({ oldLogin: res.oldLogin, newLogin: res.newLogin, flowId });
          break;
        case "pending":
          clearFlowMarker();
          setError("GitHub authorization wasn't completed. Try linking again.");
          break;
        case "unknown":
          clearFlowMarker();
          setError("This GitHub link request expired. Try linking again.");
          break;
        case "error":
          clearFlowMarker();
          setError(res.message);
          break;
      }
    } finally {
      setBusy(false);
    }
  }, [refresh]);

  // Redeem-on-return, gated on bootstrap completion (status === 'authed'). The top-level OAuth
  // navigation wiped the in-memory Bearer; bootstrap()/silent-refresh restores it first (L12).
  useEffect(() => {
    if (status !== "authed") return;
    if (resumedRef.current) return;
    if (parseLinkError(getSearch())) return; // error return handled above
    const marker = getFlowMarker();
    if (!marker) return;
    resumedRef.current = true;
    void doRedeem(marker, false);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [status]);

  const onLink = useCallback(async () => {
    setBusy(true);
    setError(null);
    try {
      const { authorizeUrl, flowId } = await startGithubLink();
      setFlowMarker(flowId);
      navigateTop(authorizeUrl);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to start GitHub link");
      setBusy(false);
    }
  }, [navigateTop]);

  const onRevoke = useCallback(async (secretId: string) => {
    setBusy(true);
    setError(null);
    try {
      await revokeGithubLink(secretId);
      await refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to revoke GitHub link");
    } finally {
      setBusy(false);
    }
  }, [refresh]);

  const link = links?.[0] ?? null;

  return (
    <div className="space-y-4" data-testid="github-settings">
      {loading && <p className="text-sm text-muted-foreground" data-testid="gh-loading">Loading…</p>}
      {error && <p className="text-sm text-destructive" data-testid="gh-error">{error}</p>}

      {!loading && (link === null || link.status === "revoked") && (
        <div className="space-y-2" data-testid={link?.status === "revoked" ? "gh-revoked" : "gh-unlinked"}>
          <p className="text-sm text-muted-foreground">
            {link?.status === "revoked" ? "GitHub link revoked." : "No GitHub account linked."}
          </p>
          <LinkButton onClick={() => void onLink()} disabled={busy} label="Link GitHub" />
        </div>
      )}

      {!loading && link?.status === "relink_required" && (
        <div className="space-y-2" data-testid="gh-relink-required">
          <p className="text-sm">Relink required for <span className="font-medium">@{link.login}</span>.</p>
          <LinkButton onClick={() => void onLink()} disabled={busy} label="Relink GitHub" />
        </div>
      )}

      {!loading && link?.status === "linked" && (
        <div className="space-y-2" data-testid="gh-linked">
          <p className="text-sm">
            Linked as <span className="font-medium" data-testid="gh-login">@{link.login}</span>{" "}
            <span className="text-xs text-muted-foreground">(v{link.version})</span>
          </p>
          <div className="flex gap-2">
            <LinkButton onClick={() => void onLink()} disabled={busy} label="Relink" />
            <button
              type="button"
              data-testid="gh-revoke"
              disabled={busy}
              onClick={() => void onRevoke(link.secretId)}
              className="text-xs text-destructive hover:underline disabled:opacity-50"
            >
              Revoke
            </button>
          </div>
        </div>
      )}

      {identity && (
        <div className="fixed inset-0 bg-black/50 flex items-center justify-center z-50" data-testid="gh-identity-modal">
          <div className="bg-background border border-border rounded-lg p-6 max-w-md w-full mx-4 space-y-4">
            <h3 className="text-base font-semibold">Different GitHub identity</h3>
            <p className="text-sm text-muted-foreground">
              This link is currently <span className="font-medium">@{identity.oldLogin}</span>. You just
              authorized as <span className="font-medium">@{identity.newLogin}</span>. Switch the link to
              the new identity?
            </p>
            <div className="flex gap-2 justify-end">
              <button
                type="button"
                data-testid="gh-identity-cancel"
                onClick={() => { setIdentity(null); clearFlowMarker(); }}
                className="px-4 py-2 text-sm border border-border rounded hover:bg-muted"
              >
                Cancel
              </button>
              <button
                type="button"
                data-testid="gh-identity-confirm"
                disabled={busy}
                onClick={() => void doRedeem(identity.flowId, true)}
                className="px-4 py-2 text-sm bg-primary text-primary-foreground rounded hover:bg-primary/90 disabled:opacity-50"
              >
                Switch to @{identity.newLogin}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

function LinkButton({ onClick, disabled, label }: { onClick: () => void; disabled: boolean; label: string }) {
  const testid = label.toLowerCase().startsWith("relink") ? "gh-relink" : "gh-link";
  return (
    <button
      type="button"
      data-testid={testid}
      onClick={onClick}
      disabled={disabled}
      className="px-3 py-2 text-sm bg-primary text-primary-foreground rounded hover:bg-primary/90 disabled:opacity-50"
    >
      {label}
    </button>
  );
}
