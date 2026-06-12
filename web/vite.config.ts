import { defineConfig } from "vite";
import { configDefaults } from "vitest/config";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "path";
import { sriHeadersPlugin } from "./build/sri-headers-plugin";

export default defineConfig({
  plugins: [
    react(),
    tailwindcss(),
    sriHeadersPlugin({
      cpOrigin: process.env.VITE_CP_ORIGIN,
      asOrigin: process.env.VITE_AS_ORIGIN,
    }),
  ],
  resolve: { alias: { "@": path.resolve(__dirname, "./src") } },
  server: {
    host: true,
    proxy: {
      "/cp.v1.SpawnService": { target: "http://127.0.0.1:8080", changeOrigin: true },
      "/ws": { target: "http://127.0.0.1:8080", ws: true, changeOrigin: true },
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
