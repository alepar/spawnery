import { describe, it, expect, vi } from "vitest";
import { DevTokenAuth, DEV_TOKEN_OVERRIDE_KEY } from "./devtoken";
import type { Identity } from "../fixtures/identity-pool";

const identity: Identity = { token: "tok-abc", owner: "acc-owner-1" };

describe("DevTokenAuth", () => {
  it("cliArgs returns -token <token>", () => {
    expect(new DevTokenAuth().cliArgs(identity)).toEqual(["-token", "tok-abc"]);
  });

  it("oracleToken returns the identity's token", () => {
    expect(new DevTokenAuth().oracleToken(identity)).toBe("tok-abc");
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
