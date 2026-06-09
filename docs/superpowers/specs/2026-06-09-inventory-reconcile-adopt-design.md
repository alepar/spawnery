# Inventory reconciliation: adopt + orphan arms

**Date:** 2026-06-09
**Epic:** sp-537t
**Status:** Approved 2026-06-09

## Problem

`reconcileInventory` (`internal/cp/server.go`) implements only one of the three arms the CP
state/DAO design specifies (2026-05-31-state-dao-layer-design.md §6.2 "Inventory reconciliation"):
it marks unreported active spawns `unreachable`. The other two arms never landed, so:

- a **returning node** (CP restart, network blip, node restart with surviving pods) reports its
  running `(spawn_id, gen)` inventory and the CP ignores it — spawns stay `unreachable` forever
  even though their pods are alive; the only recovery is a user-driven `RecreateSpawn`, which
  needlessly destroys a healthy container. The store was built for this: `store.Adopt` exists
  (gen-fenced node_id rebind) but has no production caller, and `MarkUnreachable` deliberately
  keeps the live container row "(adopt arm needs it)".
- an **orphaned pod** (suspended/deleted/superseded-gen spawn still running on a node) is never
  told to stop.

## Design

This is a gap-closing implementation of DAO spec §6.2 — that section remains the authoritative
design. Three arms, run on `Register` and on every `Heartbeat` (both already call
`reconcileInventory`), each idempotent:

1. **Adopt** (new): reported `(spawn_id, gen)` matches a live container row →
   `store.Adopt(id, nodeID, gen)` (rebinds `node_id`; gen fence makes superseded reports a
   no-op `ErrConflict` → fall through to the orphan arm) and `rt.Bind(spawnID, nodeID, sender)`
   when the route is unbound. If the spawn's status is `unreachable`, flip it back to `active`
   (the Wait→adopt path) via a store method gen-fenced against the live container. **Not**
   `MarkRecovered` — that flag means "recovered from unclean shutdown" (RecreateSpawn's concern,
   sp-unhh); adopt is a clean rebind.
2. **Orphan** (new): reported `(spawn_id, gen)` with **no** matching live row (suspended,
   deleted, errored, or superseded generation after recreate) → send `StopSpawn(spawn_id, gen)`
   to the reporting node. The gen fence on the node side means this can only kill the stale pod,
   never a current episode.
3. **Unreachable** (existing, unchanged): live `active` container the node does not report →
   `rt.Drop` + `MarkUnreachable`, live row kept.

### Decisions (deltas vs the §6.2 text)

- **No grace period.** §6.2 says "after grace"; the heartbeat inventory is built atomically
  node-side and `starting` spawns are already skipped (the case grace was guarding). Immediate
  marking has been the shipped behavior; keep it.
- **Heartbeat sweep, not just (re)connect.** §6.2 scopes reconciliation to node (re)connect; the
  shipped code also sweeps every heartbeat (catches mid-connection pod deaths). Keep it — with
  the adopt arm idempotent (route bound + status active ⇒ cheap no-op), per-heartbeat cost is a
  map diff.
- **Node mismatch is handled by Adopt itself.** A different node reporting the same
  `(spawn_id, gen)` rebinds `node_id` — that is `Adopt`'s documented contract ("rebind on
  reconnect"). No special casing.

## Testing

- Store: `unreachable→active` flip method honors the gen fence and only flips `unreachable`
  (an `active`/`suspended` spawn is untouched); `Adopt` gen-fence conflict path.
- Server (fake node sender): returning node's Register inventory re-adopts — route rebound,
  unreachable spawn flips active, no Stop sent; a superseded-gen report gets `StopSpawn` with
  the old gen; suspended/deleted spawn reported running gets `StopSpawn`; unreported active
  spawn still goes unreachable (existing behavior preserved).

## Out of scope

- Boot-time reconciliation / persist-marker probing (sp-f0jw — E3-gated, separate decision).
- `RecreateSpawn`'s `MarkRecovered` call (sp-unhh).
- Node-side suspend protocol (sp-a7fs, deferred while Scratch-only).
