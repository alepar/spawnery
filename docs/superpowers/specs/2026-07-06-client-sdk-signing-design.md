# Client SDKs (Go + TypeScript) with A4 intent-signing

**Status:** draft · **Date:** 2026-07-06 · **Epic:** (bd, prefix `sp`) · **Plan:** approved (dazzling-swinging-piglet)

## Problem

Spawn creation on the prod-real stack is gated by the **A4 two-phase intent-signing** protocol:
the enforced node (`NODE_AUTH_MODE=enforced`) NACKs provisioning with `MISSING_INTENT` unless a
client-signed `IntentBody` (ephemeral ECDSA-P256, P1363, over `DomainFor(op) || proto.Marshal(body)`)
is polled/built/submitted by the client. Three clients re-implement this by hand today, and the
acceptance suite's Connect-JSON "api oracle" (`acceptance/src/drivers/api.ts`) can't sign at all — so
`api.createSpawn` cannot provision on the enforced node. That surfaced as the acceptance setup-create
failures on the e2e VM, but the root cause is broader: **there is no canonical client library.**

Duplication today:
- **Go**: `internal/intent` (pure leaf) + `cmd/spawnctl/intent.go` (`pollAndSign`, `provisionWithIntent`)
  + client construction scattered across `cmd/spawnctl/*.go`; `internal/cp/intent_threading_test.go`
  re-implements `pollAndSign` verbatim; ~10 e2e tests + `cmd/authsvc` hand-build the Connect client.
- **TS (web)**: `web/src/auth/{intent,protobuf,keypair,pop}.ts` + `web/src/keys/{der,hkdf,encoding}.ts`
  — hand-rolled Connect-JSON transport + a `ProtoWriter` proto codec + signing.
- **TS (acceptance)**: `api.ts` (near-copy of web transport, no signing) + `auth/pop.ts` (byte-for-byte
  hand-port of web `pop.ts`) + a copy-pasted dev-token key.

## Solution

One canonical **client SDK per language**, generated from proto, consumed by every client.

**Locked decisions:** (1) generate TS from proto (`protoc-gen-es`/`connect-es`), not hand-rolled;
(2) npm **workspace** packaging; (3) **one epic**, all consumers migrated together.

### Proto-generated foundation
`buf.gen.yaml` gains `protoc-gen-es` + `protoc-gen-connect-es`; `make gen` emits TS types + Connect
clients into `sdk/ts/src/gen/` (the SDK is the sole TS consumer of generated code — web/acceptance
consume the SDK, never raw `gen/`). Go generation unchanged.

### npm workspace
Root `package.json` with `workspaces: ["sdk/ts", "web", "acceptance"]`; both apps import
`@spawnery/client`.

### Go SDK — `internal/client` (in-module)
In-module (spawnctl, e2e tests, authsvc are the only consumers), so `internal/intent` (shared with the
node **verifier**) needn't move. Wraps `cpv1connect.SpawnServiceClient`, parameterized by endpoint +
a **token-source interface** (`Token`/`OnUnauthenticated`, already implemented by `cpTokenSource`) +
TLS (`newSchemeTransport` seam). Absorbs `cmd/spawnctl/intent.go` (`pollAndSign`,
`provisionWithIntent`, `intentClient`, `intentParams`) and a `BuildSessionOpenIntent` helper (dedups
the inline block at `main.go:273-289`). Method surface (extracted reusable cores):
`CreateSpawn`/`WaitActive`, `Resume`, `Fork`, `Migrate` (+ owner-sealed key travel), `SetModel`,
`List`, `Status`, `Delete`, `Stop`, `Session`. CLI keeps `driveFrames`/flags/rendering.

### TS SDK — `sdk/ts` (`@spawnery/client`), environment-neutral
Only `crypto.subtle` + `fetch` + `TextEncoder` (browser & Node ≥20). `createConnectTransport`
(`connect-web`, fetch) over the generated `SpawnService`, pluggable **`AuthProvider`**
(`getBearer` + optional `signPoP`/`refresh`) and **`KeyStore`** (browser `IDBKeyStore` / Node
`MemoryKeyStore` injected). Signing ports `web/src/auth/intent.ts` but marshals `IntentBody` with
**protobuf-es `toBinary`** (replacing `ProtoWriter`). Surfaces `ConnectError` (structured code+message).
`SpawnClient`: `createSpawn` (signs via concurrent `pollAndSign`), `resume`/`recreate`/`migrate`/`fork`,
`list`/`findSpawn`/`deleteSpawn`/`stopSpawn`/`listApps`, customization (`profiles`/`catalog`/`secrets`
— the sole secrets surface), `buildSessionOpenSignedIntentB64`.

### Consumer migrations
- **spawnctl** → thin CLI over `internal/client` (`authstate.go` PoP/refresh stays behind the
  token-source interface).
- **web** → `web/src/api/*` + `auth/*` + `keys/*` become thin wrappers over `@spawnery/client`
  (inject `IDBKeyStore`, zustand token provider, `import.meta.env` endpoints); no behavior change.
- **acceptance** → **delete `ApiDriver`**; SDK-based oracle/janitor/setup client whose `createSpawn`
  **signs** (fixes the blocker). Re-point `DriverCtx.api`, `harness/test.ts`, `SpawnRegistry`, `sweep`,
  `scenarios/wait.ts`, `scenarios/tenancy.ts` (retire `rawCreateSpawn` via structured errors),
  `fixtures/preflight.ts`, `tests/sessions/support.ts`; delete duplicated `auth/pop.ts`; **fold
  `MarketOracle`** into the SDK. No `· api` test arm — the client is an oracle/helper, not a
  driver-under-test.
- **e2e tests + authsvc** → adopt `internal/client`; delete the `intent_threading` re-impl.

### Critical wire-compat gate (first)
Switching TS `IntentBody` marshalling from `ProtoWriter` to protobuf-es makes Go↔TS byte-parity
load-bearing. Gate with a golden-vector spike (reuse `internal/intent/testdata` + `vectors_test.go`):
assert protobuf-es `toBinary` == Go `proto.Marshal` bytes and that a TS-built `SignedIntent` verifies
under `intent.VerifySig`. IntentBody has no maps → field-ordered serialization is deterministic and
should match. **Fallback if not:** keep protobuf-es for types/transport, retain `ProtoWriter` for
`IntentBody` only.

## Task graph (bd epic)
T0 foundation (buf TS + workspace + `sdk/ts` skeleton) → T1 wire-compat spike → {T2 Go SDK → T3
spawnctl, T4 e2e/authsvc} ∥ {T5 TS SDK core → T6 web, T7 acceptance}. Go and TS tracks disjoint →
parallel after T0/T1; T6/T7 disjoint dirs → parallel after T5. **T7 fixes the original blocker.**
Execute via the parallel-subagent Workflow (per-task worktrees, spec+quality review, serial
merge-back), per CLAUDE.md.

## Verification
- Go: `CGO_ENABLED=1 go test -race ./...` (incl. intent vectors), `golangci-lint` = 0, `spawnctl -detach` create.
- TS: `make gen` emits `sdk/ts/src/gen`; `sdk/ts` unit tests (signing vs vectors); web `tsc -b && vite build` + vitest; acceptance `tsc --noEmit` + vitest.
- E2E payoff: `scripts/e2e-vm/run.sh --profile fake` against the enforced VM — setup-create specs
  (`exec-exitcode`, `prompt-transcript.agent`, `delete`, `tenancy/*`) pass because the oracle signs.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from
the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*

**2026-07-06 (sp-lan2.5, T4 e2e/authsvc):** "e2e tests + authsvc adopt `internal/client`" only
holds for a narrow subset. Converted to the SDK: `internal/cp/e2e_test.go`
(`TestCPEndToEndStub`) and `internal/cp/devstack_e2e_test.go` (`TestDevStackSpawnE2E`) — both
self-contained, using only `CreateSpawn`/`WaitActive`/`List`/`Session`/`Stop`. Left raw, with
evidence:
- The other ~7 e2e-tagged CP test files (`fork`, `skill_ingest`, `tmux`, `profile_mcp_loadproof`,
  `acp`, `lifecycle`, `datafs_perms`) call RPCs off the SDK's curated surface (`CreateProfile`,
  `ForkSpawn`+response, `DeliverSecrets`, `GetSpawnNodeKey`, …) or share the raw-typed helper web
  (`waitActive`, `findSpawnGeneration`, `h2cClient`) with tests that need those RPCs — converting
  one entangled file would force duplicate SDK-typed helper variants instead of reducing them.
- `cmd/authsvc/main.go`'s CP client authenticates with a static `X-Spawnery-AS-Secret` header over
  plain Connect (not gRPC+Bearer) and calls `AuthorizeGitHubMint`/`SignalGitHubTokenRotated`, which
  aren't on the SDK's curated surface. `client.New` forces gRPC+Bearer — adopting it would change
  the wire protocol and break AS→CP auth. Left unchanged (one-line comment added in-code).
- `internal/cp/intent_threading_test.go`'s `goSubmitIntent` web is a white-box helper in package
  `cp` driving `*Server` directly (deterministic test JTI, `Secrets`/`onReady` hooks, a fake
  `NodeAccessToken`); the SDK's `pollAndSign` is unexported and has none of those hooks. Left
  unchanged (one-line comment added in-code).

Follow-ups needed to close the gap (not filed as bd issues from this worktree — no Dolt DB here;
file from the main repo):
1. Export a `client.PollAndSign` core (+ `SubmitOption`s for secrets/onReady/JTI override) so
   `goSubmitIntent` can delegate instead of re-implementing.
2. Give the SDK a server-to-server constructor (static-header auth, Connect protocol) and expose
   `AuthorizeGitHubMint`/`SignalGitHubTokenRotated` for authsvc to adopt.
3. Expose the SDK's transport/dial seam (or a `NewFromRPC(cpv1connect.SpawnServiceClient)`
   constructor) so the raw-RPC e2e tests can drop `h2cClient`/`bearer` while keeping raw RPC
   access.

Verification run (in `dev-spawnery`): `CGO_ENABLED=1 go test -race ./internal/cp/... ./cmd/authsvc/...`
green; `CGO_ENABLED=1 go test -tags e2e -run 'NONE' ./internal/cp/...` compiles clean (e2e bodies
not executed — no Docker/images provisioned in this lane); `golangci-lint run` (untagged and
`--build-tags e2e`) = 0 issues; `gofmt -l` clean.
