/**
 * Run-namespacing: every artifact this suite creates is named `acc-<runId>-w<workerIndex>-<base>`
 * so the pre-run/teardown sweeps can recognize and reap them (design §Isolation, namespacing).
 * runId embeds a sortable base36 millisecond timestamp so a stale-artifact sweep can parse the
 * artifact's age straight from its name without a side-channel lookup.
 */

const RUN_ID_RE = /^r([0-9a-z]+)-[0-9a-z]{4}$/;

/** newRunId mints a run id embedding `now` (ms) as a sortable base36 prefix + a random suffix. */
export function newRunId(now: number = Date.now()): string {
  const ts = Math.floor(now).toString(36);
  const rand = Math.random().toString(36).slice(2, 6).padEnd(4, "0");
  return `r${ts}-${rand}`;
}

/** runTimestamp parses the embedded ms timestamp back out of a runId; null if unparseable. */
export function runTimestamp(runId: string): number | null {
  const m = RUN_ID_RE.exec(runId);
  if (!m) return null;
  const n = parseInt(m[1], 36);
  return Number.isFinite(n) ? n : null;
}

/** nsName builds a namespaced artifact name for the given run/worker. */
export function nsName(runId: string, workerIndex: number, base: string): string {
  return `acc-${runId}-w${workerIndex}-${base}`;
}

/** isAccArtifact reports whether a name was created by this suite (namespaced with the acc- prefix). */
export function isAccArtifact(name: string): boolean {
  return name.startsWith("acc-");
}

/** runIdOf extracts the runId component from a namespaced artifact name, or null if not one of ours. */
export function runIdOf(name: string): string | null {
  const m = /^acc-(r[0-9a-z]+-[0-9a-z]{4})-w\d+-/.exec(name);
  return m ? m[1] : null;
}
