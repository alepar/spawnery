import { describe, it, expect, vi } from "vitest";
import { DevTokenAuth, DEV_TOKEN_OVERRIDE_KEY } from "./devtoken";
import type { Identity } from "../fixtures/identity-pool";

const identity: Identity = { token: "tok-abc", owner: "acc-owner-1" };

describe("DevTokenAuth", () => {
  it("prepareCli returns the explicit test-only token", async () => {
    expect(await new DevTokenAuth().prepareCli({}, identity, {
      spawnctlBin: "spawnctl", asOrigin: "http://as", configHome: "/tmp/auth",
    })).toEqual({ authArgs: ["-token", "tok-abc"] });
  });

  it("returns the identity's token for both dev audiences", async () => {
    expect(await new DevTokenAuth().cpAccessToken(identity)).toBe("tok-abc");
    expect(await new DevTokenAuth().nodeAccessToken(identity)).toBe("tok-abc");
  });

  it("seedWeb calls page.addInitScript with the override key and the identity's token", async () => {
    const addInitScript = vi.fn().mockResolvedValue(undefined);
    const fakePage = { addInitScript };

    await new DevTokenAuth().seedWeb(fakePage, identity);

    expect(addInitScript).toHaveBeenCalledTimes(1);
    const [, arg] = addInitScript.mock.calls[0];
    expect(arg).toEqual([DEV_TOKEN_OVERRIDE_KEY, "tok-abc"]);
  });
});
