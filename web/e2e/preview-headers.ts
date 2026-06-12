/**
 * Middleware that injects the CSP and cache headers from dist/_headers into vite preview
 * responses. Vite preview does not read _headers natively, so this emulation ensures
 * the Playwright CSP-prod suite exercises the REAL enforced CSP (not an approximation).
 *
 * The _headers file is the single source of truth — this module parses it directly, so
 * the emulation always matches what the static host will send ([WL4]).
 */

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import type { IncomingMessage, ServerResponse } from "node:http";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const HEADERS_FILE = path.resolve(__dirname, "../dist/_headers");

interface HeaderRule {
  /** Glob pattern (simple: exact, ends with /*, or /*). */
  pattern: string;
  headers: Record<string, string>;
}

/**
 * Parses the Netlify/Cloudflare-style _headers file into a list of rules.
 * Format:
 *   /path
 *     Header-Name: value
 *   /other-path
 *     Header-Name: value
 * Lines starting with # are comments. Blank lines are separators.
 */
export function parseHeaders(content: string): HeaderRule[] {
  const rules: HeaderRule[] = [];
  let current: HeaderRule | null = null;

  for (const rawLine of content.split("\n")) {
    const line = rawLine.trim();
    if (!line || line.startsWith("#")) continue;

    if (!line.startsWith(" ") && !line.startsWith("\t")) {
      // New path rule.
      current = { pattern: line, headers: {} };
      rules.push(current);
    } else if (current) {
      // Header line.
      const idx = line.indexOf(":");
      if (idx > 0) {
        const name = line.slice(0, idx).trim();
        const value = line.slice(idx + 1).trim();
        current.headers[name] = value;
      }
    }
  }

  return rules;
}

/**
 * Returns true if the given URL path matches the _headers pattern.
 * Supports: exact match, /assets/* (prefix wildcard), /* (catch-all).
 */
function matchesPattern(urlPath: string, pattern: string): boolean {
  if (pattern.endsWith("*")) {
    const prefix = pattern.slice(0, -1);
    return urlPath.startsWith(prefix);
  }
  return urlPath === pattern;
}

/**
 * Loads and parses the dist/_headers file.
 * Returns an empty array if the file does not exist (no prod build yet).
 */
export function loadHeaderRules(): HeaderRule[] {
  try {
    const content = fs.readFileSync(HEADERS_FILE, "utf8");
    return parseHeaders(content);
  } catch {
    return [];
  }
}

/**
 * Returns the headers to apply for a given URL path from the rules array.
 * Later rules override earlier ones (same as Netlify/Cloudflare semantics).
 */
export function headersForPath(urlPath: string, rules: HeaderRule[]): Record<string, string> {
  const result: Record<string, string> = {};
  for (const rule of rules) {
    if (matchesPattern(urlPath, rule.pattern)) {
      Object.assign(result, rule.headers);
    }
  }
  return result;
}

/**
 * Creates a Vite preview plugin middleware that injects the _headers CSP + cache
 * directives parsed from dist/_headers.
 *
 * Usage in playwright.csp.config.ts:
 *   import { createHeadersMiddleware } from "./e2e/preview-headers";
 *   // In the vite preview server config, add this as a middleware.
 */
export function createHeadersMiddleware() {
  const rules = loadHeaderRules();
  if (rules.length === 0) {
    console.warn("[preview-headers] No dist/_headers found — headers not enforced");
  } else {
    console.log(`[preview-headers] Loaded ${rules.length} header rules from dist/_headers`);
  }

  return function headersMiddleware(
    req: IncomingMessage,
    res: ServerResponse,
    next: () => void,
  ) {
    const urlPath = req.url?.split("?")[0] ?? "/";
    const headers = headersForPath(urlPath, rules);
    for (const [name, value] of Object.entries(headers)) {
      res.setHeader(name, value);
    }
    next();
  };
}
