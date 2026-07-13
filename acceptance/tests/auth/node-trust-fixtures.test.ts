import { describe, expect, it } from "vitest";
import { execFileSync } from "node:child_process";
import type { DestructiveVMAuthConfig } from "./root-anchored-artifacts";
import {
  generateNodeTrustFixtures,
  generateShortLivedNodeCRL,
  nodeTrustFixtureGenerationCommand,
  parseNodeTrustFixtureOutput,
} from "./node-trust-fixtures";
import { vmRunMarkerVerificationCommand } from "./root-anchored-artifacts";

const cfg: DestructiveVMAuthConfig = {
  ip: "192.0.2.10",
  sshKey: "/tmp/key",
  sshUser: "spawnery",
  cpEndpoint: "https://vm.example",
  asOrigin: "https://vm.example",
  webOrigin: "https://vm.example",
  appId: "spawnery/secret-app",
  model: "test-model",
  owner: "owner'; printf injected",
  destructiveDevToken: "devtoken1",
  vmRunId: "run-123",
};

function encoded(value: string): string {
  return Buffer.from(value).toString("base64");
}

describe("node trust fixtures", () => {
  it("emits a shell-parseable fixture command for hostile owner text", () => {
    expect(() => execFileSync("/bin/sh", [
      "-n",
      "-c",
      nodeTrustFixtureGenerationCommand("owner'; false; printf '"),
    ])).not.toThrow();
  });

  it("returns only public target material after verifying the disposable VM", async () => {
    const commands: string[] = [];
    const executeSSH = async (_cfg: DestructiveVMAuthConfig, command: string) => {
      commands.push(command);
      if (command === vmRunMarkerVerificationCommand(cfg.vmRunId)) return "verified";
      return [
        `foreign_root_chain=${encoded("-----BEGIN CERTIFICATE-----\nforeign\n-----END CERTIFICATE-----\n")}`,
        `unstamped_issuer_chain=${encoded("-----BEGIN CERTIFICATE-----\nsame-root\n-----END CERTIFICATE-----\n")}`,
        `expired_crl=${encoded("-----BEGIN X509 CRL-----\nexpired\n-----END X509 CRL-----\n")}`,
        "expired_crl_next_update_ms=1700000000000",
      ].join("\n");
    };

    await expect(generateNodeTrustFixtures(cfg, executeSSH)).resolves.toEqual({
      foreignRootChainPEM: "-----BEGIN CERTIFICATE-----\nforeign\n-----END CERTIFICATE-----\n",
      unstampedIssuerChainPEM: "-----BEGIN CERTIFICATE-----\nsame-root\n-----END CERTIFICATE-----\n",
      expiredCRLPEM: "-----BEGIN X509 CRL-----\nexpired\n-----END X509 CRL-----\n",
      expiredCRLNextUpdateMs: 1_700_000_000_000,
    });
    expect(commands[0]).toBe(vmRunMarkerVerificationCommand(cfg.vmRunId));
    expect(commands).toHaveLength(2);
    expect(commands[1]).toBe(nodeTrustFixtureGenerationCommand(cfg.owner));
    expect(commands[1]).not.toMatch(/auth\.json|spawnctl|session[-_ ]?key|client[-_ ]?key/i);
  });

  it("rejects any unexpected field instead of accepting private material", () => {
    expect(() => parseNodeTrustFixtureOutput([
      `foreign_root_chain=${encoded("foreign")}`,
      `unstamped_issuer_chain=${encoded("same-root")}`,
      `expired_crl=${encoded("expired")}`,
      "expired_crl_next_update_ms=1700000000000",
      `client_private_key=${encoded("forbidden")}`,
    ].join("\n"))).toThrow(/unexpected field client_private_key/);
  });

  it("generates a short-lived issuer CRL with its exact public expiry", async () => {
    const commands: string[] = [];
    const executeSSH = async (_cfg: DestructiveVMAuthConfig, command: string) => {
      commands.push(command);
      if (command === vmRunMarkerVerificationCommand(cfg.vmRunId)) return "verified";
      return [
        `crl=${encoded("-----BEGIN X509 CRL-----\nfresh\n-----END X509 CRL-----\n")}`,
        "next_update_ms=1700000005000",
      ].join("\n");
    };

    await expect(generateShortLivedNodeCRL(cfg, 20, executeSSH)).resolves.toEqual({
      crlPEM: "-----BEGIN X509 CRL-----\nfresh\n-----END X509 CRL-----\n",
      nextUpdateMs: 1_700_000_005_000,
    });
    expect(commands[0]).toBe(vmRunMarkerVerificationCommand(cfg.vmRunId));
    expect(commands).toHaveLength(2);
  });
});
