import {
  createServer as createHTTP2Server,
  type Http2ServerRequest,
  type Http2ServerResponse,
} from "node:http2";
import { request as httpsRequest, type RequestOptions } from "node:https";
import type { ClientRequest, IncomingHttpHeaders, OutgoingHttpHeaders } from "node:http";
import type { Socket } from "node:net";
import type { Readable } from "node:stream";
import { fromBinary } from "@bufbuild/protobuf";
import { cpv1 } from "@spawnery/client";

import {
  rewriteReadyPendingIntentResponse,
  type PendingTargetSubstitution,
} from "./pending-substitution";

const GET_PENDING_INTENT = "/cp.v1.SpawnService/GetPendingIntent";
const SUBMIT_INTENT = "/cp.v1.SpawnService/SubmitIntent";
const DEFAULT_MAX_BODY_BYTES = 4 * 1024 * 1024;

class BodyTooLargeError extends Error {}

export interface CPLoopbackProxyOptions {
  upstreamOrigin: string;
  rootCAPEM: string;
  substitution?: PendingTargetSubstitution;
  maxBodyBytes?: number;
}

export interface CPRequestCounts {
  total: number;
  getPendingIntent: number;
  submitIntent: number;
}

export interface CPLoopbackProxy {
  origin: string;
  requestCounts(): CPRequestCounts;
  pendingSpawnIds(): string[];
  close(): Promise<void>;
}

function pendingSpawnId(body: Uint8Array, contentType: string | undefined): string | undefined {
  try {
    if (contentType?.toLowerCase().includes("proto")) {
      return fromBinary(cpv1.GetPendingIntentRequestSchema, body).spawnId || undefined;
    }
    const decoded = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(body)) as unknown;
    if (typeof decoded !== "object" || decoded === null || Array.isArray(decoded)) return undefined;
    const spawnId = (decoded as Record<string, unknown>).spawnId;
    return typeof spawnId === "string" && spawnId !== "" ? spawnId : undefined;
  } catch {
    return undefined;
  }
}

function readBounded(stream: Readable, maxBodyBytes: number): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let length = 0;
    stream.on("data", (chunk: Buffer) => {
      length += chunk.length;
      if (length > maxBodyBytes) {
        stream.removeAllListeners("data");
        stream.resume();
        reject(new BodyTooLargeError(`body exceeds ${maxBodyBytes} bytes`));
        return;
      }
      chunks.push(chunk);
    });
    stream.once("end", () => resolve(Buffer.concat(chunks, length)));
    stream.once("error", reject);
  });
}

function responseHeaders(headers: IncomingHttpHeaders, mutated: boolean): OutgoingHttpHeaders {
  const forwarded: OutgoingHttpHeaders = { ...headers };
  delete forwarded.connection;
  delete forwarded["transfer-encoding"];
  if (mutated) delete forwarded["content-length"];
  return forwarded;
}

function sendError(response: Http2ServerResponse, status: number, message: string): void {
  if (response.headersSent || response.destroyed) return;
  response.writeHead(status, { "content-type": "text/plain; charset=utf-8" });
  response.end(message);
}

/** Start an HTTP loopback proxy that verifies the real CP's TLS certificate with a public root. */
export async function startCPLoopbackProxy(
  options: CPLoopbackProxyOptions,
): Promise<CPLoopbackProxy> {
  const upstream = new URL(options.upstreamOrigin);
  if (upstream.protocol !== "https:") throw new Error("CP loopback proxy requires an HTTPS upstream");
  if (!options.rootCAPEM.trim()) throw new Error("CP loopback proxy requires a public root CA");
  const maxBodyBytes = options.maxBodyBytes ?? DEFAULT_MAX_BODY_BYTES;
  if (!Number.isSafeInteger(maxBodyBytes) || maxBodyBytes <= 0) {
    throw new Error("CP loopback proxy maxBodyBytes must be positive");
  }

  const counts: CPRequestCounts = { total: 0, getPendingIntent: 0, submitIntent: 0 };
  const observedPendingSpawnIds = new Set<string>();
  const sockets = new Set<Socket>();
  const upstreamRequests = new Set<ClientRequest>();

  const handleRequest = async (request: Http2ServerRequest, response: Http2ServerResponse) => {
    counts.total++;
    const pathname = new URL(request.url ?? "/", "http://127.0.0.1").pathname;
    if (pathname === GET_PENDING_INTENT) counts.getPendingIntent++;
    if (pathname === SUBMIT_INTENT) counts.submitIntent++;

    let requestBody: Buffer;
    try {
      requestBody = await readBounded(request, maxBodyBytes);
    } catch (error) {
      if (error instanceof BodyTooLargeError) sendError(response, 413, "request body too large");
      else sendError(response, 400, "invalid request body");
      return;
    }
    if (pathname === GET_PENDING_INTENT) {
      const spawnId = pendingSpawnId(requestBody, request.headers["content-type"]);
      if (spawnId) observedPendingSpawnIds.add(spawnId);
    }

    const target = new URL(request.url ?? "/", upstream);
    const headers: OutgoingHttpHeaders = { ...request.headers, host: upstream.host };
    delete headers.connection;
    for (const name of Object.keys(headers)) {
      if (name.startsWith(":")) delete headers[name];
    }
    const requestOptions: RequestOptions = {
      protocol: "https:",
      hostname: upstream.hostname,
      port: upstream.port,
      method: request.method,
      path: `${target.pathname}${target.search}`,
      headers,
      ca: options.rootCAPEM,
      rejectUnauthorized: true,
      servername: upstream.hostname,
      agent: false,
    };

    await new Promise<void>((resolve) => {
      const forwarded = httpsRequest(requestOptions, async (upstreamResponse) => {
        try {
          const body = await readBounded(upstreamResponse, maxBodyBytes);
          const status = upstreamResponse.statusCode ?? 502;
          const shouldMutate = pathname === GET_PENDING_INTENT && options.substitution !== undefined;
          const wire = shouldMutate
            ? rewriteReadyPendingIntentResponse({
              status,
              headers: upstreamResponse.headers,
              body,
            }, options.substitution!)
            : { status, headers: upstreamResponse.headers, body };
          response.writeHead(wire.status, responseHeaders(wire.headers, shouldMutate));
          response.end(wire.body);
        } catch (error) {
          if (error instanceof BodyTooLargeError) sendError(response, 502, "upstream response body too large");
          else sendError(response, 502, "invalid upstream response");
        } finally {
          resolve();
        }
      });
      upstreamRequests.add(forwarded);
      forwarded.once("close", () => upstreamRequests.delete(forwarded));
      forwarded.once("error", () => {
        sendError(response, 502, "upstream unavailable");
        resolve();
      });
      forwarded.setTimeout(30_000, () => forwarded.destroy(new Error("upstream timeout")));
      forwarded.end(requestBody);
    });
  };
  const server = createHTTP2Server(handleRequest);
  server.on("connection", (socket) => {
    sockets.add(socket);
    socket.once("close", () => sockets.delete(socket));
  });

  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => resolve());
  });
  const address = server.address();
  if (!address || typeof address === "string") {
    server.close();
    throw new Error("CP loopback proxy failed to bind TCP");
  }

  let closePromise: Promise<void> | undefined;
  return {
    origin: `http://127.0.0.1:${address.port}`,
    requestCounts: () => ({ ...counts }),
    pendingSpawnIds: () => [...observedPendingSpawnIds],
    close: () => {
      closePromise ??= new Promise<void>((resolve, reject) => {
        for (const request of upstreamRequests) request.destroy();
        for (const socket of sockets) socket.destroy();
        server.close((error) => error ? reject(error) : resolve());
      });
      return closePromise;
    },
  };
}
