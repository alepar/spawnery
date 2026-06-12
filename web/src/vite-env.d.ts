/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** CP API origin baked in at build time (e.g. "https://cp.spawnery.dev"). Empty/unset in dev:
   * same-origin relative URLs through the vite proxy. */
  readonly VITE_CP_ORIGIN?: string;
  /** AS API origin baked in at build time. Empty/unset in dev. */
  readonly VITE_AS_ORIGIN?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
