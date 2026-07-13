import { describe, it, expect, vi, beforeEach } from "vitest";
import {
  listBundles,
  getBundle,
  reingestBundle,
  getBundleDiff,
  repinProfileBundle,
} from "./bundles";

function mockFetch(json: unknown, ok = true) {
  return vi.fn().mockResolvedValue({
    ok,
    status: ok ? 200 : 400,
    json: async () => json,
    text: async () => JSON.stringify(json),
  });
}

describe("bundles api", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("listBundles POSTs ListBundles and maps bundles, coercing int64-as-string fields", async () => {
    const f = mockFetch({
      bundles: [
        {
          bundleId: "b1",
          name: "superpowers",
          sourceUrl: "https://github.com/obra/superpowers",
          sourceRef: "main",
          sourceSubdir: "",
          createdAt: "1000",
          updatedAt: "2000",
          latestVersionId: "v3",
          latestSeq: "3",
          memberCount: 12,
        },
      ],
    });
    vi.stubGlobal("fetch", f);
    const bundles = await listBundles();
    expect(f).toHaveBeenCalledWith("/cp.v1.SpawnService/ListBundles", expect.objectContaining({ method: "POST" }));
    expect(JSON.parse((f.mock.calls[0][1] as any).body)).toEqual({});
    expect(bundles).toHaveLength(1);
    expect(bundles[0].bundleId).toBe("b1");
    expect(bundles[0].latestSeq).toBe(3);
    expect(bundles[0].memberCount).toBe(12);
  });

  it("listBundles tolerates a missing bundles array", async () => {
    vi.stubGlobal("fetch", mockFetch({}));
    expect(await listBundles()).toEqual([]);
  });

  it("getBundle POSTs GetBundle with bundleId and normalizes missing arrays", async () => {
    const f = mockFetch({
      bundle: { bundleId: "b1", name: "superpowers", memberCount: 2, latestSeq: "1" },
    });
    vi.stubGlobal("fetch", f);
    const r = await getBundle("b1");
    expect(JSON.parse((f.mock.calls[0][1] as any).body)).toEqual({ bundleId: "b1" });
    expect(r.bundle.bundleId).toBe("b1");
    expect(r.versions).toEqual([]);
    expect(r.members).toEqual([]);
  });

  it("getBundle preserves versions and members when present", async () => {
    const f = mockFetch({
      bundle: { bundleId: "b1", name: "superpowers" },
      versions: [{ versionId: "v1", seq: "1", sourceCommit: "aaaa", createdAt: "100" }],
      members: [{ catalogId: "c1", sourceSubdir: "skills/a", name: "a", sha256: "deadbeef", position: 0 }],
    });
    vi.stubGlobal("fetch", f);
    const r = await getBundle("b1");
    expect(r.versions).toHaveLength(1);
    expect(r.versions[0].versionId).toBe("v1");
    expect(r.members).toHaveLength(1);
    expect(r.members[0].sourceSubdir).toBe("skills/a");
  });

  it("reingestBundle POSTs ReingestBundle with bundleId and defaults absent fields", async () => {
    const f = mockFetch({ versionId: "v1", changed: false });
    vi.stubGlobal("fetch", f);
    const r = await reingestBundle("b1");
    expect(JSON.parse((f.mock.calls[0][1] as any).body)).toEqual({ bundleId: "b1" });
    expect(r.versionId).toBe("v1");
    expect(r.changed).toBe(false);
    expect(r.notModified).toBe(false);
    expect(r.addedSubdirs).toEqual([]);
    expect(r.updatedSubdirs).toEqual([]);
    expect(r.removedSubdirs).toEqual([]);
    expect(r.warnings).toEqual([]);
    expect(r.diffToken).toBe("");
  });

  it("reingestBundle preserves a changed response with a diff token", async () => {
    const f = mockFetch({
      versionId: "v2",
      changed: true,
      addedSubdirs: ["skills/new"],
      updatedSubdirs: ["skills/a"],
      removedSubdirs: [],
      diffToken: "tok-1",
      fromVersionId: "v1",
      notModified: false,
      warnings: ["renamed skills/b to skills/b-2"],
    });
    vi.stubGlobal("fetch", f);
    const r = await reingestBundle("b1");
    expect(r.changed).toBe(true);
    expect(r.diffToken).toBe("tok-1");
    expect(r.fromVersionId).toBe("v1");
    expect(r.addedSubdirs).toEqual(["skills/new"]);
    expect(r.warnings).toEqual(["renamed skills/b to skills/b-2"]);
  });

  it("getBundleDiff POSTs GetBundleDiff with camelCase bundleId/fromVersion/toVersion", async () => {
    const f = mockFetch({ members: [], fromCommit: "aaaa", toCommit: "bbbb", diffToken: "tok-1" });
    vi.stubGlobal("fetch", f);
    const r = await getBundleDiff("b1", "v1", "v2");
    expect(f).toHaveBeenCalledWith("/cp.v1.SpawnService/GetBundleDiff", expect.objectContaining({ method: "POST" }));
    expect(JSON.parse((f.mock.calls[0][1] as any).body)).toEqual({ bundleId: "b1", fromVersion: "v1", toVersion: "v2" });
    expect(r.fromCommit).toBe("aaaa");
    expect(r.toCommit).toBe("bbbb");
    expect(r.diffToken).toBe("tok-1");
    expect(r.members).toEqual([]);
  });

  it("getBundleDiff normalizes a missing members array and empty diffToken", async () => {
    vi.stubGlobal("fetch", mockFetch({ fromCommit: "aaaa", toCommit: "bbbb" }));
    const r = await getBundleDiff("b1", "v1", "v2");
    expect(r.members).toEqual([]);
    expect(r.diffToken).toBe("");
  });

  it("getBundleDiff passes through member change enum and diff fields", async () => {
    const member = {
      sourceSubdir: "skills/a",
      change: "CHANGED",
      name: "a",
      oldSha256: "aaa",
      newSha256: "bbb",
      oldSkillMd: "old body",
      newSkillMd: "new body",
      oldTruncated: false,
      newTruncated: false,
      bodyUnavailable: false,
    };
    vi.stubGlobal("fetch", mockFetch({ members: [member], fromCommit: "aaaa", toCommit: "bbbb", diffToken: "tok-1" }));
    const r = await getBundleDiff("b1", "v1", "v2");
    expect(r.members[0]).toEqual(member);
  });

  it("repinProfileBundle POSTs RepinProfileBundle with all fields and normalizes warnings", async () => {
    const f = mockFetch({ version: "5" });
    vi.stubGlobal("fetch", f);
    const r = await repinProfileBundle("p1", "e1", "v2", "tok-1", 4);
    expect(f).toHaveBeenCalledWith("/cp.v1.SpawnService/RepinProfileBundle", expect.objectContaining({ method: "POST" }));
    expect(JSON.parse((f.mock.calls[0][1] as any).body)).toEqual({
      profileId: "p1",
      entryId: "e1",
      versionId: "v2",
      diffToken: "tok-1",
      expectedVersion: 4,
    });
    expect(r.version).toBe(5);
    expect(r.warnings).toEqual([]);
  });

  it("repinProfileBundle preserves warnings when present", async () => {
    vi.stubGlobal("fetch", mockFetch({ version: "6", warnings: ["member skills/x dropped: removed upstream"] }));
    const r = await repinProfileBundle("p1", "e1", "v2", "tok-1", 5);
    expect(r.warnings).toEqual(["member skills/x dropped: removed upstream"]);
  });
});
