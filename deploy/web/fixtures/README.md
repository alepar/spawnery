# Falsifiable self-test fixtures (W1, sp-2ckv.6, [WM21])

These fixtures support the pipeline self-test that asserts the cosign verify gate
is FALSIFIABLE — it must refuse invalid/stripped/wrong-identity signatures.

## Files

- `stripped-artifact.txt` — a plain text file with NO accompanying signature bundle.
  The pipeline self-test runs `verify.sh` against this file and asserts it FAILS
  (exit non-zero). If verify unexpectedly passes, the pipeline fails.

- `wrong-identity.bundle` — a cosign bundle signed by a DIFFERENT workflow identity
  (e.g. a personal fork or a test workflow). The pipeline self-test runs `verify.sh`
  against `stripped-artifact.txt` with this bundle and asserts it FAILS due to
  identity mismatch. This is a placeholder: the real fixture is generated during
  initial CI setup by a separate test-signing workflow (see deploy/web/README.md).

## Purpose

The self-test exists because a broken `verify.sh` (e.g. wrong flags, missing `--certificate-identity`)
could silently pass on ANY signature. By asserting the failure cases are reachable,
we prove the gate is meaningful.

The self-test runs on EVERY release:
```yaml
- name: "Self-test: verify MUST fail on stripped artifact"
  run: |
    if deploy/web/verify.sh deploy/web/fixtures/stripped-artifact.txt /dev/null 2>/dev/null; then
      echo "FATAL: verify passed on stripped artifact — gate is broken"
      exit 1
    fi
    echo "Self-test PASSED: verify correctly rejected stripped artifact"
```
