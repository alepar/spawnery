/**
 * Phase 5 — user secrets: CRUD, oracle-only.
 *
 * CLI parity gap (product debt, sp-tq0t): `spawnctl` has NO `secret` subcommand — secret CRUD is
 * driven only by the CP RPCs (CreateSecret/GetSecret/ListSecrets/DeleteSecret), i.e. the apiDriver
 * oracle here, not a cli-primary surface. A `spawnctl secret` CRUD verb set is a documented
 * follow-up.
 *
 * Secret *injection* into the pod is explicitly OUT OF SCOPE for this suite: profile assembly
 * marks sensitive/secret artifacts as deferred (sp-nrzf.4, internal/cp/profiles_assembly.go), and
 * unsealing requires a device-set the acceptance harness does not enroll in dev-token mode. This
 * scenario covers secret CRUD (and, in profiles.spec.ts, profile<->secret attach/detach) only —
 * never that a secret's value actually lands in a spawn. injection.spec.ts instead observes
 * profile-attached materialization via a non-secret (skill) entry.
 *
 * Secrets aren't covered by the spawn teardown sweeper (fixtures/sweep.ts only sweeps spawns) —
 * cleaned up explicitly here.
 */

import { ConnectError, Code } from "@spawnery/client";
import { test, expect } from "../../src/harness/test";
import { dummyAtRestEnvelope } from "../../src/drivers/customization";

test("secrets: create -> list -> get -> delete (oracle-only)", { tag: "@mutating" }, async ({ identity, auth, api, ns }) => {
  const secretId = ns("sec-crud");
  const accountId = await auth.accountId(identity);
  let created = false;

  try {
    const write = {
      secretId,
      type: "USER_SECRET_TYPE_GENERIC_KV",
      name: secretId,
      targetContainer: "ARTIFACT_TARGET_AGENT",
      envVarName: `ACC_SECRET_${secretId.replace(/[^A-Za-z0-9]/g, "_").toUpperCase()}`,
      devicesetEpoch: 0,
      envelope: dummyAtRestEnvelope(accountId, secretId),
    };

    const detail = await api.createSecret(write);
    created = true;
    expect(detail.secretId).toBe(secretId);
    expect(detail.type).toBe("USER_SECRET_TYPE_GENERIC_KV");
    expect(detail.targetContainer).toBe("ARTIFACT_TARGET_AGENT");
    expect(detail.envVarName).toBe(write.envVarName);
    expect(detail.version).toBe(1);

    const list = await api.listSecrets();
    expect(list.some((s) => s.secretId === secretId)).toBe(true);

    const got = await api.getSecret(secretId);
    expect(got.secretId).toBe(secretId);
    expect(got.name).toBe(secretId);
    expect(got.envVarName).toBe(write.envVarName);

    await api.deleteSecret(secretId);
    created = false;

    const listAfterDelete = await api.listSecrets();
    expect(listAfterDelete.some((s) => s.secretId === secretId)).toBe(false);
  } finally {
    if (created) await api.deleteSecret(secretId).catch(() => {});
  }
});

test("secrets: createSecret rejects an envelope whose at_rest AAD owner doesn't match the caller", { tag: "@mutating" }, async ({ api, ns }) => {
  const secretId = ns("sec-aad-mismatch");
  const write = {
    secretId,
    type: "USER_SECRET_TYPE_GENERIC_KV",
    name: secretId,
    targetContainer: "ARTIFACT_TARGET_AGENT",
    envVarName: "ACC_SECRET_AAD_MISMATCH",
    devicesetEpoch: 0,
    // Wrong owner in the AAD — validateEnvelopeAAD (internal/cp/secrets_catalog.go) must reject
    // this regardless of who's actually authenticated, proving the AAD check is real and not a
    // no-op at CRUD time.
    envelope: dummyAtRestEnvelope("acc-not-the-real-owner", secretId),
  };

  let err: unknown;
  try {
    await api.createSecret(write);
  } catch (e) {
    err = e;
  }
  expect(err).toBeInstanceOf(ConnectError);
  expect((err as ConnectError).code).toBe(Code.InvalidArgument);

  // Best-effort: if the CP somehow accepted it, don't leak the row into the next run.
  await api.deleteSecret(secretId).catch(() => {});
});
