import { readFileSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import globalTeardown from "./global-teardown";

const AS_PID = path.join(os.tmpdir(), "spawnery-e2e-as.pid");

export default async function globalTeardownAuth() {
  // Tear down the AS process first, then the CP + node.
  try {
    const pid = parseInt(readFileSync(AS_PID, "utf8").trim(), 10);
    if (pid) {
      try { process.kill(pid, "SIGTERM"); } catch { /* already gone */ }
    }
  } catch { /* no pid file */ } finally {
    try { rmSync(AS_PID); } catch { /* ignore */ }
  }
  await globalTeardown();
}
