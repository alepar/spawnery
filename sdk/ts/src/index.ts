// @spawnery/client — the Spawnery client SDK: Connect transport + A4 intent signing, shared by
// the web SPA and the acceptance harness (and mirrored by the Go SDK internal/client).
//
// This is the T0 skeleton. It re-exports the generated proto types (schemas + message types) under
// namespaces. The signing engine, transport wrapper, AuthProvider/KeyStore interfaces, and the
// SpawnClient are added in T5 (bd sp-lan2.6) — ported from web/src/auth/* using protobuf-es
// toBinary for IntentBody marshalling (validated byte-compatible with Go proto.Marshal in T1).
//
// Generated code lives under ./gen (gitignored; run `make gen` or `npm run gen`).

export * as authv1 from "./gen/auth/v1/auth_pb.js";
export * as cpv1 from "./gen/cp/v1/cp_pb.js";
