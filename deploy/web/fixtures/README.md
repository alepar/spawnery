# Falsifiable self-test fixtures (W1, sp-2ckv.6, [WM21])

These fixtures support the pipeline self-test that asserts the cosign verify gate
is FALSIFIABLE — it must refuse invalid/stripped/wrong-identity signatures.

## Files

- `stripped-artifact.txt` — a plain text file with NO accompanying signature bundle.
  The pipeline self-test runs `verify.sh` against this file and asserts it FAILS
  (exit non-zero). If verify unexpectedly passes, the pipeline fails.

- `wrong-identity.bundle` — a cosign bundle signed by the `gen-test-fixture.yml`
  workflow (certificate identity: `.../gen-test-fixture.yml@...`), which is a DIFFERENT
  identity from `web-release.yml`. The deploy self-test verifies that cosign REJECTS
  this bundle when pinned to `web-release.yml`'s identity, proving the gate blocks
  attacker-signed (different-workflow) artifacts.

  **To generate:** run `gh workflow run gen-test-fixture.yml` once (requires id-token:write
  permission on the repository). The workflow signs `stripped-artifact.txt`, commits
  the resulting bundle here, and pushes. The bundle is STATIC — regenerate only if the
  Sigstore CT log entry expires or the repository path changes.

  If the file is missing, the deploy job **fails** (exits 1) with a message directing
  the operator to run `gen-test-fixture.yml`. The gate is fail-closed: the certificate-identity
  pin must be proven falsifiable before the first release. Generate this fixture once before
  the first `web/v*` tag.

## Purpose

The self-test exists because a broken verify command (e.g. wrong flags, missing
`--certificate-identity`) could silently pass on ANY signature. By asserting multiple
failure cases are reachable we prove the gate is meaningful:

1. **stripped-artifact.txt** — proves verify fails when there is no signature at all.
2. **wrong-oidc-issuer** — proves verify checks the OIDC issuer (not just the certificate).
3. **wrong-identity.bundle** — proves verify rejects a bundle signed by a DIFFERENT workflow
   identity (the attacker-fork scenario [WM21]).

The self-test runs on EVERY release via `web-release.yml`.
