import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import {
  assertDisposableVM,
  cpAuthModePlan,
  cpAuthModeReadinessCommand,
  deployCurrentRevocation,
  loadDestructiveVMAuthConfig,
  loadVMAuthConfig,
  posixShellQuote,
  setCPAuthMode,
  vmRunMarkerVerificationCommand,
} from "./root-anchored-artifacts";

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
