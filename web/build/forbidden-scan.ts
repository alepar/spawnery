/**
 * Pre-sign forbidden-value scanner (W1, sp-2ckv.6).
 *
 * Scans a dist/ directory and fails (exits 1) if it finds any of:
 *  - Localhost/loopback origins in non-dev bundle files (localhost, 127.0.0.1, [::1]).
 *  - `unsafe-eval` CSP tokens — never permitted.
 *  - `unsafe-inline` outside the ONE documented exception (style-src for xterm/sonner).
 *    The documented exception string is the exact value the plugin emits; any OTHER
 *    occurrence fails the scan.
 *  - The literal `dev-token` string outside of a comment or test file.
 *  - The PLACEHOLDER trust anchor markers (must be replaced before release).
 *
 * Usage:
 *   npx tsx web/build/forbidden-scan.ts [dist-dir]
 *
 * Exit 0 = clean; exit 1 = violations found.
 */

import fs from "node:fs";
import path from "node:path";

export interface ScanResult {
  ok: boolean;
  violations: string[];
}

// The ONE documented 'unsafe-inline' value the plugin is allowed to emit (style-src only).
// Any OTHER occurrence is a violation.
const DOCUMENTED_UNSAFE_INLINE = "style-src 'self' 'unsafe-inline'";

async function* walk(dir: string): AsyncGenerator<string> {
  const entries = fs.readdirSync(dir, { withFileTypes: true });
  for (const e of entries) {
    const full = path.join(dir, e.name);
    if (e.isDirectory()) {
      yield* walk(full);
    } else {
      yield full;
    }
  }
}

function isTextFile(filePath: string): boolean {
  const base = path.basename(filePath);
  // Extensionless files that are always text.
  if (base === "_headers" || base === "_redirects") return true;
  const ext = path.extname(filePath).toLowerCase();
  return [".html", ".js", ".mjs", ".css", ".json", ".txt", ".sh", ".ts", ".tsx"].includes(ext);
}

export async function scan(distDir: string): Promise<ScanResult> {
  const violations: string[] = [];

  for await (const filePath of walk(distDir)) {
    if (!isTextFile(filePath)) continue;

    let content: string;
    try {
      content = fs.readFileSync(filePath, "utf8");
    } catch {
      continue;
    }

    const rel = path.relative(distDir, filePath);

    // 1. Localhost/loopback origins — never in a production bundle.
    // Match URL patterns like http://localhost, ws://127.0.0.1, etc.
    // Allow the string "localhost" only in comments (// ... localhost or /* ... localhost).
    const localhostRe = /(?:https?|wss?):\/\/(?:localhost|127\.0\.0\.1|\[::1\])/g;
    let m: RegExpExecArray | null;
    while ((m = localhostRe.exec(content)) !== null) {
      violations.push(`${rel}: localhost/loopback origin at offset ${m.index}: ${m[0]}`);
    }

    // 2. unsafe-eval — never permitted.
    if (content.includes("unsafe-eval")) {
      violations.push(`${rel}: contains forbidden 'unsafe-eval'`);
    }

    // 3. unsafe-inline — only the ONE documented exception is allowed.
    //    Scan for any unsafe-inline that is NOT part of the documented string.
    const uiRe = /unsafe-inline/g;
    while ((uiRe.lastIndex, (m = uiRe.exec(content))) !== null) {
      // Extract surrounding context (the full directive it's part of).
      const start = Math.max(0, m.index - 50);
      const end = Math.min(content.length, m.index + 100);
      const ctx = content.slice(start, end);
      if (!ctx.includes(DOCUMENTED_UNSAFE_INLINE)) {
        violations.push(`${rel}: undocumented 'unsafe-inline' at offset ${m.index}: ...${ctx.trim()}...`);
      }
    }

    // 4. dev-token literal — must not appear in the signed bundle.
    //    The string 'dev-token' is allowed only in source comments and test files.
    //    In dist/ it is a release blocker.
    if (content.includes("dev-token")) {
      // Only flag it if the file is in dist/ (not a source or test file).
      // The scanner is always called on dist/, so every file here is post-build.
      violations.push(`${rel}: contains forbidden 'dev-token' literal`);
    }

    // 5. PLACEHOLDER trust anchor markers.
    if (
      content.includes("PLACEHOLDER-TRUST-ANCHOR-ROOT-CA") ||
      content.includes("PLACEHOLDER-TRUST-ANCHOR-AS-PUBKEY")
    ) {
      violations.push(`${rel}: contains PLACEHOLDER trust anchor — replace before release`);
    }
  }

  // 6. _headers must exist and contain a real connect-src. A missing _headers means no
  //    CSP and no connect-src pinning will be applied — the core goal of W1 is defeated.
  //    connect-src 'none' is equally bad: it means origins were empty at build time.
  const headersPath = path.join(distDir, "_headers");
  if (!fs.existsSync(headersPath)) {
    violations.push(
      `_headers: file is absent — production bundle must include _headers with a pinned connect-src`,
    );
  } else {
    const headersContent = fs.readFileSync(headersPath, "utf8");
    const connectMatch = headersContent.match(/connect-src\s+([^;\n]+)/);
    if (!connectMatch) {
      violations.push(
        `_headers: CSP is missing connect-src directive — CP/AS origins must be pinned`,
      );
    } else {
      const val = connectMatch[1].trim().replace(/;.*$/, "").trim();
      if (!val || val === "'none'") {
        violations.push(
          `_headers: connect-src is '${val || "(empty)"}' — CP/AS origins must be pinned`,
        );
      }
    }
  }

  return { ok: violations.length === 0, violations };
}

// ── CLI entry point ────────────────────────────────────────────────────────

if (import.meta.url === `file://${process.argv[1]}`) {
  const distDir = process.argv[2] ?? path.join(process.cwd(), "dist");
  scan(distDir).then(({ ok, violations }) => {
    if (ok) {
      console.log("forbidden-scan: PASS — no violations found");
      process.exit(0);
    } else {
      console.error("forbidden-scan: FAIL — violations found:");
      for (const v of violations) console.error("  " + v);
      process.exit(1);
    }
  });
}
