import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Nav } from "@/nav/nav";

vi.mock("@/api/catalog", () => ({
  listApps: vi.fn().mockResolvedValue([
    { id: "spawnery/wiki", displayName: "Wiki", latestTier: "TRUST_TIER_REVIEWED" },
  ]),
  listMyApps: vi.fn().mockResolvedValue([]),
  getApp: vi.fn().mockResolvedValue({
    app: { id: "spawnery/wiki", displayName: "Wiki", latestTier: "TRUST_TIER_REVIEWED" },
    versions: [],
    manifest: { id: "spawnery/wiki", title: "Wiki" },
  }),
  tierLabel: (_t?: string) => ({ label: "reviewed", variant: "default" as const }),
}));

vi.mock("@/api/spawnlet", () => ({
  listAgentImages: vi.fn().mockResolvedValue([]),
}));

import { TemplatesView } from "./TemplatesView";

describe("TemplatesView (nav-driven)", () => {
  it("renders the Browse tab for the templates section", async () => {
    render(<TemplatesView nav={{ section: "templates" }} navigate={vi.fn()} />);
    expect(screen.getByTestId("templates-tab-browse").getAttribute("data-variant")).toBe("secondary");
    await waitFor(() => screen.getByTestId("market-search"));
  });

  it("renders Detail for the app section with nav.appId", async () => {
    const nav: Nav = { section: "app", appId: "spawnery/wiki" };
    render(<TemplatesView nav={nav} navigate={vi.fn()} />);
    await waitFor(() => screen.getByTestId("detail-back"));
  });

  it("opening an app card navigates to the app section", async () => {
    const navigate = vi.fn();
    render(<TemplatesView nav={{ section: "templates" }} navigate={navigate} />);
    await waitFor(() => screen.getByTestId("app-card-spawnery/wiki"));
    await userEvent.click(screen.getByTestId("app-card-spawnery/wiki"));
    expect(navigate).toHaveBeenCalledWith({ section: "app", appId: "spawnery/wiki" });
  });

  it("clicking a tab navigates to the right section", async () => {
    const navigate = vi.fn();
    render(<TemplatesView nav={{ section: "templates" }} navigate={navigate} />);
    await userEvent.click(screen.getByTestId("templates-tab-mine"));
    expect(navigate).toHaveBeenCalledWith({ section: "my-apps" });
    await userEvent.click(screen.getByTestId("templates-tab-publish"));
    expect(navigate).toHaveBeenCalledWith({ section: "publish" });
  });

  it("highlights the My Apps tab for the my-apps section", () => {
    render(<TemplatesView nav={{ section: "my-apps" }} navigate={vi.fn()} />);
    expect(screen.getByTestId("templates-tab-mine").getAttribute("data-variant")).toBe("secondary");
    expect(screen.getByTestId("templates-tab-browse").getAttribute("data-variant")).toBe("ghost");
  });
});
