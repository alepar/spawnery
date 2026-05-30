import { spawn } from "node:child_process";
import { writeFileSync } from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const REPO = path.resolve(__dirname, "..", ".."); // web/e2e -> repo root
const PID_FILE = path.join(os.tmpdir(), "spawnery-e2e-spawnlet.pid");

function run(cmd: string, args: string[], cwd: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const p = spawn(cmd, args, { cwd, stdio: "inherit" });
    p.on("exit", (code) => (code === 0 ? resolve() : reject(new Error(`${cmd} exited ${code}`))));
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
        if (Date.now() > deadline) reject(new Error(`spawnlet did not open ${host}:${port} in ${timeoutMs}ms`));
        else setTimeout(tick, 250);
      });
    };
    tick();
  });
}

export default async function globalSetup() {
  // 1. build the spawnlet binary
  await run("go", ["build", "-o", path.join(REPO, "bin", "spawnlet"), "./cmd/spawnlet"], REPO);

  // 2. start it with the STUB agent image (deterministic; no key needed)
  const child = spawn(path.join(REPO, "bin", "spawnlet"), [], {
    cwd: REPO, // so the hardcoded relative examples/secret-app resolves
    env: {
      ...process.env,
      AGENT_IMAGE: "spawnery/stubagent:dev",
      SIDECAR_IMAGE: "spawnery/sidecar:dev",
      OPENROUTER_API_KEY: "unused",
      DATA_ROOT: path.join(REPO, ".spawns"),
      SPAWNLET_ADDR: "127.0.0.1:9090",
    },
    stdio: "inherit",
  });
  writeFileSync(PID_FILE, String(child.pid));

  // 3. wait until it's listening, else FAIL the run (no silent skip)
  await waitForPort("127.0.0.1", 9090, 15_000);
}
