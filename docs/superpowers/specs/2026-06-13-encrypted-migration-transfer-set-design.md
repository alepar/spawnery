# Encrypted Migration Transfer Set — Runtime/Storage Bridge

**Date:** 2026-06-13  
**Status:** Implemented
**Beads:** `sp-u53` blocker design for `sp-ei4.1.13`  
**Amends:** [Transient Tier — Kopia Journal + Migration](2026-06-10-transient-tier-kopia-journal-design.md),
[Writable Rootfs Survival](2026-06-12-writable-rootfs-survival-design.md),
[Owner-Sealed Secrets](2026-06-10-owner-sealed-secrets-design.md)

## Problem

`sp-ei4.1.13` is the last open child of the writable-rootfs epic. Stages 0-1 already make rootfs
writes survive same-node suspend/resume by keeping a node-local delta image. Stage 2 needs to move
that rootfs delta across nodes through the existing Kopia/Garage transient tier. The blocker is not
the GitHub/Drive persistent-storage adapters and not a permanent "owner-sealed spawn" mode. The
blocker is a migration-time transport contract: cross-node migration must produce a generation-pinned
transfer set whose Garage contents are encrypted and whose decryption key material is delivered only
through the owner-sealed secret-delivery path after the owner's key ceremony has completed.

## Decisions

1. **Rootfs deltas are keyed by `(spawn_id, generation)`.** A rootfs delta is never selected by
   "latest" and never addressed only by a local image tag. The generation is the source generation
   that produced the artifact.
2. **Owner-sealed is a migration operation, not a spawn trait.** A spawn may be node-local during
   normal same-node operation. When cross-node migration is requested, the system performs the owner
   key ceremony if needed, generates fresh migration snapshot/transfer encryption key material, and
   seals that key material for the verified source/target nodes through the owner-mediated path.
3. **No plaintext Garage contents.** Every transfer-set object written to Garage is encrypted before
   upload. The CP may coordinate metadata and ciphertext but must never receive plaintext artifact
   bytes or plaintext transfer keys.
4. **Ceremony first, suspend second.** If the owner has no enrolled device/key ceremony, migration
   stops in preflight. The source spawn is not suspended, captured, or torn down until the owner can
   authorize key delivery.
5. **Rootfs delta payloads are stored in Kopia-friendly form.** The delta should be fed to Kopia as
   an uncompressed OCI layer/layout artifact, not pre-gzipped, so Kopia's content-defined chunking can
   deduplicate successive deltas. Revisit unpacked-tree storage only if measurement says the artifact
   form deduplicates poorly.

## Readiness Gate

Define a gate named **Encrypted Migration Transport Ready**. `sp-ei4.1.13` must depend on this gate
instead of vaguely depending on all of `sp-u53`.

The gate is satisfied when:

- The owner key ceremony/device set is implemented enough for the caller initiating migration to
  prove it can unseal/reseal transfer keys.
- Migration preflight checks ceremony readiness before source suspend.
- The journal/transfer layer can store and retrieve non-mount artifacts keyed by
  `(spawn_id, generation)`.
- Fresh transfer encryption key material is generated at migration time, stored only as owner-sealed
  ciphertext at the CP, and delivered through `DeliverSecrets` to the source node before encrypted
  capture and to the verified target node before restore.
- Target restore is pinned to the CP-recorded source generation and artifact IDs.
- Replay/AAD gaps in owner-sealed delivery are either implemented for transfer keys or explicitly
  accepted with a follow-up bead before `sp-ei4.1.13` is closed.

Existing implementation notes show much of the owner-sealed journal-key path is present:
`GetJournalKeyCiphertext`, `PutJournalKeyCiphertext`, `GetSpawnNodeKey`, `DeliverSecrets`,
`journalkey`, node HPKE subkeys, and journal-key routing into `OwnerSealedCustody`. The audit still
has to verify current code against this gate, especially delivery replay hardening and device-registry
durability.

## Transfer Set

A migration creates a **transfer set**. It is recorded by the CP before source capture begins and is
the only restore authority for the target episode.

Transfer-set metadata:

- `spawn_id`
- `source_generation`
- `target_generation`
- `source_node_id`
- `target_node_id`
- `base_image_digest`
- pinned data-mount manifest IDs
- pinned rootfs artifact IDs
- transfer key ciphertext metadata
- terminal state: pending, capturing, key-delivery-pending, restoring, active, failed

Rootfs artifact descriptor:

- `spawn_id`
- `generation`
- `artifact_type = rootfs_delta`
- `artifact_id`
- `sequence` or `chain_index`
- `base_image_digest`
- `format = oci_layer_tar` or `oci_layout`
- `content_digest`
- `uncompressed_size`
- `producer_node_id`
- `producer_runtime = docker | cri-runsc`
- `created_at`

The node-local `DeltaImageRef` remains valid for same-node resume. Cross-node migration consumes the
pinned transfer-set artifact, not the local image tag.

## Journal Artifact API

The transient tier currently snapshots mount directories. Stage 2 needs a sibling API for typed
artifacts:

```go
type ArtifactDescriptor struct {
    SpawnID         string
    Generation      uint64
    Type            string
    ArtifactID      string
    Sequence        int
    BaseImageDigest string
    Format          string
    ContentDigest   string
    UncompressedSize int64
    ProducerNodeID  string
    ProducerRuntime string
    CreatedAt       time.Time
}

PutArtifact(ctx, spawnID string, generation uint64, desc ArtifactDescriptor, r io.Reader) (ArtifactDescriptor, error)
GetArtifact(ctx, spawnID string, generation uint64, artifactID string, w io.Writer) (ArtifactDescriptor, error)
ListArtifacts(ctx, spawnID string, generation uint64, typ string) ([]ArtifactDescriptor, error)
```

Normal restore uses only `GetArtifact` with CP-pinned IDs. `ListArtifacts` is for diagnostics and
crash recovery; it is not a "latest" selector.

## Migration Flow

1. User requests `Move to...` or `spawnctl move`.
2. CP/web/spawnctl preflight checks owner ceremony readiness and target eligibility.
3. If ceremony is missing, run the ceremony first. If it fails or is canceled, do not touch the
   source spawn.
4. Fresh transfer encryption key material is generated for this migration and delivered to the
   verified source node before capture/upload. The CP stores only ciphertext metadata.
5. CP records a transfer set with source generation, target generation, target node, and pending
   artifact slots.
6. Source node suspends under the normal generation lock.
7. Source node captures final mount snapshots and rootfs delta for the recorded source generation.
8. Source node writes only encrypted Garage contents and reports pinned mount manifest IDs and rootfs
   artifact IDs.
9. Owner client unseals/reseals the transfer key material to the verified target node subkey.
10. CP relays the ciphertext with `DeliverSecrets`; target waits for key delivery before restore.
11. Target restores pinned mount manifests, fetches the rootfs delta artifact keyed by
    `(spawn_id, source_generation)`, pulls the pinned base by digest, imports/assembles the image,
    and starts the target generation.
12. CP marks migration complete only after the target reports active for the target generation.

## Failure Semantics

- **Before ceremony or before source suspend:** no source mutation; migration is canceled or remains
  in preflight.
- **After source suspend but before target active:** source can resume on the original node using
  same-node local state, or stay suspended with an explicit "migration failed, data preserved" status.
- **After target active:** source generation is fenced; late writes/uploads from the source generation
  are ignored unless they match the transfer-set pins.
- **Garage outage:** do not upload plaintext fallback artifacts. If encrypted upload cannot complete,
  fail the migration or remain suspended according to the transient-tier degraded-mode policy.

## Security Notes

Rootfs deltas can include secrets copied by the agent from tmpfs into the rootfs. This residual was
accepted by the writable-rootfs design only because cross-node transport is encrypted and key delivery
is owner-mediated. That acceptance does not apply to plaintext Garage objects or CP-readable keys.

The target node must reject:

- rootfs artifacts whose generation does not match the transfer set;
- artifacts whose base digest does not match the transfer set;
- artifacts not listed in the transfer set;
- transfer-key deliveries with stale generation/AAD context once replay hardening is implemented.

## Implementation Slices

1. **Audit owner-sealed transfer readiness.** Verify current `sp-2ckv`/`sp-u53.5.4` implementation
   against the gate above. File focused follow-ups for delivery replay/AAD or device-registry gaps.
2. **Journal artifact API.** Add typed artifact storage keyed by `(spawn_id, generation)` with
   encrypted Garage storage and no "latest" restore path.
3. **Transfer-set CP model.** Record source/target generations, artifact pins, key-delivery state,
   and terminal status.
4. **Runtime producer/consumer.** Extend the runtime backends or spawnlet with export/import hooks
   for uncompressed rootfs delta artifacts. Target restore pulls pinned base, imports delta, assembles,
   and launches.
5. **E2E tests.** Docker-to-Docker first; then Docker-to-runsc and runsc-to-Docker where host gates
   allow. Assertions: generation pinning, deleted-file whiteouts, uid preservation, encrypted Garage
   contents, ceremony-first behavior, and no "latest" restore.

## Amendments To Existing Work

- `sp-ei4.1.13` should be updated to depend on **Encrypted Migration Transport Ready** and should
  explicitly state `(spawn_id, generation)` rootfs artifact keys plus uncompressed Kopia input.
- The writable-rootfs design §8 should be read through this spec: "through the Kopia journal" means
  through an encrypted, generation-pinned transfer set, not a spawn-level owner-sealed mode.
- The transient-tier design should treat rootfs deltas as typed artifacts in the same encrypted
  migration transport as mount snapshots.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged from the assumptions above — append a dated note here, whether or not a formal debugging skill was used.*

### 2026-06-13 — Implementation Complete

- Storage/journal work under `sp-u53.6` shipped owner-sealed readiness audit, generation-keyed
  typed journal artifacts, and the CP transfer-set model with encrypted transfer-key ceremony
  preflight.
- Runtime/node work under `sp-ei4.1.13.1` and `.2` added rootfs delta export/import hooks,
  generation-keyed journal `rootfs_delta` artifact production on migration suspend, and target
  restore using CP-pinned artifact IDs only.
- CP orchestration under `sp-ei4.1.13.4` now requests rootfs artifact capture during `MigrateSpawn`,
  validates returned artifact id/generation/base digest, records mount and rootfs pins on the
  transfer set, and starts the target with `rootfs_source_generation` plus the pinned artifacts.
- Normal same-node suspend/resume continues to use local delta-image state and does not upload rootfs
  artifacts.
- Residual follow-ups: `sp-ei4.1.14` verifies whether the runtime image-archive transport includes
  base blobs and tightens it to top-diff-only if needed; `sp-clxm` tracks an unrelated
  Docker-backed `TestTmuxRelayLiveAttach` failure that keeps the full `just test` gate red.

Verification:

- `env GOCACHE=/tmp/spawnery-go-cache go test ./internal/runtime ./internal/runtime/cri ./internal/spawnlet ./internal/node ./internal/contract ./internal/cp ./internal/cp/scheduler -count=1 -run 'Test(.*Rootfs.*|.*rootfs.*|MigrateSpawn|SuspendSpawn|Provision|StartSpawn|SuspendComplete|NodeContractFields|ExportDelta|ImportDelta|CaptureDelta|EnsureImage|ResolveImageDigest|ReleaseDelta|DeltaLeaseID|Journal)'` passed.
- `distrobox enter --root dev-spawnery -- bash -lc 'PATH=$(go env GOPATH)/bin:$PATH make gen'`
  passed with no generated-code drift.
- `just test` reached all packages but failed in `internal/node TestTmuxRelayLiveAttach` with
  `docker run: exit status 125`; focused migration/runtime/storage gates passed and `sp-clxm` tracks
  that repo-level blocker.
