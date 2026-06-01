import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const setAppListing = vi.fn().mockResolvedValue(undefined);
vi.mock("@/api/catalog", () => ({
  listMyApps: vi.fn().mockResolvedValue([
    { id: "alice/one", displayName: "One", latestTier: "TRUST_TIER_REVIEWED", listed: true },
    { id: "alice/two", displayName: "Two", latestTier: "TRUST_TIER_UNVERIFIED", listed: false },
  ]),
  setAppListing: (...a: unknown[]) => setAppListing(...a),
  tierLabel: () => ({ label: "x", variant: "default" as const }),
}));

import { MyApps } from "./MyApps";

describe("MyApps", () => {
  it("lists own apps incl. unlisted and toggles listing", async () => {
    render(<MyApps />);
    await waitFor(() => screen.getByTestId("myapp-alice/one"));
    expect(screen.getByTestId("myapp-alice/two")).toBeInTheDocument();
    // alice/one is listed -> toggling sends listed=false
    await userEvent.click(screen.getByTestId("listing-toggle-alice/one"));
    expect(setAppListing).toHaveBeenCalledWith("alice/one", false);
  });
});
