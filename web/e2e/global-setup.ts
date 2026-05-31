import { spawn, ChildProcess } from "node:child_process";
import { writeFileSync } from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const REPO = path.resolve(__dirname, "..", ".."); // web/e2e -> repo root
const CP_PID = path.join(os.tmpdir(), "spawnery-e2e-cp.pid");
const NODE_PID = path.join(os.tmpdir(), "spawnery-e2e-node.pid");

function build(out: string, pkg: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const p = spawn("go", ["build", "-o", path.join(REPO, "bin", out), pkg], { cwd: REPO, stdio: "inherit" });
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
        if (Date.now() > deadline) reject(new Error(`nothing listening on ${host}:${port} in ${timeoutMs}ms`));
        else setTimeout(tick, 250);
      });
    };
    tick();
  });
}

export default async function globalSetup() {
  await build("cp", "./cmd/cp");
  await build("spawnlet", "./cmd/spawnlet");

  const cp = spawn(path.join(REPO, "bin", "cp"), [], {
    cwd: REPO,
    env: {
      ...process.env,
      CP_LISTEN: "127.0.0.1:8080",
      CP_DEV_TOKENS: "dev-token=alice",
      CP_TELEMETRY: path.join(os.tmpdir(), "spawnery-e2e-events.jsonl"),
    },
    stdio: "inherit",
  });
  writeFileSync(CP_PID, String(cp.pid));
  await waitForPort("127.0.0.1", 8080, 15_000);

  const node: ChildProcess = spawn(path.join(REPO, "bin", "spawnlet"), [], {
    cwd: REPO, // so the relative examples/secret-app app_ref resolves
    env: {
      ...process.env,
      CP_ADDR: "http://127.0.0.1:8080",
      NODE_ID: "node-1",
      AGENT_IMAGE: "spawnery/stubagent:dev",
      SIDECAR_IMAGE: "spawnery/sidecar:dev",
      OPENROUTER_API_KEY: "unused",
      DATA_ROOT: path.join(REPO, ".spawns"),
    },
    stdio: "inherit",
  });
  writeFileSync(NODE_PID, String(node.pid));
  // give the node a moment to dial + register with the CP
  await new Promise((r) => setTimeout(r, 1500));
}
