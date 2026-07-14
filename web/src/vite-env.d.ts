/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** CP API origin baked in at build time (e.g. "https://cp.spawnery.dev"). Empty/unset in dev:
   * same-origin relative URLs through the vite proxy. */
  readonly VITE_CP_ORIGIN?: string;
  /** AS API origin baked in at build time. Empty/unset in dev. */
  readonly VITE_AS_ORIGIN?: string;
  /** Bearer token sent in Authorization headers. Set to "dev-token" in .env.development;
   * set from GitHub secrets in release builds so the literal "dev-token" never appears
   * in a signed production bundle (pre-sign scan rejects it). */
  readonly VITE_AUTH_TOKEN?: string;
  /** Set to "1" to enable OAuth-backed authentication in development. Production is always enabled. */
  readonly VITE_AUTH_ENABLED?: string;
  /** Root CA certificate PEM stamped into auth-enabled builds. */
  readonly VITE_ROOT_CA_PEM?: string;
  /** SPIFFE trust domain covered by the stamped root CA. */
  readonly VITE_TRUST_DOMAIN?: string;
  /** Root-authorized system account used by cloud node identities. */
  readonly VITE_CLOUD_ACCOUNT_ID?: string;
  /** JSON trust stamp containing the cloud and self-hosted node issuer certificates and CRLs. */
  readonly VITE_NODE_CRL_BUNDLE_JSON?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
