# Config Framework — Layered, Schema-Defined Process Configuration

**Status:** draft · **Date:** 2026-06-20 · **Tags:** config, infra, dx

## Problem

Every spawnery binary (`spawnery_cp`, `spawnlet`, `sidecar`, `authsvc`, `spawnctl`,
`agentinstall`, …) configures itself by hand-parsing `os.Getenv()` with copy-pasted inline
helpers in `main()`. There are no config files; environment variables are the sole input, and
there are dozens per binary (`CP_*`, `NODE_*`, `AS_*`, `SIDECAR_*`, `JOURNAL_*`). Dev/prod
divergence is smeared across a handful of mode flags (`CP_AUTH_MODE`, `AS_DEV`,
`NODE_AUTH_MODE`) plus ad-hoc env reads. Consequences:

- **No schema.** Nothing declares the shape of a process's config; typos and missing-required
  values surface at runtime, not startup.
- **Duplication.** Each binary re-implements env parsing, defaulting, and type coercion.
- **No layering.** "The base value shared across all environments, with a thin per-env delta on
  top" cannot be expressed — every value is re-stated per deployment.
- **No single override path.** Some knobs are env-only, some are flags (CLIs), with no uniform
  precedence.

We want a single, explicit, typed config framework shared across all processes: a base layer
common to every environment, per-environment overrides deep-merged on top, every parameter
overridable from the CLI, and fail-fast validation at startup.

## Goals / Non-goals

**Goals**
- One config schema per process, **explicitly defined in code** (Go structs as the source of
  truth), strongly typed and validated.
- A base layer (shared across all environments, and a shared-across-processes `common` layer)
  with per-environment overrides **deep-merged on top**.
- Every parameter overridable from env vars and CLI flags, with a single documented precedence
  chain.
- Incremental adoption: convert one binary at a time without breaking existing deployments.

**Non-goals**
- No codegen / IDL pipeline (no Protobuf-as-config, no CUE). The hand-written Go struct *is*
  the schema. (Researched and rejected — see "Approach selection".)
- No dynamic/hot reload of config at runtime. Config is loaded once at startup. (Future work.)
- No new secret store. Config references secrets; it does not custody them (see §3).
- This framework configures **infra processes**, not spawns. The owner-sealed per-spawn user-
  secret system is a separate axis and is untouched.

## Approach selection (why koanf + Go structs)

A deep-research pass compared the IDL-codegen family (Protobuf+protovalidate, CUE) against the
idiomatic Go-struct family (koanf, viper, go-envconfig) against the five requirements. Verified
conclusions:

- **koanf** loads and deep-merges arbitrary sources with **caller-controlled precedence** (the
  order of `Load()` calls *is* the precedence chain), recursively merges nested maps while
  replacing scalars/lists, and binds the flag libraries already in the tree (`cliflagv3` for
  `urfave/cli/v3`, `posflag` for `pflag`). Its `posflag` provider resolves the central
  layered-config gotcha: a config file beats a flag's **default**, but an **explicitly-set**
  `--flag` still wins.
- **CUE** was rejected: `cue exp gengotypes` is officially experimental, and validation
  constraints do **not** survive into generated Go types (a `<30` becomes a plain `float64`),
  so you'd carry a CUE runtime *and* re-validate at load. Too much toolchain.
- **Protobuf-as-config** gives the strongest single-source schema+validation, but proto models
  wire data, not deep-merged file layers; its layered-file-merge ergonomics are poor and
  "unset vs zero" needs `optional` gymnastics. Even though the repo already runs `buf`/`make
  gen`, it's the wrong fit for *layered file config*.

**Chosen:** a thin in-house `internal/config` layer over **koanf**, with **plain Go structs as
the schema** and **`go-playground/validator`** (plus per-type `Validate()` methods) for
validation.

**Trade-off accepted:** this inverts the original "generate the struct from a schema, à la
protobuf" instinct — the hand-written struct becomes the source of truth and YAML files are
validated *against* it. It still satisfies "schema explicitly defined in code"; codegen was a
means, not the end. The hard part of *this* problem is layering/precedence/merge, which is
koanf's core competency and protobuf's weak spot.

---

## 1. Package shape & file layout

```
internal/config/
  load.go        # generic Load[T](svc, opts) → layered koanf → decode → resolve → validate
  resolve.go     # ${scheme:arg} reference resolution (registry of resolvers)
  common.go      # type Common struct — shared section embedded by every binary
  cp.go          # type CP struct { Common `koanf:",squash"`; ... }
  spawnlet.go    # type Spawnlet struct { Common ...; ... }
  sidecar.go, authsvc.go, ...
config/                       # repo-root, committed
  common.yaml   common.prod.yaml   common.staging.yaml
  cp.yaml       cp.prod.yaml
  spawnlet.yaml spawnlet.prod.yaml
  sidecar.yaml  ...
```

- Each binary's `main()` collapses to roughly:
  `cfg, err := config.Load[CP]("cp", config.FromArgs(os.Args[1:]))`.
  The inline `os.Getenv` helpers are deleted.
- Each per-binary schema struct **embeds `Common`** (koanf `,squash`), so `common.yaml` and
  `cp.yaml` decode into one typed value. The shared `common` knobs (log level, telemetry,
  AS/CP endpoints, data roots, feature gates) live on `Common` and are reused by every binary.
- **Keep `common.yaml` small** — only genuinely cross-process values. 12-Factor warns that
  over-grouping config by named environment causes combinatorial growth; per-env override files
  stay **thin** (only true deltas), and instance-specific values are pushed to env vars.

## 2. Precedence chain (low → high)

```
0. in-code defaults       (Go struct literal — authoritative floor, unit-testable)
1. common.yaml            (shared base)
2. common.<env>.yaml      (shared per-env delta, deep-merged)
3. <svc>.yaml             (per-binary base)
4. <svc>.<env>.yaml       (per-binary per-env delta, deep-merged)
5. env vars               (<SVC>_ prefix, mapped to nested keys)
6. CLI: --set + curated named flags   (posflag: explicit-only beats files)
```

Implemented purely by the order of koanf `Load()` calls. Merge semantics:

- **Nested maps deep-merge recursively; scalars and lists are *replaced*, not concatenated**
  (koanf default). List-replace is the intended semantics for fields like `egress.allow_cidrs`
  — an override file's list wholly replaces the base list (predictable). If concatenation is
  ever genuinely needed for a field, that's an explicit `WithMergeFunc` opt-in, documented at
  the field.
- **No Go zero-value clobber across file/env/flag layers.** koanf merges raw `map`s, so a key
  *absent* from an override file is genuinely absent and the lower layer survives. The
  zero-vs-unset trap only bites where "explicitly set to zero must beat a non-zero default" is a
  real requirement; those specific fields become **pointers** (`*int`, `*bool`). Plain values
  everywhere else.
- `StrictMerge: true` so a type mismatch across layers is an error, not a silent coercion.

**Environment selection:** a global `SPAWNERY_ENV` env var (`dev` | `staging` | `prod`),
**default `dev`**, read by every binary, selects which `*.<env>.yaml` files are layered. A
`--env` flag overrides it for a single invocation. This matches how `just dev` /
`dev-enforced` already gate behavior.

## 3. Secret references

Config files are committed and **never contain secret values**. Instead a string value may be a
reference `${scheme:arg}` resolved at load time by a registry of resolvers keyed by scheme:

- **`${env:NAME}`** — read from environment variable `NAME`. (Implemented in v1.)
- **`${file:/path}`** — read file contents (trimmed). (Implemented in v1.)
- **`${secret:id}`** — registered **resolver interface with no concrete backend in v1**;
  returns a clear "no resolver registered for scheme `secret`" error if used. Reserved for a
  future infra secret store. **Not** wired to the owner-sealed per-spawn user-secret system,
  which is a different axis.

These two concrete resolvers cover every infra-process secret in use today (DB DSN, OAuth
client secret, signing PEMs, OpenRouter key). Resolution runs **after decode, before
validation**, so validators see resolved values.

**Redaction:** any field whose value resolved from a reference, or which is tagged
`secret:"true"`, is redacted (`***`) in config-dump / startup-log output. The framework MUST
provide a safe `Redacted()` / dump path; logging the raw struct is forbidden.

## 4. Validation

- Field-level via `go-playground/validator` struct tags: `required`, `oneof=...`, `min`/`max`,
  `hostname_port`, `cidr`, `dir`, `file`, etc.
- Cross-field via an optional `Validate() error` method per config type (e.g. "if
  `auth.mode == prod` then `auth.as_session_pubkeys` is required").
- Runs once at startup, **fail-fast**. Errors name the offending key path (dotted, e.g.
  `cp.store.dsn`) and the constraint that failed, so misconfiguration is diagnosable without a
  debugger.

## 5. CLI override surface

"Every parameter overridable from the CLI" is satisfied without hundreds of flags:

- A generic **`--set key.path=value`** (Helm-style) reaches **every leaf** in the schema.
  Repeatable; values parse as YAML scalars (so `--set egress.allow_cidrs='[10.0.0.0/8]'`
  works).
- A small **curated set of named flags** for hot params (`--listen`, `--env`, `--store-dsn`,
  …) with real `--help` text. These bind through koanf `posflag`, so an explicitly-set named
  flag wins over files while an unset flag's default does **not** override a file value.
- `--help` therefore shows ~a handful of curated flags plus one `--set`, not the full leaf set.

## 6. Backward compatibility & rollout

**Incremental, one binary at a time.** Pilot on **CP first** (richest config surface), proving
the pattern end-to-end — including a `just dev` target that reads the new `config/` files — then
roll out to `spawnlet`, `sidecar`, `authsvc`, and the CLIs.

**Existing env var names keep working.** The env provider uses an explicit **alias table**
mapping current names to nested config paths (`CP_LISTEN → listen`, `CP_STORE_DSN → store.dsn`,
…), so the Justfile and deploy scripts are unaffected during transition. New knobs follow the
auto-derived `<SVC>_<PATH>` naming. The alias table is the migration ledger; once a binary is
fully cut over, aliases can be pruned deliberately.

## 7. Testing

Hermetic, table-driven, no network:

- **Precedence:** each layer overrides the one below; a value set only in `common.yaml` survives
  to a binary that doesn't restate it; `--set` beats env beats file.
- **Merge semantics:** nested-map deep-merge; list-replace; `StrictMerge` type-mismatch error.
- **Secret resolution:** `env:`/`file:` resolvers resolve; unknown scheme errors; resolved /
  `secret:"true"` fields are redacted in the dump.
- **Validation:** required / enum / range / cross-field failures each produce a key-path-named
  error.
- **Pointer fields:** "explicitly set to zero overrides non-zero default" works; absent key
  inherits.

## Open questions / future work

- **Hot reload.** Out of scope; config loads once. koanf supports watch providers if we ever
  want it.
- **`${secret:}` backend.** A real infra secret store (Vault / parameterstore / a CP-served
  endpoint) is a clean follow-up once there's a concrete need.
- **Schema documentation generation.** A `--dump-schema` / generated reference of every key,
  type, default, and validation rule would aid ops; deferred.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged
from the assumptions above — append a dated note here, whether or not a formal debugging skill
was used.*
