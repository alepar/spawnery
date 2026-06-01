# Scheduler Routing by Node Class + Author-Self-Host Rule (sp-t5p) — Design

**Bead:** `sp-t5p` · builds on node-class (`sp-2as`) + the floor (`sp-rpa`/`sp-ach`).
**Status:** Draft v1 — autonomous track (user-chosen). **Date:** 2026-06-01

## 0. Policy (user decision 2026-06-01, flipped)

An **unverified** app version may run **only on a self-hosted node owned by the app's author**
(creator). Rationale: unblocks the author's dev iteration with no review and **zero added risk** —
their own self-hosted node already has all the author's data; nothing new is exposed. `reviewed` and
`scanned` versions route normally (any node, incl. cloud).

## 1. Scope

**In:**
1. **Node→owner propagation** (mirror `sp-2as`'s class plumbing): node reports `node_owner` at
   registration; CP records it on `registry.Node.Owner`.
2. **Placement constraint** in the registry/scheduler: `Pick` can require a node class + owner.
3. **CreateSpawn routing policy:** the resolved version's **tier** drives placement; `unverified`
   requires caller==creator + a self-hosted node owned by the caller.

**Out:** moving nodes into the durable store (registry stays in-memory) · multi-tenant node sharing ·
UI changes to spawn a specific unverified version (the web Detail "Spawn" sends no version → latest
reviewed; spawning unverified from the UI needs a version selector — a UI follow-up).

## 2. Node→owner propagation

- `node.proto` `Register` gains `string node_owner = 6;` (the self-hoster's owner-id; empty for cloud).
- `node.Config` gains `NodeOwner string`; the `Register` send includes it; `cmd/spawnlet` sets it
  from `NODE_OWNER` (default empty).
- `registry.Node` gains `Owner string`; the CP `Register` handler sets it (`server.go runNode`).

## 3. Placement (`registry` + `scheduler`)

- `registry.Placement{ Class string; Owner string }` (empty field = unconstrained).
- `registry.Registry.PickFor(p Placement) *Node` — like `Pick` but a candidate must also satisfy
  `(p.Class=="" || n.Class==p.Class) && (p.Owner=="" || n.Owner==p.Owner)`. Keep `Pick()` as
  `PickFor(Placement{})` (existing callers/tests unchanged).
- `scheduler.Provision(ctx, id, appRef, model string, placement registry.Placement)` — uses
  `reg.PickFor(placement)`; if nil → `ResourceExhausted` with a placement-aware message.

## 4. CreateSpawn policy (`server.go`)

Resolve version:
- **empty `version`** → `LatestReviewed` (UNCHANGED — normal users get the latest reviewed version).
- **explicit `version`** → `GetVersion`; **drop the slice-3 reviewed-only rejection** — any tier is
  resolvable; its tier drives placement.

Compute placement from `ver.Tier`:
- `reviewed` / `scanned` → `registry.Placement{}` (any node; current behavior).
- `unverified` → the author-self-host rule:
  - `creator, _ := s.st.Apps().Creator(appID)`; if `caller != creator` →
    `PermissionDenied` ("only the app's author can run an unverified version").
  - placement = `registry.Placement{Class: "self-hosted", Owner: owner}` (owner == caller == creator).
- (`unspecified`/unknown tier → treat as unverified for safety.)

Pass placement to `Provision`. No eligible node → `ResourceExhausted` (message: "no eligible node —
connect your self-hosted node to run an unverified app").

## 5. Testing

- **registry** (`PickFor`): seeds nodes with class/owner/free; asserts class-filter, owner-filter,
  combined, and "no match → nil"; `Pick()` still returns max-free.
- **scheduler** (`Provision` placement): existing tests pass `registry.Placement{}` (unchanged
  behavior); add a case where a placement excludes the only node → `ResourceExhausted`.
- **CreateSpawn** (`server_test`/new): explicit `reviewed` version → spawns on any node (existing
  capSender, no class set → its class defaults… NOTE: test nodes are added via `reg.Add` directly, so
  set `Class`/`Owner` on the test `registry.Node` as needed). Unverified version by the creator with a
  matching self-hosted node → spawns; unverified by a non-creator → `PermissionDenied`; unverified by
  the creator but no self-hosted node owned by them → `ResourceExhausted`.

## 6. Decision log

| # | Decision | Choice |
|---|---|---|
| T.1 | Unverified placement | self-hosted node owned by the author; caller must == creator |
| T.2 | scanned/reviewed | route anywhere (scanned passed the automated scanner) |
| T.3 | Default version | empty → `LatestReviewed` (unchanged); explicit version → any tier (placement-gated) |
| T.4 | Node owner | `node_owner` on Register (env `NODE_OWNER`, empty=cloud); `registry.Node.Owner` |
| T.5 | Pick API | add `PickFor(Placement)`; `Pick()` = `PickFor({})` (no caller churn) |
| T.6 | Provision | gains a `placement` param (sole prod caller is CreateSpawn) |
