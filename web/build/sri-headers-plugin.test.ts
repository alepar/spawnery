/**
 * Hermetic vitest for the sri-headers-plugin build logic.
 * Tests feed a synthetic Rollup bundle and assert SRI stamping + _headers correctness.
 * Covers: sha384, buildCsp, transformIndexHtml, generateBundle, closeBundle/_headers.
 */
import { describe, it, expect, beforeEach, afterEach } from "vitest";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { sha384, buildCsp, sriHeadersPlugin } from "./sri-headers-plugin";

// ── sha384 ───────────────────────────────────────────────────────────────────

describe("sha384", () => {
  it("produces a sha384- prefixed base64 string", () => {
    const h = sha384("hello");
    expect(h).toMatch(/^sha384-[A-Za-z0-9+/]+=*$/);
  });

  it("is deterministic", () => {
    expect(sha384("test")).toBe(sha384("test"));
  });

  it("differs for different content", () => {
    expect(sha384("a")).not.toBe(sha384("b"));
  });

  it("accepts Uint8Array", () => {
    const arr = new TextEncoder().encode("hello");
    expect(sha384(arr)).toBe(sha384("hello"));
  });

  it("matches content regardless of string vs Buffer encoding", () => {
    const content = "console.log('hello');\n";
    expect(sha384(content)).toBe(sha384(Buffer.from(content)));
  });
});

// ── buildCsp ─────────────────────────────────────────────────────────────────

describe("buildCsp", () => {
  it("contains expected directives with pinned origins", () => {
    const csp = buildCsp("https://cp.spawnery.dev", "https://as.spawnery.dev");
    expect(csp).toContain("default-src 'none'");
    expect(csp).toContain("script-src 'self'");
    expect(csp).toContain("font-src 'self'");
    expect(csp).toContain("frame-ancestors 'none'");
    expect(csp).toContain("base-uri 'none'");
    expect(csp).toContain("object-src 'none'");
    expect(csp).toContain("img-src 'self' data:");
    // Both https and wss variants of CP + AS must appear in connect-src.
    expect(csp).toContain("https://cp.spawnery.dev");
    expect(csp).toContain("wss://cp.spawnery.dev");
    expect(csp).toContain("https://as.spawnery.dev");
    expect(csp).toContain("wss://as.spawnery.dev");
  });

  it("contains the deliberate style-src unsafe-inline (xterm/sonner constraint)", () => {
    const csp = buildCsp("https://cp.spawnery.dev", "https://as.spawnery.dev");
    expect(csp).toContain("style-src 'self' 'unsafe-inline'");
  });

  it("does NOT contain unsafe-eval", () => {
    const csp = buildCsp("https://cp.spawnery.dev", "https://as.spawnery.dev");
    expect(csp).not.toContain("unsafe-eval");
  });
});

// ── Plugin helpers ─────────────────────────────────────────────────────────
// Type-erased accessors for the plugin's hooks (Rollup Plugin types are complex;
// we call the hooks directly without the full plugin context since they don't use `this`).

function getTransformHandler(plugin: ReturnType<typeof sriHeadersPlugin>) {
  return (plugin.transformIndexHtml as { order: string; handler: (html: string, ctx: unknown) => string }).handler;
}
function callGenerateBundle(plugin: ReturnType<typeof sriHeadersPlugin>, bundle: Record<string, unknown>) {
  (plugin.generateBundle as unknown as (opts: unknown, bundle: unknown) => void)({}, bundle);
}
function callConfigResolved(plugin: ReturnType<typeof sriHeadersPlugin>, outDir: string) {
  (plugin.configResolved as unknown as (cfg: { build: { outDir: string } }) => void)({ build: { outDir } });
}
function callCloseBundle(plugin: ReturnType<typeof sriHeadersPlugin>) {
  (plugin.closeBundle as unknown as () => void)();
}

// ── transformIndexHtml — inline-script extraction ─────────────────────────

describe("sriHeadersPlugin.transformIndexHtml", () => {
  it("extracts a bare inline script and replaces with an external SRI reference", () => {
    const plugin = sriHeadersPlugin();
    const html = `<html><head></head><body><script>const x = 1;</script></body></html>`;
    const result = getTransformHandler(plugin)(html, {});

    // Inline script is gone.
    expect(result).not.toContain("<script>const x = 1;</script>");
    // Replaced with external reference under /assets/.
    expect(result).toMatch(/src="\/assets\/theme-bootstrap\.[0-9a-f]+\.js"/);
    // SRI attributes are set.
    expect(result).toContain('integrity="sha384-');
    expect(result).toContain('crossorigin="anonymous"');
  });

  it("leaves type=module scripts with src untouched", () => {
    const plugin = sriHeadersPlugin();
    const html = `<html><head><script type="module" src="/assets/main.js"></script></head></html>`;
    const result = getTransformHandler(plugin)(html, {});
    expect(result).toBe(html);
  });

  it("leaves empty inline script blocks untouched", () => {
    const plugin = sriHeadersPlugin();
    const html = `<html><head><script>  </script></head></html>`;
    const result = getTransformHandler(plugin)(html, {});
    // Empty content script: no extraction.
    expect(result).not.toContain("/assets/theme-bootstrap");
  });

  it("produces a stable asset name for identical inline content", () => {
    const code = "window.__theme='dark';";
    const html = `<html><head><script>${code}</script></head></html>`;
    const p1 = sriHeadersPlugin();
    const p2 = sriHeadersPlugin();
    const r1 = getTransformHandler(p1)(html, {});
    const r2 = getTransformHandler(p2)(html, {});
    const m1 = r1.match(/src="([^"]+)"/);
    const m2 = r2.match(/src="([^"]+)"/);
    expect(m1).not.toBeNull();
    expect(m1![1]).toBe(m2![1]);
  });
});

// ── generateBundle — [WL4] coverage invariant ─────────────────────────────

describe("sriHeadersPlugin.generateBundle", () => {
  it("processes a bundle with multiple chunks without throwing", () => {
    const plugin = sriHeadersPlugin({ cpOrigin: "https://cp.example.com" });
    const bundle = {
      "assets/main.js": { type: "chunk", code: "console.log('main');" },
      "assets/vendor.js": { type: "chunk", code: "console.log('vendor');" },
    };
    expect(() => callGenerateBundle(plugin, bundle)).not.toThrow();
  });

  it("processes asset entries without throwing", () => {
    const plugin = sriHeadersPlugin();
    const bundle = {
      "assets/style.css": { type: "asset", source: "body{}" },
      "assets/main.js": { type: "chunk", code: "console.log();" },
    };
    expect(() => callGenerateBundle(plugin, bundle)).not.toThrow();
  });

  it("[WL4] all chunks in the bundle receive integrity coverage (invariant is always maintained)", () => {
    // The generateBundle loop unconditionally calls sha384(chunk.code) for every chunk
    // entry and stores the result in integrityMap before running the check. This means
    // all chunks are always covered — the FATAL guard at the end of generateBundle is a
    // defensive assertion that currently cannot fire via normal Rollup output.
    // This test documents that behaviour: three chunks → no throw.
    const plugin = sriHeadersPlugin({ cpOrigin: "https://cp.example.com" });
    const bundle = {
      "assets/main.js":    { type: "chunk", code: "console.log('main');" },
      "assets/vendor.js":  { type: "chunk", code: "console.log('vendor');" },
      "assets/dynamic.js": { type: "chunk", code: "export const x = 1;" },
    };
    expect(() => callGenerateBundle(plugin, bundle)).not.toThrow();
  });

  it("[WL4] sha384() of a chunk with non-string code throws immediately (build fails fast)", () => {
    // If a Rollup chunk has no code (e.g. a bug in a custom plugin), sha384() throws
    // a Node TypeError before the FATAL check even runs — the build still fails, just
    // with a crypto-level error rather than the custom FATAL message. This is acceptable
    // because the build still breaks rather than silently producing an uncovered chunk.
    const plugin = sriHeadersPlugin();
    const bundle = {
      "assets/broken.js": { type: "chunk", code: undefined as unknown as string },
    };
    expect(() => callGenerateBundle(plugin, bundle)).toThrow();
  });
});

// ── closeBundle — _headers emission and SRI stamping ─────────────────────

describe("sriHeadersPlugin.closeBundle", () => {
  let tmpDir: string;

  beforeEach(() => {
    tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "sri-test-"));
    fs.mkdirSync(path.join(tmpDir, "assets"), { recursive: true });
  });

  afterEach(() => {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  });

  function writeIndexHtml(content: string) {
    fs.writeFileSync(path.join(tmpDir, "index.html"), content, "utf8");
  }

  it("emits _headers with CSP under /* rule when CP origin is configured", () => {
    writeIndexHtml("<html><head></head><body></body></html>");
    const plugin = sriHeadersPlugin({ cpOrigin: "https://cp.example.com", asOrigin: "https://as.example.com" });
    callConfigResolved(plugin, tmpDir);
    callGenerateBundle(plugin, {});
    callCloseBundle(plugin);

    expect(fs.existsSync(path.join(tmpDir, "_headers"))).toBe(true);
    const content = fs.readFileSync(path.join(tmpDir, "_headers"), "utf8");

    // CSP must be under the /* rule, not just under /index.html.
    const lines = content.split("\n");
    const wcIdx = lines.findIndex((l) => l.trim() === "/*");
    const cspIdx = lines.findIndex((l) => l.trim().startsWith("Content-Security-Policy:"));
    expect(wcIdx).toBeGreaterThanOrEqual(0);
    expect(cspIdx).toBeGreaterThan(wcIdx); // CSP appears INSIDE the /* block

    // Sanity: CSP must not contain unsafe-eval.
    const cspLine = lines[cspIdx];
    expect(cspLine).not.toContain("unsafe-eval");
    // CSP must contain script-src 'self' (no inline allowed).
    expect(cspLine).toContain("script-src 'self'");
  });

  it("CSP rule /* comes BEFORE /assets/* so asset caching can override Cache-Control", () => {
    writeIndexHtml("<html><head></head><body></body></html>");
    const plugin = sriHeadersPlugin({ cpOrigin: "https://cp.example.com" });
    callConfigResolved(plugin, tmpDir);
    callGenerateBundle(plugin, {});
    callCloseBundle(plugin);

    const content = fs.readFileSync(path.join(tmpDir, "_headers"), "utf8");
    const lines = content.split("\n");
    const wcIdx = lines.findIndex((l) => l.trim() === "/*");
    const assetsIdx = lines.findIndex((l) => l.trim() === "/assets/*");
    expect(wcIdx).toBeGreaterThanOrEqual(0);
    expect(assetsIdx).toBeGreaterThan(wcIdx); // /assets/* after /* so it can override no-cache
  });

  it("does NOT emit _headers when no CP origin is configured", () => {
    writeIndexHtml("<html><head></head><body></body></html>");
    const plugin = sriHeadersPlugin(); // no origin
    callConfigResolved(plugin, tmpDir);
    callGenerateBundle(plugin, {});
    callCloseBundle(plugin);

    expect(fs.existsSync(path.join(tmpDir, "_headers"))).toBe(false);
  });

  it("stamps SRI integrity on script src tags in index.html", () => {
    const code = "console.log('main');";
    const integ = sha384(code);
    fs.writeFileSync(path.join(tmpDir, "assets/main.js"), code, "utf8");
    writeIndexHtml(
      `<html><head><script type="module" src="/assets/main.js"></script></head><body></body></html>`,
    );
    const plugin = sriHeadersPlugin({ cpOrigin: "https://cp.example.com" });
    callConfigResolved(plugin, tmpDir);
    callGenerateBundle(plugin, {
      "assets/main.js": { type: "chunk", code },
    });
    callCloseBundle(plugin);

    const html = fs.readFileSync(path.join(tmpDir, "index.html"), "utf8");
    expect(html).toContain(`integrity="${integ}"`);
    expect(html).toContain('crossorigin="anonymous"');
  });

  it("skips processing when index.html does not exist", () => {
    // No index.html written — closeBundle must exit cleanly.
    const plugin = sriHeadersPlugin({ cpOrigin: "https://cp.example.com" });
    callConfigResolved(plugin, tmpDir);
    callGenerateBundle(plugin, {});
    expect(() => callCloseBundle(plugin)).not.toThrow();
    expect(fs.existsSync(path.join(tmpDir, "_headers"))).toBe(false);
  });
});
