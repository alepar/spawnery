import { spawn as nodeSpawn } from "node:child_process";
import { stat as nodeStat } from "node:fs/promises";
import { join } from "node:path";

const MAX_OUTPUT_BYTES = 64 * 1024;
const USER_CODE = /enter code:\s*([BCDFGHJKLMNPQRSTVWXYZ]{4}-[BCDFGHJKLMNPQRSTVWXYZ]{4})/i;

interface DeviceVerifyResponse {
  ok(): boolean;
  text(): Promise<string>;
}

export interface DeviceLoginPage {
  request: {
    post(url: string, options: { form: { user_code: string } }): Promise<DeviceVerifyResponse>;
  };
}

export interface CliDeviceLoginOptions {
  spawnctlBin: string;
  asOrigin: string;
  configHome: string;
  page: DeviceLoginPage;
  timeoutMs?: number;
}

interface CliDeviceDeps {
  spawn: typeof nodeSpawn;
  stat: typeof nodeStat;
}

const defaultDeps: CliDeviceDeps = { spawn: nodeSpawn, stat: nodeStat };

/** Drives spawnctl's real RFC 8628 flow while leaving its generated private key in auth.json. */
export async function runCliDeviceLogin(
  options: CliDeviceLoginOptions,
  deps: CliDeviceDeps = defaultDeps,
): Promise<void> {
  const asOrigin = options.asOrigin.replace(/\/$/, "");
  const child = deps.spawn(options.spawnctlBin, [
    "login", "--device", "--as", asOrigin, "--config-dir", options.configHome,
  ], {
    env: { ...process.env, XDG_CONFIG_HOME: options.configHome },
    stdio: ["ignore", "pipe", "pipe"],
  });

  let output = "";
  let approval: Promise<void> | undefined;
  let outputFailure: Error | undefined;

  const capture = (chunk: Buffer | string) => {
    output += chunk.toString();
    if (Buffer.byteLength(output) > MAX_OUTPUT_BYTES) {
      outputFailure = new Error(`spawnctl login output exceeded ${MAX_OUTPUT_BYTES} bytes`);
      child.kill("SIGTERM");
      return;
    }
    const match = USER_CODE.exec(output);
    if (!match || approval) return;
    approval = (async () => {
      const response = await options.page.request.post(`${asOrigin}/device/verify`, {
        form: { user_code: match[1].toUpperCase() },
      });
      if (!response.ok()) {
        throw new Error(`device verification rejected ${match[1]}: ${await response.text()}`);
      }
    })();
  };
  child.stdout.on("data", capture);
  child.stderr.on("data", capture);

  const exited = new Promise<void>((resolve, reject) => {
    child.once("error", reject);
    child.once("exit", (code, signal) => {
      if (outputFailure) return reject(outputFailure);
      if (!approval) {
        return reject(new Error(`spawnctl login exited before emitting a user code (code=${code}, signal=${signal}): ${output}`));
      }
      if (code !== 0) {
        return reject(new Error(`spawnctl login failed (code=${code}, signal=${signal}): ${output}`));
      }
      resolve();
    });
  });

  const timeoutMs = options.timeoutMs ?? 60_000;
  let timeout: NodeJS.Timeout | undefined;
  try {
    await Promise.race([
      exited,
      new Promise<never>((_, reject) => {
        timeout = setTimeout(() => {
          child.kill("SIGTERM");
          reject(new Error(`spawnctl device login timed out after ${timeoutMs}ms`));
        }, timeoutMs);
      }),
    ]);
    await approval;
  } finally {
    if (timeout) clearTimeout(timeout);
  }

  const authPath = join(options.configHome, "auth.json");
  const info = await deps.stat(authPath);
  const mode = info.mode & 0o777;
  if (mode !== 0o600) {
    throw new Error(`spawnctl auth.json mode ${mode.toString(8).padStart(4, "0")}; want 0600`);
  }
}
