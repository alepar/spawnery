# Config Framework — Layered, Schema-Defined Process Configuration

**Status:** draft (roast round 1: BLOCK → revised) · **Date:** 2026-06-20 · **Tags:** config, infra, dx

## Problem

Every spawnery binary (`spawnery_cp`, `spawnlet`, `authsvc`, `spawnctl`, …) configures itself by
hand-parsing `os.Getenv()` with copy-pasted inline helpers in `main()`. There are no config
files; environment variables are the sole input, dozens per binary (`CP_*`, `NODE_*`, `AS_*`,
`SIDECAR_*`, `JOURNAL_*`). Dev/prod divergence is smeared across a handful of mode flags
(`CP_AUTH_MODE`, `AS_DEV`, `NODE_AUTH_MODE`) plus ad-hoc env reads. Consequences: no schema
(typos/missing-required surface at runtime), per-binary duplication, no layering, and no uniform
override path.

We want a single, explicit, typed config framework shared across processes: a base layer common
to every environment, per-environment overrides deep-merged on top, every parameter overridable
from the CLI, and fail-fast validation at startup.

## Goals / Non-goals

**Goals**
- One config schema per process, **explicitly defined in code** (Go structs as the source of
  truth), strongly typed and validated.
- A base layer (a shared-across-processes `common` layer plus a per-binary base) with
  per-environment overrides **deep-merged on top**.
- Every parameter overridable from env vars and CLI, with a single documented precedence chain.
- Incremental adoption: convert one binary at a time without breaking existing deployments.
- **No silent fallback to an unconfigured state** — a process must either load its intended
  layers or fail loudly.

**Non-goals**
- No codegen / IDL pipeline (no Protobuf-as-config, no CUE). The hand-written Go struct *is* the
  schema. (Researched and rejected — see "Approach selection".)
- No dynamic/hot reload. Config loads once at startup. **Accepted consequence:** rotating a
  compromised credential requires restarting the consuming process (see "Accepted limitations").
- No new secret *server*, and **no reuse of the per-spawn HPKE owner-sealed user-secret system**
  for infra secrets — that system is owner-online-gated, per-device-keyed, and CP-blind, built to
  deliver *user* secrets to a spawn with a ceremony; infra startup secrets must resolve
  **unattended at process boot with no owner online**, a different shape (§3). Config *references*
  infra secrets and resolves them at load through pluggable backends (`env`/`file`/`sops` in v1;
  Vault/cloud managers slot in later behind the same interface).
- **Out of framework scope** (§1.1): per-spawn dynamic config injected into pod containers
  (sidecar/agent), and incidental OS/runtime env reads in library code. This framework owns the
  **startup configuration of long-lived processes and CLIs**, nothing else.

## Approach selection (why koanf + Go structs)

A deep-research pass (adversarially verified) compared the IDL-codegen family
(Protobuf+protovalidate, CUE) against the idiomatic Go-struct family (koanf, viper,
go-envconfig). koanf wins: it loads/deep-merges arbitrary sources with **caller-controlled
precedence** (the order of `Load()` calls *is* the chain), and binds the flag library the repo
already uses (`urfave/cli/v3` via the `cliflagv3` provider). CUE was rejected (its Go codegen is
experimental and constraints don't survive into the generated types); Protobuf-as-config models
wire data, not deep-merged file layers, and its layered-merge ergonomics are poor even though
the repo runs `buf`.

**Chosen:** a thin in-house `internal/config` layer over **koanf**, **plain Go structs as the
schema**, and **`go-playground/validator`** (plus per-type `Validate()` methods) for validation.

**Trade-off accepted:** this inverts the original "generate the struct from a schema" instinct —
the hand-written struct is the source of truth and YAML is validated *against* it. The hard part
of *this* problem is layering/precedence/merge, koanf's core competency.

---

## 1. Package shape, file layout & distribution

```
internal/config/
  load.go        # Load[T](svc, opts) → bootstrap env → layered koanf → resolve refs → decode → validate
  embed.go       # //go:embed config/*.yaml  (baked-in committed defaults)
  resolve.go     # ${scheme:arg} dispatch on the raw map → Resolver registry
  resolvers.go   # the Resolver interface + env/file resolvers
  sops.go        # ${sops:key} — in-process SOPS+age decrypt, decrypt-once cache
  secret.go      # type Secret string — always renders *** in String/Marshal/Format
  envmap.go      # per-binary explicit env-name → dotted-key tables
  setflag.go     # --set key=value → confmap provider
  common.go      # type Common struct — shared section, embedded by every binary
  cp.go          # type CP struct { Common `koanf:",squash"`; ... }
  spawnlet.go, authsvc.go, ...
config/                       # repo-root, committed, AND //go:embed-ed into binaries
  common.yaml   common.staging.yaml   common.prod.yaml
  cp.yaml       cp.prod.yaml   (cp.dev.yaml etc. only if a delta exists)
  spawnlet.yaml ...
  secrets.dev.sops.yaml  secrets.staging.sops.yaml  secrets.prod.sops.yaml   # SOPS+age-encrypted
```

**Distribution / runtime discovery (closes the "where do prod files come from" gap).** The
committed `config/` tree — including the **SOPS-encrypted** `secrets.<env>.sops.yaml` files — is
**`//go:embed`-ed into every binary**. A binary therefore *always* has its full baseline
regardless of CWD or container layout — there is no path where it silently runs on bare Go
defaults. For ops to change a value **without a rebuild**, an optional external directory
`SPAWNERY_CONFIG_DIR` may hold same-named files; if set, each present external file is
**deep-merged over its embedded counterpart** (so ops can drop a single `cp.prod.yaml` override).
Embedded files are required to exist (build-time guarantee); external files are all optional. The
secrets files are safe to embed because they are ciphertext; the **age identity that decrypts
them is never embedded or committed** — it is delivered out-of-band per §3.

**Per-binary structs embed `Common`** (koanf `,squash`), so `common.yaml` and `<svc>.yaml`
decode into one typed value. Keep `common.yaml` small — only genuinely cross-process values.

### 1.1 Scope boundary

In scope: `main()`-level startup config for **`spawnery_cp`, `spawnlet`, `authsvc`**, and the
**CLIs** (`spawnctl`, `agentinstall`, …). The "delete the inline `os.Getenv` helpers" cleanup
applies to *these* call sites only.

Out of scope, explicitly **left as-is**:
- **Per-spawn dynamic env** injected into pod containers by spawnlet at pod creation
  (`SIDECAR_SPAWN_ID`, `SIDECAR_GETTOKEN_*`, the git-proxy vars). The **sidecar** is configured
  this way; it does **not** get a meaningful committed `config/sidecar.yaml`, and is not in the
  rollout list. (If the sidecar later grows static knobs, they can join then.)
- **Incidental OS/runtime env reads** in library code (`HOME`, `XDG_CONFIG_HOME`, `CODEX_HOME`,
  etc.) and env read *inside* spawn/agent containers. These are runtime environment, not process
  config.

### 1.2 CLI binaries (urfave/cli/v3 subcommands)

`spawnctl`/`agentinstall` are multi-subcommand `urfave/cli/v3` apps. The config load is wired
**once at the root `Before` hook** (after global-flag parse, before subcommand dispatch); curated
flags that map to config keys are global flags. This is materially more involved than the
single-command servers and is called out as its own rollout task, not assumed free.

## 2. Precedence chain & merge semantics

Lowest → highest. Implemented purely by the order of koanf `Load()` calls:

```
0. in-code struct defaults     (structs.Provider — EVERY key present; the authoritative floor)
1. common.yaml                 (shared base)               ┐ read from the EMBEDDED FS,
2. common.<env>.yaml           (shared env delta; optional)│ then each present same-named file
3. <svc>.yaml                  (per-binary base)           │ from $SPAWNERY_CONFIG_DIR is
4. <svc>.<env>.yaml            (per-binary env delta; opt.)┘ deep-merged on top
5. env vars                    (explicit name→key map, §6)
6. curated named flags         (cliflagv3; explicit beats lower, unset default does NOT)
7. --set key=value             (custom confmap provider; wins over everything)

→ resolve ${scheme:arg} references on the merged raw map (§3)
→ Decode into the typed struct (mapstructure, WeaklyTypedInput — see below)
→ Validate (§4), fail-fast
```

**Defaults are a real koanf layer, not a separate floor.** Layer 0 is a `structs.Provider` over
the defaults struct, so *every* key exists in the map before higher layers load. This is what
makes cliflagv3's "unset flag's default does not override" behave: default-suppression only works
for keys already present in the map (closes the layer-0-vs-flag-default conflict).

**No global `StrictMerge`.** koanf's env/`--set`/cliflag providers emit **string** values, while
YAML emits typed ints/bools; `MergeStrict`'s `reflect.TypeOf` equality would hard-fail every
typed env override at startup (verified against koanf `maps.go`). Instead: layers merge
permissively, and **type coercion happens once at `Decode` with `mapstructure` `WeaklyTypedInput:
true`** (string→int/bool/duration). A genuinely uncoercible value (`"abc"` into an int) surfaces
as a **decode error naming the dotted key path** — fail-fast, but at decode, not merge. (Spike S1
pins this behavior before we build on it.)

**Merge rules:** nested maps deep-merge recursively; **scalars and lists are *replaced*, not
concatenated** (koanf default — the intended semantics for `egress.allow_cidrs`-style lists). A
key *absent* from an override file is genuinely absent, so the lower layer survives (no Go
zero-value clobber). Pointers (`*int`, `*bool`) are an **escape hatch used only where a real
field needs "explicitly-set-zero must beat a non-zero default"** — none identified today; not a
blanket policy and not a standing test.

**Optional-file handling:** the two base files (`common.yaml`, `<svc>.yaml`) are **required**
(embedded, so guaranteed present); the `*.<env>.yaml` deltas are **optional** (skipped if absent
in the embedded FS / external dir). Sparse layouts are normal.

### 2.1 Environment selection (two-phase bootstrap; fail-closed)

Selecting the env is a **bootstrap input that determines which files form layers 2 & 4**, so it
cannot ride the normal precedence pipeline. Phase 1 (before any `Load`): scan `os.Args` for
`--env` and read `SPAWNERY_ENV`; `--env` wins if both present. Phase 2: build the layered chain
for that env.

- **No silent default.** If neither is set, **fatal**: `SPAWNERY_ENV must be one of dev|staging|prod`.
  (Closes the fail-open hole — a prod box that forgets the var must not boot on auth-relaxed dev
  config.) `just dev` sets `SPAWNERY_ENV=dev` explicitly.
- The value is **validated against the known set** `{dev,staging,prod}`; an unknown/typo'd value
  (`prod ` , `production`) is **fatal**, never a silent no-op.
- `--env` is consumed in the bootstrap; it is **not** also a layer-7 `--set`/curated flag.

## 3. Secret references & the SOPS+age backend

Config files are committed and **never contain plaintext secret values**. A string value may be a
reference `${scheme:arg}` resolved by a registry of pluggable **Resolvers**, where the **scheme
selects the backend**:

```go
// Resolver turns the arg of a ${scheme:arg} reference into cleartext.
// Registered by scheme; the scheme prefix selects the backend.
type Resolver interface {
    Scheme() string                     // "env", "file", "sops", … "vault"/"awssm" later
    Resolve(arg string) (string, error) // fail-closed: the loader panics on a non-nil error
}
```

v1 ships three resolvers; more (Vault, AWS/GCP/Azure managers) slot in later by implementing the
same interface. (koanf's `vault/v2` and `parameterstore` providers exist but integrate secret
stores as *config sources*, not `${…}` token resolvers, so we own this layer regardless — modeled
on `DevLabFoundry/configmanager`'s prefix-selects-backend idiom.)

- **`${env:NAME}`** — environment variable `NAME`.
- **`${file:/path}`** — file contents, trimmed.
- **`${sops:dotted.key}`** — looked up in the decrypted `secrets.<env>.sops.yaml` (below).

**Ordering: references resolve on the merged *raw koanf map*, BEFORE decode** (corrected from the
prior post-decode ordering, which couldn't put `${env:PORT}` into an int and destroyed redaction
provenance). References are only valid in **string-typed leaves**; a reference in a non-string
field is a config error naming the key. Resolution runs after all layers merge and before
`Decode`, so decode/validation see real values.

**Failure semantics (fail-closed):** an unset `${env:NAME}`, a missing/unreadable `${file:}`, a
`${sops:}` key absent from the decrypted map, an undecryptable secrets file, or an unknown scheme
is a **fatal error** naming the key — never a silent empty string. (Note: this is *deliberately
unlike* the Azure Key Vault reference idiom, which fails **open** by passing the literal token
through. Our resolver layer enforces fail-closed itself, independent of any backend.)

### 3.1 The SOPS+age backend (the v1 `${sops:}` resolver)

**Why SOPS+age** (deep-researched, primary-sourced): a maintained, stable in-process Go decrypt
API (`github.com/getsops/sops/v3/decrypt`) decrypts at load with **no sidecar/init-container and
no mandatory cloud dependency**; age is SOPS's officially-recommended key type; and cloud KMS
(AWS/GCP/Azure) drops into the *same encrypted file* as an additional recipient when a node has
it. Accepted trade-off: single-decrypt-key blast radius + manual rotation, bounded by
**per-environment age recipients**.

**The secrets file.** `config/secrets.<env>.sops.yaml` is a committed, SOPS+age-encrypted map of
`id → value` (the values are ciphertext; the structure is cleartext). `${sops:store.dsn}` looks
up `store.dsn` in the decrypted map for the active env.

**Resolution at startup** (operator-controlled hosts only — see 3.2):
1. Locate the **age identity** (secret-zero) via `SOPS_AGE_KEY_FILE` — a perms-`0600` file, or a
   systemd `LoadCredential` on the CP host, or a cloud-KMS recipient on cloud nodes.
2. `decrypt.Data(embeddedSecretsFileForEnv)` **once** → cleartext map, **cached in-process** for
   the process lifetime (never re-decrypt per reference).
3. `${sops:store.dsn}` → lookup in that map; the value lands in a `config.Secret` field.
4. Missing key / undecryptable file / absent id → **panic at startup** (fail-closed).

**Secret-zero delivery (no cloud required).** The age identity is the *one* out-of-band secret;
everything else is the committed ciphertext. Delivery options, operator's choice: a perms-`0600`
key file referenced by `SOPS_AGE_KEY_FILE`; a systemd credential (`LoadCredential=`, AES256-GCM at
rest, no env/child-leak) on a systemd CP host; or a cloud-KMS recipient on cloud nodes (the KMS
unwraps, no age file shipped). It is **never embedded, committed, or placed in a config file**.

**Rotation / blast radius.** Each env's file is encrypted to that env's age recipient(s) — a
compromised dev key does not expose prod. Rotation = `sops updatekeys` (re-wrap the data key to a
new recipient) on the affected file; per-value rotation is editing that value and re-encrypting.

### 3.2 Trust boundary: infra secrets never reach third-party self-hosted nodes

`${sops:}` is for **operator-controlled processes** — the CP, AS, and cloud nodes the operator
runs. Shipping the age identity to an untrusted third-party node would hand its operator all
plaintext. A **third-party self-hosted node holds only its *own* credentials** (node identity key,
enrollment token), already provisioned through the existing node-enrollment path and read via
`${file:}`/`${env:}` — **not** via `${sops:}`. Operator infra secrets stay on operator hosts.

### 3.3 Redaction & transport caveats

**Redaction is type-level, not provenance-tracked.** Secret-bearing fields are declared as
`config.Secret` (a `string` newtype) whose `String()`, `MarshalJSON()`, `MarshalYAML()`,
`GoString()`, and `Format()` **always render `***`** — robust against `fmt %+v`, error-wrapping,
panics, and third-party loggers (the failure mode a dump-path-only redaction misses). No per-field
provenance bookkeeping needed.

- `${env:}` secrets remain readable via `/proc/<pid>/environ`, core dumps, and child inheritance
  (spawnlet launches pods). **`${file:}`/`${sops:}` are preferred for real secrets**; the
  resolvers are *not* security-equivalent.
- **Secrets must never be passed on argv** (`--set`, `--store-dsn`): argv leaks via `ps`,
  `/proc/<pid>/cmdline`, and shell history. Secret-bearing keys are resolved from
  `${sops:}`/`${file:}`/env, never flags.

## 4. Validation

- **Format-only** field tags via `go-playground/validator`: `required`, `oneof=...`, `min`/`max`,
  `hostname_port`, `cidr`, `url`, etc. **No `dir`/`file` existence tags** — those `os.Stat` at
  startup, coupling config validity to live filesystem state and breaking hermetic tests and
  lazily-created data roots. Path *existence/permission* checks are the owning component's job at
  use time, not config validation.
- **Dotted key paths in errors:** `RegisterTagNameFunc` maps validator field errors to the koanf
  key (`cp.store.dsn`, not `CP.Store.DSN`); hand-rolled `Validate()` methods format their errors
  with the dotted path too.
- **Cross-field** via a `Validate() error` per type. Because Go method promotion would let a
  binary's `Validate()` **shadow** the embedded `Common.Validate()`, each type's `Validate()`
  **must call `c.Common.Validate()` explicitly** (tag-based validation already descends into the
  squashed `Common` fields; only the method needs the explicit call).
- Runs once after decode, **fail-fast**.

## 5. CLI override surface

"Every parameter overridable from the CLI" without hundreds of flags:

- **`--set key.path=value`** reaches **every leaf**. This is **not** a koanf/cliflag built-in —
  it is a small **custom provider**: split on the first `=`, split the key on `.`, build a nested
  `map`, load via `confmap` as the **top layer (7)**. Values are kept as **strings** and coerced
  at decode (so `--set listen=:8080` stays `":8080"`, dodging YAML-scalar mis-coercion of
  `:8080`/`yes`/`null`). **Scalar-only in v1**; lists/maps are set via files (keeps the provider
  trivial and predictable).
- **Curated named flags** for hot params (`--listen`, `--store-dsn`, …) bound through
  **`cliflagv3`** (the repo's flag lib), at layer 6. cliflagv3's explicit-vs-default
  (`Context.IsSet`) gives "explicit flag beats files, unset default does not" — *because* layer 0
  loaded every key. Flag-name→key remapping is part of the binding (spike S2 confirms the
  remapped-and-absent case).
- **Precedence is defined:** when both `--set store.dsn=x` and `--store-dsn y` are given, **`--set`
  wins** (layer 7 > layer 6).

## 6. Env-var mapping & backward compatibility

The env layer (5) uses an **explicit per-binary table mapping full env-var name → dotted config
key** — *not* prefix-plus-auto-derivation. Two reasons, both verified:
- koanf's env provider uses `_` as the nested-path delimiter, so an auto-derived
  `CP_EGRESS_ALLOW_CIDRS` would become `egress.allow.cidrs`, never `egress.allow_cidrs`. An
  explicit table sidesteps the collision for every key with an underscore leaf
  (`allow_cidrs`, `as_session_pubkeys`, `max_families`).
- Legacy names don't share a clean prefix: `spawnlet` uses `NODE_*`, `authsvc` uses `AS_*`, and
  many are bare (`OPENROUTER_API_KEY`, `JOURNAL_*`, `GITHUB_CLIENT_ID/SECRET`, `GARAGE_*`,
  `CONTAINER_RUNTIME`, `REGISTRATION_ENABLED`, `ENROLL_TOKEN`). The table lists each explicitly,
  so **all existing names keep working** — the prefix-filter approach would silently miss the
  bare ones.

The table is the migration ledger: it carries every currently-honored env var plus any new
ones. There is no separate "auto-derived" scheme to collide with it.

**Rollout sequencing hazard (must be honored).** Layer 5 (env) sits **above** the file layers, so
while a key is migrating, **a still-exported legacy env var silently overrides the new YAML file**
— the file is inert until the export is removed. Therefore, moving a key into a YAML file and
**removing its export from the Justfile/`deploy/` unit** is **one atomic change**, per key, per
binary. Pilot on **CP first**; prove end-to-end (including a `just dev` target that sets
`SPAWNERY_ENV=dev` and reads the files) before rolling to `spawnlet`, `authsvc`, and the CLIs.

## 7. Testing

Hermetic, table-driven, **no network, no filesystem dependence**:

- **Precedence:** each layer overrides the one below; a `common.yaml`-only value survives to a
  binary that doesn't restate it; `--set` > curated flag > env > file > default.
- **Coercion/merge:** string env/`--set` over typed YAML decodes correctly (WeaklyTypedInput);
  an uncoercible value yields a key-path decode error; nested-map deep-merge; list-replace.
- **Bootstrap:** missing `SPAWNERY_ENV` is fatal; unknown env value is fatal; `--env` beats the
  var; optional `*.<env>.yaml` absent is fine.
- **References:** `${env:}`/`${file:}` resolve on the map pre-decode; unset/missing is fatal;
  reference in a non-string leaf errors; unknown scheme errors.
- **SOPS backend:** a fixture `secrets.test.sops.yaml` encrypted to a **throwaway test age key**
  decrypts in-process via `decrypt.Data`, resolves `${sops:k}`, and is decrypted only once
  (cache); an absent key, undecryptable file, or missing id is fatal. (Test age key is for
  fixtures only — never a real recipient.)
- **Redaction:** `config.Secret` renders `***` under `%v`, `%+v`, `%#v`, JSON, and YAML.
- **Validation:** required/enum/range/cross-field failures each produce a dotted-key-path error;
  `Common.Validate()` is actually invoked when a binary defines its own `Validate()`.

Because validation is format-only and `${file:}` is exercised via temp files supplied by the
test (not real data roots), the suite stays hermetic. Embedded-FS loading needs no disk.

## Spikes (run first, before building on the load-bearing assumptions)

- **S1 — koanf merge + WeaklyTypedInput coercion.** *Question:* with no `StrictMerge`, does a
  string env/`--set` value over a typed YAML leaf merge cleanly and decode (string→int/bool/
  duration) via mapstructure `WeaklyTypedInput`? *Cheapest test:* ~30-line Go: load `{port:8080}`
  YAML, then `PORT=9090` env, then `--set timeout=5s`, decode into a struct. *Kill criteria:* if
  merge or decode errors on type, revisit the coercion strategy (per-key typed env transform).
- **S2 — cliflagv3 explicit-vs-default with key remapping.** *Question:* given curated flags whose
  names remap to dotted keys, does an explicitly-set `--flag` override a file value while an unset
  flag's default does **not**, with layer-0 defaults present? *Cheapest test:* ~30-line harness,
  one set + two unset remapped flags, assert the merged map. *Kill criteria:* if an unset default
  clobbers the file value, bind curated flags via a manual `confmap` from `Context.IsSet` instead.
- **S3 — embed + external-dir deep-merge.** *Question:* do `$SPAWNERY_CONFIG_DIR` files
  deep-merge over their embedded counterparts as layers 1–4? *Cheapest test:* embed a base, point
  the dir at a one-key override, assert the merge. *Kill criteria:* if embed/external ordering is
  awkward, fall back to "external dir, if set, fully replaces the embedded file set."
- **S4 — SOPS in-process decrypt of an embedded file.** *Question:* does
  `decrypt.Data(embeddedBytes, "yaml")` from `getsops/sops/v3/decrypt`, with the age identity at
  `SOPS_AGE_KEY_FILE`, decrypt an `//go:embed`-ed `secrets.*.sops.yaml` in-process and yield the
  cleartext map? *Cheapest test:* `sops -e` a 2-key fixture to a throwaway age recipient, embed it,
  decrypt in a unit test. *Kill criteria:* if `decrypt.Data` needs filesystem/CLI access or chokes
  on embedded bytes, write the embedded ciphertext to a temp file and use `decrypt.File`, or shell
  to the `sops` binary as a fallback.

## Accepted limitations

- **No hot reload → credential rotation requires a process restart.** A rotated OAuth secret /
  signing PEM / DB password / model key is only picked up on restart of each consumer. Accepted
  for v1; revisit if rotation cadence demands it (koanf has watch providers).
- **Backends shipping in v1: `env`, `file`, `sops`.** Vault and the cloud secret managers are not
  implemented; they slot in later behind the `Resolver` interface (§3). The koanf
  `vault/v2`/`parameterstore` providers exist but resolve config *sources*, not `${…}` tokens.
- **`${env:}` secrets remain process-environment-exposed** (§3); `${sops:}`/`${file:}` are the
  recommended paths for sensitive values.
- **No in-memory secret zeroing in v1.** Decrypted secrets live as ordinary Go strings/`Secret`
  values for the process lifetime; an enclave/zeroing layer (e.g. `awnumar/memguard`) is noted as
  future hardening, not v1 scope.

## Adversarial review (roast)

Round 1 (2026-06-20): **BLOCK** → revised. An 8-lens opus critic panel + 3-judge verification
surfaced (after collapsing duplicates) ~12 distinct load-bearing gaps, all folded above:
`StrictMerge`-vs-string-env hard-fail (dropped `StrictMerge`, decode-time coercion); runtime file
discovery (`//go:embed` + optional `SPAWNERY_CONFIG_DIR`); `SPAWNERY_ENV` fail-open (now explicit,
validated, fail-closed); `--env` two-phase bootstrap; `--set`/posflag correction (custom provider
+ `cliflagv3`, defined precedence); secret-resolution moved pre-decode + type-level `Secret`
redaction + fail-closed resolver semantics + argv/env caveats; explicit env-name→key table
(underscore-delimiter + non-prefixed legacy names); format-only validation (no `os.Stat` tags) +
dotted-path errors + explicit `Common.Validate()`; scope boundary excluding per-spawn sidecar
config; rollout file/env atomic-swap sequencing; CLI subcommand wiring; hot-reload limitation made
explicit. Three spikes (S1–S3) gate the remaining empirical unknowns.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged
from the assumptions above — append a dated note here, whether or not a formal debugging skill
was used.*
