import { readFileSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";

const PIDS = [
  path.join(os.tmpdir(), "spawnery-e2e-node.pid"),
  path.join(os.tmpdir(), "spawnery-e2e-cp.pid"),
];

export default async function globalTeardown() {
  for (const f of PIDS) {
    try {
      const pid = parseInt(readFileSync(f, "utf8").trim(), 10);
      if (pid) {
        try { process.kill(pid, "SIGTERM"); } catch { /* already gone */ }
      }
    } catch { /* no pid file */ } finally {
      try { rmSync(f); } catch { /* ignore */ }
    }
  }
}
