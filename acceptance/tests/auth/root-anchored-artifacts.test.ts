import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { mkdir, mkdtemp, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, relative } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import {
  assertDisposableVM,
  cpAuthModePlan,
  cpAuthModeReadinessCommand,
  deployAlternateSPABundle,
  deployCurrentRevocation,
  expectNoRuntimeObjects,
  loadDestructiveVMAuthConfig,
  loadVMAuthConfig,
  posixShellQuote,
  setCPAuthMode,
  spaBundlePublicationPlan,
  vmRunMarkerVerificationCommand,
} from "./root-anchored-artifacts";

const temporaryDirectories: string[] = [];

async function spaBundle(index: "file" | "directory" | "missing" = "file"): Promise<string> {
  const directory = await mkdtemp(join(tmpdir(), "spawnery-spa-bundle-"));
  temporaryDirectories.push(directory);
  if (index === "file") await writeFile(join(directory, "index.html"), "<!doctype html>");
  if (index === "directory") await mkdir(join(directory, "index.html"));
  return directory;
}

afterEach(async () => {
  await Promise.all(temporaryDirectories.splice(0).map((directory) => rm(directory, {
    recursive: true,
    force: true,
  })));
});

describe("AS signer custody", () => {
  it("has no acceptance-side short-lived token mint or AS private-key read/sign path", () => {
    const source = readFileSync(new URL("./root-anchored-artifacts.ts", import.meta.url), "utf8");
    expect(source).not.toContain("export async function mintShortLivedNodeToken");
    expect(source).not.toContain('await read("auth-signer-current-key.pem")');
    expect(source).not.toContain("nodeSign(null, message");
    expect(source).not.toMatch(/createPrivateKey|sign as nodeSign/);
    expect(source).not.toContain('await read("key.pem")');
  });
});

describe("posixShellQuote", () => {
  it.each([
    "",
    "acct-owner",
    "owner with spaces",
    "owner'; touch /tmp/not-created; printf '",
    "$(printf injected) `printf again` $HOME & | ; < >",
    "line one\nline two",
  ])("round-trips one literal shell argument", (value) => {
    const output = execFileSync("/bin/sh", ["-c", `printf %s ${posixShellQuote(value)}`]);
    expect(output.toString()).toBe(value);
  });
});

describe("expectNoRuntimeObjects", () => {
  const cfg = loadVMAuthConfig({
    ACC_E2E_VM_IP: "192.0.2.10",
    ACC_E2E_SSH_KEY: "/tmp/key",
    ACC_E2E_SSH_USER: "spawnery",
    ACC_CP_ENDPOINT: "https://vm.example",
    ACC_WEB_ORIGIN: "https://vm.example",
    ACC_TEST_APP_ID: "spawnery/secret-app",
    ACC_TEST_MODEL: "test-model",
    ACC_IDENTITY_POOL: "sub=owner",
  });

  it("fails closed when the pod query fails", async () => {
    const executeSSH = async () => { throw new Error("pods unavailable"); };

    await expect(expectNoRuntimeObjects(cfg, "sp1", executeSSH)).rejects.toThrow("pods unavailable");
  });

  it("fails closed when the container query fails after an empty pod query", async () => {
    const commands: string[] = [];
    const executeSSH = async (_cfg: typeof cfg, command: string) => {
      commands.push(command);
      if (command.includes("crictl ps")) throw new Error("containers unavailable");
      return "";
    };

    await expect(expectNoRuntimeObjects(cfg, "sp1", executeSSH)).rejects.toThrow("containers unavailable");
    expect(commands).toHaveLength(2);
  });
});

describe("cpAuthModePlan", () => {
  const cfg = {
    destructiveDevToken: "devtoken1",
    owner: "acc-owner-1",
  };

  it("loads the destructive dev environment through a systemd drop-in", () => {
    expect(cpAuthModePlan(cfg, "dev")).toEqual({
      configureCommand: "install -d /etc/systemd/system/spawnery-cp.service.d; "
        + "printf %s 'CP_AUTH_MODE=dev\nCP_DEV_TOKENS=devtoken1=acc-owner-1\n' > /etc/spawnery/env.d/zz-destructive.env; "
        + "printf %s '[Service]\nEnvironmentFile=/etc/spawnery/env.d/zz-destructive.env\n' > /etc/systemd/system/spawnery-cp.service.d/zz-destructive.conf",
      expectedLog: "cp: auth mode=dev",
    });
  });

  it("removes both overrides and requires explicit prod readiness", () => {
    expect(cpAuthModePlan(cfg, "prod")).toEqual({
      configureCommand: "rm -f /etc/spawnery/env.d/zz-destructive.env /etc/systemd/system/spawnery-cp.service.d/zz-destructive.conf",
      expectedLog: "cp: auth mode=prod",
    });
  });
});

describe("destructive VM identity", () => {
  const baseEnv = {
    ACC_E2E_VM_IP: "192.0.2.10",
    ACC_E2E_SSH_KEY: "/tmp/key",
    ACC_E2E_SSH_USER: "spawnery",
    ACC_CP_ENDPOINT: "https://vm.example",
    ACC_WEB_ORIGIN: "https://vm.example",
    ACC_TEST_APP_ID: "spawnery/secret-app",
    ACC_TEST_MODEL: "test-model",
    ACC_IDENTITY_POOL: "acc-owner-1=acc-owner-1",
  };

  it("loads ordinary VM auth without destructive-only variables", () => {
    expect(loadVMAuthConfig(baseEnv)).toMatchObject({
      ip: "192.0.2.10",
      owner: "acc-owner-1",
    });
  });

  it("requires the destructive dev token only in the destructive loader", () => {
    expect(() => loadDestructiveVMAuthConfig({ ...baseEnv, ACC_E2E_VM_RUNID: "run-123" }))
      .toThrow("ACC_DESTRUCTIVE_DEV_TOKEN");
  });

  it("requires the disposable VM run id only in the destructive loader", () => {
    expect(() => loadDestructiveVMAuthConfig({ ...baseEnv, ACC_DESTRUCTIVE_DEV_TOKEN: "devtoken1" }))
      .toThrow("ACC_E2E_VM_RUNID");
  });

  const env = {
    ...baseEnv,
    ACC_DESTRUCTIVE_DEV_TOKEN: "devtoken1",
  };

  it("verifies exact marker contents and root-only ownership", () => {
    expect(vmRunMarkerVerificationCommand("run-'quoted")).toBe(
      "marker=/run/spawnery-e2e-runid; "
      + "test \"$(sudo cat \"$marker\")\" = 'run-'\"'\"'quoted' && "
      + "test \"$(sudo stat -c '%U:%G:%a' \"$marker\")\" = root:root:600 && echo verified",
    );
  });

  it("fails closed unless SSH confirms the exact disposable marker", async () => {
    const cfg = loadDestructiveVMAuthConfig({ ...env, ACC_E2E_VM_RUNID: "run-123" });
    const commands: string[] = [];
    const executeSSH = async (_cfg: typeof cfg, command: string) => {
      commands.push(command);
      return "wrong-run";
    };
    await expect(assertDisposableVM(cfg, executeSSH)).rejects.toThrow("did not verify");
    expect(commands).toEqual([vmRunMarkerVerificationCommand("run-123")]);
  });

  it.each(["dev", "prod"] as const)(
    "checks the disposable marker before the %s CP auth-mode mutation",
    async (mode) => {
      const cfg = loadDestructiveVMAuthConfig({ ...env, ACC_E2E_VM_RUNID: "run-123" });
      const commands: string[] = [];
      const executeSSH = async (_cfg: typeof cfg, command: string) => {
        commands.push(command);
        if (command.includes("journalctl")) return "ready";
        return "wrong-run";
      };

      await expect(setCPAuthMode(cfg, mode, executeSSH)).rejects.toThrow("did not verify");
      expect(commands).toEqual([vmRunMarkerVerificationCommand("run-123")]);
    },
  );

  it("checks the disposable marker before revocation deployment performs any remote mutation", async () => {
    const cfg = loadDestructiveVMAuthConfig({ ...env, ACC_E2E_VM_RUNID: "run-123" });
    const commands: string[] = [];
    const executeSSH = async (_cfg: typeof cfg, command: string) => {
      commands.push(command);
      if (command.includes("signer-revocation")) return "wire";
      if (command.includes("curl -fsS")) return "ready";
      return "wrong-run";
    };

    await expect(deployCurrentRevocation(cfg, 1, executeSSH)).rejects.toThrow("did not verify");
    expect(commands).toEqual([vmRunMarkerVerificationCommand("run-123")]);
  });

  it("does not publish an alternate SPA bundle when the disposable marker fails", async () => {
    const cfg = loadDestructiveVMAuthConfig({ ...env, ACC_E2E_VM_RUNID: "run-123" });
    const bundleDir = await spaBundle();
    const publications: string[] = [];
    const executeFile = async (file: string) => { publications.push(file); };
    const executeSSH = async () => "wrong-run";

    await expect(deployAlternateSPABundle(
      cfg,
      bundleDir,
      executeSSH,
      executeFile,
    )).rejects.toThrow("did not verify");
    expect(publications).toEqual([]);
  });

  it("plans an absolute rsync-over-SSH publication with an operand boundary", async () => {
    const cfg = loadDestructiveVMAuthConfig({ ...env, ACC_E2E_VM_RUNID: "run-123" });
    const bundleDir = await spaBundle();
    expect((await stat(bundleDir)).mode & 0o777).toBe(0o700);
    const plan = await spaBundlePublicationPlan(cfg, relative(process.cwd(), bundleDir));

    expect(plan).toEqual({
      file: "rsync",
      args: [
        "-a",
        "--chmod=Dugo+rx",
        "--delete",
        "-e",
        "ssh -i '/tmp/key' -o BatchMode=yes -o StrictHostKeyChecking=no",
        "--rsync-path",
        "sudo rsync",
        "--",
        `${bundleDir}/`,
        "spawnery@192.0.2.10:/var/www/spawnery/",
      ],
      options: { maxBuffer: 4 * 1024 * 1024 },
    });
  });

  it.each([
    ["empty", ""],
    ["dash-prefixed", "--delete"],
    ["missing", join(tmpdir(), `missing-spa-bundle-${process.pid}`)],
  ])("rejects a %s SPA bundle input before exec", async (_name, bundleDir) => {
    const cfg = loadDestructiveVMAuthConfig({ ...env, ACC_E2E_VM_RUNID: "run-123" });
    const executions: string[] = [];

    await expect(deployAlternateSPABundle(
      cfg,
      bundleDir,
      async () => "verified",
      async (file) => { executions.push(file); },
    )).rejects.toThrow("alternate SPA bundle");
    expect(executions).toEqual([]);
  });

  it.each(["missing", "directory"] as const)(
    "rejects a bundle whose index.html is %s before exec",
    async (index) => {
      const cfg = loadDestructiveVMAuthConfig({ ...env, ACC_E2E_VM_RUNID: "run-123" });
      const bundleDir = await spaBundle(index);
      const executions: string[] = [];

      await expect(deployAlternateSPABundle(
        cfg,
        bundleDir,
        async () => "verified",
        async (file) => { executions.push(file); },
      )).rejects.toThrow("regular index.html");
      expect(executions).toEqual([]);
    },
  );

  it("stops revocation deployment when the disposable marker changes after artifact generation", async () => {
    const cfg = loadDestructiveVMAuthConfig({ ...env, ACC_E2E_VM_RUNID: "run-123" });
    const commands: string[] = [];
    const marker = vmRunMarkerVerificationCommand("run-123");
    let markerChecks = 0;
    const executeSSH = async (_cfg: typeof cfg, command: string) => {
      commands.push(command);
      if (command === marker) {
        markerChecks++;
        return markerChecks === 1 ? "verified" : "wrong-run";
      }
      if (command.includes("signer-revocation")) return "wire";
      return "";
    };

    await expect(deployCurrentRevocation(cfg, 1, executeSSH)).rejects.toThrow("did not verify");
    expect(markerChecks).toBe(2);
    expect(commands.some((command) => command.includes("signer-revocation"))).toBe(true);
    expect(commands.some((command) => command.includes("signer-revocations.artifact"))).toBe(false);
    expect(commands.some((command) => command.includes("systemctl restart"))).toBe(false);
  });

  it("rechecks the disposable marker immediately before installing revocation state", async () => {
    const cfg = loadDestructiveVMAuthConfig({ ...env, ACC_E2E_VM_RUNID: "run-123" });
    const commands: string[] = [];
    const marker = vmRunMarkerVerificationCommand("run-123");
    const executeSSH = async (_cfg: typeof cfg, command: string) => {
      commands.push(command);
      if (command === marker) return "verified";
      if (command.includes("signer-revocation")) return "wire";
      if (command.includes("curl -fsS")) return "ready";
      return "";
    };

    await deployCurrentRevocation(cfg, 1, executeSSH);
    const installIndex = commands.findIndex((command) => command.includes("signer-revocations.artifact"));
    expect(installIndex).toBeGreaterThan(0);
    expect(commands[installIndex - 1]).toBe(marker);
  });

  it("loads destructive config and gates the destructive spec before its first SSH operation", () => {
    const source = readFileSync(new URL("./root-anchored-artifacts.spec.ts", import.meta.url), "utf8");
    expect(source).toMatch(
      /const cfg = loadDestructiveVMAuthConfig\(\);\s+await assertDisposableVM\(cfg\);\s+const env = await ssh/,
    );
  });
});

describe("cpAuthModeReadinessCommand", () => {
  it("requires auth mode and node registration from the current CP process", () => {
    expect(cpAuthModeReadinessCommand("cp: auth mode=dev")).toBe(
      "sudo systemctl is-active spawnery-cp >/dev/null && "
      + "pid=$(sudo systemctl show --property MainPID --value spawnery-cp); "
      + "test \"$pid\" -gt 0 && "
      + "logs=$(sudo journalctl _SYSTEMD_UNIT=spawnery-cp.service _PID=\"$pid\" --no-pager); "
      + "printf %s \"$logs\" | grep -Fq 'cp: auth mode=dev' && "
      + "printf %s \"$logs\" | grep -Fq 'node connected' && echo ready",
    );
  });
});
