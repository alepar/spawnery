import { unary } from "./connect";
import { openEnvelope, sealEnvelope, type Envelope } from "@/keys/hpke";
import { fromBase64, toBase64 } from "@/keys/encoding";
import { exportDeviceRef, type DeviceKeys } from "@/keys/device";

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

type SecretPlaintext = string | Uint8Array;

export interface SecretRoutingMetadata {
  secretId: string;
  type: string;
  name: string;
  provider?: string;
  targetContainer: string;
  envVarName?: string;
  destPath?: string;
}

export interface CreateSecretValueOptions extends SecretRoutingMetadata {
  accountId: string;
  devicesetEpoch: number;
  plaintext: SecretPlaintext;
  recipientPubkeys: Uint8Array[];
}

export interface UpdateSecretValueOptions {
  accountId: string;
  secretId: string;
  devicesetEpoch: number;
  plaintext: SecretPlaintext;
  recipientPubkeys: Uint8Array[];
}

function decodeEnvelope(envelopeB64: string): Envelope {
  const raw = fromBase64(envelopeB64);
  return JSON.parse(new TextDecoder().decode(raw)) as Envelope;
}

function encodeEnvelope(env: Envelope): string {
  return toBase64(new TextEncoder().encode(JSON.stringify(env)));
}

function plaintextBytes(plaintext: SecretPlaintext): Uint8Array {
  if (typeof plaintext === "string") {
    return new TextEncoder().encode(plaintext);
  }
  return new Uint8Array(plaintext);
}

function requireRecipients(recipientPubkeys: Uint8Array[], operation: string): Uint8Array[] {
  if (recipientPubkeys.length === 0) {
    throw new Error(`${operation} requires at least one recipient public key`);
  }
  return recipientPubkeys.map((pubkey) => new Uint8Array(pubkey));
}

function formatSafeUint64(value: number, field: string): string {
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new Error(`${field} must be a safe non-negative integer`);
  }
  return String(value);
}

function parseCurrentVersion(version: string | undefined, secretId: string): number {
  if (version === undefined || !/^(0|[1-9]\d*)$/.test(version)) {
    throw new Error(`GetSecret returned invalid version for ${secretId}`);
  }
  const parsed = Number(version);
  if (!Number.isSafeInteger(parsed) || parsed < 1 || parsed >= Number.MAX_SAFE_INTEGER) {
    throw new Error(`GetSecret returned unsafe version for ${secretId}`);
  }
  return parsed;
}

function secretWrite(metadata: SecretRoutingMetadata, env: Envelope, devicesetEpoch: number) {
  return {
    secretId: metadata.secretId,
    type: metadata.type,
    name: metadata.name,
    provider: metadata.provider ?? "",
    targetContainer: metadata.targetContainer,
    envVarName: metadata.envVarName ?? "",
    destPath: metadata.destPath ?? "",
    devicesetEpoch: formatSafeUint64(devicesetEpoch, "devicesetEpoch"),
    envelope: encodeEnvelope(env),
  };
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

export async function createSecretValue(opts: CreateSecretValueOptions): Promise<void> {
  const recipients = requireRecipients(opts.recipientPubkeys, "createSecretValue");
  const plaintext = plaintextBytes(opts.plaintext);
  try {
    const env = await sealEnvelope(plaintext, recipients, {
      account_id: opts.accountId,
      secret_id: opts.secretId,
      version: 1,
    });
    await unary<Record<string, never>>("CreateSecret", {
      secret: secretWrite(opts, env, opts.devicesetEpoch),
    });
  } finally {
    plaintext.fill(0);
  }
}

export async function updateSecretValue(opts: UpdateSecretValueOptions): Promise<void> {
  const recipients = requireRecipients(opts.recipientPubkeys, "updateSecretValue");
  const current = await unary<{ secret?: SecretWire }>("GetSecret", { secretId: opts.secretId });
  const currentSecret = current.secret;
  if (!currentSecret) {
    throw new Error(`GetSecret missing secret metadata for ${opts.secretId}`);
  }
  const currentVersion = parseCurrentVersion(currentSecret.version, opts.secretId);
  const plaintext = plaintextBytes(opts.plaintext);
  try {
    const env = await sealEnvelope(plaintext, recipients, {
      account_id: opts.accountId,
      secret_id: opts.secretId,
      version: currentVersion + 1,
    });
    await unary<Record<string, never>>("PutSecret", {
      expectedVersion: String(currentVersion),
      secret: secretWrite({ ...currentSecret, secretId: opts.secretId }, env, opts.devicesetEpoch),
    });
  } finally {
    plaintext.fill(0);
  }
}

export async function openSecretValue(secretId: string, deviceKeys: DeviceKeys): Promise<Uint8Array> {
  const env = await getEnvelope(secretId);
  const deviceRef = await exportDeviceRef(deviceKeys);
  return openEnvelope(env, deviceKeys.x25519Private, deviceRef.x25519Pub);
}

export async function putEnvelope(secretId: string, env: Envelope, devicesetEpoch: number): Promise<void> {
  const current = await unary<{ secret?: SecretWire }>("GetSecret", { secretId });
  if (!current.secret) {
    throw new Error(`GetSecret missing secret metadata for ${secretId}`);
  }
  if (!Number.isSafeInteger(env.at_rest.version)) {
    throw new Error(`PutSecret envelope version must be a safe integer for ${secretId}`);
  }
  const expectedVersion = env.at_rest.version - 1;
  if (!Number.isSafeInteger(expectedVersion) || expectedVersion < 1) {
    throw new Error(`PutSecret expected_version must be a safe integer >= 1 for ${secretId}`);
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
      devicesetEpoch: formatSafeUint64(devicesetEpoch, "devicesetEpoch"),
      envelope: encodeEnvelope(env),
    },
  });
}
