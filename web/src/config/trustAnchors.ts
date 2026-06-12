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

/** sp-ova Root CA certificate, PEM. PLACEHOLDER — replaced at release build time. */
export const PINNED_ROOT_CA_PEM: string = `-----BEGIN CERTIFICATE-----
PLACEHOLDER-TRUST-ANCHOR-ROOT-CA
-----END CERTIFICATE-----`;

/** AS session/device-set signing pubkeys, base64url raw. PLACEHOLDER — replaced at release build time. */
export const AS_PUBKEYS: string[] = ["PLACEHOLDER-TRUST-ANCHOR-AS-PUBKEY"];
