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
  const result = spawnSync(scanner, [distDir], {
    encoding: "utf8",
    env: {
      ...process.env,
      PATH: "/usr/bin:/bin",
      NODE_PATH: "",
      npm_config_cache: path.join(tmpDir, "npm-cache"),
      npm_config_offline: "true",
      npm_config_update_notifier: "false",
    },
  });

  expect(
    result.status,
    [result.stdout, result.stderr].filter(Boolean).join("\n"),
  ).toBe(0);
});
