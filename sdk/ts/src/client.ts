/**
 * SpawnClient — the SDK's method surface over cp.v1.SpawnService.
 *
 * Signing is gated on a KeyStore being supplied at construction (the SDK analogue of web's
 * authEnabled()): no KeyStore => skip signing (dev/unenforced CP). When a KeyStore is present,
 * lifecycle ops that block at the CP on the client's SignedIntent (resume/recreate/migrate/fork)
 * MUST kick off pollAndSign concurrently with (never awaited before) the blocking RPC — awaiting
 * first deadlocks: the CP waits for the intent, the client waits for the RPC. createSpawn is the
 * one exception (async at the CP), so it signs after the RPC resolves.
 */

import { createClient, type Client, type Transport } from "@connectrpc/connect";
import type { MessageInitShape } from "@bufbuild/protobuf";
import {
  SpawnService,
  CreateSpawnRequestSchema,
  ProfileEntrySchema,
  SecretWriteSchema,
  SealedSecretSchema,
} from "./gen/cp/v1/cp_pb.js";
import type { KeyStore } from "./keystore.js";
import { WebCryptoSessionSigner } from "./signing/sessionSigner.js";
import {
  pollAndSign,
  registerPendedOp,
  clearPendedOp,
  buildSessionOpenSignedIntentB64,
  type PendedOp,
  type ResolvedTargetVerifier,
} from "./signing/intent.js";

export type SpawnClientOptions = {
  transport: Transport;
  keyStore?: undefined;
  getNodeAccessToken?: undefined;
  verifyTarget?: undefined;
} | {
  transport: Transport;
  /** Load-only persistent key storage. SpawnClient never creates a replacement key. */
  keyStore: KeyStore;
  /** Explicit node-audience credential provider, separate from the CP transport bearer. */
  getNodeAccessToken: () => Promise<string>;
  verifyTarget: ResolvedTargetVerifier;
};

export interface ForkOptions {
  targetNodeId?: string;
  targetClass?: string;
  name?: string;
}

export class SpawnClient {
  private readonly client: Client<typeof SpawnService>;
  private readonly keyStore?: KeyStore;
  private readonly getNodeAccessToken?: () => Promise<string>;
  private readonly verifyTarget?: ResolvedTargetVerifier;

  constructor(opts: SpawnClientOptions) {
    this.client = createClient(SpawnService, opts.transport);
    this.keyStore = opts.keyStore;
    this.getNodeAccessToken = opts.getNodeAccessToken;
    this.verifyTarget = opts.verifyTarget;
  }

  /**
   * signConcurrently fires-and-forgets pollAndSign for a pended op. Callers MUST NOT await this
   * before issuing the corresponding blocking RPC (see class doc). No-op when no KeyStore was
   * supplied.
   */
  private signConcurrently(spawnId: string, pended: PendedOp): void {
    if (!this.keyStore) return;
    void Promise.all([this.keyStore.get(), this.getNodeAccessToken!()])
      .then(([kp, nodeAccessToken]) => {
        if (!kp) throw new Error("SpawnClient: no session keypair in KeyStore; create one first");
        if (!nodeAccessToken) throw new Error("SpawnClient: missing node access token");
        registerPendedOp(pended);
        return pollAndSign({
          client: this.client,
          spawnId,
          pended,
          signer: new WebCryptoSessionSigner(kp.privateKey, kp.publicKey),
          nodeAccessToken,
          verifyTarget: this.verifyTarget!,
        });
      })
      .catch((e: unknown) => console.error("intent sign failed:", e))
      .finally(() => clearPendedOp(spawnId));
  }

  async createSpawn(input: MessageInitShape<typeof CreateSpawnRequestSchema>): Promise<string> {
    const { spawnId } = await this.client.createSpawn(input);
    // appRef is intentionally omitted from pended: the caller picks an app *id*, not the immutable
    // app_ref the CP resolves it to, so validating against a ref we never supplied would always
    // mismatch. pollAndSign's tuple validation skips the appRef check when it's undefined.
    this.signConcurrently(spawnId, {
      op: "create-spawn",
      spawnId,
      model: input.model,
      image: input.image,
      mounts: input.mounts,
      attachedSecretIds: input.attachedSecretIds,
    });
    return spawnId;
  }

  async resume(spawnId: string, attachedSecretIds: string[] = []): Promise<void> {
    this.signConcurrently(spawnId, { op: "resume-spawn", spawnId, attachedSecretIds });
    await this.client.resumeSpawn({ spawnId, attachedSecretIds });
  }

  async recreate(spawnId: string): Promise<void> {
    this.signConcurrently(spawnId, { op: "recreate-spawn", spawnId });
    await this.client.recreateSpawn({ spawnId });
  }

  async migrate(spawnId: string): Promise<void> {
    this.signConcurrently(spawnId, { op: "migrate-spawn", spawnId });
    await this.client.migrateSpawn({ spawnId });
  }

  /**
   * fork signs + calls ForkSpawn only. Owner-sealed journal-key delivery (HPKE/device keys) is an
   * app-side concern layered on top (mirrors web's fork.ts, out of scope here).
   */
  async fork(sourceSpawnId: string, opts: ForkOptions = {}): Promise<{
    forkSpawnId: string;
    transferSetId: string;
    nodeId: string;
  }> {
    this.signConcurrently(sourceSpawnId, {
      op: "fork-spawn",
      spawnId: sourceSpawnId,
      targetNodeId: opts.targetNodeId || undefined,
    });
    const res = await this.client.forkSpawn({
      spawnId: sourceSpawnId,
      targetNodeId: opts.targetNodeId ?? "",
      targetClass: opts.targetClass ?? "",
      name: opts.name ?? "",
    });
    return { forkSpawnId: res.forkSpawnId, transferSetId: res.transferSetId, nodeId: res.nodeId };
  }

  async list() {
    return (await this.client.listSpawns({})).spawns;
  }

  async findSpawn(spawnId: string) {
    return (await this.list()).find((s) => s.spawnId === spawnId);
  }

  async deleteSpawn(spawnId: string, destroyData = false): Promise<void> {
    await this.client.deleteSpawn({ spawnId, destroyData });
  }

  async stopSpawn(spawnId: string): Promise<void> {
    await this.client.stopSpawn({ spawnId });
  }

  async listApps(query = "") {
    return this.client.listApps({ query });
  }

  // ── Customization: profiles ────────────────────────────────────────────────

  async listProfiles() {
    return (await this.client.listProfiles({})).profiles;
  }
  async createProfile(name: string) {
    return this.client.createProfile({ name });
  }
  async getProfile(profileId: string) {
    return (await this.client.getProfile({ profileId })).profile;
  }
  async updateProfile(profileId: string, expectedVersion: bigint, name: string) {
    return this.client.updateProfile({ profileId, expectedVersion, name });
  }
  async deleteProfile(profileId: string) {
    return this.client.deleteProfile({ profileId });
  }
  async addProfileEntry(profileId: string, expectedVersion: bigint, entry: MessageInitShape<typeof ProfileEntrySchema>) {
    return this.client.addProfileEntry({ profileId, expectedVersion, entry });
  }
  async removeProfileEntry(profileId: string, expectedVersion: bigint, entryId: string) {
    return this.client.removeProfileEntry({ profileId, expectedVersion, entryId });
  }

  // ── Customization: catalog ─────────────────────────────────────────────────

  async listCatalogEntries() {
    return (await this.client.listCatalogEntries({})).entries;
  }
  async getCatalogEntry(catalogId: string) {
    return (await this.client.getCatalogEntry({ catalogId })).entry;
  }
  async ingestSkillFromURL(input: { url: string; ref?: string; subdir?: string; name?: string; description?: string }) {
    return this.client.ingestSkillFromURL({
      url: input.url,
      ref: input.ref ?? "",
      subdir: input.subdir ?? "",
      name: input.name ?? "",
      description: input.description ?? "",
    });
  }

  // ── Secrets (sole secrets surface; raw CRUD only — HPKE owner-sealing is app-side) ─────────────

  async listSecrets(devicesetEpochBefore = 0n) {
    return (await this.client.listSecrets({ devicesetEpochBefore })).secrets;
  }
  async getSecret(secretId: string) {
    return (await this.client.getSecret({ secretId })).secret;
  }
  async createSecret(secret: MessageInitShape<typeof SecretWriteSchema>) {
    return (await this.client.createSecret({ secret })).secret;
  }
  async deleteSecret(secretId: string) {
    return this.client.deleteSecret({ secretId });
  }
  async deliverSecrets(spawnId: string, secrets: MessageInitShape<typeof SealedSecretSchema>[]) {
    return this.client.deliverSecrets({ spawnId, secrets });
  }
  async putSecret(secret: MessageInitShape<typeof SecretWriteSchema>, expectedVersion: bigint) {
    return (await this.client.putSecret({ secret, expectedVersion })).secret;
  }

  /** Delegate to signing/intent.ts — see there for the session-open intent's WS bind-frame shape. */
  buildSessionOpenSignedIntentB64 = buildSessionOpenSignedIntentB64;
}
