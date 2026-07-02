import { describe, it, expect } from "vitest";
import { redactString, redactHar, type Har } from "./redact";

describe("redactString", () => {
  it("scrubs a Bearer token", () => {
    const out = redactString("Authorization: Bearer eyJhbGciOiJFUzI1NiJ9.abc123");
    expect(out).not.toContain("eyJhbGciOiJFUzI1NiJ9");
    expect(out).toContain("Bearer <redacted>");
  });

  it("scrubs access_token/refresh_token params", () => {
    const out = redactString("https://x/callback?access_token=abcDEF123.xyz&state=1");
    expect(out).not.toContain("abcDEF123");
    expect(out).toContain("access_token=<redacted>");
  });

  it("scrubs cookie-shaped values while keeping the cookie name", () => {
    const out = redactString("spawnery-rth=deadbeefCAFE1234567890");
    expect(out).toContain("spawnery-rth=<redacted>");
    expect(out).not.toContain("deadbeefCAFE1234567890");
  });

  it("leaves non-secret text untouched", () => {
    const s = "spawn acc-r123-abcd-w0-test created, status=active";
    expect(redactString(s)).toBe(s);
  });
});

describe("redactHar", () => {
  const har: Har = {
    log: {
      entries: [
        {
          request: {
            headers: [
              { name: "Authorization", value: "Bearer eyJsecretpayload1234567890abcdefg" },
              { name: "Content-Type", value: "application/json" },
            ],
            cookies: [{ name: "spawnery-rth", value: "secretvalue1234567890abcdef" }],
          },
          response: {
            headers: [{ name: "set-cookie", value: "spawnery-rth=secretvalue1234567890abcdef; HttpOnly" }],
          },
        },
      ],
    },
  };

  it("redacts the Authorization header value", () => {
    const out = redactHar(har);
    const reqHeaders = out.log!.entries![0].request!.headers!;
    const auth = reqHeaders.find((h) => h.name === "Authorization")!;
    expect(auth.value).not.toContain("eyJsecretpayload1234567890abcdefg");
  });

  it("drops set-cookie headers entirely", () => {
    const out = redactHar(har);
    const resHeaders = out.log!.entries![0].response!.headers!;
    expect(resHeaders.some((h) => h.name.toLowerCase() === "set-cookie")).toBe(false);
  });

  it("drops the cookies array", () => {
    const out = redactHar(har);
    expect(out.log!.entries![0].request!.cookies).toBeUndefined();
  });

  it("leaves non-secret headers untouched", () => {
    const out = redactHar(har);
    const reqHeaders = out.log!.entries![0].request!.headers!;
    const ct = reqHeaders.find((h) => h.name === "Content-Type")!;
    expect(ct.value).toBe("application/json");
  });

  it("is a no-op for a HAR with no entries", () => {
    const empty: Har = {};
    expect(redactHar(empty)).toEqual(empty);
  });
});
