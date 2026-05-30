import { readFileSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";

const PID_FILE = path.join(os.tmpdir(), "spawnery-e2e-spawnlet.pid");

export default async function globalTeardown() {
  try {
    const pid = parseInt(readFileSync(PID_FILE, "utf8").trim(), 10);
    if (pid) {
      try { process.kill(pid, "SIGTERM"); } catch { /* already gone */ }
    }
  } finally {
    try { rmSync(PID_FILE); } catch { /* ignore */ }
  }
}
