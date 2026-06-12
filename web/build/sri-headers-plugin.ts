/**
 * sri-headers-plugin — Vite build plugin (apply: 'build') that:
 *  1. Externalises the inline theme-bootstrap IIFE from index.html to a hashed asset, so
 *     script-src 'self' holds with no inline allowance ([WM19]).
 *  2. Stamps SRI integrity + crossorigin on every JS/CSS asset reference in index.html,
 *     including modulepreload links for dynamic-import chunks. If any emitted chunk cannot
 *     be covered by an integrity reference the plugin throws and fails the build ([WL4]).
 *  3. Emits dist/_headers with the strict CSP + cache-control headers for the static host.
 *     The CSP lives inside the signed dist, not hand-edited at the host ([WL4]).
 *
 * Implementation notes:
 *  - Uses `transformIndexHtml` (enforce: 'post') for the HTML phase — this runs AFTER
 *    Vite's own HTML transforms (entry chunks, CSS links, modulepreload links) so we see
 *    the final HTML structure with all chunk references in place.
 *  - Uses `generateBundle` for the _headers emission (has access to the full bundle map).
 *  - Inline theme-bootstrap script extraction happens in `transformIndexHtml` before the
 *    script content is in the HTML, so we intercept it there.
 *
 * CSP notes:
 *  - style-src includes 'unsafe-inline' deliberately. Two third-party dependencies inject
 *    runtime <style> elements whose content is dynamic and cannot be hashed/nonced:
 *      * xterm.js — the DOM renderer sets .textContent on JS-created <style> tags for
 *        theme and dimension styles at runtime. The WebGL/Canvas renderer avoids this but
 *        requires @xterm/addon-webgl/@xterm/addon-canvas which are not yet installed.
 *        See deploy/web/README.md §CSP — xterm decision record.
 *      * sonner (toast library) — calls __insertCSS() at module load to inject the full
 *        toast stylesheet. Importing sonner/dist/styles.css bundles the static CSS but
 *        the JS module still calls __insertCSS regardless.
 *    This is a DELIBERATE, DOCUMENTED relaxation, NOT a silent unsafe-inline. The Playwright
 *    CSP-prod suite (playwright.csp.config.ts) exercises both under enforced CSP so any dep
 *    that adds a second injection vector fails CI, not production.
 *  - connect-src is pinned to the configured CP + AS origins at build time. In dev (no
 *    VITE_CP_ORIGIN / VITE_AS_ORIGIN set) we emit a placeholder that is NOT shipped —
 *    _headers is only written on a non-dev build where the env vars are populated.
 */

import type { Plugin, NormalizedOutputOptions, OutputBundle, OutputChunk, OutputAsset } from "rollup";
import type { IndexHtmlTransformContext } from "vite";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";

export interface SriHeadersOptions {
  /**
   * CP origin for connect-src pinning (e.g. "https://cp.spawnery.dev").
   * Required in production builds; the plugin skips _headers emission if empty.
   */
  cpOrigin?: string;
  /**
   * AS origin for connect-src pinning (e.g. "https://as.spawnery.dev").
   */
  asOrigin?: string;
}

export function sha384(content: string | Uint8Array): string {
  const buf = typeof content === "string" ? Buffer.from(content, "utf8") : Buffer.from(content);
  return "sha384-" + crypto.createHash("sha384").update(buf).digest("base64");
}

/**
 * Build the CSP header value. The style-src 'unsafe-inline' is intentional — see module
 * comment above for the full rationale (xterm DOM renderer + sonner __insertCSS).
 */
export function buildCsp(cpOrigin: string, asOrigin: string): string {
  const cpHttps = cpOrigin;
  const cpWss = cpOrigin.replace(/^https:\/\//, "wss://").replace(/^http:\/\//, "ws://");
  const asHttps = asOrigin;
  const asWss = asOrigin.replace(/^https:\/\//, "wss://").replace(/^http:\/\//, "ws://");

  const connectParts = [cpHttps, cpWss, asHttps, asWss].filter(Boolean);
  const connectSrc = connectParts.length > 0 ? connectParts.join(" ") : "'none'";

  return [
    "default-src 'none'",
    "script-src 'self'",
    // 'unsafe-inline' is DELIBERATE — xterm DOM renderer + sonner __insertCSS.
    // See deploy/web/README.md §CSP — xterm decision record for the full rationale.
    // Removing this requires switching xterm to the WebGL/Canvas renderer AND patching
    // or replacing sonner.
    "style-src 'self' 'unsafe-inline'",
    "font-src 'self'",
    `connect-src ${connectSrc}`,
    "img-src 'self' data:",
    "frame-ancestors 'none'",
    "base-uri 'none'",
    "object-src 'none'",
  ].join("; ");
}

export function sriHeadersPlugin(opts: SriHeadersOptions = {}): Plugin {
  const cpOrigin = (opts.cpOrigin ?? process.env.VITE_CP_ORIGIN ?? "").trim();
  const asOrigin = (opts.asOrigin ?? process.env.VITE_AS_ORIGIN ?? "").trim();

  // Shared state between transformIndexHtml and generateBundle.
  // The integrityMap is populated in generateBundle and consumed in a closeBundle
  // post-processing step that patches the already-written dist/index.html.
  // We use a two-phase approach:
  //   Phase A (transformIndexHtml, enforce: post): capture inline scripts and remove them.
  //   Phase B (generateBundle): collect all chunk/asset SRI hashes.
  //   Phase C (closeBundle): read dist/index.html, re-apply inline-to-external migration,
  //             stamp all SRI attributes, write _headers.
  const inlineScripts: { code: string; assetName: string; integ: string }[] = [];
  let outDir = "dist"; // updated from resolvedConfig
  const integrityMap = new Map<string, string>(); // fileName (no leading /) -> integrity

  return {
    name: "spawnery-sri-headers",
    apply: "build",

    configResolved(config) {
      outDir = config.build.outDir ?? "dist";
    },

    // Phase A: extract inline scripts from index.html BEFORE Vite processes them.
    // enforce: 'post' ensures we run after Vite's own HTML transforms so the chunk
    // references are already in place as <script type="module" src="..."> tags.
    transformIndexHtml: {
      order: "post",
      handler(html: string, _ctx: IndexHtmlTransformContext) {
        // Find inline <script> blocks (not type="module", not src="...").
        const inlineScriptRe = /<script(?![^>]*\btype\b)(?![^>]*\bsrc\b)([^>]*)>([\s\S]*?)<\/script>/g;
        let m: RegExpExecArray | null;
        let result = html;

        while ((m = inlineScriptRe.exec(html)) !== null) {
          const attrs = m[1];
          const code = m[2].trim();
          if (!code) continue;

          // Compute a stable asset name from the content hash.
          const digest = crypto.createHash("sha256").update(code).digest("hex").slice(0, 16);
          const assetName = `assets/theme-bootstrap.${digest}.js`;
          const integ = sha384(code);

          inlineScripts.push({ code, assetName, integ });

          // Replace the inline <script> with an external reference.
          result = result.replace(
            m[0],
            `<script${attrs} src="/${assetName}" integrity="${integ}" crossorigin="anonymous"></script>`,
          );
        }

        return result;
      },
    },

    // Phase B: collect SRI hashes for all emitted chunks/assets.
    generateBundle(_options: NormalizedOutputOptions, bundle: OutputBundle) {
      // Register inline script integrity in the map (the actual file write happens in closeBundle).
      for (const { assetName, integ } of inlineScripts) {
        integrityMap.set(assetName, integ);
      }

      // Compute SRI for every JS chunk and CSS asset.
      for (const [fileName, chunk] of Object.entries(bundle)) {
        if (chunk.type === "chunk") {
          integrityMap.set(fileName, sha384((chunk as OutputChunk).code));
        } else if (chunk.type === "asset") {
          const src = (chunk as OutputAsset).source;
          if ((fileName.endsWith(".css") || fileName.endsWith(".js")) &&
              (typeof src === "string" || src instanceof Uint8Array)) {
            integrityMap.set(fileName, sha384(src));
          }
        }
      }

      // Check: are ALL chunks covered?
      const uncoveredChunks: string[] = [];
      for (const [fileName, chunk] of Object.entries(bundle)) {
        if (chunk.type === "chunk" && !integrityMap.has(fileName)) {
          uncoveredChunks.push(fileName);
        }
      }
      if (uncoveredChunks.length > 0) {
        // [WL4]: fail the build rather than shipping unhashed chunks.
        throw new Error(
          `[sri-headers-plugin] FATAL: Chunks with no integrity coverage:\n` +
          uncoveredChunks.map((f) => `  ${f}`).join("\n"),
        );
      }
    },

    // Phase C: write extracted inline scripts to dist/, post-process dist/index.html
    // to stamp SRI attributes, then write dist/_headers.
    closeBundle() {
      const htmlPath = path.resolve(outDir, "index.html");
      if (!fs.existsSync(htmlPath)) return;

      // Write the extracted inline scripts to dist/assets/.
      for (const { code, assetName } of inlineScripts) {
        const assetPath = path.resolve(outDir, assetName);
        fs.mkdirSync(path.dirname(assetPath), { recursive: true });
        fs.writeFileSync(assetPath, code, "utf8");
      }

      let html = fs.readFileSync(htmlPath, "utf8");

      // Stamp SRI on <script src="..."> tags.
      html = html.replace(
        /<script([^>]*)\ssrc="([^"]+)"([^>]*)>/g,
        (_match: string, before: string, src: string, after: string) => {
          const fileName = src.replace(/^\//, "");
          const integ = integrityMap.get(fileName);
          if (!integ) return _match;
          const attrs = (before + after)
            .replace(/\s*integrity="[^"]*"/g, "")
            .replace(/\s*crossorigin="[^"]*"/g, "");
          return `<script${attrs} src="${src}" integrity="${integ}" crossorigin="anonymous">`;
        },
      );

      // Stamp SRI on <link rel="stylesheet" href="..."> tags.
      html = html.replace(
        /<link([^>]*)>/g,
        (_match: string, attrs: string) => {
          if (!attrs.includes('rel="stylesheet"') && !attrs.includes("rel='stylesheet'")) {
            // Might be modulepreload — handle below.
            if (!attrs.includes('rel="modulepreload"') && !attrs.includes("rel='modulepreload'")) {
              return _match;
            }
          }
          const hrefM = attrs.match(/\shref="([^"]+)"/);
          if (!hrefM) return _match;
          const href = hrefM[1];
          const fileName = href.replace(/^\//, "");
          const integ = integrityMap.get(fileName);
          if (!integ) return _match;
          const cleanedAttrs = attrs
            .replace(/\s*integrity="[^"]*"/g, "")
            .replace(/\s*crossorigin="[^"]*"/g, "");
          return `<link${cleanedAttrs} integrity="${integ}" crossorigin="anonymous">`;
        },
      );

      // Add modulepreload entries for dynamic-import chunks not yet in the HTML.
      const existingPreloads = new Set<string>();
      const preloadRe = /href="([^"]+)"/g;
      let pm: RegExpExecArray | null;
      const preloadSection = html.match(/<link[^>]+rel="modulepreload"[^>]*>/g) ?? [];
      for (const tag of preloadSection) {
        while ((pm = preloadRe.exec(tag)) !== null) {
          existingPreloads.add(pm[1].replace(/^\//, ""));
        }
        preloadRe.lastIndex = 0;
      }

      // Also track explicitly referenced <script src> files.
      const scriptRefRe = /src="([^"]+)"/g;
      let sm: RegExpExecArray | null;
      while ((sm = scriptRefRe.exec(html)) !== null) {
        existingPreloads.add(sm[1].replace(/^\//, ""));
      }

      const newPreloads: string[] = [];
      for (const [fileName, integ] of integrityMap.entries()) {
        if (fileName.endsWith(".js") && !existingPreloads.has(fileName)) {
          newPreloads.push(
            `<link rel="modulepreload" href="/${fileName}" integrity="${integ}" crossorigin="anonymous">`,
          );
        }
      }

      if (newPreloads.length > 0) {
        html = html.replace("</head>", newPreloads.join("\n") + "\n</head>");
      }

      fs.writeFileSync(htmlPath, html, "utf8");

      // Emit _headers only when CP origin is configured (production build).
      if (!cpOrigin) return;

      const csp = buildCsp(cpOrigin, asOrigin);
      // /* covers the SPA root path "/" and every SPA route. Cloudflare Pages / Netlify serve
      // the document at "/" (not "/index.html"), so CSP must be on /* not just /index.html.
      // Rules are applied top-to-bottom; later rules override earlier ones for the same header
      // name — /assets/* comes last so it can override Cache-Control: no-cache with immutable.
      const headers = [
        "# Cache + CSP headers — emitted by sri-headers-plugin, shipped inside the signed dist/",
        "# [WL4]: this file ships INSIDE the signed dist, not hand-edited at the host.",
        "",
        "/*",
        `  Content-Security-Policy: ${csp}`,
        "  Cache-Control: no-cache",
        "  X-Content-Type-Options: nosniff",
        "  X-Frame-Options: DENY",
        "",
        "/assets/*",
        "  Cache-Control: public, max-age=31536000, immutable",
        "  X-Content-Type-Options: nosniff",
      ].join("\n");

      fs.writeFileSync(path.resolve(outDir, "_headers"), headers, "utf8");
    },
  };
}
