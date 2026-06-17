import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { GitHubSettings } from "./GitHubSettings";
import { useSessionStore } from "@/auth/session";
import { setFlowMarker, getFlowMarker } from "@/github/flow";
import type { GithubLinkMeta, RedeemResult } from "@/api/githubLink";

const api = vi.hoisted(() => ({
  startGithubLink: vi.fn(),
  redeemGithubLink: vi.fn(),
  listGithubLinks: vi.fn(),
  revokeGithubLink: vi.fn(),
}));
vi.mock("@/api/githubLink", () => api);

const linked: GithubLinkMeta = { secretId: "gh:a", host: "github.com", login: "octocat", githubUserId: "42", version: 3, updatedAt: 1, status: "linked" };

beforeEach(() => {
  vi.clearAllMocks();
  sessionStorage.clear();
  useSessionStore.setState({ status: "authed" });
  api.listGithubLinks.mockResolvedValue([]);
  api.redeemGithubLink.mockResolvedValue({ kind: "linked", meta: linked } satisfies RedeemResult);
  api.revokeGithubLink.mockResolvedValue(undefined);
});

const noopProps = { navigateTop: vi.fn(), getSearch: () => "", stripSearch: vi.fn() };

describe("status rendering", () => {
  it("renders the unlinked state with a Link button", async () => {
    render(<GitHubSettings {...noopProps} />);
    expect(await screen.findByTestId("gh-unlinked")).toBeInTheDocument();
    expect(screen.getByTestId("gh-link")).toHaveTextContent(/Link GitHub/i);
  });
  it("renders the linked state with @login (vN), Relink + Revoke", async () => {
    api.listGithubLinks.mockResolvedValue([linked]);
    render(<GitHubSettings {...noopProps} />);
    expect(await screen.findByTestId("gh-linked")).toHaveTextContent("@octocat");
    expect(screen.getByTestId("gh-linked")).toHaveTextContent("v3");
    expect(screen.getByTestId("gh-relink")).toBeInTheDocument();
    expect(screen.getByTestId("gh-revoke")).toBeInTheDocument();
  });
  it("renders the relink_required state", async () => {
    api.listGithubLinks.mockResolvedValue([{ ...linked, status: "relink_required" }]);
    render(<GitHubSettings {...noopProps} />);
    expect(await screen.findByTestId("gh-relink-required")).toBeInTheDocument();
  });
  it("renders the revoked state with a Link button", async () => {
    api.listGithubLinks.mockResolvedValue([{ ...linked, status: "revoked" }]);
    render(<GitHubSettings {...noopProps} />);
    expect(await screen.findByTestId("gh-revoked")).toBeInTheDocument();
    expect(screen.getByTestId("gh-link")).toBeInTheDocument();
  });
});

describe("start (Link/Relink)", () => {
  it("stores the marker and navigates top-level to the authorize_url", async () => {
    api.startGithubLink.mockResolvedValue({ authorizeUrl: "https://gh/authorize", flowId: "flow-9" });
    const navigateTop = vi.fn();
    render(<GitHubSettings {...noopProps} navigateTop={navigateTop} />);
    await userEvent.click(await screen.findByTestId("gh-link"));
    await waitFor(() => expect(navigateTop).toHaveBeenCalledWith("https://gh/authorize"));
    expect(getFlowMarker()).toBe("flow-9");
  });
});

describe("redeem-on-return is gated on bootstrap", () => {
  it("does NOT redeem while status !== authed, then redeems once authed + marker present", async () => {
    setFlowMarker("flow-1");
    useSessionStore.setState({ status: "loading" });
    const { rerender } = render(<GitHubSettings {...noopProps} />);
    await screen.findByTestId("github-settings");
    expect(api.redeemGithubLink).not.toHaveBeenCalled();

    useSessionStore.setState({ status: "authed" });
    rerender(<GitHubSettings {...noopProps} />);
    await waitFor(() => expect(api.redeemGithubLink).toHaveBeenCalledWith("flow-1", false));
    await waitFor(() => expect(getFlowMarker()).toBeNull()); // cleared on success
  });
});

describe("409 identity change", () => {
  it("shows the @old→@new modal, then re-redeems with confirm_switch=true on confirm", async () => {
    setFlowMarker("flow-1");
    api.redeemGithubLink
      .mockResolvedValueOnce({ kind: "identity-change", oldLogin: "old", newLogin: "new" })
      .mockResolvedValueOnce({ kind: "linked", meta: linked });
    render(<GitHubSettings {...noopProps} />);
    const modal = await screen.findByTestId("gh-identity-modal");
    expect(modal).toHaveTextContent("@old");
    expect(modal).toHaveTextContent("@new");
    expect(getFlowMarker()).toBe("flow-1"); // marker kept while confirming
    await userEvent.click(screen.getByTestId("gh-identity-confirm"));
    await waitFor(() => expect(api.redeemGithubLink).toHaveBeenLastCalledWith("flow-1", true));
    await waitFor(() => expect(getFlowMarker()).toBeNull());
  });
});

describe("callback ?error= surfacing", () => {
  it("surfaces the error, clears the marker, and strips the URL", async () => {
    setFlowMarker("flow-1");
    const stripSearch = vi.fn();
    render(<GitHubSettings {...noopProps} getSearch={() => "?error=access_denied"} stripSearch={stripSearch} />);
    expect(await screen.findByTestId("gh-error")).toHaveTextContent(/declined/i);
    expect(getFlowMarker()).toBeNull();
    expect(stripSearch).toHaveBeenCalled();
    expect(api.redeemGithubLink).not.toHaveBeenCalled(); // no redeem on an error return
  });
});

describe("revoke", () => {
  it("calls revoke then refreshes", async () => {
    api.listGithubLinks.mockResolvedValueOnce([linked]).mockResolvedValueOnce([]);
    render(<GitHubSettings {...noopProps} />);
    await userEvent.click(await screen.findByTestId("gh-revoke"));
    await waitFor(() => expect(api.revokeGithubLink).toHaveBeenCalledWith("gh:a"));
    await waitFor(() => expect(screen.getByTestId("gh-unlinked")).toBeInTheDocument());
  });
});
