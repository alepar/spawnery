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

import { Detail } from "./Detail";

describe("Detail", () => {
  it("renders manifest + versions and spawns", async () => {
    const onSpawn = vi.fn();
    render(<Detail id="spawnery/wiki" onBack={() => {}} onSpawn={onSpawn} />);
    await waitFor(() => screen.getByTestId("spawn-btn"));
    expect(screen.getByText("a wiki")).toBeInTheDocument();
    expect(screen.getByText("1.0.0")).toBeInTheDocument();
    await userEvent.click(screen.getByTestId("spawn-btn"));
    expect(onSpawn).toHaveBeenCalledWith("spawnery/wiki");
  });
});
