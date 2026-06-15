import { unary } from "./connect";
import type { Envelope } from "@/keys/hpke";
import { fromBase64, toBase64 } from "@/keys/encoding";

interface SecretWire {
  secretId: string;
  type: string;
  name: string;
  provider?: string;
  targetContainer: string;
  envVarName?: string;
  destPath?: string;
  version: string;
  devicesetEpoch: string;
  envelope?: string;
}

function decodeEnvelope(envelopeB64: string): Envelope {
  const raw = fromBase64(envelopeB64);
  return JSON.parse(new TextDecoder().decode(raw)) as Envelope;
}

function encodeEnvelope(env: Envelope): string {
  return toBase64(new TextEncoder().encode(JSON.stringify(env)));
}

export async function listSecretIdsForSweep(devicesetEpochBefore: number): Promise<string[]> {
  const r = await unary<{ secrets?: Array<{ secretId?: string }> }>("ListSecrets", {
    devicesetEpochBefore: String(devicesetEpochBefore),
  });
  return (r.secrets ?? []).map((secret) => secret.secretId ?? "").filter((id) => id !== "");
}

export async function getEnvelope(secretId: string): Promise<Envelope> {
  const r = await unary<{ secret?: SecretWire }>("GetSecret", { secretId });
  const secret = r.secret;
  if (!secret?.envelope) {
    throw new Error(`GetSecret missing envelope for ${secretId}`);
  }
  return decodeEnvelope(secret.envelope);
}

export async function putEnvelope(secretId: string, env: Envelope, devicesetEpoch: number): Promise<void> {
  const current = await unary<{ secret?: SecretWire }>("GetSecret", { secretId });
  if (!current.secret) {
    throw new Error(`GetSecret missing secret metadata for ${secretId}`);
  }
  const expectedVersion = env.at_rest.version - 1;
  if (expectedVersion < 1) {
    throw new Error(`PutSecret expected_version must be >= 1 for ${secretId}`);
  }
  await unary<Record<string, never>>("PutSecret", {
    expectedVersion: String(expectedVersion),
    secret: {
      secretId,
      type: current.secret.type,
      name: current.secret.name,
      provider: current.secret.provider ?? "",
      targetContainer: current.secret.targetContainer,
      envVarName: current.secret.envVarName ?? "",
      destPath: current.secret.destPath ?? "",
      devicesetEpoch: String(devicesetEpoch),
      envelope: encodeEnvelope(env),
    },
  });
}
