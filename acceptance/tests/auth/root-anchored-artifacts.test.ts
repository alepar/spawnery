import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { cpAuthModePlan, cpAuthModeReadinessCommand, posixShellQuote } from "./root-anchored-artifacts";

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
