import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, expect, it } from "vitest";

const scanner = fileURLToPath(
  new URL("../../deploy/web/forbidden-scan.sh", import.meta.url),
);

let tmpDir: string;
let distDir: string;

function isDependencyBin(dir: string): boolean {
  const normalized = path.resolve(dir);
  return path.basename(normalized) === ".bin" &&
    path.basename(path.dirname(normalized)) === "node_modules";
}

function findExecutable(
  name: string,
  searchPath: string,
  excludeDependencyBins = false,
): string {
  for (const dir of searchPath.split(path.delimiter)) {
    if (excludeDependencyBins && isDependencyBin(dir)) continue;
    const candidate = path.join(dir, name);
    try {
      fs.accessSync(candidate, fs.constants.X_OK);
      return candidate;
    } catch {
      // Keep searching the active PATH.
    }
  }
  throw new Error(`${name} is not executable on PATH`);
}

function toolOnlyPath(searchPath: string): string {
  const directories = ["node", "npx", "bash", "sh", "dirname"].map((tool) =>
    path.dirname(findExecutable(tool, searchPath, true)),
  );
  return [...new Set(directories)].join(path.delimiter);
}

beforeEach(() => {
  tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "spawnery-scan-wrapper-"));
  distDir = path.join(tmpDir, "dist");
  fs.mkdirSync(distDir);
  fs.writeFileSync(path.join(distDir, "index.html"), "<!doctype html><title>Spawnery</title>");
  fs.writeFileSync(
    path.join(distDir, "_headers"),
    "/*\n  Content-Security-Policy: default-src 'none'; connect-src https://cp.spawnery.test wss://cp.spawnery.test\n",
  );
});

afterEach(() => {
  fs.rmSync(tmpDir, { recursive: true, force: true });
});

it("runs the real wrapper with only the web workspace's tsx", () => {
  const setupNodeBin = path.join(tmpDir, "setup-node-bin");
  fs.mkdirSync(setupNodeBin);
  for (const tool of ["node", "npx"]) {
    fs.symlinkSync(
      fs.realpathSync(findExecutable(tool, process.env.PATH ?? "")),
      path.join(setupNodeBin, tool),
    );
  }
  const activePath = [setupNodeBin, process.env.PATH ?? ""].join(path.delimiter);
  const activeNpxDir = path.dirname(findExecutable("npx", activePath));
  const npmCache = path.join(tmpDir, "npm-cache");
  const scannerPath = toolOnlyPath(activePath);

  expect(activeNpxDir).toBe(setupNodeBin);
  expect(scannerPath.split(path.delimiter)).toContain(activeNpxDir);
  expect(scannerPath.split(path.delimiter).some(isDependencyBin)).toBe(false);
  expect(fs.existsSync(npmCache)).toBe(false);

  const result = spawnSync(scanner, [distDir], {
    encoding: "utf8",
    env: {
      ...process.env,
      PATH: scannerPath,
      NODE_PATH: "",
      npm_config_cache: npmCache,
      npm_config_offline: "true",
      npm_config_update_notifier: "false",
    },
  });

  expect(
    result.status,
    [result.stdout, result.stderr].filter(Boolean).join("\n"),
  ).toBe(0);
});
