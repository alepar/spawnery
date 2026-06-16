# Design Spec Index

One-line index of every design spec in this folder. **Consult the relevant entries before
designing a similar or adjacent feature** — most cross-cutting decisions (and their rationale)
were already made in one of these docs.

> **Keep this current:** whenever you add a new design spec to `docs/superpowers/specs/`, add a
> one-line entry here in the right section (same session, same commit). Implementation *plans*
> live in `docs/superpowers/plans/` and are **not** indexed here.

## System & scope
- [High-Level System Design](2026-05-26-spawnery-system-design.md) — the whole-platform architecture; every decision tagged MVP-now vs deferred (the master doc).
- [Demo MVP Scope](2026-05-28-spawnery-demo-mvp-scope.md) — the reduced "demo" cut: what ships vs what's deferred (the locked scope overlay other docs reference).
- [Demo Experiment Design](2026-05-29-spawnery-demo-experiment-design.md) — the demo framed as a retention experiment (hypothesis, cohort, metrics).
- [E11 App Ideas](2026-05-29-spawnery-e11-app-ideas.md) — candidate demo apps; the architecture is the product, apps are interchangeable demonstrations.

## Foundational epics (E0–E8)
- [E0 — Cross-component APIs & Contracts](2026-05-26-spawnery-e0-contracts-design.md) — the seams every other epic consumes (proto/RPC, manifest, channels, E2E key delivery).
- [E1 — Runtime & Orchestration Core](2026-05-27-spawnery-e1-runtime-core-design.md) — node agent, cold-start spawn lifecycle, pod model, scheduler/placement, isolation-backend seam.
- [E2 — Model Layer](2026-05-27-spawnery-e2-model-layer-design.md) — per-spawn inference sidecar (OpenAI-compat), managed gateway vs BYO-direct, key custody, metering, routing.
- [E3 — Storage Layer](2026-05-28-spawnery-e3-storage-design.md) — per-mount git-working-tree substrate, storage-backend adapters (GitHub/blob), credential custody/refresh, conflict handling.
- [E4 — Identity & Secrets](2026-05-28-spawnery-e4-identity-secrets-design.md) — OAuth (Google/GitHub), account/creator model, server-blind vault, BYO-key E2E delivery to the sidecar.
- [E5 — App Packaging & Catalog](2026-05-28-spawnery-e5-packaging-catalog-design.md) — manifest schema, versioning, trust tiers, registration, marketplace discovery.
- [E6 — Web Client](2026-05-27-spawnery-e6-web-client-design.md) — the production React SPA (chat, marketplace, spawn management) over Connect/ACP.
- [E7 — Launch Coach Repos](2026-05-28-spawnery-e7-launch-coaches-design.md) — the coached-app onboarding/launch-coach concept.
- [E8 — Trust, Safety & Audit](2026-05-28-spawnery-e8-trust-safety-audit-design.md) — app scanner, content-audit hook points, abuse containment.

## Isolation, networking & node
- [Egress Floor (sp-rpa)](2026-06-01-egress-floor-sp-rpa.md) — per-pod default-allow egress block-floor via iptables in the sidecar netns.
- [gVisor Research Brief](2026-06-01-gvisor-isolation-research-brief.md) — deep-research prompt for gVisor multi-tenant agent sandboxing.
- [gVisor Evaluation (Results)](2026-06-01-gvisor-isolation-research-results.md) — production security/systems evaluation of `runsc`.
- [runsc Pod-Backend Research Brief](2026-06-01-runsc-pod-backend-research-brief.md) — how to run a gVisor two-container pod from a Go daemon.
- [runsc → CRI Pod Backend](2026-06-01-runsc-cri-pod-backend-design.md) — runsc-on-containerd CRI pod backend design.
- [Phase-1 Isolation Hardening](2026-06-01-phase1-isolation-hardening.md) — isolation hardening + runsc readiness (egress-under-runsc fixes).
- [Per-Backend ACP Transport](2026-06-01-per-backend-transport-design.md) — rootless self-hosted transport; root required only for cloud nodes (egress floor).
- [Resource Limits + Quotas (sp-ach)](2026-06-01-resource-limits-quotas-sp-ach.md) — CPU/mem limits, per-user quotas, isolation-runtime selection.
- [Node-Class Propagation (sp-2as)](2026-06-01-node-class-propagation-sp-2as.md) — propagate node class (cloud|self-hosted) to the CP.
- [Scheduler Routing (sp-t5p)](2026-06-01-scheduler-routing-sp-t5p.md) — placement by node class + author-self-host rule for unverified apps.
- [Node Auth & Unified Identity](2026-06-05-node-auth-unified-identity-design.md) — node authentication, auth service, unified identity/tenancy model.
- [Owner-Sealed Secrets Research Brief](2026-06-10-owner-sealed-secrets-research-brief.md) — deep-research prompt for E2E user secrets (CP ciphertext-only, plaintext only at verified nodes) + key-agreement groundwork atop sp-ova.
- [Owner-Sealed Secrets Research (Results + Merged Synthesis)](2026-06-10-owner-sealed-secrets-research-results.md) — verified findings + merged conclusions of both runs: per-device-keypair custody (PRF Tier-2), cert-signed HPKE sub-keys, HPKE+AAD node leg, fingerprint-bound enrollment, never-persist invariant.
- [Owner-Sealed Secrets Research (Cloud Run)](2026-06-10-owner-sealed-secrets-research-results-cloud.md) — parallel cloud-session report, imported verbatim; fills the in-session gaps (identity binding, headless-delegation survey, format comparison, WebCrypto/Go realities) + recommended enrollment flow.
- [Owner-Sealed Secrets Design](2026-06-10-owner-sealed-secrets-design.md) — E2E secret custody/delivery: per-device X25519 keys + BIP-39 recovery, HPKE-everywhere envelopes, cert-signed node HPKE sub-keys, AAD context binding, fingerprint-bound enrollment; CP ciphertext-only (closes sp-gtm for secrets).
- [Storage + Secrets Adversarial Review](2026-06-10-storage-secrets-adversarial-review.md) — 23 panel-confirmed roast findings against both specs (2 critical: restore-pin C1, SPA-delivery C2) with per-section amendments + pre-impl spike/benchmark gates.
- [User Secrets Store — Owner-Online Auto-Injection (sp-7h6.1)](2026-06-14-user-secrets-store-design.md) — durable CP-blind secrets catalog + `CreateSecret`/`PutSecret`(CAS)/`GetSecret`/`ListSecrets` (unblocks the W3 sweep stub); a user secret = a `sensitive` `spawn_artifact`, **reusing** the existing artifact substrate (DeliverSecrets→InjectSecret→tmpfs, secretwait); owner-online delivery **folded into the A4 intent round-trip** (node sub-key in GetPendingIntent, sealed secrets in SubmitIntent→StartSpawn, node injects in the StartPod→StartAgent gap — no new CP state/deadlock); BYOK→sidecar control endpoint (pre-StartAgent, Unix-socket-hardened); **roast-hardened twice** (R1 REVISE, R2 BLOCK→rebuilt on the real substrate); surfaces a live journal-key AAD bug (sp-7h6.1.8) + the revocation no-op (sp-7h6.1.11); GitHub-App provisioning split to sp-v40s (linked sp-u53.1).
- [Web Epic — SPA Delivery + Device Keys + Move-to](2026-06-11-web-epic-spa-delivery-device-keys-migration-design.md) — canonical pinned static origin (signed releases, SRI, strict CSP; closes roast C2), web-first device ceremony/enrollment/management (WebCrypto non-extractable, AS-stored hash-chained device set), and the spawn-scoped "Move to…" migration modal with the in-browser journal-key re-seal leg.
- [Web Epic Adversarial Review](2026-06-11-web-epic-adversarial-review.md) — 21 panel-confirmed major findings (WM1–WM21: device-set CAS/anti-rollback, non-atomic re-seal, false revert promise, enrollment SAS, TOFU anchor, in-browser node PKI, canonical encoding/u64 interop, IndexedDB eviction, CSP-vs-bundle reality, supply chain) + 8 minors + 5 refuted-with-reasoning; amendment checklist per spec section.
- [Auth & Identity](2026-06-11-auth-identity-design.md) — GitHub login at the AS (IdP), bespoke Ed25519-signed access tokens verified offline by CP **and nodes**, rotating refresh families with reuse-detection, cnf-bound session keys + client-signed intents on node-verified ops (SessionOpen + the four StartSpawn-causers); accountId keys everything incl. device-set logs; registration kill-switch; closes web-epic WM17.
- [Auth & Identity Adversarial Review](2026-06-12-auth-identity-adversarial-review.md) — 1 critical (AC1: SignedIntent artifact undefined/unbindable) + 13 majors (resume-leg signing oracle, cross-origin /refresh cookie, reuse-kill races, no kid/rotation, refresh-PoP claim false, IndexedDB key loss, OOB device-code antipattern, missing OAuth state, githubSub undefined, revocation severs nothing live, cnf wire facts unpinned, dev/prod pipeline divergence, AS data-loss story absent) + minors + 3 refuted-with-reasoning.
- [Writable Rootfs Survival Research Brief (sp-ei4.1)](2026-06-12-writable-rootfs-survival-research-brief.md) — deep-research prompt for survivable writable agent rootfs: delta-as-OCI-layers + user namespaces, pinned base images, same-node survival sans journal, delta-only migration via Kopia.
- [Writable Rootfs Survival Research Results (sp-ei4.1.2)](2026-06-12-writable-rootfs-survival-research-results.md) — verified findings (72 confirmed claims, 7 sections): userns makes apt/chown work via a ≥65536-wide subid map (caps are automatic, not the constraint); delta-as-OCI-layer via `docker commit`/containerd `diff`; runsc is the strong multi-tenant sandbox; upperdir-harvesting rejected (CAP_SYS_ADMIN-on-restore trap). Open: CRI diff mechanics, runsc `--overlay2` visibility, userns uid-shift, egress-floor netns ownership.
- [Writable Rootfs Survival Design (sp-ei4.1.3)](2026-06-12-writable-rootfs-survival-design.md) — spike-verified design: Docker lane userns-remap + default caps + `docker commit` capture; runsc lane `overlay2=none` + sentry-native privilege (no kernel userns); one OCI delta artifact, pinned base digests, same-node survival, staged to Kopia migration; retires HARDEN_ROOTFS + the 0777 mount hack.
- [CRI Thin Byte-Bridge Adapter (sp-j5b)](2026-06-03-cri-thin-adapter-pump-design.md) — collapse the in-pod adapter to a byte-bridge; the node pump owns ACP work.
- [Node Readiness Probe](2026-06-03-node-readiness-probe-design.md) — "green = actually ready" (not just `mgr.Create` returned).

## State & storage internals
- [Per-Mount Data Backends](2026-05-29-data-mounts-design.md) — N named data mounts under `/app`, each bound to a `StorageBackend` (supersedes single `/data`).
- [GitHub Storage Backend — Impl (sp-u53.1)](2026-06-14-github-storage-backend-impl-design.md) — implements the `github:owner/repo` `storage.Backend`: per-mount factory dispatch, **user-to-server OAuth token consumed as a generic secret** (CP never mints — `sp-v40s`/`sp-7h6.1`), node-side provisioning + suspend backstop push to `spawn/<id>/<generation>/<branch>`, GitHub=permanent / Garage-journal=transient layering, E3 LWW-surfaced conflict; supersedes E3 §2a/§3's installation-token model. **Roasted → BLOCK (see review).**
- [GitHub Storage Backend — Adversarial Review](2026-06-14-github-storage-backend-adversarial-review.md) — roast of the above: **BLOCK** (2 blockers: undesigned node credential path contradicted by `secrets.go` plaintext-zeroing; backstop misses `git checkout -b` untracked branches) + 22 majors/minors + escalation E1 (AS refresh-token custody breaks "CP cannot read any token") + 13 spikes. CP-compromise property holds *within* `sp-u53.1` but has end-to-end cracks in `sp-v40s`/`sp-7h6.1`.
- [GitHub Credentials + Storage Backend — Unified Design](2026-06-14-github-credentials-and-storage-unified-design.md) — unified `sp-v40s` + `sp-u53.1` contract, revised 2026-06-16 after GitHub-App spikes: owner-online-to-bootstrap + node+AS-to-sustain custody (AS mint/refresh keeps App `client_secret` AS-side, node-retained refresh, installation-selection-scoped access tokens), single GitHub App with Spawnery-enforced create policy (no GitHub permission profiles), exact-repo credential helper as the normal in-spawn boundary, no-push-rails (agent owns real pushes; Spawnery writes only journal-excluded backstop machine refs under `refs/spawnery/backstop/<date>/<spawn>/<generation>/<branch>/<hash>`), Kopia-journal-is-durability, defined fail-closed outcomes; spike verdicts: create passes, selected-install new-repo coverage passes, refresh+`repository_id` does not narrow, revocation passes, scaled-AS nonce needs shared volatile store. **Round-3 (2026-06-16, §16): after a round-2 roast BLOCK, custody reworked to AS-custodial refresh + CP-coordinated fanout — node never holds the refresh token, AS is sole refresher (node-identity authZ), one shared single-active access token per link with an accepted per-refresh window, suspend backstop DEFERRED from MVP (journaled mounts required), git proxy is the tracked future upgrade.** status: round-3 credential path IMPLEMENTED (sp-v40s.10-.15, on feat/sp-v40s-u53-integration, gates green + review PASS); remaining: sp-u53.1 storage backend, .16/.17/.18/.19, backstop epic sp-u53.8; tags: github,secrets,storage.
- [GitHub Storage Backend — Adversarial Review (Round 2)](2026-06-16-github-storage-backend-adversarial-review-r2.md) — round-2 roast of the unified design: **BLOCK** (29 confirmed, 1 escalation). Found the prior blockers relabeled-not-resolved and the repo-scoping keystone a doc misreading (spike-confirmed: refresh+`repository_id` doesn't narrow). Root cause = node-retains-refresh posture (node-compromise blast radius F6, refresh-leak-to-agent F30, no rotation writeback F2). Drove the round-3 AS-custodial rework. tags: github,secrets,storage,roast.
- [State/DAO Research Brief](2026-05-31-state-dao-layer-research-brief.md) — requirements for moving CP state off in-memory maps.
- [CP State / DAO Layer](2026-05-31-state-dao-layer-design.md) — persistent control-plane state/DAO design.
- [CP Store Driver (sp-ylw)](2026-06-01-cp-store-driver-sp-ylw.md) — store driver selection (sqlite modernc vs postgres pgx; goose migrations).
- [Inventory Reconcile Adopt (sp-537t)](2026-06-09-inventory-reconcile-adopt-design.md) — gap-closing impl of DAO §6.2: adopt/flip-back + orphan-stop arms.
- [Spawn Transition Coordination (sp-u53.7)](2026-06-13-transition-coordination-design.md) — DB-claim lease + `status_seq` optimistic CAS as the single locking substrate; CP-authoritative ("no CP, no transition"), node = generation-fenced executor + reporter; explicit `Suspending`/`Resuming`, recovery sweep, drops the `inFlight` hack; supersedes the narrow `sp-u53.7.1`, absorbs `sp-csks`.
- [Tiered Storage & Migration Research Brief](2026-06-10-tiered-storage-migration-research-brief.md) — deep-research prompt for the transient journaled tier + local↔cloud data-plane spawn migration.
- [Tiered Storage & Migration Research (Results + Merged Synthesis)](2026-06-10-tiered-storage-migration-research-results.md) — verified findings + merged conclusions of both runs: watcher+rescan capture, embedded-Kopia journal (repo-per-mount), Garage sink, staged adoption; Mutagen-as-is + Duplicacy rejections.
- [Tiered Storage & Migration Research (Cloud Run)](2026-06-10-tiered-storage-migration-research-results-cloud.md) — parallel cloud-session report, imported verbatim; fills the in-session run's gaps (Kopia/restic, Garage-vs-MinIO, fanotify/overlayfs, fencing theory, Coder/DevPod/Daytona/Modal).
- [Transient Tier — Kopia Journal + Migration](2026-06-10-transient-tier-kopia-journal-design.md) — per-spawn embedded-Kopia journal of all mounts (incl. `.git` + scratch) to Garage; watcher-triggered, generation-fenced; resume = pure Kopia restore; `MigrateSpawn` local↔cloud; owner-sealed keys (blocked on secret-delivery epic).
- [Encrypted Migration Transfer Set](2026-06-13-encrypted-migration-transfer-set-design.md) — cross-epic bridge for `sp-ei4.1.13`: ceremony-first encrypted Garage transfer sets, typed rootfs artifacts keyed by `(spawn_id, generation)`, and target restore pins; implemented 2026-06-13; `sp-u53`, `sp-ei4.1`.
- [Spawn Fork (sp-li7h)](2026-06-14-spawn-fork-design.md) — clone a running spawn, both stay Active: `ForkSpawn` reuses the migration path but mints a new spawn id seeded into the fork's **own fully-isolated Kopia repo** (rehydrate + re-journal; full seed upload) — **v2 (roast BLOCK) dropped the shared-repo "dedup-free seed" → `sp-3y92`; v3 (roast REVISE) made the fork-point a pause-first single-pause capture** (warm pre-snapshot bounds it) to kill the v2 split-skew; holds the source's fork-point generation; idempotent `Forking` recovery + ordered failed-fork unwind + orphan-GC; cross-node = ceremony-first + source-side rehydrate + transfer-set fork variant; rootfs delta carries claude-code/codex conversation forward (truncate-torn-JSONL then `--continue`); opencode deferred (`sp-5h3.8`); status draft.
- [Spawn Fork — Adversarial Review (roast)](2026-06-14-spawn-fork-adversarial-review.md) — two roast passes: v1 → **BLOCK** (shared-repo lineage broke isolation / the per-`(spawn,gen)` delete-fence / Kopia maintenance; panel degraded 51/90) → drove isolated repos; v2 → **REVISE** (full panel 85/85, 0 blockers, 71 majors in 7 clusters — headline: the live/paused split-capture skew) → drove v3 pause-first capture + compensation/cross-node hardening.

## Marketplace (E5/E6 slices)
- [E5 Slice 1 — Catalog Read Surface](2026-06-01-e5-catalog-read-surface-slice1.md) — the read side of the catalog (browse/search), pure-CP.
- [E5 Slice 2 — App-Version Registration](2026-06-01-e5-app-registration-slice2.md) — `RegisterAppVersion`, API (full manifest proto) as source of truth.
- [E5 Slice 3 — Version Selection & Pinning](2026-06-01-e5-version-selection-slice3.md) — choose/pin an app version at `CreateSpawn`.
- [E5 Slice 4 — Enrichment + Moderation](2026-06-01-e5-catalog-enrich-moderation-slice4.md) — catalog detail enrichment + listing takedown/relist.
- [E5 Slice 5 — Creator "My Apps"](2026-06-01-e5-my-apps-slice5.md) — creator management view (includes unlisted apps).
- [E6 Marketplace Web UI](2026-06-01-e6-marketplace-web-ui.md) — React marketplace surface (browse/detail/mine/publish).

## Spawn lifecycle & chat
- [Spawn Lifecycle](2026-05-31-spawn-lifecycle-design.md) — suspend/resume/persist; consistency protocol (hard-depends on E3 storage).
- [Fail-Closed Suspend (sp-ei4.1.15)](2026-06-13-fail-closed-suspend-design.md) — abort suspend + keep the spawn ACTIVE if the journal snapshot fails: pause-snapshot gate before any teardown, `error` on `SuspendComplete`, node reorders gate→reap→finish, existing web toast.
- [Spawn Lifecycle UI](2026-06-02-spawn-lifecycle-ui-design.md) — spawns as first-class, named, multi-instance objects in the web UI.
- [Visible "Starting" Lifecycle](2026-06-02-spawn-starting-lifecycle-design.md) — non-blocking provision + surfaced "starting" status.
- [Suspend Clears Message History](2026-06-03-suspend-clears-message-history-design.md) — drop a suspended spawn's in-UI message history.
- [Turn-State + Prompt Queueing](2026-06-03-chat-turn-state-and-queueing-design.md) — per-spawn turn state + server-side prompt queueing.
- [Runtime Model Switching](2026-06-08-runtime-model-switch-design.md) — change a running spawn's model seamlessly (sidecar override; no restart, no context loss) via web + spawnctl.

## ACP transport, pump & transcripts
- [Node-Relay Transcript Replay](2026-06-02-node-relay-transcript-replay-design.md) — record + replay transcripts at the node relay.
- [Per-Spawn Pump + Multi-Client](2026-06-03-spawn-pump-multiclient-design.md) — long-lived per-spawn pump fanning out to multiple clients.
- [ACP Enrichment (sp-ufz)](2026-06-07-acp-enrichment-design.md) — decode/render more of what agents emit over ACP.
- [ACP Enrichment Store Restoration (sp-x8y4)](2026-06-09-acp-enrichment-store-restoration-design.md) — restore the enrichment cases the tabs-migration reducer dropped; wire panel controls.

## Agents & terminal
- [opencode Swap + Terminal](2026-06-05-opencode-swap-and-terminal-design.md) — swap goose→opencode; ACP as canonical protocol with concurrent TUI + web.
- [Tmux Terminal Mode](2026-06-06-tmux-terminal-mode-design.md) — tmux terminal mode + agent-container capability taxonomy.
- [Codex CLI Support](2026-06-08-codex-cli-support-design.md) — OpenAI Codex CLI as a selectable agent (tmux mode), model wired via the sidecar like claude-code.
- [Cross-Agent Installer Research (sp-l5sx/sp-1bia)](2026-06-14-cross-agent-installer-research-results.md) — ultradeep research grounding the artifact-injection substrate + universal install adapter: canonical-source→per-agent-emitter architecture, the launcher-clobber/idempotency/secret-indirection gotchas, per-agent (Claude/Codex/opencode/Hermes) format+scope reality.
- [Artifact-Injection + Cross-Agent Installer Design (sp-l5sx/sp-1bia)](2026-06-14-cross-agent-installer-design.md) — three-tier design: content-agnostic delivery substrate (sensitive→E2E relay, plain→staging tmpfs) under a standalone `agentinstall` CLI (canonical artifact→per-agent emitters, upsert+atomic, no-op+report), launcher-sequenced in-pod to avoid the config clobber.
- [Cross-Agent Installer Adversarial Review (roast)](2026-06-14-cross-agent-installer-adversarial-review.md) — BLOCK verdict: 26 confirmed findings (1 blocker: Claude `.mcp.json` won't load headlessly) clustered into 9 themes — secrets-for-MCP timing/M10 conflict, undefined substrate↔engine staging contract, by-ref vacuum, resume artifact loss, launcher integration, trust model — with 7 spikes + a 10-point amendment plan.
- [Cross-Agent Installer Adversarial Review v2 (roast)](2026-06-14-cross-agent-installer-adversarial-review-v2.md) — REVISE: 8 confirmed productization gaps (undesigned user library/selection; missing semantic→wire assembly layer; no mutation/uninstall/prune+provenance; emergent conflict precedence; no live-agent load validation) = the requirements list for the customization tool.
- [Profile Config Facet — Cross-Agent Settings Research](2026-06-14-profile-config-settings-research.md) — verified per-agent behavior-config surface (Claude/Codex/opencode/Hermes; goose unverified) + the normalized settings model for profiles: approvalPosture enum (the anchor) + allow/deny/disabled tools + instructions-as-managed-file; sandbox/inference/sampling EXCLUDED (launcher-managed); rest = native passthrough.
- [Profiles — The Customization Tool (v2)](2026-06-14-profiles-customization-tool-design.md) — named, web-managed bundles (skills/MCPs/plugins/configs/secrets) assembled onto a generic base image at spawn creation (no per-combo image); CP-side semantic→wire assembly, snapshot-at-create, sp-7h6.1 secret refs, normalized config (approvalPosture anchor), plugin 4th kind; closes the roast-v2 productization gaps.
- [Profiles Adversarial Review (roast)](2026-06-14-profiles-adversarial-review.md) — REVISE: 22 confirmed — config-normalization facet is fictional vs the built engine + ForbiddenConfigKeys exclusion unenforced (security); owner-online-every-resume breaks availability; managed-by marker unencoded; instructions clobber CLAUDE.md memory; plugins headless-hostile. Revision plan + 3 forks.
- [Profiles — Gating Spike Results](2026-06-14-profiles-spike-results.md) — empirical (claude/codex/opencode): MCP load-proof CONFIRMED end-to-end; plugins headless-viable on all 3 (no trust gate); approvalPosture realized (no launch flag, auto egress-safe but model-tier-gated, default=yolo); commands claude+opencode-native + codex via experimental execpolicy; per-agent capability/lossiness signal.

## Web UI surface
- [UI Framework Adoption](2026-05-30-web-ui-framework-adoption-design.md) — adopt React 19 + Tailwind v4 + shadcn/ui (Radix).
- [Web ACP Client (demo)](2026-05-30-web-acp-client-design.md) — browser equivalent of `spawnctl`; seeds the E6 web client.
- [Web E2E (Playwright)](2026-05-30-web-e2e-playwright-design.md) — browser end-to-end testing with Playwright vs a stub.
- [WS Status Indicator](2026-06-02-ws-status-indicator-design.md) — connection-status indicator in the chat header.
- [WS Reconnect (partysocket)](2026-06-03-ws-reconnect-partysocket-design.md) — WebSocket reconnect + escalating connect-timeout.
- [URL History (sp-jpn)](2026-06-07-sp-jpn-url-history-design.md) — URL + `document.title` reflect the current view (add routing).
- [Multi-Session Tabs (sp-npxq)](2026-06-08-sp-npxq-multi-session-tabs-design.md) — multiple sessions per spawn via a spawn-view tab bar.
- [Terminal Appearance Settings](2026-06-08-terminal-appearance-settings-design.md) — xterm.js themes + fonts for the web terminal.
- [Sidebar Lifecycle Action](2026-06-08-sidebar-lifecycle-action-follows-status-design.md) — kebab menu action follows actual spawn status (wires RecreateSpawn for unreachable/error).

## Dev infrastructure & vertical slices
- [Spawnlet Slice](2026-05-29-spawnlet-slice-design.md) — the first end-to-end node (spawnlet) vertical slice.
- [CP Mediation Slice](2026-05-30-cp-mediation-slice-design.md) — control plane relaying between the web client and spawnlet.
- [Make/Just Dev-Stack](2026-05-30-make-just-dev-stack-design.md) — the `make` + `just` build/codegen/images + dev-run recipe ecosystem.

## Eval
- [Eval Harness MVP](2026-06-03-eval-harness-mvp-design.md) — creator-private "Test Your App" measurement slice (roles = spawns, judge = E8 scanner).
