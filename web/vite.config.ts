import { defineConfig } from "vite";
import { configDefaults } from "vitest/config";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "path";

export default defineConfig({
  plugins: [react(), tailwindcss()],
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
    // e2e/ holds Playwright specs (own runner via `npm run test:e2e`); keep them
    // out of the hermetic Vitest unit run.
    exclude: [...configDefaults.exclude, "e2e/**"],
  },
});
