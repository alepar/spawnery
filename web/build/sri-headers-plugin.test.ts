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
    // data: is required: Vite inlines webfonts as data: URI @font-face in the CSS bundle.
    expect(csp).toContain("font-src 'self' data:");
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
function callConfigResolved(plugin: ReturnType<typeof sriHeadersPlugin>, outDir: string, mode = "development") {
  (plugin.configResolved as unknown as (cfg: { build: { outDir: string }; mode: string }) => void)({ build: { outDir }, mode });
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

  it("CSP rule /* comes BEFORE /assets/* (CSP propagates to all paths)", () => {
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
    expect(assetsIdx).toBeGreaterThan(wcIdx);
  });

  it("no-cache is scoped to / and /index.html, NOT on /*", () => {
    // The /* rule carries only CSP + security headers. Cache-Control: no-cache is on
    // / and /index.html only, so /assets/* never inherits no-cache and no host-specific
    // override semantics are relied upon.
    writeIndexHtml("<html><head></head><body></body></html>");
    const plugin = sriHeadersPlugin({ cpOrigin: "https://cp.example.com" });
    callConfigResolved(plugin, tmpDir);
    callGenerateBundle(plugin, {});
    callCloseBundle(plugin);

    const content = fs.readFileSync(path.join(tmpDir, "_headers"), "utf8");
    const lines = content.split("\n");
    const wcIdx = lines.findIndex((l) => l.trim() === "/*");
    const nextRuleIdx = lines.findIndex((l, i) => i > wcIdx && l.trim().match(/^\//) && l.trim() !== "/*");
    // Everything between /* and the next rule should NOT include Cache-Control.
    const wcBlock = lines.slice(wcIdx + 1, nextRuleIdx).join("\n");
    expect(wcBlock).not.toContain("Cache-Control");

    // / and /index.html rules must exist with no-cache.
    const slashIdx = lines.findIndex((l) => l.trim() === "/");
    const indexHtmlIdx = lines.findIndex((l) => l.trim() === "/index.html");
    expect(slashIdx).toBeGreaterThan(wcIdx);
    expect(indexHtmlIdx).toBeGreaterThan(wcIdx);

    // Verify the actual Cache-Control values.
    expect(lines[slashIdx + 1]?.trim()).toBe("Cache-Control: no-cache");
    expect(lines[indexHtmlIdx + 1]?.trim()).toBe("Cache-Control: no-cache");
  });

  it("does NOT emit _headers in dev mode when no CP origin is configured", () => {
    writeIndexHtml("<html><head></head><body></body></html>");
    const plugin = sriHeadersPlugin(); // no origin, dev mode (default)
    callConfigResolved(plugin, tmpDir); // mode defaults to 'development'
    callGenerateBundle(plugin, {});
    callCloseBundle(plugin);

    expect(fs.existsSync(path.join(tmpDir, "_headers"))).toBe(false);
  });

  it("[WL4] throws in production mode when cpOrigin is empty (fail-closed, not silent)", () => {
    // A release build with no VITE_CP_ORIGIN must fail rather than ship a bundle with no CSP.
    writeIndexHtml("<html><head></head><body></body></html>");
    const plugin = sriHeadersPlugin(); // no origin
    callConfigResolved(plugin, tmpDir, "production");
    callGenerateBundle(plugin, {});
    expect(() => callCloseBundle(plugin)).toThrow(/VITE_CP_ORIGIN is not set for a production build/);
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

  it("[WL4] throws when a CSS asset is emitted but not referenced with integrity in index.html", () => {
    // Simulates a dynamically-imported CSS chunk: Vite emits it as an asset but does NOT
    // add a <link rel="stylesheet"> for it in index.html — Vite injects it at runtime via
    // __vitePreload without an integrity attribute.  The plugin must catch this and fail.
    const css = "body { background: blue; }";
    fs.writeFileSync(path.join(tmpDir, "assets/lazy.css"), css, "utf8");
    // index.html has NO <link> for lazy.css — it would be injected at runtime.
    writeIndexHtml("<html><head></head><body></body></html>");
    const plugin = sriHeadersPlugin({ cpOrigin: "https://cp.example.com" });
    callConfigResolved(plugin, tmpDir);
    callGenerateBundle(plugin, {
      "assets/lazy.css": { type: "asset", source: css },
    });
    expect(() => callCloseBundle(plugin)).toThrow(/FATAL \[WL4\]: CSS assets emitted but not referenced/);
  });

  it("[WL4] does NOT throw when a CSS asset IS referenced with integrity in index.html", () => {
    const css = "body { color: red; }";
    const integ = sha384(css);
    fs.writeFileSync(path.join(tmpDir, "assets/main.css"), css, "utf8");
    writeIndexHtml(
      `<html><head><link rel="stylesheet" crossorigin href="/assets/main.css"></head><body></body></html>`,
    );
    const plugin = sriHeadersPlugin({ cpOrigin: "https://cp.example.com" });
    callConfigResolved(plugin, tmpDir);
    callGenerateBundle(plugin, { "assets/main.css": { type: "asset", source: css } });
    expect(() => callCloseBundle(plugin)).not.toThrow();
    // Also verify the integrity was stamped.
    const html = fs.readFileSync(path.join(tmpDir, "index.html"), "utf8");
    expect(html).toContain(`integrity="${integ}"`);
  });

  it("skips processing when index.html does not exist", () => {
    // No index.html written — closeBundle must exit cleanly.
    const plugin = sriHeadersPlugin({ cpOrigin: "https://cp.example.com" });
    callConfigResolved(plugin, tmpDir);
    callGenerateBundle(plugin, {});
    expect(() => callCloseBundle(plugin)).not.toThrow();
    expect(fs.existsSync(path.join(tmpDir, "_headers"))).toBe(false);
  });

  it("deduplicates crossorigin — no bare crossorigin left after SRI stamping", () => {
    // Vite emits <script type="module" crossorigin src="..."> with a bare crossorigin (no value).
    // The plugin must strip it before re-adding crossorigin="anonymous", so the output never
    // has two crossorigin attributes on the same tag.
    const code = "console.log('main');";
    const integ = sha384(code);
    fs.writeFileSync(path.join(tmpDir, "assets/main.js"), code, "utf8");
    // Simulate Vite's output: bare `crossorigin` without a value, no integrity yet.
    writeIndexHtml(
      `<html><head><script type="module" crossorigin src="/assets/main.js"></script></head><body></body></html>`,
    );
    const plugin = sriHeadersPlugin({ cpOrigin: "https://cp.example.com" });
    callConfigResolved(plugin, tmpDir);
    callGenerateBundle(plugin, { "assets/main.js": { type: "chunk", code } });
    callCloseBundle(plugin);

    const html = fs.readFileSync(path.join(tmpDir, "index.html"), "utf8");
    expect(html).toContain(`integrity="${integ}"`);
    expect(html).toContain('crossorigin="anonymous"');
    // Exactly one crossorigin attribute — the bare one must be stripped.
    const matches = html.match(/\bcrossorigin\b/g) ?? [];
    expect(matches).toHaveLength(1);
  });

  it("deduplicates crossorigin on link tags — bare crossorigin stripped before re-add", () => {
    // Same issue for <link rel="stylesheet" crossorigin href="..."> emitted by Vite.
    const css = "body { color: red; }";
    const integ = sha384(css);
    fs.writeFileSync(path.join(tmpDir, "assets/main.css"), css, "utf8");
    writeIndexHtml(
      `<html><head><link rel="stylesheet" crossorigin href="/assets/main.css"></head><body></body></html>`,
    );
    const plugin = sriHeadersPlugin({ cpOrigin: "https://cp.example.com" });
    callConfigResolved(plugin, tmpDir);
    callGenerateBundle(plugin, { "assets/main.css": { type: "asset", source: css } });
    callCloseBundle(plugin);

    const html = fs.readFileSync(path.join(tmpDir, "index.html"), "utf8");
    expect(html).toContain(`integrity="${integ}"`);
    expect(html).toContain('crossorigin="anonymous"');
    const matches = html.match(/\bcrossorigin\b/g) ?? [];
    expect(matches).toHaveLength(1);
  });
});
