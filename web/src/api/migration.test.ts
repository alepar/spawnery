import { beforeEach, afterEach, describe, expect, it, vi } from "vitest";

const unaryMock = vi.fn();
const openEnvelopeMock = vi.fn();
const hpkeSealMock = vi.fn();
const verifyNodeForSealingMock = vi.fn();
const randomUUIDMock = vi.fn();

vi.mock("./connect", () => ({ unary: (...a: unknown[]) => unaryMock(...a) }));
vi.mock("@/auth/session", () => ({
  authEnabled: () => false,
  useSessionStore: {
    getState: () => ({ account: { accountId: "acct-1" } }),
  },
}));
vi.mock("@/auth/intent", () => ({
  pollAndSign: vi.fn(),
  registerPendedOp: vi.fn(),
  clearPendedOp: vi.fn(),
}));
vi.mock("@/keys/hpke", () => ({
  openEnvelope: (...a: unknown[]) => openEnvelopeMock(...a),
  hpkeSeal: (...a: unknown[]) => hpkeSealMock(...a),
}));
vi.mock("@/keys/subkey", () => ({
  verifyNodeForSealing: (...a: unknown[]) => verifyNodeForSealingMock(...a),
}));

import { runMigrate } from "./migration";

function b64(s: string): string {
  return btoa(s);
}

describe("runMigrate", () => {
  beforeEach(() => {
    unaryMock.mockReset();
    openEnvelopeMock.mockReset();
    hpkeSealMock.mockReset();
    verifyNodeForSealingMock.mockReset();
    randomUUIDMock.mockReset();
    randomUUIDMock.mockReturnValue("fixed-delivery-id");

    vi.stubGlobal("crypto", {
      subtle: {
        exportKey: vi.fn(async () => new Uint8Array([1, 2, 3]).buffer),
      },
      randomUUID: randomUUIDMock,
    });

    openEnvelopeMock.mockResolvedValue(new Uint8Array([9, 9, 9]));
    hpkeSealMock.mockResolvedValue({ enc: "sealed-enc", ct: "sealed-ct" });
    verifyNodeForSealingMock.mockResolvedValue({
      hpkePub: new Uint8Array([4, 5, 6]),
      identity: { nodeId: "node-a", accountId: "acct-1", nodeClass: "self-hosted" },
    });
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("delivers the same version and delivery ID used for in-flight AAD", async () => {
    unaryMock.mockImplementation(async (method: string) => {
      switch (method) {
        case "GetJournalKeyCiphertext":
          return {
            entries: [{
              mount: "main",
              ciphertext: b64(JSON.stringify({ recipients: [], nonce: "", ct: "" })),
            }],
          };
        case "MigrateSpawn":
          return { nodeId: "node-a", transferSetId: "ts-1" };
        case "GetSpawnNodeKey":
          return {
            nodeCertChain: "",
            signedSubkey: b64(JSON.stringify({ node_id: "node-a", not_after: "2030-01-01T00:00:00Z" })),
            generation: "7",
          };
        case "DeliverSecrets":
          return {};
        default:
          throw new Error(`unexpected RPC ${method}`);
      }
    });

    await runMigrate(
      "sp1",
      { nodeId: "node-a", class: "self-hosted" },
      { x25519Public: {} as CryptoKey, x25519Private: {} as CryptoKey },
      "",
      new Date("2026-06-15T00:00:00Z"),
    );

    const deliverCall = unaryMock.mock.calls.find(([method]) => method === "DeliverSecrets");
    expect(deliverCall).toBeTruthy();
    const deliverReq = deliverCall?.[1] as { secrets: Array<{ version?: number; deliveryId?: string }> };
    expect(deliverReq.secrets[0].version).toBe(7);
    expect(deliverReq.secrets[0].deliveryId).toBe("fixed-delivery-id");
    expect(randomUUIDMock).toHaveBeenCalledTimes(1);
  });

  it("uses the AS revocation checker by default for node verification", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ revoked_node_ids: [] }), { status: 200 })));
    unaryMock.mockImplementation(async (method: string) => {
      switch (method) {
        case "GetJournalKeyCiphertext":
          return {
            entries: [{
              mount: "main",
              ciphertext: b64(JSON.stringify({ recipients: [], nonce: "", ct: "" })),
            }],
          };
        case "MigrateSpawn":
          return { nodeId: "node-a", transferSetId: "ts-1" };
        case "GetSpawnNodeKey":
          return {
            nodeCertChain: b64("chain-pem"),
            signedSubkey: b64(JSON.stringify({ node_id: "node-a", not_after: "2030-01-01T00:00:00Z" })),
            generation: "7",
          };
        case "DeliverSecrets":
          return {};
        default:
          throw new Error(`unexpected RPC ${method}`);
      }
    });

    await runMigrate(
      "sp1",
      { nodeId: "node-a", class: "self-hosted" },
      { x25519Public: {} as CryptoKey, x25519Private: {} as CryptoKey },
      "root-pem",
      new Date("2026-06-15T00:00:00Z"),
    );

    const checker = verifyNodeForSealingMock.mock.calls[0]?.[5] as { check(nodeId: string): Promise<void> } | undefined;
    expect(checker).toBeTruthy();
    await expect(checker?.check("node-a")).resolves.toBeUndefined();
  });

  it("surfaces AS node revocation as a delivery failure", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ revoked_node_ids: ["node-a"] }), { status: 200 })));
    verifyNodeForSealingMock.mockImplementation(async (...args: unknown[]) => {
      const checker = args[5] as { check(nodeId: string): Promise<void> };
      await checker.check("node-a");
      return { hpkePub: new Uint8Array([4, 5, 6]) };
    });
    unaryMock.mockImplementation(async (method: string) => {
      switch (method) {
        case "GetJournalKeyCiphertext":
          return {
            entries: [{
              mount: "main",
              ciphertext: b64(JSON.stringify({ recipients: [], nonce: "", ct: "" })),
            }],
          };
        case "MigrateSpawn":
          return { nodeId: "node-a", transferSetId: "ts-1" };
        case "GetSpawnNodeKey":
          return {
            nodeCertChain: b64("chain-pem"),
            signedSubkey: b64(JSON.stringify({ node_id: "node-a", not_after: "2030-01-01T00:00:00Z" })),
            generation: "7",
          };
        default:
          throw new Error(`unexpected RPC ${method}`);
      }
    });

    await expect(runMigrate(
      "sp1",
      { nodeId: "node-a", class: "self-hosted" },
      { x25519Public: {} as CryptoKey, x25519Private: {} as CryptoKey },
      "root-pem",
      new Date("2026-06-15T00:00:00Z"),
    )).rejects.toMatchObject({ leg: "delivery" });
  });

  it("fails delivery when verified node identity differs from the resolved node", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ revoked_node_ids: [] }), { status: 200 })));
    verifyNodeForSealingMock.mockResolvedValue({
      hpkePub: new Uint8Array([4, 5, 6]),
      identity: { nodeId: "node-c", accountId: "acct-1", nodeClass: "self-hosted" },
    });
    unaryMock.mockImplementation(async (method: string) => {
      switch (method) {
        case "GetJournalKeyCiphertext":
          return {
            entries: [{
              mount: "main",
              ciphertext: b64(JSON.stringify({ recipients: [], nonce: "", ct: "" })),
            }],
          };
        case "MigrateSpawn":
          return { nodeId: "node-b", transferSetId: "ts-1" };
        case "GetSpawnNodeKey":
          return {
            nodeCertChain: b64("chain-pem"),
            signedSubkey: b64(JSON.stringify({ node_id: "node-c", not_after: "2030-01-01T00:00:00Z" })),
            generation: "7",
          };
        default:
          throw new Error(`unexpected RPC ${method}`);
      }
    });

    await expect(runMigrate(
      "sp1",
      { nodeId: "node-b", class: "self-hosted" },
      { x25519Public: {} as CryptoKey, x25519Private: {} as CryptoKey },
      "root-pem",
      new Date("2026-06-15T00:00:00Z"),
    )).rejects.toMatchObject({ leg: "delivery" });
    expect(unaryMock.mock.calls.some(([method]) => method === "DeliverSecrets")).toBe(false);
  });

  it("fails delivery when a pinned root is configured but the CP omits the node cert chain", async () => {
    unaryMock.mockImplementation(async (method: string) => {
      switch (method) {
        case "GetJournalKeyCiphertext":
          return {
            entries: [{
              mount: "main",
              ciphertext: b64(JSON.stringify({ recipients: [], nonce: "", ct: "" })),
            }],
          };
        case "MigrateSpawn":
          return { nodeId: "node-a", transferSetId: "ts-1" };
        case "GetSpawnNodeKey":
          return {
            nodeCertChain: "",
            signedSubkey: b64(JSON.stringify({ node_id: "node-a", not_after: "2030-01-01T00:00:00Z" })),
            generation: "7",
          };
        default:
          throw new Error(`unexpected RPC ${method}`);
      }
    });

    await expect(runMigrate(
      "sp1",
      { nodeId: "node-a", class: "self-hosted" },
      { x25519Public: {} as CryptoKey, x25519Private: {} as CryptoKey },
      "root-pem",
      new Date("2026-06-15T00:00:00Z"),
    )).rejects.toMatchObject({ leg: "delivery" });
    expect(verifyNodeForSealingMock).not.toHaveBeenCalled();
    expect(unaryMock.mock.calls.some(([method]) => method === "DeliverSecrets")).toBe(false);
  });
});
