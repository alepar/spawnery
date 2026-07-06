import type { AcceptanceClient } from "../drivers/oracle";

/**
 * SpawnRegistry: per-test tracker for spawns a scenario creates so they are reliably deleted at
 * test end via the api oracle. Needed because web/cli createSpawn cannot set a name, so the
 * name-based teardown sweep (fixtures/sweep.ts) cannot see them — this closes that leak.
 */
export class SpawnRegistry {
  private readonly ids = new Set<string>();
  constructor(private readonly api: Pick<AcceptanceClient, "deleteSpawn">) {}

  /** track records a created spawn id for end-of-test cleanup (idempotent). */
  track(id: string): string {
    this.ids.add(id);
    return id;
  }

  /** cleanup best-effort deletes every tracked spawn; one failure never blocks the rest. */
  async cleanup(): Promise<void> {
    for (const id of this.ids) {
      try {
        await this.api.deleteSpawn(id);
      } catch (e) {
        console.warn(`SpawnRegistry.cleanup: failed to delete ${id}: ${(e as Error).message}`);
      }
    }
    this.ids.clear();
  }
}
