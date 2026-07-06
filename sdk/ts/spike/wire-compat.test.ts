// T1 wire-compat spike: prove protobuf-es `toBinary(IntentBody)` is byte-identical to Go
// `proto.Marshal(IntentBody)`. Goldens are Go-produced (sdk/ts/spike/genvec/main.go); the
// no-mounts case also matches the committed internal/intent/testdata/intent_vectors.json.
// If these pass, the "generate TS from proto" decision holds and we can drop the hand-rolled
// ProtoWriter for signing.
import { test } from "node:test";
import assert from "node:assert/strict";
import { create, toBinary } from "@bufbuild/protobuf";
import { IntentBodySchema } from "../src/gen/auth/v1/auth_pb.js";

const toHex = (u: Uint8Array): string => Buffer.from(u).toString("hex");

const GOLDEN_NO_MOUNTS =
  "0a1566697865642d6a74692d666f722d766563746f727310809d80cc061a0a73702d7665632d30303120012a0a6e6f64652d7665632d31320c6372656174652d737061776e3a186170702f74657374407368613235363a6465616462656566421c72656769737472792f696d67407368613235363a63616665626162654a0b636c617564652d74657374";
const GOLDEN_WITH_MOUNTS =
  "0a0a6d6f756e74732d76656310819d80cc061a0a73702d7665632d3030322007320c6372656174652d737061776e3a0e6170702f6d407368613235363a3162180a0773637261746368120b736372617463683a2f2f732001621c0a026768120867683a2f2f6f2f721a057365632d312a053132333435";

test("protobuf-es toBinary == Go proto.Marshal — no mounts (committed vector)", () => {
  const body = create(IntentBodySchema, {
    jti: "fixed-jti-for-vectors",
    issuedAt: 1770000000n,
    spawnId: "sp-vec-001",
    generation: 1n,
    targetNodeId: "node-vec-1",
    op: "create-spawn",
    appRef: "app/test@sha256:deadbeef",
    image: "registry/img@sha256:cafebabe",
    model: "claude-test",
  });
  assert.equal(toHex(toBinary(IntentBodySchema, body)), GOLDEN_NO_MOUNTS);
});

test("protobuf-es toBinary == Go proto.Marshal — with mounts (repeated MountRef)", () => {
  const body = create(IntentBodySchema, {
    jti: "mounts-vec",
    issuedAt: 1770000001n,
    spawnId: "sp-vec-002",
    generation: 7n,
    op: "create-spawn",
    appRef: "app/m@sha256:1",
    mounts: [
      { name: "scratch", backendUri: "scratch://s", createIfMissing: true },
      { name: "gh", backendUri: "gh://o/r", credentialSecretId: "sec-1", repositoryId: "12345" },
    ],
  });
  assert.equal(toHex(toBinary(IntentBodySchema, body)), GOLDEN_WITH_MOUNTS);
});
