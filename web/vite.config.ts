import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/spawn.v1.SpawnService": { target: "http://127.0.0.1:9090", changeOrigin: true },
      "/ws": { target: "http://127.0.0.1:9090", ws: true, changeOrigin: true },
    },
  },
  test: {
    passWithNoTests: true,
  },
});
