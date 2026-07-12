import { test } from "node:test";
import assert from "node:assert/strict";
import { SpawnClient } from "./client.js";
import type { KeyStore } from "./keystore.js";
import type { Transport } from "@connectrpc/connect";

function rejectingClient(stored: Awaited<ReturnType<KeyStore["get"]>> = null): { client: SpawnClient; rpcCalls: () => number } {
  let calls = 0;
  const transport = {
    unary: async () => { calls++; throw new Error("RPC must not start"); },
    stream: async () => { throw new Error("unexpected stream"); },
  } as Transport;
  const keyStore: KeyStore = {
    get: async () => stored,
    put: async () => {},
    delete: async () => {},
  };
  return {
    client: new SpawnClient({
      transport,
      keyStore,
      getNodeAccessToken: async () => "node-token",
      verifyTarget: async () => {},
    }),
    rpcCalls: () => calls,
  };
}

test("authorized lifecycle methods fail preflight before any mutating RPC", async (t) => {
  const cases: Array<[string, (client: SpawnClient) => Promise<unknown>]> = [
    ["create", (client) => client.createSpawn({ appId: "app-1" })],
    ["resume", (client) => client.resume("sp-1")],
    ["recreate", (client) => client.recreate("sp-1")],
    ["migrate", (client) => client.migrate("sp-1")],
    ["fork", (client) => client.fork("sp-1", { targetClass: "cloud" })],
  ];
  for (const [name, invoke] of cases) {
    await t.test(name, async () => {
      const { client, rpcCalls } = rejectingClient();
      await assert.rejects(invoke(client), /no session keypair/);
      assert.equal(rpcCalls(), 0);
    });
  }
});

test("authorized lifecycle methods reject an unusable stored key before any mutating RPC", async (t) => {
  const pair = await crypto.subtle.generateKey(
    { name: "ECDSA", namedCurve: "P-256" }, false, ["sign", "verify"],
  ) as CryptoKeyPair;
  const unusable = { privateKey: pair.publicKey, publicKey: pair.publicKey };
  const cases: Array<[string, (client: SpawnClient) => Promise<unknown>]> = [
    ["create", (client) => client.createSpawn({ appId: "app-1" })],
    ["resume", (client) => client.resume("sp-1")],
    ["recreate", (client) => client.recreate("sp-1")],
    ["migrate", (client) => client.migrate("sp-1")],
    ["fork", (client) => client.fork("sp-1", { targetClass: "cloud" })],
  ];
  for (const [name, invoke] of cases) {
    await t.test(name, async () => {
      const { client, rpcCalls } = rejectingClient(unusable);
      await assert.rejects(invoke(client), /cannot sign/);
      assert.equal(rpcCalls(), 0);
    });
  }
});

test("authorization options are all-or-none", () => {
  const transport = {} as Transport;
  assert.throws(() => new SpawnClient({
    transport,
    keyStore: { get: async () => null, put: async () => {}, delete: async () => {} },
  }), /configured together/);
});
