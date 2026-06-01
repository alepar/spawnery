import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

vi.mock("@/api/catalog", () => ({
  listApps: vi.fn().mockResolvedValue([
    { id: "spawnery/wiki", displayName: "Wiki", summary: "notes", tags: ["notes"], latestTier: "TRUST_TIER_REVIEWED" },
    { id: "alice/x", displayName: "X", latestTier: "TRUST_TIER_UNVERIFIED" },
  ]),
  tierLabel: (t?: string) => ({ label: t === "TRUST_TIER_REVIEWED" ? "reviewed" : "unverified", variant: "default" as const }),
}));

import { Browse } from "./Browse";

describe("Browse", () => {
  it("renders app cards and opens one", async () => {
    const onOpen = vi.fn();
    render(<Browse onOpen={onOpen} />);
    await waitFor(() => screen.getByTestId("app-card-spawnery/wiki"));
    expect(screen.getByText("Wiki")).toBeInTheDocument();
    expect(screen.getByTestId("app-card-alice/x")).toBeInTheDocument();
    await userEvent.click(screen.getByTestId("app-card-spawnery/wiki"));
    expect(onOpen).toHaveBeenCalledWith("spawnery/wiki");
  });
});
