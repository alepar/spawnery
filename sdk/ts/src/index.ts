// @spawnery/client — the Spawnery client SDK: Connect transport + A4 intent signing, shared by
// the web SPA and the acceptance harness (and mirrored by the Go SDK internal/client).
//
// Environment-neutral: only crypto.subtle, fetch, TextEncoder, btoa/atob (browser & Node >=20).
// Generated code lives under ./gen (gitignored; run `make gen` or `npm run gen`).

export * as authv1 from "./gen/auth/v1/auth_pb.js";
export * as cpv1 from "./gen/cp/v1/cp_pb.js";

export { ConnectError, Code } from "@connectrpc/connect";

export { SpawnClient, type SpawnClientOptions, type ForkOptions } from "./client.js";
export { createTransport, type AuthProvider, type TransportOptions } from "./transport.js";
export type { KeyStore, SessionKeyPair } from "./keystore.js";
export { WebCryptoSessionSigner, type SessionSigner } from "./signing/sessionSigner.js";

export {
  domainForOp,
  buildIntentBodyBytes,
  buildSignedIntent,
  buildSessionOpenSignedIntentB64,
  pollAndSign,
  registerPendedOp,
  clearPendedOp,
  type IntentFields,
  type PendedOp,
  type PollAndSignDeps,
  type ResolvedTarget,
  type ResolvedTargetVerifier,
  DOMAIN_CREATE_SPAWN,
  DOMAIN_RESUME_SPAWN,
  DOMAIN_RECREATE_SPAWN,
  DOMAIN_MIGRATE_SPAWN,
  DOMAIN_FORK_SPAWN,
  DOMAIN_SESSION_OPEN,
} from "./signing/intent.js";

export { buildPoP, POP_DOMAIN, type PoPHeaders } from "./signing/pop.js";

export { DEV_TOKEN_OVERRIDE_KEY } from "./dev.js";

export { signP1363, exportSpkiDer, sessionKeyHash, keyCanSign } from "./keys/crypto.js";
export { derToP1363, p1363ToDer } from "./keys/der.js";
export { hkdfExpand } from "./keys/hkdf.js";
export {
  encodeFields,
  sha256,
  toBase64,
  fromBase64,
  toBase64Url,
  fromBase64Url,
  bytesEqual,
  parseBigIntNanos,
} from "./keys/encoding.js";
