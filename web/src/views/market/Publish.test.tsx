import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const registerAppVersion = vi.fn().mockResolvedValue({ appId: "alice/app", version: "1.0.0", tier: "TRUST_TIER_UNVERIFIED" });
vi.mock("@/api/catalog", () => ({
  registerAppVersion: (...a: unknown[]) => registerAppVersion(...a),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { Publish } from "./Publish";

describe("Publish", () => {
  it("submits an assembled manifest", async () => {
    render(<Publish onPublished={() => {}} />);
    await userEvent.type(screen.getByTestId("publish-id"), "alice/app");
    await userEvent.type(screen.getByTestId("publish-title"), "My App");
    await userEvent.type(screen.getByTestId("publish-version"), "1.0.0");
    await userEvent.type(screen.getByTestId("publish-ref"), "alice/app@sha");
    await userEvent.click(screen.getByTestId("publish-submit"));
    expect(registerAppVersion).toHaveBeenCalledTimes(1);
    const arg = registerAppVersion.mock.calls[0][0] as any;
    expect(arg.version).toBe("1.0.0");
    expect(arg.ref).toBe("alice/app@sha");
    expect(arg.manifest.id).toBe("alice/app");
    expect(arg.manifest.title).toBe("My App");
    expect(arg.manifest.apiVersion).toBe("spawnery/v1");
    expect(arg.manifest.visibility).toBe("open");
    expect(arg.manifest.mounts?.length).toBeGreaterThanOrEqual(1);
  });
});
