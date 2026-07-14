export const meta = {
  name: 'client-sdk-epic',
  description: 'Implement client-SDK epic T2-T7: Go SDK + TS SDK + consumer migrations (plan→implement→2 reviews→fixes→serial gated merge to feat/client-sdk)',
  phases: [
    { title: 'T2 Go SDK' },
    { title: 'T5 TS SDK' },
    { title: 'T3 spawnctl' },
    { title: 'T4 e2e/authsvc' },
    { title: 'T6 web' },
    { title: 'T7 acceptance' },
  ],
}

const REPO = '/var/home/alepar/AleCode/spawnery'
const SPEC = 'docs/superpowers/specs/2026-07-06-client-sdk-signing-design.md'
const INTEGRATION = 'feat/client-sdk' // all task branches merge here (NOT master)

// Shared guidance every agent gets.
const COMMON = `
Main repo: ${REPO} (module path "spawnery"). Branch base: ${INTEGRATION}. Design spec: ${SPEC} — READ IT.
Context from the epic's exploration (already done, do not redo): the A4 signing is proven byte- and
sig-compatible Go<->TS (T1 spike). protobuf-es toBinary(IntentBody) == Go proto.Marshal; a TS-signed
intent verifies under Go intent.VerifySig. sdk/ts (@spawnery/client) is an npm-workspace package;
make gen regenerates Go+TS (protoc-gen-es -> sdk/ts/src/gen, gitignored). internal/intent stays put
(shared with the node verifier). connect-es v2 needs NO protoc-gen-connect-es (protobuf-es v2 emits
service descriptors; createClient consumes them).
BUILD/TEST ONLY IN THE dev-spawnery DISTROBOX, IN YOUR OWN CHECKOUT (a worktree agent MUST cd to its
own worktree root, NOT the main repo). Pattern (run from your checkout root so $(pwd) is correct):
  distrobox enter --root dev-spawnery -- bash -lc "cd \\"$(pwd)\\" && <your cmd>"
- Go: CGO_ENABLED=1 go test -race ./...  ; golangci-lint: GOTOOLCHAIN=go1.26.0 "$(go env GOPATH)/bin/golangci-lint" run ./...
- TS: a fresh worktree has NO node_modules/gen (gitignored) — first run \`npm install\` at the worktree
  root (workspace) then \`make gen\`; then per-package tsc/vitest/vite build. buf is at ~/go/bin/buf
  (add to PATH). A prod web vite build needs VITE_CP_ORIGIN + VITE_AS_ORIGIN set.
NEVER edit gen/ by hand. NEVER push to master. Keep changes to the task's file set; do not drift.
`

const IMPL_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['branch', 'summary', 'testsRun', 'status'],
  properties: {
    branch: { type: 'string', description: 'the git branch the work was committed to' },
    summary: { type: 'string' },
    testsRun: { type: 'string', description: 'exact build/test commands run and their result' },
    status: { enum: ['DONE', 'DONE_WITH_CONCERNS', 'BLOCKED'] },
    concerns: { type: 'string' },
  },
}
const REVIEW_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['pass', 'issues'],
  properties: {
    pass: { type: 'boolean' },
    issues: { type: 'array', items: { type: 'string' } },
  },
}
const MERGE_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['merged', 'gatesPassed', 'detail'],
  properties: {
    merged: { type: 'boolean' },
    gatesPassed: { type: 'boolean' },
    detail: { type: 'string' },
  },
}

// Serialize merge integrators (one merge to the integration branch at a time).
let mergeChain = Promise.resolve()
function serialMerge(fn) {
  const run = mergeChain.then(fn, fn)
  mergeChain = run.then(() => {}, () => {})
  return run
}

async function runTask(t) {
  // 1. Plan (opus)
  const plan = await agent(
    `You are the PLANNER for ${t.id} — ${t.title}.\n${COMMON}\nTask brief:\n${t.brief}\nFile set: ${t.files}\n` +
      `Write a focused, concrete implementation plan: the files to create/edit, the approach, and the exact tests/gates to run. Be specific about reusing existing code (cite paths). Return the plan text only.`,
    { phase: t.phase, model: 'opus', label: `plan:${t.id}` },
  )
  if (!plan) return { id: t.id, ok: false, why: 'planner died' }

  // 2. Implement in an isolated worktree (sonnet)
  let impl = await agent(
    `You are the IMPLEMENTER for ${t.id} — ${t.title}. You are in an isolated git worktree branched from ${INTEGRATION}.\n${COMMON}\n` +
      `Plan to follow:\n${plan}\n\nImplement it fully. Create a branch feat/${t.id} in this worktree, commit your work there (use git commit --no-verify). Run the task's build/test gates IN THE DISTROBOX and report the exact commands+results. Do NOT merge. Report your branch name.`,
    { phase: t.phase, model: 'sonnet', isolation: 'worktree', label: `impl:${t.id}`, schema: IMPL_SCHEMA },
  )
  if (!impl || impl.status === 'BLOCKED') return { id: t.id, ok: false, why: 'implementer blocked: ' + (impl?.concerns || 'died') }

  // 3+4. Reviews (opus) — spec compliance then code quality; bounded fix loop (<=2)
  for (let round = 0; round < 2; round++) {
    const spec = await agent(
      `You are the SPEC-COMPLIANCE REVIEWER for ${t.id} on branch ${impl.branch}.\n${COMMON}\nTask brief:\n${t.brief}\n` +
        `Check the committed diff on ${impl.branch} vs ${INTEGRATION} implements the brief+spec with nothing missing and nothing out-of-scope. Read the actual code. Return pass + issues.`,
      { phase: t.phase, model: 'opus', label: `spec:${t.id}`, schema: REVIEW_SCHEMA },
    )
    const quality = await agent(
      `You are the CODE-QUALITY REVIEWER for ${t.id} on branch ${impl.branch}.\n${COMMON}\n` +
        `Review the committed diff for correctness, matching codebase conventions, error handling, and that build/test gates genuinely pass (re-run them in the distrobox if unsure). Return pass + issues.`,
      { phase: t.phase, model: 'opus', label: `quality:${t.id}`, schema: REVIEW_SCHEMA },
    )
    const issues = [...(spec?.issues || []), ...(quality?.issues || [])]
    if ((spec?.pass ?? false) && (quality?.pass ?? false)) break
    if (round === 1) return { id: t.id, ok: false, why: 'reviews failed after fixes: ' + issues.join('; ') }
    // fix (sonnet, same worktree branch)
    const fix = await agent(
      `You are the FIXER for ${t.id} on branch ${impl.branch} (its worktree still exists).\n${COMMON}\n` +
        `Address these review issues, commit the fixes to ${impl.branch}, re-run the gates in the distrobox:\n- ${issues.join('\n- ')}\nReport branch + gate results.`,
      { phase: t.phase, model: 'sonnet', isolation: 'worktree', label: `fix:${t.id}`, schema: IMPL_SCHEMA },
    )
    if (fix?.branch) impl = fix
  }

  // 5. Serial merge integrator (sonnet) — merge to integration branch, full gates, bd close, push
  const merge = await serialMerge(() =>
    agent(
      `You are the MERGE INTEGRATOR for ${t.id}. In the MAIN repo (${REPO}, not a worktree):\n${COMMON}\n` +
        `1) git checkout ${INTEGRATION} && git pull --rebase (if a remote exists) \n2) git merge --no-ff ${impl.branch}\n` +
        `3) Run the FULL relevant gates in the distrobox (Go: build + go test -race + golangci-lint if Go changed; TS: make gen + affected tsc/vitest/vite build). Resolve gen/ conflicts by re-running make gen. If gates fail, fix minimally or report gatesPassed:false.\n` +
        `4) Only if gates pass: run \`bd close ${t.id}\` and \`bd dolt push\` from ${REPO}, then \`git push origin ${INTEGRATION}\`.\n` +
        `5) Clean up the worktree for ${impl.branch}. Report merged/gatesPassed/detail.`,
      { model: 'sonnet', label: `merge:${t.id}`, schema: MERGE_SCHEMA },
    ),
  )
  return { id: t.id, ok: !!(merge?.merged && merge?.gatesPassed), why: merge?.detail || 'merge failed', merge }
}

// ---- The DAG ----
const T2 = {
  id: 'sp-lan2.3', phase: 'T2 Go SDK', title: 'Go SDK internal/client',
  files: 'internal/client/* (new); reads internal/intent, cmd/spawnctl/intent.go',
  brief: `Create internal/client: a Go client SDK wrapping cpv1connect.SpawnServiceClient, parameterized by endpoint + a token-source interface (Token(ctx)/OnUnauthenticated(ctx), already satisfied by spawnctl's cpTokenSource) + a TLS config (the newSchemeTransport(tlsConf) seam in cmd/spawnctl/main.go). Absorb cmd/spawnctl/intent.go VERBATIM (pollAndSign, provisionWithIntent, intentClient, intentParams) minus the log.Printf (surface via returned error/callback). Add BuildSessionOpenIntent(spawnID, generation) dedup helper (currently inline in main.go ~273-289). Extract the reusable client cores from runCP/fork.go/move.go/resume.go/setmodel.go/list.go/status.go into SDK methods: CreateSpawn+WaitActive, Resume, Fork, Migrate (incl. the owner-sealed key-travel: deliverOwnerSealedJournalKeys + internal/secrets/*), SetModel, List, Status, Delete, Stop, Session (bidi stream adapter). Do NOT move driveFrames/flags/fmt/stdout — those stay in spawnctl (T3). Do NOT change cmd/spawnctl yet (T3 migrates it). Add unit tests. Gates: CGO_ENABLED=1 go test -race ./internal/client/... and go build ./..., golangci-lint clean.`,
}
const T5 = {
  id: 'sp-lan2.6', phase: 'T5 TS SDK', title: 'TS SDK core @spawnery/client',
  files: 'sdk/ts/src/*',
  brief: `Build the @spawnery/client SDK core (env-neutral: crypto.subtle + fetch + TextEncoder only). Transport: createConnectTransport (@connectrpc/connect-web, fetch) over the generated SpawnService, with a pluggable AuthProvider (getBearer(): Promise<string>, optional signPoP(rth)+refresh()) and a pluggable KeyStore interface (browser IDBKeyStore / Node MemoryKeyStore stay app-side). Surface ConnectError (structured code+message). Signing: PORT web/src/auth/intent.ts but marshal IntentBody with protobuf-es toBinary (drop ProtoWriter — T1 proved byte-parity); keep signP1363/exportSpkiDer/sessionKeyHash + keys/{der,hkdf,encoding} + pop. SpawnClient methods: createSpawn (runs pollAndSign concurrently -> signs), resume/recreate/migrate/fork (sign), list/findSpawn/deleteSpawn(id,destroyData)/stopSpawn/listApps, customization (profiles/catalog/secrets), buildSessionOpenSignedIntentB64. Unit-test signing against internal/intent/testdata/intent_vectors.json (the committed golden) using node --import tsx --test. Do NOT modify web/ or acceptance/ yet (T6/T7). Gates: make gen; npm run -w @spawnery/client build (tsc) clean; node --import tsx --test the SDK tests green.`,
}
const T3 = {
  id: 'sp-lan2.4', phase: 'T3 spawnctl', title: 'spawnctl -> internal/client',
  files: 'cmd/spawnctl/*.go',
  brief: `Migrate spawnctl subcommands to consume internal/client (from T2, now on ${INTEGRATION}). Replace the open-coded pollAndSign/provisionWithIntent/client-construction/lifecycle-RPC cores with SDK calls. Keep authstate.go (auth.json/PoP refresh) behind the token-source interface the SDK accepts, and keep driveFrames/driveACP, flag parsing, and all rendering (fmt/tabwriter/os.Stdout) in the CLI. Behavior must be identical (incl. the -detach create path). Delete now-dead duplicated code (e.g. cmd/spawnctl/intent.go if fully absorbed). Gates: go build ./..., CGO_ENABLED=1 go test -race ./cmd/spawnctl/..., golangci-lint clean, and \`bin/spawnctl -detach\` create still works against a dev CP if reachable (else note).`,
}
const T4 = {
  id: 'sp-lan2.5', phase: 'T4 e2e/authsvc', title: 'e2e tests + authsvc -> Go SDK',
  files: 'internal/cp/*_test.go, cmd/authsvc/main.go',
  brief: `Adopt internal/client (from T2) in the CP e2e tests and authsvc. Delete internal/cp/intent_threading_test.go's hand-rolled pollAndSign re-implementation and use the SDK. Repoint e2e tests that hand-build cpv1connect.NewSpawnServiceClient (e2e_test.go, devstack_e2e_test.go, lifecycle_e2e_test.go, fork_e2e_test.go, etc.) + cmd/authsvc/main.go's client onto the SDK constructor where it reduces duplication. Do NOT weaken test coverage. Gates: CGO_ENABLED=1 go test -race ./internal/cp/... ./cmd/authsvc/... (e2e-tagged tests need their deps; run the hermetic set + build-tag-compile the rest), golangci-lint clean.`,
}
const T6 = {
  id: 'sp-lan2.7', phase: 'T6 web', title: 'web -> @spawnery/client',
  files: 'web/src/api/*, web/src/auth/*, web/src/keys/*, web/package.json',
  brief: `Make the web SPA consume @spawnery/client (from T5, on ${INTEGRATION}). Add "@spawnery/client" as a workspace dep in web/package.json and fix any install/build call sites (run.sh/build-base.sh do \`cd web && npm ci\` — ensure that still works; the SDK must be built/available, so add an SDK build step or use the workspace symlink + Vite alias as needed). Turn web/src/api/* + auth/{intent,protobuf,keypair,pop,refresh} + keys/* into thin wrappers/re-exports over the SDK, injecting the browser-specific bits: IDBKeyStore, the zustand token provider (session.ts getAccessToken), and config/endpoints.ts (import.meta.env). Delete the now-duplicated hand-rolled signing/protobuf/pop code. NO behavior change. Gates: make gen; VITE_CP_ORIGIN=https://x.e2e.test VITE_AS_ORIGIN=https://x.e2e.test npx vite build (in web) passes; npx tsc -b passes; npm run -w spawnery-web test (vitest) green. Note the known sp-rxvb bug (session-open intent on WS bind) — if the SDK's session-open helper makes it trivial to fix, do so; else leave it.`,
}
const T7 = {
  id: 'sp-lan2.8', phase: 'T7 acceptance', title: 'acceptance -> SDK (delete ApiDriver)',
  files: 'acceptance/src/*, acceptance/tests/**, acceptance/package.json',
  brief: `Replace the acceptance ApiDriver with an @spawnery/client-based oracle/janitor whose createSpawn SIGNS (fixes the enforced-node blocker — the whole point). Add "@spawnery/client" as a workspace dep + fix install call sites (\`cd acceptance && npm ci\` in run.sh must still work). DELETE acceptance/src/drivers/api.ts; build a thin SDK-backed client exposing the roles the harness needs: signing createSpawn(with name/model/profileId), deleteSpawn(id,destroyData), listSpawns({spawnId,name,status,createdAt,parentSpawnId}), findSpawn, listApps, and customization (profiles/catalog/secrets). Re-point: src/drivers/types.ts (DriverCtx.api), src/harness/test.ts (api fixture + ctx + toContainSpawn), src/harness/spawn-registry.ts, src/fixtures/{sweep,preflight}.ts, src/scenarios/wait.ts, src/scenarios/tenancy.ts (retire rawCreateSpawn using the SDK's structured 429 error), tests/sessions/support.ts (waitActiveApi), src/harness/global-setup.ts + global-teardown.ts. DELETE the duplicated acceptance/src/auth/pop.ts + the copy-pasted dev-token key (use the SDK). Fold MarketOracle into the SDK if clean. There is NO ·api test arm — the client stays an oracle/helper. Use OAuth-PoP/dev-token via the SDK's AuthProvider (the existing AuthStrategy). Gates: make gen; npx tsc --noEmit (in acceptance) passes; npm run -w spawnery-acceptance test (vitest) green. (The live-VM run is a separate manual step, not a gate here.)`,
}

log('Client-SDK epic: T2 (Go) ∥ T5 (TS) foundational, then T3/T4 ∥ T6/T7.')

// Two independent chains run in parallel. Within a chain: foundational task must MERGE before its dependents start (so their worktrees fork from the updated integration branch).
const goChain = (async () => {
  const r2 = await runTask(T2)
  if (!r2.ok) { log(`T2 failed (${r2.why}) — skipping T3/T4`); return [r2] }
  const deps = await parallel([() => runTask(T3), () => runTask(T4)])
  return [r2, ...deps.filter(Boolean)]
})()

const tsChain = (async () => {
  const r5 = await runTask(T5)
  if (!r5.ok) { log(`T5 failed (${r5.why}) — skipping T6/T7`); return [r5] }
  const deps = await parallel([() => runTask(T6), () => runTask(T7)])
  return [r5, ...deps.filter(Boolean)]
})()

const [go, ts] = await parallel([() => goChain, () => tsChain])
const all = [...(go || []), ...(ts || [])]
const ok = all.filter((r) => r && r.ok).map((r) => r.id)
const bad = all.filter((r) => r && !r.ok).map((r) => `${r.id}: ${r.why}`)
log(`epic done. merged: ${ok.join(', ') || 'none'}. failed: ${bad.join(' | ') || 'none'}`)
return { merged: ok, failed: bad }
