import { execFile } from "node:child_process";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { createServer, type Server as HTTPSServer } from "node:https";
import { connect as connectHTTP2 } from "node:http2";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";
import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { cpv1 } from "@spawnery/client";

import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { startCPLoopbackProxy } from "./cp-loopback-proxy";

const execFileP = promisify(execFile);
const encoder = new TextEncoder();

function grpcEnvelope(payload: Uint8Array, compressed = false): Buffer {
  const envelope = Buffer.alloc(5 + payload.length);
  envelope[0] = compressed ? 1 : 0;
  envelope.writeUInt32BE(payload.length, 1);
  envelope.set(payload, 5);
  return envelope;
}

function grpcPayload(envelope: Uint8Array): Uint8Array {
  expect(envelope[0]).toBe(0);
  expect(envelope.length).toBe(5 + Buffer.from(envelope).readUInt32BE(1));
  return envelope.subarray(5);
}

let tlsDir = "";
let keyPEM = "";
let rootPEM = "";

beforeAll(async () => {
  tlsDir = await mkdtemp(join(tmpdir(), "spawnery-cp-proxy-tls-"));
  const keyPath = join(tlsDir, "key.pem");
  const certPath = join(tlsDir, "cert.pem");
  await execFileP("openssl", [
    "req", "-x509", "-newkey", "rsa:2048", "-nodes",
    "-keyout", keyPath, "-out", certPath,
    "-subj", "/CN=localhost", "-addext", "subjectAltName=DNS:localhost", "-days", "1",
  ]);
  [keyPEM, rootPEM] = await Promise.all([
    readFile(keyPath, "utf8"),
    readFile(certPath, "utf8"),
  ]);
});

afterAll(async () => {
  await rm(tlsDir, { recursive: true, force: true });
});

async function listenUpstream(
  handler: Parameters<typeof createServer>[1],
): Promise<{ origin: string; close: () => Promise<void> }> {
  const server: HTTPSServer = createServer({ key: keyPEM, cert: rootPEM }, handler);
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => resolve());
  });
  const address = server.address();
  if (!address || typeof address === "string") throw new Error("test HTTPS server did not bind TCP");
  return {
    origin: `https://localhost:${address.port}`,
    close: async () => {
      server.closeAllConnections();
      await new Promise<void>((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
    },
  };
}

async function proxyRequest(
  origin: string,
  path: string,
  options: { method?: string; headers?: Record<string, string>; body?: string | Uint8Array } = {},
): Promise<{
  status: number;
  headers: Record<string, string | string[]>;
  trailers: Record<string, string | string[]>;
  body: Buffer;
}> {
  const session = connectHTTP2(origin);
  try {
    return await new Promise((resolve, reject) => {
      const request = session.request({
        ":method": options.method ?? "POST",
        ":path": path,
        ...options.headers,
      });
      let status = 0;
      let headers: Record<string, string | string[]> = {};
      let trailers: Record<string, string | string[]> = {};
      const chunks: Buffer[] = [];
      request.on("response", (received) => {
        status = Number(received[":status"] ?? 0);
        headers = Object.fromEntries(Object.entries(received)
          .filter(([name]) => !name.startsWith(":"))
          .map(([name, value]) => [name, Array.isArray(value) ? value.map(String) : String(value)]));
      });
      request.on("trailers", (received) => {
        trailers = Object.fromEntries(Object.entries(received)
          .filter(([name]) => !name.startsWith(":"))
          .map(([name, value]) => [name, Array.isArray(value) ? value.map(String) : String(value)]));
      });
      request.on("data", (chunk: Buffer) => chunks.push(chunk));
      request.on("end", () => resolve({ status, headers, trailers, body: Buffer.concat(chunks) }));
      request.on("error", reject);
      request.end(options.body);
    });
  } finally {
    session.destroy();
  }
}

describe("startCPLoopbackProxy", () => {
  it.each(["0", "7"])("preserves upstream gRPC status %s and custom trailers exactly", async (grpcStatus) => {
    const trailers = {
      "grpc-status": grpcStatus,
      "grpc-message": grpcStatus === "0" ? "" : "permission denied",
      "x-custom-trailer": "kept",
    };
    const upstream = await listenUpstream((_request, response) => {
      response.writeHead(200, {
        "content-type": "application/grpc",
        trailer: Object.keys(trailers).join(", "),
      });
      response.write(grpcEnvelope(encoder.encode("payload")));
      response.addTrailers(trailers);
      response.end();
    });
    const proxy = await startCPLoopbackProxy({
      upstreamOrigin: upstream.origin,
      transportCAPEM: rootPEM,
    });
    try {
      const result = await proxyRequest(proxy.origin, "/cp.v1.SpawnService/ListSpawns");
      expect(result.trailers).toEqual(trailers);
    } finally {
      await proxy.close();
      await upstream.close();
    }
  });

  it("unframes, mutates, and reframes one unary gRPC GetPendingIntent message", async () => {
    const spawnId = "spawn-grpc-1";
    let acceptEncoding: string | undefined;
    const upstream = await listenUpstream((request, response) => {
      const chunks: Buffer[] = [];
      const receivedAcceptEncoding = request.headers["grpc-accept-encoding"];
      acceptEncoding = Array.isArray(receivedAcceptEncoding)
        ? receivedAcceptEncoding.join(",")
        : receivedAcceptEncoding;
      request.on("data", (chunk: Buffer) => chunks.push(chunk));
      request.on("end", () => {
        const received = fromBinary(cpv1.GetPendingIntentRequestSchema, grpcPayload(Buffer.concat(chunks)));
        expect(received.spawnId).toBe(spawnId);
        const message = create(cpv1.GetPendingIntentResponseSchema, {
          ready: true,
          targetNodeId: "node-1",
          targetNodeClass: "cloud",
          targetNodeAccountId: "spawnery-system",
          nodeCertChain: encoder.encode("cert-chain"),
        });
        response.writeHead(200, { "content-type": "application/grpc", trailer: "grpc-status" });
        response.write(grpcEnvelope(toBinary(cpv1.GetPendingIntentResponseSchema, message)));
        response.addTrailers({ "grpc-status": "0" });
        response.end();
      });
    });
    const proxy = await startCPLoopbackProxy({
      upstreamOrigin: upstream.origin,
      transportCAPEM: rootPEM,
      substitution: { field: "targetNodeId", value: "node-substituted" },
    });
    try {
      const request = grpcEnvelope(toBinary(cpv1.GetPendingIntentRequestSchema,
        create(cpv1.GetPendingIntentRequestSchema, { spawnId })));
      const result = await proxyRequest(proxy.origin, "/cp.v1.SpawnService/GetPendingIntent", {
        headers: {
          "content-type": "application/grpc",
          "grpc-accept-encoding": "gzip",
        },
        body: request,
      });
      const rewritten = fromBinary(cpv1.GetPendingIntentResponseSchema, grpcPayload(result.body));
      expect(rewritten.targetNodeId).toBe("node-substituted");
      expect(result.trailers).toEqual({ "grpc-status": "0" });
      expect(acceptEncoding).toBeUndefined();
      expect(proxy.pendingSpawnIds()).toEqual([spawnId]);
      expect(proxy.requestCounts().submitIntent).toBe(0);
    } finally {
      await proxy.close();
      await upstream.close();
    }
  });

  it.each([
    ["compressed", (payload: Uint8Array) => grpcEnvelope(payload, true)],
    ["multi-message", (payload: Uint8Array) => Buffer.concat([grpcEnvelope(payload), grpcEnvelope(payload)])],
  ])("rejects a %s gRPC GetPendingIntent request before upstream", async (_name, body) => {
    let upstreamRequests = 0;
    const upstream = await listenUpstream((_request, response) => {
      upstreamRequests++;
      response.end("{}");
    });
    const proxy = await startCPLoopbackProxy({
      upstreamOrigin: upstream.origin,
      transportCAPEM: rootPEM,
      substitution: { field: "targetNodeId", value: "node-substituted" },
    });
    try {
      const payload = toBinary(cpv1.GetPendingIntentRequestSchema,
        create(cpv1.GetPendingIntentRequestSchema, { spawnId: "spawn-1" }));
      const result = await proxyRequest(proxy.origin, "/cp.v1.SpawnService/GetPendingIntent", {
        headers: { "content-type": "application/grpc" },
        body: body(payload),
      });
      expect(result.status).toBe(400);
      expect(upstreamRequests).toBe(0);
    } finally {
      await proxy.close();
      await upstream.close();
    }
  });

  it("uses process transport trust when no transport CA is provided", async () => {
    const upstream = await listenUpstream((_request, response) => response.end("{}"));
    const proxy = await startCPLoopbackProxy({ upstreamOrigin: upstream.origin });
    try {
      const response = await proxyRequest(proxy.origin, "/cp.v1.SpawnService/ListSpawns");
      expect(response.status).toBe(502);
      expect(response.body.toString("utf8")).toBe("upstream unavailable");
    } finally {
      await proxy.close();
      await upstream.close();
    }
  });

  it("accepts the h2c transport used by spawnctl", async () => {
    const upstream = await listenUpstream((_request, response) => response.end("{}"));
    const proxy = await startCPLoopbackProxy({
      upstreamOrigin: upstream.origin,
      transportCAPEM: rootPEM,
      substitution: { field: "targetNodeId", value: "node-substituted" },
    });
    try {
      const result = await proxyRequest(proxy.origin, "/cp.v1.SpawnService/ListSpawns", {
        headers: { "content-type": "application/proto" },
      });
      expect(result.body.toString("utf8")).toBe("{}");
    } finally {
      await proxy.close();
      await upstream.close();
    }
  });

  it("passes through method, body, authorization, status, and headers", async () => {
    let observed: { method?: string; authorization?: string; body?: string } = {};
    const upstream = await listenUpstream((request, response) => {
      const chunks: Buffer[] = [];
      request.on("data", (chunk: Buffer) => chunks.push(chunk));
      request.on("end", () => {
        observed = {
          method: request.method,
          authorization: request.headers.authorization,
          body: Buffer.concat(chunks).toString("utf8"),
        };
        response.writeHead(201, { "content-type": "application/json", "x-upstream": "kept" });
        response.end('{"spawns":[]}');
      });
    });
    const proxy = await startCPLoopbackProxy({
      upstreamOrigin: upstream.origin,
      transportCAPEM: rootPEM,
      substitution: { field: "targetNodeId", value: "node-substituted" },
    });
    try {
      const response = await proxyRequest(proxy.origin, "/cp.v1.SpawnService/ListSpawns", {
        method: "POST",
        headers: { authorization: "Bearer opaque", "content-type": "application/json" },
        body: '{"filter":"all"}',
      });
      expect(response.status).toBe(201);
      expect(response.headers["x-upstream"]).toBe("kept");
      expect(response.body.toString("utf8")).toBe('{"spawns":[]}');
      expect(observed).toEqual({
        method: "POST",
        authorization: "Bearer opaque",
        body: '{"filter":"all"}',
      });
    } finally {
      await proxy.close();
      await upstream.close();
    }
  });

  it("mutates only ready GetPendingIntent responses and records zero SubmitIntent requests", async () => {
    const original = {
      ready: true,
      pending: { spawnId: "spawn-1", targetNodeId: "node-1" },
      targetNodeId: "node-1",
      targetNodeClass: "cloud",
      targetNodeAccountId: "spawnery-system",
      nodeCertChain: "Y2hhaW4=",
    };
    const upstream = await listenUpstream((_request, response) => {
      response.writeHead(200, { "content-type": "application/json", "x-upstream": "kept" });
      response.end(JSON.stringify(original));
    });
    const proxy = await startCPLoopbackProxy({
      upstreamOrigin: upstream.origin,
      transportCAPEM: rootPEM,
      substitution: { field: "targetNodeId", value: "node-substituted" },
    });
    try {
      const response = await proxyRequest(proxy.origin, "/cp.v1.SpawnService/GetPendingIntent", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: '{"spawnId":"spawn-1"}',
      });
      expect(JSON.parse(response.body.toString("utf8"))).toEqual({ ...original, targetNodeId: "node-substituted" });
      expect(response.headers["x-upstream"]).toBe("kept");
      expect(proxy.requestCounts()).toEqual({
        total: 1,
        getPendingIntent: 1,
        submitIntent: 0,
      });
      expect(proxy.pendingSpawnIds()).toEqual(["spawn-1"]);
    } finally {
      await proxy.close();
      await upstream.close();
    }
  });

  it("can count GetPendingIntent without mutating its response", async () => {
    const original = { ready: true, targetNodeId: "node-1" };
    const upstream = await listenUpstream((_request, response) => {
      response.writeHead(200, { "content-type": "application/json" });
      response.end(JSON.stringify(original));
    });
    const proxy = await startCPLoopbackProxy({ upstreamOrigin: upstream.origin, transportCAPEM: rootPEM });
    try {
      const response = await proxyRequest(proxy.origin, "/cp.v1.SpawnService/GetPendingIntent", {
        headers: { "content-type": "application/json" },
        body: '{"spawnId":"spawn-1"}',
      });
      expect(JSON.parse(response.body.toString("utf8"))).toEqual(original);
      expect(proxy.requestCounts().getPendingIntent).toBe(1);
    } finally {
      await proxy.close();
      await upstream.close();
    }
  });

  it("returns a bounded gateway error when the TLS upstream is unavailable", async () => {
    const proxy = await startCPLoopbackProxy({
      upstreamOrigin: "https://localhost:1",
      transportCAPEM: rootPEM,
      substitution: { field: "targetNodeId", value: "node-substituted" },
    });
    try {
      const response = await proxyRequest(proxy.origin, "/cp.v1.SpawnService/ListSpawns", {
        method: "POST",
        body: "{}",
      });
      expect(response.status).toBe(502);
      expect(response.body.length).toBeLessThan(256);
    } finally {
      await proxy.close();
    }
  });

  it("rejects an oversized client body before contacting the upstream", async () => {
    let upstreamRequests = 0;
    const upstream = await listenUpstream((_request, response) => {
      upstreamRequests++;
      response.end("{}");
    });
    const proxy = await startCPLoopbackProxy({
      upstreamOrigin: upstream.origin,
      transportCAPEM: rootPEM,
      substitution: { field: "targetNodeId", value: "node-substituted" },
      maxBodyBytes: 8,
    });
    try {
      const response = await proxyRequest(proxy.origin, "/cp.v1.SpawnService/GetPendingIntent", {
        method: "POST",
        body: encoder.encode("123456789"),
      });
      expect(response.status).toBe(413);
      expect(upstreamRequests).toBe(0);
      expect(proxy.requestCounts().submitIntent).toBe(0);
    } finally {
      await proxy.close();
      await upstream.close();
    }
  });
});
