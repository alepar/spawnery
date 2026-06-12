/**
 * Global setup for auth e2e tests (playwright.auth.config.ts).
 *
 * Extends the main global setup (CP + node) with an AS process (authsvc) running
 * AS_DEV=1 + AS_FAKE_GITHUB=1 on 127.0.0.1:8090 so the Playwright auth suite can
 * exercise a real OAuth login + /refresh cold-reload path.
 *
 * Gate: E2E_AUTH=1 (tests skip themselves; setup runs to allow infra to stay warm).
 */
import { spawn, type ChildProcess } from "node:child_process";
import { writeFileSync } from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import globalSetup from "./global-setup";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const REPO = path.resolve(__dirname, "..", ".."); // web/e2e -> repo root
const AS_PID = path.join(os.tmpdir(), "spawnery-e2e-as.pid");

function buildBin(out: string, pkg: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const p = spawn("go", ["build", "-o", path.join(REPO, "bin", out), pkg], {
      cwd: REPO,
      stdio: "inherit",
    });
    p.on("exit", (c) => (c === 0 ? resolve() : reject(new Error(`go build ${pkg} exited ${c}`))));
    p.on("error", reject);
  });
}

function waitForPort(host: string, port: number, timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  return new Promise((resolve, reject) => {
    const tick = () => {
      const s = net.connect(port, host);
      s.once("connect", () => { s.destroy(); resolve(); });
      s.once("error", () => {
        s.destroy();
        if (Date.now() > deadline) reject(new Error(`nothing on ${host}:${port} after ${timeoutMs}ms`));
        else setTimeout(tick, 250);
      });
    };
    tick();
  });
}

export default async function globalSetupAuth() {
  // Build and start the CP + node (same as the standard e2e suite).
  await globalSetup();

  // Build the AS binary.
  await buildBin("authsvc", "./cmd/authsvc");

  // Start the AS with fake GitHub on 8090 so the Vite proxy (/oauth, /refresh, etc.)
  // can forward SPA requests to it without cross-origin cookie complications.
  // AS_GITHUB_REDIRECT_URI points back through the Vite proxy so the HttpOnly
  // refresh cookie is set for localhost:5174 (the SPA origin), not 127.0.0.1:8090.
  const dbPath = path.join(os.tmpdir(), "spawnery-e2e-as.db");
  const as: ChildProcess = spawn(path.join(REPO, "bin", "authsvc"), [], {
    cwd: REPO,
    env: {
      ...process.env,
      AS_LISTEN: "127.0.0.1:8090",
      AS_DEV: "1",
      AS_FAKE_GITHUB: "1",
      AS_DB_DSN: `file:${dbPath}?_pragma=foreign_keys(1)`,
      // SPA runs on port 5174 under the auth Playwright config (VITE_AUTH_ENABLED=1).
      AS_SPA_ORIGINS: "http://localhost:5174",
      // The fake GitHub redirects to this URL; routing it through Vite proxy ensures
      // the Set-Cookie response header is scoped to localhost:5174 (SPA origin) [AM2].
      AS_GITHUB_REDIRECT_URI: "http://localhost:5174/oauth/callback",
      // Registered redirect URIs for the SPA client.
      AS_REDIRECT_URIS: "http://localhost:5174/callback",
      REGISTRATION_ENABLED: "true",
    },
    stdio: "inherit",
  });
  writeFileSync(AS_PID, String(as.pid));
  await waitForPort("127.0.0.1", 8090, 15_000);
}
