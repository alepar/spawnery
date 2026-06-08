// Typed surface for the Windows-Terminal -> xterm ITheme mapper.
//
// The actual mapping lives in `scripts/wt.mjs` (plain ESM) so the codegen can run under bare
// `node` with no build step. This module re-exports it with full TS types (see `scripts/wt.d.mts`)
// so app code and vitest get a single, type-checked, unit-tested implementation.
export { wtToITheme } from "../../scripts/wt.mjs";
export type { WtScheme } from "../../scripts/wt.mjs";
