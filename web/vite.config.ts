import { defineConfig } from "vite";
import { configDefaults } from "vitest/config";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "path";
import { sriHeadersPlugin } from "./build/sri-headers-plugin";
import { createHeadersMiddleware } from "./e2e/preview-headers";

export default defineConfig({
  plugins: [
    react(),
    tailwindcss(),
    sriHeadersPlugin({
      cpOrigin: process.env.VITE_CP_ORIGIN,
      asOrigin: process.env.VITE_AS_ORIGIN,
    }),
    // Inject dist/_headers into the preview server so the Playwright CSP suite
    // exercises the REAL enforced CSP (vite preview ignores _headers natively).
    {
      name: "preview-csp-headers",
      configurePreviewServer(server) {
        server.middlewares.use(createHeadersMiddleware());
      },
    },
  ],
  resolve: { alias: { "@": path.resolve(__dirname, "./src") } },
  server: {
    host: true,
    proxy: {
      "/cp.v1.SpawnService": { target: "http://127.0.0.1:8080", changeOrigin: true },
      "/ws": { target: "http://127.0.0.1:8080", ws: true, changeOrigin: true },
      // AS routes: proxy /oauth, /refresh, /logout, /ca to the AS dev origin.
      // When VITE_AS_ORIGIN is unset (dev without AS), these 404 gracefully.
      // Same-origin proxy so HttpOnly cookie + credentialed CORS work in dev (AM2).
      "/oauth": { target: process.env.VITE_AS_ORIGIN ?? "http://127.0.0.1:8090", changeOrigin: true },
      "/refresh": { target: process.env.VITE_AS_ORIGIN ?? "http://127.0.0.1:8090", changeOrigin: true },
      "/logout": { target: process.env.VITE_AS_ORIGIN ?? "http://127.0.0.1:8090", changeOrigin: true },
      // NOTE trailing slash: vite proxies by PREFIX, so a bare "/ca" also swallows the SPA's
      // "/callback" auth-return route (it starts with "/ca") and 404s it from the AS. The AS
      // endpoint is "/ca/root", so scope the rule to "/ca/".
      "/ca/": { target: process.env.VITE_AS_ORIGIN ?? "http://127.0.0.1:8090", changeOrigin: true },
    },
  },
  test: {
    passWithNoTests: true,
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    // e2e/ holds Playwright specs (own runner via `npm run test:e2e`); keep them
    // out of the hermetic Vitest unit run.
    exclude: [...configDefaults.exclude, "e2e/**"],
    // build/ contains hermetic plugin tests that run under Node (no jsdom APIs).
    include: ["src/**/*.test.{ts,tsx}", "build/**/*.test.ts"],
    environmentMatchGlobs: [
      // build/ tests are pure Node — no jsdom needed.
      ["build/**", "node"],
    ],
  },
});
