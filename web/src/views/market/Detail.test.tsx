import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

vi.mock("@/api/catalog", () => ({
  getApp: vi.fn().mockResolvedValue({
    app: { id: "spawnery/wiki", displayName: "Wiki", latestTier: "TRUST_TIER_REVIEWED" },
    versions: [{ version: "1.0.0", tier: "TRUST_TIER_REVIEWED", createdAt: "100" }],
    manifest: { id: "spawnery/wiki", title: "Wiki", description: "a wiki", tools: ["qmd"], model: { recommendedDefault: "deepseek" }, mounts: [{ name: "main", path: "data" }] },
  }),
  tierLabel: (_t?: string) => ({ label: "reviewed", variant: "default" as const }),
}));

vi.mock("@/api/spawnlet", () => ({
  listAgentImages: vi.fn().mockResolvedValue([
    { image: "img:1", runnables: [
      { id: "goose-acp", label: "Goose · rich web", mode: "acp" },
      { id: "goose-tui", label: "Goose · terminal", mode: "tmux" },
    ] },
  ]),
}));

vi.mock("@/api/profiles", () => ({
  listProfiles: vi.fn().mockResolvedValue([]),
}));

import { Detail } from "./Detail";
import { getApp } from "@/api/catalog";

describe("Detail", () => {
  it("renders manifest + versions and spawns", async () => {
    const onSpawn = vi.fn();
    render(<Detail id="spawnery/wiki" onBack={() => {}} onSpawn={onSpawn} />);
    await waitFor(() => screen.getByTestId("spawn-btn"));
    expect(screen.getByText("a wiki")).toBeInTheDocument();
    expect(screen.getByText("1.0.0")).toBeInTheDocument();
    await userEvent.click(screen.getByTestId("spawn-btn"));
    expect(onSpawn).toHaveBeenCalledWith("spawnery/wiki", "img:1", "goose-acp", "", []);
  });

  it("shows the agent selector and spawns with the chosen runnable", async () => {
    const onSpawn = vi.fn();
    render(<Detail id="spawnery/wiki" onBack={() => {}} onSpawn={onSpawn} />);
    await waitFor(() => screen.getByTestId("runnable-select"));
    await userEvent.selectOptions(screen.getByTestId("runnable-select"), "goose-tui");
    await userEvent.click(screen.getByTestId("spawn-btn"));
    expect(onSpawn).toHaveBeenCalledWith("spawnery/wiki", "img:1", "goose-tui", "", []);
  });

  it("sets document.title to the app's human title once loaded", async () => {
    render(<Detail id="spawnery/wiki" onBack={() => {}} onSpawn={vi.fn()} />);
    await waitFor(() => expect(document.title).toBe("Spawnery — Wiki"));
  });
});

describe("Detail — github mount slot", () => {
  it("renders the github owner/repo input and create checkbox for a github slot", async () => {
    vi.mocked(getApp).mockResolvedValueOnce({
      app: { id: "spawnery/github-app", displayName: "GitHub App", latestTier: "TRUST_TIER_REVIEWED" },
      versions: [],
      manifest: {
        id: "spawnery/github-app",
        title: "GitHub App",
        mounts: [{ name: "repo", path: "repo", durability: "node-local", github: true }],
      } as any,
    });
    render(<Detail id="spawnery/github-app" onBack={() => {}} onSpawn={vi.fn()} />);
    await waitFor(() => screen.getByTestId("github-mount-repo"));
    expect(screen.getByTestId("github-mount-repo")).toBeInTheDocument();
    expect(screen.getByTestId("github-create-repo")).toBeInTheDocument();
  });

  it("spawn button is disabled until a valid owner/repo is entered", async () => {
    vi.mocked(getApp).mockResolvedValueOnce({
      app: { id: "spawnery/github-app", displayName: "GitHub App", latestTier: "TRUST_TIER_REVIEWED" },
      versions: [],
      manifest: {
        id: "spawnery/github-app",
        title: "GitHub App",
        mounts: [{ name: "repo", path: "repo", github: true }],
      } as any,
    });
    render(<Detail id="spawnery/github-app" onBack={() => {}} onSpawn={vi.fn()} />);
    await waitFor(() => screen.getByTestId("github-mount-repo"));
    expect(screen.getByTestId("spawn-btn")).toBeDisabled();
    await userEvent.type(screen.getByTestId("github-mount-repo"), "octocat/hello");
    expect(screen.getByTestId("spawn-btn")).not.toBeDisabled();
  });

  it("onSpawn called with github mount binding when spawning", async () => {
    vi.mocked(getApp).mockResolvedValueOnce({
      app: { id: "spawnery/github-app", displayName: "GitHub App", latestTier: "TRUST_TIER_REVIEWED" },
      versions: [],
      manifest: {
        id: "spawnery/github-app",
        title: "GitHub App",
        mounts: [{ name: "repo", path: "repo", github: true }],
      } as any,
    });
    const onSpawn = vi.fn();
    render(<Detail id="spawnery/github-app" onBack={() => {}} onSpawn={onSpawn} />);
    await waitFor(() => screen.getByTestId("github-mount-repo"));
    await userEvent.type(screen.getByTestId("github-mount-repo"), "octocat/hello");
    await userEvent.click(screen.getByTestId("github-create-repo"));
    await userEvent.click(screen.getByTestId("spawn-btn"));
    expect(onSpawn).toHaveBeenCalledWith(
      "spawnery/github-app",
      "img:1",
      "goose-acp",
      "",
      [{ name: "repo", backendUri: "github:octocat/hello", createIfMissing: true }],
    );
  });

  it("non-github app renders no github field and spawn works as before", async () => {
    // Default mock has no github slots (mounts: [{name:"main", path:"data"}])
    const onSpawn = vi.fn();
    render(<Detail id="spawnery/wiki" onBack={() => {}} onSpawn={onSpawn} />);
    await waitFor(() => screen.getByTestId("spawn-btn"));
    expect(screen.queryByTestId("github-mount-main")).not.toBeInTheDocument();
    await userEvent.click(screen.getByTestId("spawn-btn"));
    expect(onSpawn).toHaveBeenCalledWith("spawnery/wiki", "img:1", "goose-acp", "", []);
  });
});
