// Pinned trust anchors compiled into the signed SPA build ([WM8], web-epic W1).
//
// These are how the browser later verifies node sub-key chains (W4 owner-sealed
// migration) WITHOUT trusting the CP relay, and how it authenticates AS-signed
// device-set data (W2). They are deliberately build-time constants:
//   - There is NO runtime fetch fallback. The AS's /ca/root endpoint is bootstrap/ops
//     convenience, NOT the trust mechanism (see internal/authsvc/handler.go) — fetching
//     anchors at runtime would let whoever serves the response substitute them.
//   - Real values are an OPS step stamped into the release build (see
//     deploy/web/README.md). The release forbidden-value scan refuses to ship a bundle
//     that still carries the PLACEHOLDER markers below.

function requiredTrustInput(name: string, value: string | undefined): string {
  const configured = value?.trim() ?? "";
  if (!configured || configured.toLowerCase().includes("placeholder")) {
    throw new Error(`${name} is required and must contain release trust material`);
  }
  return configured;
}

/** sp-ova Root CA certificate, PEM, stamped into the immutable SPA bundle. */
export const PINNED_ROOT_CA_PEM = requiredTrustInput("VITE_ROOT_CA_PEM", import.meta.env.VITE_ROOT_CA_PEM);

/** SPIFFE trust domain paired with PINNED_ROOT_CA_PEM. */
export const PINNED_TRUST_DOMAIN = requiredTrustInput("VITE_TRUST_DOMAIN", import.meta.env.VITE_TRUST_DOMAIN);

/** Root-authorized system account used by cloud node SPIFFE identities. */
export const PINNED_CLOUD_ACCOUNT_ID = requiredTrustInput(
  "VITE_CLOUD_ACCOUNT_ID",
  import.meta.env.VITE_CLOUD_ACCOUNT_ID,
);
