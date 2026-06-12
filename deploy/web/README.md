# Spawnery SPA Deployment Ops Checklist (W1, sp-2ckv.6)

This document covers the host requirements, signing identity values, CSP decision record,
and release process for the Spawnery SPA.

## Host Requirements

**Cloudflare Pages, Netlify, or equivalent class** — the host MUST support custom response
headers. GitHub Pages is explicitly excluded (no custom header support).

Minimum requirements:
- Custom `Content-Security-Policy` header on `index.html`
- Custom `Cache-Control` headers per path (`/index.html` vs `/assets/*`)
- `X-Content-Type-Options: nosniff` global
- No automatic inline HTML injection (some CDN optimizers break SRI)

The `dist/_headers` file (emitted by the SRI plugin, shipped inside the signed dist/) is
the authoritative source for CSP and cache headers. Map it to your host's format:

| Host | How to apply `_headers` |
|------|------------------------|
| Cloudflare Pages | `_headers` file in root of publish dir is read natively. |
| Netlify | Rename to `_headers` in publish dir (same format, natively supported). |
| AWS S3 + CloudFront | Convert to CloudFront response headers policy or Lambda@Edge. |

## Cosign Identity and Issuer Values

The `deploy/web/verify.sh` script and the deploy job in `web-release.yml` pin these EXACT
values — no regexp flags ([WM21]). Update when the repository path changes:

```
CERT_IDENTITY=https://github.com/gastownhall/spawnery/.github/workflows/web-release.yml@refs/heads/master
OIDC_ISSUER=https://token.actions.githubusercontent.com
```

To run local verification:
```bash
COSIGN_CERT_IDENTITY="<above>" COSIGN_OIDC_ISSUER="<above>" \
  deploy/web/verify.sh dist.tar.gz dist.tar.gz.bundle
```

## CSP Decision Record — xterm.js `style-src 'unsafe-inline'`

The CSP emitted by `web/build/sri-headers-plugin.ts` includes `'unsafe-inline'` in `style-src`.
This is **DELIBERATE and DOCUMENTED** ([WM19], not a silent relaxation).

**Root cause (two offenders):**

1. **xterm.js DOM renderer**: Creates `<style>` elements and sets `.textContent` at runtime
   for theme styles and dimension styles (confirmed in
   `node_modules/@xterm/xterm/lib/xterm.js`). Because the content is runtime-dynamic (theme
   colors change, dimensions depend on container size), it cannot be hashed at build time.
   A nonce would require server-side rendering or a per-page-load nonce injection mechanism,
   neither of which is compatible with a static SPA deployment.

   **Preferred fix (deferred)**: Switch from the xterm DOM renderer to the WebGL renderer
   (`@xterm/addon-webgl`) or Canvas renderer (`@xterm/addon-canvas`). These render into a
   canvas element and do NOT inject `<style>` nodes. This requires installing the addon,
   verifying feature parity (the DOM renderer is the default), and testing the terminal
   under enforced CSP. When this migration is complete, remove `'unsafe-inline'` from
   `style-src` and this decision record.

   Tracking: file a bead when implementing the renderer swap.

2. **sonner (toast library v2.0.7)**: Calls `__insertCSS(fullCssString)` at module load
   time, injecting a large `<style>` block. Importing `sonner/dist/styles.css` bundles the
   static styles as a hashed stylesheet link, but the JS module still calls `__insertCSS`
   regardless.

   **Preferred fix (deferred)**: Patch or replace sonner with a library that does not
   inject styles at runtime, OR submit an upstream PR to add an `injectStyle: false` option.

**Security impact**: `'unsafe-inline'` for `style-src` allows an attacker who can inject
arbitrary HTML to set inline styles (e.g. for CSS-based data exfiltration via attribute
selectors). It does NOT allow inline `<script>` execution or eval. The `script-src 'self'`
and `default-src 'none'` directives remain strict. This is a known, acceptable tradeoff
documented here.

**Detection**: The `web/playwright.csp.config.ts` Playwright suite exercises fonts, toasts,
and terminal rendering under the enforced CSP. If a dependency is added that introduces
`unsafe-eval` or a second injection vector, this CI gate will catch it before production.

## Trust Anchor Population (Root CA PEM + AS Pubkeys)

The bundle includes placeholder trust anchors in `web/src/config/trustAnchors.ts`:

```typescript
export const PINNED_ROOT_CA_PEM: string = `-----BEGIN CERTIFICATE-----
PLACEHOLDER-TRUST-ANCHOR-ROOT-CA
-----END CERTIFICATE-----`;

export const AS_PUBKEYS: string[] = ["PLACEHOLDER-TRUST-ANCHOR-AS-PUBKEY"];
```

**Before the first production release:**
1. Obtain the sp-ova Root CA certificate PEM from the node-auth PKI setup.
2. Obtain the AS session/device-set signing pubkey(s) in base64url raw format.
3. Replace the PLACEHOLDER values in `web/src/config/trustAnchors.ts`.
4. The pre-sign forbidden-value scan in `web-release.yml` will FAIL if PLACEHOLDER
   markers survive into the signed dist/, preventing accidental shipment.

## Public DNS Flip Sequencing Gate [WM17]

**The public DNS flip is gated on minimal per-account auth (invite-token scheme).**

W1–W4 deploy to a private/staging origin on the current dev-token mechanism. The canonical
public origin (e.g. `app.spawnery.dev`) MUST NOT be pointed at the SPA until:

1. A per-account token issuance mechanism is in place (at minimum, invite tokens).
2. The AS has defined what identity it keys device-set logs on.
3. The CP/AS are NOT accessible via shared dev-token credentials.

Until then, the deploy step in `web-release.yml` is a STUB that echoes the intended
action without executing it. The `deploy` GitHub Environment gates actual deployment
credentials.

See `docs/superpowers/specs/2026-06-11-web-epic-spa-delivery-device-keys-migration-design.md`
§1 for the full sequencing gate rationale.

## Anti-Rollback [WL2]

`deploy/web/release-counter.txt` holds the last released numeric version. The deploy job
reads this file and refuses to deploy a release whose version is <= the prior counter.

After a successful deploy:
1. Update `deploy/web/release-counter.txt` with the new version number.
2. Commit and push to master.

Host-dashboard rollbacks are DISABLED: `index.html` ships `Cache-Control: no-cache` so
every page load fetches the current version. Hashed assets ship `immutable` (safe because
their names change with content). Disabling preview deployments on the host prevents
accidentally serving a stale or unverified bundle.

## Release Process Summary

1. Ensure trust anchors are populated (not PLACEHOLDER).
2. Create a git tag: `web/vN` (where N > the previous release counter).
3. Push the tag — `web-release.yml` fires automatically.
4. Monitor the pipeline: build → scan → sign → self-test → deploy stub.
5. When the deploy stub is replaced with real hosting:
   a. Verify the `_headers` file is being served by the host.
   b. Verify the CSP with browser DevTools (no blocked resources).
   c. Update `deploy/web/release-counter.txt` and push.
6. DNS flip only after per-account auth is in place ([WM17]).
