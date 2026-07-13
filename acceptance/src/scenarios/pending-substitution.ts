import { fromBinary, toBinary } from "@bufbuild/protobuf";
import { cpv1 } from "@spawnery/client";

export type PendingTargetField =
  | "targetNodeId"
  | "targetNodeClass"
  | "targetNodeAccountId"
  | "nodeCertChain";

export interface PendingTargetSubstitution {
  field: PendingTargetField;
  value: string;
}

export interface PendingIntentWireResponse<Headers> {
  status: number;
  headers: Headers;
  body: Uint8Array;
}

function contentType(headers: unknown): string {
  if (typeof headers !== "object" || headers === null) return "";
  const get = (headers as { get?: unknown }).get;
  if (typeof get === "function") {
    return String(get.call(headers, "content-type") ?? "").toLowerCase();
  }
  for (const [name, value] of Object.entries(headers)) {
    if (name.toLowerCase() === "content-type") {
      return Array.isArray(value) ? String(value[0] ?? "").toLowerCase() : String(value ?? "").toLowerCase();
    }
  }
  return "";
}

function rewriteProtobuf<Headers>(
  response: PendingIntentWireResponse<Headers>,
  substitution: PendingTargetSubstitution,
): PendingIntentWireResponse<Headers> {
  let message: cpv1.GetPendingIntentResponse;
  try {
    message = fromBinary(cpv1.GetPendingIntentResponseSchema, response.body);
  } catch {
    throw new Error("pending substitution: malformed GetPendingIntentResponse protobuf");
  }
  if (!message.ready) return response;
  if (substitution.field === "nodeCertChain") {
    message.nodeCertChain = Buffer.from(substitution.value, "base64");
  } else {
    message[substitution.field] = substitution.value;
  }
  return {
    status: response.status,
    headers: response.headers,
    body: toBinary(cpv1.GetPendingIntentResponseSchema, message),
  };
}

/** Rewrite one resolved-target field in a ready Connect JSON or protobuf response. */
export function rewriteReadyPendingIntentResponse<Headers>(
  response: PendingIntentWireResponse<Headers>,
  substitution: PendingTargetSubstitution,
): PendingIntentWireResponse<Headers> {
  if (contentType(response.headers).includes("proto")) {
    return rewriteProtobuf(response, substitution);
  }

  let decoded: unknown;
  try {
    decoded = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(response.body));
  } catch {
    throw new Error("pending substitution: malformed GetPendingIntentResponse JSON");
  }
  if (typeof decoded !== "object" || decoded === null || Array.isArray(decoded)) {
    throw new Error("pending substitution: malformed GetPendingIntentResponse JSON");
  }

  const message = decoded as Record<string, unknown>;
  if (message.ready !== true) return response;
  return {
    status: response.status,
    headers: response.headers,
    body: new TextEncoder().encode(JSON.stringify({
      ...message,
      [substitution.field]: substitution.value,
    })),
  };
}
