/**
 * Hermetic vitest for the forbidden-scan logic.
 * Each test case constructs a minimal dist/ directory in-memory and asserts pass/fail.
 */
import { describe, it, expect, beforeEach, afterEach } from "vitest";
import fs from "node:fs";
import path from "node:path";
import os from "node:os";
import { scan } from "./forbidden-scan";

let tmpDir: string;

beforeEach(() => {
  tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "spawnery-scan-"));
});

afterEach(() => {
  fs.rmSync(tmpDir, { recursive: true, force: true });
});

function write(name: string, content: string) {
  const p = path.join(tmpDir, name);
  fs.mkdirSync(path.dirname(p), { recursive: true });
  fs.writeFileSync(p, content);
}

// ── Clean dist ───────────────────────────────────────────────────────────────

describe("clean dist", () => {
  it("passes a dist with only allowed content", async () => {
    write("index.html", `<!doctype html><html><head>
  <meta charset="utf-8">
  <link rel="stylesheet" href="/assets/index.abc123.css" integrity="sha384-abc" crossorigin="anonymous">
  <title>Spawnery</title>
</head><body><div id="root"></div><script src="/assets/main.def456.js" integrity="sha384-def" crossorigin="anonymous"></script></body></html>`);
    write("assets/main.def456.js", `console.log("hello");`);
    write("assets/index.abc123.css", `.app { color: red; }`);
    write("_headers", `Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; font-src 'self'; connect-src https://cp.spawnery.dev wss://cp.spawnery.dev; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; object-src 'none'`);

    const result = await scan(tmpDir);
    expect(result.ok).toBe(true);
    expect(result.violations).toHaveLength(0);
  });
});

// ── Localhost origin ─────────────────────────────────────────────────────────

describe("localhost origin", () => {
  it("fails when localhost URL appears in a JS file", async () => {
    write("assets/main.js", `fetch("http://localhost:8080/api")`);
    const result = await scan(tmpDir);
    expect(result.ok).toBe(false);
    expect(result.violations.some(v => v.includes("localhost/loopback"))).toBe(true);
  });

  it("fails for 127.0.0.1 origin", async () => {
    write("assets/main.js", `const url = "ws://127.0.0.1:8080/ws/session";`);
    const result = await scan(tmpDir);
    expect(result.ok).toBe(false);
    expect(result.violations.some(v => v.includes("localhost/loopback"))).toBe(true);
  });

  it("fails for [::1] origin", async () => {
    write("assets/main.js", `const url = "http://[::1]:3000/api";`);
    const result = await scan(tmpDir);
    expect(result.ok).toBe(false);
  });
});

// ── unsafe-eval ──────────────────────────────────────────────────────────────

describe("unsafe-eval", () => {
  it("fails when unsafe-eval appears in _headers", async () => {
    write("_headers", `Content-Security-Policy: script-src 'self' 'unsafe-eval'`);
    const result = await scan(tmpDir);
    expect(result.ok).toBe(false);
    expect(result.violations.some(v => v.includes("unsafe-eval"))).toBe(true);
  });
});

// ── unsafe-inline ────────────────────────────────────────────────────────────

describe("unsafe-inline", () => {
  it("passes for the single documented unsafe-inline (style-src for xterm/sonner)", async () => {
    write("_headers", `Content-Security-Policy: default-src 'none'; style-src 'self' 'unsafe-inline'; script-src 'self'`);
    const result = await scan(tmpDir);
    // This occurrence is documented — should NOT appear in violations.
    const uiViolations = result.violations.filter(v => v.includes("undocumented"));
    expect(uiViolations).toHaveLength(0);
  });

  it("fails for unsafe-inline in script-src", async () => {
    write("_headers", `Content-Security-Policy: script-src 'self' 'unsafe-inline'`);
    const result = await scan(tmpDir);
    expect(result.ok).toBe(false);
    expect(result.violations.some(v => v.includes("undocumented") && v.includes("unsafe-inline"))).toBe(true);
  });

  it("fails for unsafe-inline in script-src even when style-src unsafe-inline is adjacent (regression: context-window bypass)", async () => {
    // The old context-window approach was fooled when script-src and style-src were within
    // ~80 chars of each other: the documented style-src string fell inside the window for the
    // script-src occurrence and the scanner silently passed a script-src 'unsafe-inline'.
    write("_headers", `Content-Security-Policy: script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'`);
    const result = await scan(tmpDir);
    expect(result.ok).toBe(false);
    expect(result.violations.some(v => v.includes("undocumented") && v.includes("unsafe-inline"))).toBe(true);
  });

  it("fails for unsafe-inline in a JS bundle", async () => {
    write("assets/main.js", `// CSP: script-src 'unsafe-inline' is not allowed`);
    const result = await scan(tmpDir);
    expect(result.ok).toBe(false);
    expect(result.violations.some(v => v.includes("unsafe-inline"))).toBe(true);
  });
});

// ── dev-token ────────────────────────────────────────────────────────────────

describe("dev-token", () => {
  it("fails when dev-token appears in a dist file", async () => {
    write("assets/main.js", `const token = "dev-token";`);
    const result = await scan(tmpDir);
    expect(result.ok).toBe(false);
    expect(result.violations.some(v => v.includes("dev-token"))).toBe(true);
  });
});

// ── PLACEHOLDER trust anchors ─────────────────────────────────────────────────

describe("placeholder trust anchors", () => {
  it("fails when PLACEHOLDER-TRUST-ANCHOR-ROOT-CA is in the bundle", async () => {
    write("assets/main.js", `const CA = "PLACEHOLDER-TRUST-ANCHOR-ROOT-CA";`);
    write("_headers", `/*\n  Content-Security-Policy: default-src 'none'; connect-src https://cp.example.com`);
    const result = await scan(tmpDir);
    expect(result.ok).toBe(false);
    expect(result.violations.some(v => v.includes("PLACEHOLDER"))).toBe(true);
  });

  it("fails when PLACEHOLDER-TRUST-ANCHOR-AS-PUBKEY is in the bundle", async () => {
    write("assets/main.js", `const AS_PUBKEYS = ["PLACEHOLDER-TRUST-ANCHOR-AS-PUBKEY"];`);
    write("_headers", `/*\n  Content-Security-Policy: default-src 'none'; connect-src https://cp.example.com`);
    const result = await scan(tmpDir);
    expect(result.ok).toBe(false);
    expect(result.violations.some(v => v.includes("PLACEHOLDER"))).toBe(true);
  });
});

// ── _headers presence + connect-src check ────────────────────────────────────

describe("_headers structural check", () => {
  it("fails when _headers is absent", async () => {
    // No _headers written — must be flagged as a violation.
    write("index.html", `<!doctype html><html><body></body></html>`);
    const result = await scan(tmpDir);
    expect(result.ok).toBe(false);
    expect(result.violations.some(v => v.includes("_headers: file is absent"))).toBe(true);
  });

  it("fails when _headers has no connect-src directive", async () => {
    write("_headers", `/*\n  Content-Security-Policy: default-src 'none'; script-src 'self'`);
    const result = await scan(tmpDir);
    expect(result.ok).toBe(false);
    expect(result.violations.some(v => v.includes("missing connect-src directive"))).toBe(true);
  });

  it("fails when connect-src is 'none' (origins were empty at build time)", async () => {
    write("_headers", `/*\n  Content-Security-Policy: default-src 'none'; connect-src 'none'`);
    const result = await scan(tmpDir);
    expect(result.ok).toBe(false);
    expect(result.violations.some(v => v.includes("connect-src is"))).toBe(true);
  });

  it("passes when _headers has a real pinned connect-src", async () => {
    write("_headers", `/*\n  Content-Security-Policy: default-src 'none'; connect-src https://cp.spawnery.dev wss://cp.spawnery.dev\n  X-Frame-Options: DENY`);
    const result = await scan(tmpDir);
    const headersViolations = result.violations.filter(v => v.startsWith("_headers:"));
    expect(headersViolations).toHaveLength(0);
  });
});
