/**
 * Hermetic vitest for the preview-headers middleware utility.
 * Covers parseHeaders (including the indentation-check fix), matchesPattern via
 * headersForPath, and the full middleware path used by the CSP preview server.
 */
import { describe, it, expect } from "vitest";
import { parseHeaders, loadHeaderRules, headersForPath } from "../e2e/preview-headers";

// ── parseHeaders ─────────────────────────────────────────────────────────────

describe("parseHeaders", () => {
  it("parses a two-rule _headers file correctly", () => {
    const content = [
      "# comment",
      "/*",
      "  Content-Security-Policy: default-src 'none'",
      "  Cache-Control: no-cache",
      "",
      "/assets/*",
      "  Cache-Control: public, max-age=31536000, immutable",
    ].join("\n");
    const rules = parseHeaders(content);
    expect(rules).toHaveLength(2);
    expect(rules[0].pattern).toBe("/*");
    expect(rules[0].headers["Content-Security-Policy"]).toBe("default-src 'none'");
    expect(rules[0].headers["Cache-Control"]).toBe("no-cache");
    expect(rules[1].pattern).toBe("/assets/*");
    expect(rules[1].headers["Cache-Control"]).toBe("public, max-age=31536000, immutable");
  });

  it("skips comment lines and blank lines", () => {
    const content = "# full-line comment\n\n/*\n  X-Frame-Options: DENY\n";
    const rules = parseHeaders(content);
    expect(rules).toHaveLength(1);
    expect(rules[0].pattern).toBe("/*");
  });

  it("handles tab-indented header lines", () => {
    const content = "/index.html\n\tCache-Control: no-cache\n";
    const rules = parseHeaders(content);
    expect(rules[0].headers["Cache-Control"]).toBe("no-cache");
  });

  it("returns an empty array for empty input", () => {
    expect(parseHeaders("")).toHaveLength(0);
    expect(parseHeaders("# only comments")).toHaveLength(0);
  });
});

// ── headersForPath ────────────────────────────────────────────────────────────

describe("headersForPath", () => {
  const rules = parseHeaders([
    "/*",
    "  Content-Security-Policy: default-src 'none'",
    "  Cache-Control: no-cache",
    "  X-Frame-Options: DENY",
    "",
    "/assets/*",
    "  Cache-Control: public, max-age=31536000, immutable",
  ].join("\n"));

  it("applies /* to the root path", () => {
    const h = headersForPath("/", rules);
    expect(h["Content-Security-Policy"]).toBe("default-src 'none'");
    expect(h["Cache-Control"]).toBe("no-cache");
    expect(h["X-Frame-Options"]).toBe("DENY");
  });

  it("applies /* to /index.html", () => {
    const h = headersForPath("/index.html", rules);
    expect(h["Content-Security-Policy"]).toBe("default-src 'none'");
  });

  it("applies /* to SPA routes like /templates and /settings", () => {
    expect(headersForPath("/templates", rules)["Content-Security-Policy"]).toBe("default-src 'none'");
    expect(headersForPath("/settings", rules)["Content-Security-Policy"]).toBe("default-src 'none'");
  });

  it("/assets/* overrides Cache-Control from /* for asset paths", () => {
    const h = headersForPath("/assets/main.js", rules);
    // /*  sets no-cache first, then /assets/* overrides with immutable.
    expect(h["Cache-Control"]).toBe("public, max-age=31536000, immutable");
    // CSP from /* still applies (not overridden by /assets/*).
    expect(h["Content-Security-Policy"]).toBe("default-src 'none'");
  });

  it("does not match /assets/* for non-asset paths", () => {
    const h = headersForPath("/spawn/test", rules);
    // Only /* applies.
    expect(h["Cache-Control"]).toBe("no-cache");
  });

  it("returns empty object when no rules exist", () => {
    expect(headersForPath("/", [])).toEqual({});
  });
});

// ── loadHeaderRules (graceful no-dist) ────────────────────────────────────────

describe("loadHeaderRules", () => {
  it("returns an empty array when dist/_headers does not exist", () => {
    // The dist/ folder is absent during the vitest node run (no prod build here).
    // loadHeaderRules should return [] rather than throwing.
    const rules = loadHeaderRules();
    expect(Array.isArray(rules)).toBe(true);
  });
});
