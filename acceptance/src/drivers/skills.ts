/**
 * SkillIngestOracle: a thin Connect-JSON client for cp.v1.SpawnService/IngestSkillFromURL
 * (sp-mwco.4.6 §4.8 S5 needs a real GitHub-backed catalog entry to attach to a profile before
 * measuring StartSpawn -> last-by-ref-GET). Mirrors market-oracle.ts's fetch pattern exactly (no
 * codegen, plain fetch) rather than extending the signed SpawnClient SDK — IngestSkillFromURL is a
 * plain bearer-authenticated RPC (auth.OwnerFromContext), not an A4 intent-signed spawn lifecycle
 * op, so it needs none of oracle.ts's signing machinery. Attaching the resulting catalog entry to a
 * profile reuses the existing ProfileCli.entryAddCatalog (drivers/customization.ts) — no new
 * driver surface needed for that half.
 */

export interface IngestSkillFromURLInput {
  url: string;
  ref?: string;
  subdir?: string;
  name?: string;
  description?: string;
}

export class SkillIngestOracle {
  constructor(
    private readonly cpEndpoint: string,
    private readonly token: string,
  ) {}

  private async call<T>(method: string, body: unknown): Promise<T> {
    const res = await fetch(`${this.cpEndpoint}/cp.v1.SpawnService/${method}`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "Connect-Protocol-Version": "1",
        Authorization: `Bearer ${this.token}`,
      },
      body: JSON.stringify(body),
    });
    if (!res.ok) {
      const text = await res.text().catch(() => "");
      throw new Error(`${method} failed: ${res.status} ${text}`);
    }
    return (await res.json()) as T;
  }

  /** ingestSkillFromURL fetches+repacks+stores a GitHub-hosted skill and returns its catalog id.
   * Idempotent CP-side on (creator, sha256) — callers doing bulk ingest (S5's K-member bundle)
   * should vary `ref`/`subdir`/`name` per call rather than rely on distinct source repos, since a
   * duplicate produces the SAME catalog id instead of a new listed entry. */
  async ingestSkillFromURL(input: IngestSkillFromURLInput): Promise<{ catalogId: string }> {
    const body: Record<string, string> = { url: input.url };
    if (input.ref) body.ref = input.ref;
    if (input.subdir) body.subdir = input.subdir;
    if (input.name) body.name = input.name;
    if (input.description) body.description = input.description;
    return this.call<{ catalogId: string }>("IngestSkillFromURL", body);
  }
}
