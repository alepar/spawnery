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
- [Web Epic — SPA Delivery + Device Keys + Move-to](2026-06-11-web-epic-spa-delivery-device-keys-migration-design.md) — canonical pinned static origin (signed releases, SRI, strict CSP; closes roast C2), web-first device ceremony/enrollment/management (WebCrypto non-extractable, AS-stored hash-chained device set), and the spawn-scoped "Move to…" migration modal with the in-browser journal-key re-seal leg.
- [Web Epic Adversarial Review](2026-06-11-web-epic-adversarial-review.md) — 21 panel-confirmed major findings (WM1–WM21: device-set CAS/anti-rollback, non-atomic re-seal, false revert promise, enrollment SAS, TOFU anchor, in-browser node PKI, canonical encoding/u64 interop, IndexedDB eviction, CSP-vs-bundle reality, supply chain) + 8 minors + 5 refuted-with-reasoning; amendment checklist per spec section.
- [Auth & Identity](2026-06-11-auth-identity-design.md) — GitHub login at the AS (IdP), bespoke Ed25519-signed access tokens verified offline by CP **and nodes**, rotating refresh families with reuse-detection, cnf-bound session keys + client-signed intents on node-verified ops (SessionOpen + the four StartSpawn-causers); accountId keys everything incl. device-set logs; registration kill-switch; closes web-epic WM17.
- [Auth & Identity Adversarial Review](2026-06-12-auth-identity-adversarial-review.md) — 1 critical (AC1: SignedIntent artifact undefined/unbindable) + 13 majors (resume-leg signing oracle, cross-origin /refresh cookie, reuse-kill races, no kid/rotation, refresh-PoP claim false, IndexedDB key loss, OOB device-code antipattern, missing OAuth state, githubSub undefined, revocation severs nothing live, cnf wire facts unpinned, dev/prod pipeline divergence, AS data-loss story absent) + minors + 3 refuted-with-reasoning.
- [Writable Rootfs Survival Research Brief (sp-ei4.1)](2026-06-12-writable-rootfs-survival-research-brief.md) — deep-research prompt for survivable writable agent rootfs: delta-as-OCI-layers + user namespaces, pinned base images, same-node survival sans journal, delta-only migration via Kopia.
- [Writable Rootfs Survival Research Results (sp-ei4.1.2)](2026-06-12-writable-rootfs-survival-research-results.md) — verified findings (72 confirmed claims, 7 sections): userns makes apt/chown work via a ≥65536-wide subid map (caps are automatic, not the constraint); delta-as-OCI-layer via `docker commit`/containerd `diff`; runsc is the strong multi-tenant sandbox; upperdir-harvesting rejected (CAP_SYS_ADMIN-on-restore trap). Open: CRI diff mechanics, runsc `--overlay2` visibility, userns uid-shift, egress-floor netns ownership.
- [CRI Thin Byte-Bridge Adapter (sp-j5b)](2026-06-03-cri-thin-adapter-pump-design.md) — collapse the in-pod adapter to a byte-bridge; the node pump owns ACP work.
- [Node Readiness Probe](2026-06-03-node-readiness-probe-design.md) — "green = actually ready" (not just `mgr.Create` returned).

## State & storage internals
- [Per-Mount Data Backends](2026-05-29-data-mounts-design.md) — N named data mounts under `/app`, each bound to a `StorageBackend` (supersedes single `/data`).
- [State/DAO Research Brief](2026-05-31-state-dao-layer-research-brief.md) — requirements for moving CP state off in-memory maps.
- [CP State / DAO Layer](2026-05-31-state-dao-layer-design.md) — persistent control-plane state/DAO design.
- [CP Store Driver (sp-ylw)](2026-06-01-cp-store-driver-sp-ylw.md) — store driver selection (sqlite modernc vs postgres pgx; goose migrations).
- [Inventory Reconcile Adopt (sp-537t)](2026-06-09-inventory-reconcile-adopt-design.md) — gap-closing impl of DAO §6.2: adopt/flip-back + orphan-stop arms.
- [Tiered Storage & Migration Research Brief](2026-06-10-tiered-storage-migration-research-brief.md) — deep-research prompt for the transient journaled tier + local↔cloud data-plane spawn migration.
- [Tiered Storage & Migration Research (Results + Merged Synthesis)](2026-06-10-tiered-storage-migration-research-results.md) — verified findings + merged conclusions of both runs: watcher+rescan capture, embedded-Kopia journal (repo-per-mount), Garage sink, staged adoption; Mutagen-as-is + Duplicacy rejections.
- [Tiered Storage & Migration Research (Cloud Run)](2026-06-10-tiered-storage-migration-research-results-cloud.md) — parallel cloud-session report, imported verbatim; fills the in-session run's gaps (Kopia/restic, Garage-vs-MinIO, fanotify/overlayfs, fencing theory, Coder/DevPod/Daytona/Modal).
- [Transient Tier — Kopia Journal + Migration](2026-06-10-transient-tier-kopia-journal-design.md) — per-spawn embedded-Kopia journal of all mounts (incl. `.git` + scratch) to Garage; watcher-triggered, generation-fenced; resume = pure Kopia restore; `MigrateSpawn` local↔cloud; owner-sealed keys (blocked on secret-delivery epic).

## Marketplace (E5/E6 slices)
- [E5 Slice 1 — Catalog Read Surface](2026-06-01-e5-catalog-read-surface-slice1.md) — the read side of the catalog (browse/search), pure-CP.
- [E5 Slice 2 — App-Version Registration](2026-06-01-e5-app-registration-slice2.md) — `RegisterAppVersion`, API (full manifest proto) as source of truth.
- [E5 Slice 3 — Version Selection & Pinning](2026-06-01-e5-version-selection-slice3.md) — choose/pin an app version at `CreateSpawn`.
- [E5 Slice 4 — Enrichment + Moderation](2026-06-01-e5-catalog-enrich-moderation-slice4.md) — catalog detail enrichment + listing takedown/relist.
- [E5 Slice 5 — Creator "My Apps"](2026-06-01-e5-my-apps-slice5.md) — creator management view (includes unlisted apps).
- [E6 Marketplace Web UI](2026-06-01-e6-marketplace-web-ui.md) — React marketplace surface (browse/detail/mine/publish).

## Spawn lifecycle & chat
- [Spawn Lifecycle](2026-05-31-spawn-lifecycle-design.md) — suspend/resume/persist; consistency protocol (hard-depends on E3 storage).
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
