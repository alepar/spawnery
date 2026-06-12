export const meta = {
  name: 'ultradeep-rootfs-research',
  description: 'Ultradeep research: survivable writable rootfs + userns for agent pods (7-section brief, per-section fan-out, 3-vote adversarial verify, deliverable synthesis).',
  phases: [
    { title: 'Scope', detail: 'per-section angle generation (7 sections × 3 angles)' },
    { title: 'Search', detail: 'one WebSearch agent per angle' },
    { title: 'Fetch', detail: 'URL-dedup, fetch + extract falsifiable claims' },
    { title: 'Verify', detail: '3-vote adversarial verification per claim' },
    { title: 'Synthesize', detail: 'per-section synthesis + final deliverable assembly' },
  ],
}

// ── Tunables (ultradeep) ──
const ANGLES_PER_SECTION = 3
const VOTES_PER_CLAIM = 3
const REFUTATIONS_REQUIRED = 2
const MAX_FETCH = 56          // global fetch budget across all angles
const MAX_VERIFY_PER_SECTION = 11
const MAX_VERIFY_TOTAL = 84   // ≥7×11 so the last section (S7) is never truncated by the global cap

// ── Fixed context (the decisions already made — embedded so every agent shares it) ──
const FIXED = `## System under design — Spawnery
Sandboxed coding-agent pods (untrusted AGENT container + trusted inference SIDECAR sharing one netns) on NODES. A Go node daemon ("spawnlet") manages pods via two backends ("lanes"): (a) Docker/runc via Docker Engine API — rootful today, rootless Podman desired later; (b) containerd/CRI with gVisor **runsc** as the OCI runtime on cloud (multi-tenant) nodes. The agent runs as in-container ROOT (uid 0) with --cap-drop=ALL today, so apt/dpkg/useradd/chown/chmod of non-owned files FAIL. Data mounts = host dirs bind-mounted read-write at /app/<path>; a per-spawn secrets tmpfs is bind-mounted separately and must NEVER be persisted. Existing journal infra (FIXED): per-spawn embedded-Kopia repos journal data mounts to a self-hosted Garage (S3) store, watcher-triggered, generation-fenced. Migration is suspend-based + data-only (suspend on node A → resume on node B); NO CRIU, NO memory/VM snapshots. Single-writer enforced via DB claims + monotonic GENERATION fencing; pods are RECREATED per generation, not restarted.

## Decisions already made (validate the MECHANICS; do NOT relitigate the goals)
1. Capture the agent container's writable-layer DELTA as an OCI image layer — Docker lane: \`docker commit\`; CRI lane: containerd snapshot **diff** via its Go API. Resume = pinned-base + delta. Migration ships ONLY the delta layer tar(s) through the existing Kopia→Garage journal.
2. USER NAMESPACES give in-container root namespaced caps so apt/chown/chmod just work, mapping to unprivileged high uids on the host.
3. Base image PINNED per spawn by digest for the spawn's whole life; image upgrades apply to NEW spawns only; nodes must not GC pinned images while a live spawn references them.
4. Survival contract: same-node suspend/resume keeps the delta LOCALLY (no journal); migration is delta-only via Kopia. Both lanes converge on the SAME portable artifact (lane-specific capture code OK; lane-specific artifacts NOT).`

// ── The 7 brief sections ──
const SECTIONS = [
  { key: 'S1', title: 'User namespaces per engine — the support matrix',
    focus: `Docker Engine: daemon-wide userns-remap vs per-container userns as of Docker 25–28; interaction with --cap-drop/--cap-add inside a userns; what in-userns cap set makes apt/dpkg/useradd/chown -R work end to end (verify vs apt's real privilege-drop to _apt); rootless Podman --userns=auto. containerd/CRI: KEP-127 pod user namespaces state in containerd 1.7 vs 2.x, CRI UserNamespace/NamespaceMode plumbing, idmapped-mount kernel+fs requirements, whether a non-Kubernetes CRI client can drive it. gVisor/runsc: how the sentry implements userns, runsc-under-containerd KEP-127 support, do namespaced caps in the sentry behave like kernel userns for apt. Mounts under userns: what uid the agent sees on /app bind mounts + secrets tmpfs, idmapped bind mounts vs chown-into-range, fate of the world-writable-0777 workaround. Gotchas: overlayfs-as-upper inside userns, AppArmor/SELinux, /proc/sysctl limits, unprivileged-userns CVE history, nested userns.` },
  { key: 'S2', title: 'Delta capture mechanics',
    focus: `Docker lane: docker commit semantics on running/paused/stopped container (layer consistency, default pause, latency on multi-GB deltas); repeated-suspend layer-chain growth (overlayfs ~128 lowerdir limit, when/how to squash); xattr/whiteout/opaque-dir fidelity; what commit does with bind mounts + tmpfs (verify excluded). CRI/containerd lane: producing a diff layer from a CRI-created container's snapshot via containerd Go client (diff service, k8s.io namespace interop, snapshotter key discovery from container id), stopped-not-removed vs removed, required privileges. runsc: with runsc --overlay2 rootfs overlay enabled (default root:self in recent versions?), do container writes ever reach the host snapshotter upperdir — must --overlay2 be disabled/host-visible for capture to see anything, and the gofer-performance cost. Capture timing: quiesce/fsync, racing the dying container, huge transient junk (/var/cache/apt) exclusion at capture time.` },
  { key: 'S3', title: 'Resume + cross-node replay',
    focus: `Assembling pinned-base + delta(s) on a target node: docker load vs containerd image import from layer tars; building a valid OCI manifest around an out-of-band delta layer; digest bookkeeping. Cross-lane portability: a layer tar captured on Docker applied on containerd and vice versa — legacy Docker tarball vs OCI layout, whiteout encoding differences, xattr namespaces, and CRITICALLY uids inside the tar under differing userns mappings (uid-shift correctness across nodes). Kopia fit: content-defined chunking/dedup on layer tarballs vs storing the unpacked delta tree; size accounting; generation fencing of delta uploads. Node-side image GC + pinning: keeping pinned digests alive per live spawn on Docker AND containerd (lease APIs), reclaiming on spawn delete.` },
  { key: 'S4', title: 'Fallback — engine-native upperdir preservation',
    focus: `Suspend = stop-but-keep the container (Docker stop/start; CRI stopped container with sandbox kept/recreated) so the engine retains the writable layer; migration = read the graph-driver/snapshotter upperdir directly and journal that tree via Kopia. Costs: coupling to graph-driver internals/paths, overlayfs whiteout (char 0:0 device nodes) + trusted.overlay.* xattr survival through Kopia snapshot/restore, conflict with recreate-per-generation fencing, rootless upperdir access. Verdict vs the layers approach: when (if ever) is upperdir-harvesting better, e.g. same-node suspend/resume where zero capture cost matters.` },
  { key: 'S5', title: 'Package-manager reality check',
    focus: `Exactly what apt/dpkg require: caps, setuid transition to _apt, chown of partial/, maintainer scripts running useradd/chmod — verify the proposed userns cap set satisfies them. Briefly: npm -g, pip, cargo install, curl|sh installers. nosuid/noexec mount-flag interactions (setuid binaries installed into the delta; sudo's setuid bit under userns). Should the image ship sudo + a non-root default user for dev-parity, or stay root-by-default once userns makes root safe — what do comparable products ship.` },
  { key: 'S6', title: 'Prior art — who persists agent/dev-env rootfs writes',
    focus: `Modal (image layers + filesystem snapshots / sandbox snapshot_filesystem — mechanism, limits, byte pricing; closest to our design), E2B (Firecracker memory+disk snapshots), Daytona, Replit (historical overlayfs/btrfs), GitHub Codespaces / Gitpod-Ona (persist /workspace only vs full rootfs, what's lost on rebuild), Fly machines, Morph/Sprites if documented. For each: capture mechanism, granularity (full disk vs delta), restore latency, size limits/quotas, published postmortems/failure modes. What they do about image upgrades under preserved deltas (vs our per-spawn digest pinning).` },
  { key: 'S7', title: 'Security delta',
    focus: `Honest before/after: today root + cap-drop-ALL no userns; proposed root-in-userns with a generous in-userns cap set — which is the stronger sandbox vs kernel LPE given unprivileged-userns CVE history vs the caps the agent currently lacks? How much does runsc change this on the cloud lane (sentry-mediated syscalls). Egress floor: iptables rules in the pod netns keyed by pod IP — does userns change netns/iptables behavior or the node's ability to apply the floor. Secrets leakage: agent copies a secret from secrets tmpfs into rootfs → lands in delta → journaled to Garage; mitigations (owner-sealed Kopia keys already designed, capture-time exclusion lists, documented acceptance). Disk bombs: bounding delta growth (xfs project quotas, containerd snapshotter size limits, Docker --storage-opt size= support matrix per graph driver/backing fs), behavior at the cap (fail writes vs suspend-and-warn).` },
]

// ── Schemas ──
const SCOPE_SCHEMA = {
  type: "object", required: ["angles"],
  properties: {
    angles: { type: "array", minItems: 2, maxItems: 4, items: {
      type: "object", required: ["label", "query"],
      properties: { label: { type: "string" }, query: { type: "string" }, rationale: { type: "string" } },
    }},
  },
}
const SEARCH_SCHEMA = {
  type: "object", required: ["results"],
  properties: {
    results: { type: "array", maxItems: 6, items: {
      type: "object", required: ["url", "title", "relevance"],
      properties: { url: { type: "string" }, title: { type: "string" }, snippet: { type: "string" }, relevance: { enum: ["high", "medium", "low"] } },
    }},
  },
}
const EXTRACT_SCHEMA = {
  type: "object", required: ["claims", "sourceQuality"],
  properties: {
    sourceQuality: { enum: ["primary", "secondary", "blog", "forum", "unreliable"] },
    publishDate: { type: "string" },
    claims: { type: "array", maxItems: 6, items: {
      type: "object", required: ["claim", "quote", "importance"],
      properties: {
        claim: { type: "string" },
        quote: { type: "string" },
        importance: { enum: ["central", "supporting", "tangential"] },
        versionSpecific: { type: "boolean" },
      },
    }},
  },
}
const VERDICT_SCHEMA = {
  type: "object", required: ["refuted", "evidence", "confidence"],
  properties: {
    refuted: { type: "boolean" },
    evidence: { type: "string" },
    confidence: { enum: ["high", "medium", "low"] },
    counterSource: { type: "string" },
    correction: { type: "string" },
  },
}
const SECTION_SCHEMA = {
  type: "object", required: ["title", "findings", "markdown"],
  properties: {
    title: { type: "string" },
    findings: { type: "array", items: {
      type: "object", required: ["claim", "confidence"],
      properties: {
        claim: { type: "string" },
        confidence: { enum: ["high", "medium", "low"] },
        sources: { type: "array", items: { type: "string" } },
      },
    }},
    markdown: { type: "string" },   // the section's prose for the results doc
    openQuestions: { type: "array", items: { type: "string" } },
    weakEvidence: { type: "string" },
  },
}
const FINAL_SCHEMA = {
  type: "object", required: ["report"],
  properties: {
    report: { type: "string" },          // the full markdown results doc body
    recommendation: { type: "string" },  // one-paragraph bottom line
    openQuestions: { type: "array", items: { type: "string" } },
  },
}

// ════════════════ Phase 0: Scope (per section) ════════════════
phase("Scope")
const scoped = await parallel(SECTIONS.map(sec => () =>
  agent(
    "Decompose ONE section of a container-runtime research brief into " + ANGLES_PER_SECTION + " complementary web-search angles.\n\n" +
    FIXED + "\n\n## Section " + sec.key + ": " + sec.title + "\n" + sec.focus + "\n\n" +
    "## Task\nGenerate " + ANGLES_PER_SECTION + " distinct, high-signal search queries that together cover THIS section. Favor primary sources: runtime source/release notes, kernel docs, KEPs, gVisor/containerd/Docker/Podman docs, CVE databases. Make queries version-specific where the section demands it. Avoid redundancy with each other.\n\nStructured output only.",
    { label: "scope:" + sec.key, phase: "Scope", schema: SCOPE_SCHEMA }
  ).then(r => r ? { sec, angles: r.angles } : null)
)).then(rs => rs.filter(Boolean))

const angles = scoped.flatMap(s => s.angles.map(a => ({ ...a, sec: s.sec })))
log("Scoped " + scoped.length + "/7 sections → " + angles.length + " search angles")

// ── Dedup state ──
const normURL = u => { try { const p = new URL(u); return (p.hostname.replace(/^www\./, "") + p.pathname.replace(/\/$/, "")).toLowerCase() } catch { return u.toLowerCase() } }
const seen = new Map()
const relRank = { high: 0, medium: 1, low: 2 }
let fetchSlots = MAX_FETCH

const SEARCH_PROMPT = (a) =>
  "## Web Searcher — section " + a.sec.key + " (" + a.sec.title + ")\n" + "Angle: **" + a.label + "** — " + (a.rationale || "") + "\nQuery: `" + a.query + "`\n\n" +
  FIXED + "\n\n## Task\nUse WebSearch with the query (or a refined version). Return the top 4-6 results most relevant to the SECTION topic. Rank by relevance; prefer primary/authoritative sources (project docs, source, release notes, KEPs, kernel.org, CVE entries) over blogs/SEO. Short snippet each.\n\nStructured output only."

const FETCH_PROMPT = (source, a) =>
  "## Source Extractor — section " + a.sec.key + " (" + a.sec.title + ")\n" +
  "URL: " + source.url + "\nTitle: " + source.title + "\n\n" + FIXED + "\n\n" +
  "Section focus: " + a.sec.focus + "\n\n## Task\n1. WebFetch the page.\n2. Rate source quality: primary (project source/docs/release notes/KEP/kernel docs/CVE) / secondary / blog / forum / unreliable.\n3. Extract 2-6 FALSIFIABLE, concrete, checkable claims bearing on THIS section — each with a direct supporting quote and importance (central/supporting/tangential). Set versionSpecific=true for claims tied to a specific version/kernel/release.\n4. Note publish date.\nIf fetch fails/paywalled/irrelevant: claims:[] sourceQuality:\"unreliable\".\n\nStructured output only."

// ════════════════ Phase 1-2: Search → dedup → Fetch+Extract (pipelined) ════════════════
const searchResults = await pipeline(
  angles,
  a => agent(SEARCH_PROMPT(a), { label: "search:" + a.sec.key + ":" + a.label.slice(0, 24), phase: "Search", schema: SEARCH_SCHEMA })
        .then(r => r ? { a, results: r.results } : null),
  sr => {
    if (!sr) return []
    const sorted = [...sr.results].sort((x, y) => relRank[x.relevance] - relRank[y.relevance])
    const novel = sorted.filter(r => {
      const k = normURL(r.url)
      if (seen.has(k)) return false
      if (fetchSlots <= 0 && relRank[r.relevance] >= 1) return false
      seen.set(k, true); fetchSlots--; return true
    })
    return parallel(novel.map(source => () => {
      let host = "src"; try { host = new URL(source.url).hostname.replace(/^www\./, "") } catch {}
      return agent(FETCH_PROMPT(source, sr.a), { label: "fetch:" + sr.a.sec.key + ":" + host, phase: "Fetch", schema: EXTRACT_SCHEMA })
        .then(ext => ext ? {
          url: source.url, title: source.title, sec: sr.a.sec.key,
          sourceQuality: ext.sourceQuality, publishDate: ext.publishDate,
          claims: ext.claims.map(c => ({ ...c, sourceUrl: source.url, sourceQuality: ext.sourceQuality, sec: sr.a.sec.key })),
        } : null)
        .catch(() => ({ url: source.url, title: source.title, sec: sr.a.sec.key, sourceQuality: "unreliable", claims: [] }))
    }))
  }
)

const allSources = searchResults.flat().filter(Boolean)
const allClaims = allSources.flatMap(s => s.claims)
const impRank = { central: 0, supporting: 1, tangential: 2 }
const qualRank = { primary: 0, secondary: 1, blog: 2, forum: 3, unreliable: 4 }

// Rank within each section, take top-N per section, then cap globally — guarantees section coverage.
const bySection = {}
for (const c of allClaims) (bySection[c.sec] ||= []).push(c)
let rankedClaims = []
for (const sec of SECTIONS) {
  const pool = (bySection[sec.key] || []).sort((a, b) => (impRank[a.importance] - impRank[b.importance]) || (qualRank[a.sourceQuality] - qualRank[b.sourceQuality]))
  rankedClaims.push(...pool.slice(0, MAX_VERIFY_PER_SECTION))
}
rankedClaims = rankedClaims.slice(0, MAX_VERIFY_TOTAL)
log("Fetched " + allSources.length + " sources → " + allClaims.length + " claims → verifying " + rankedClaims.length + " (section-balanced)")

// ════════════════ Phase 3: Verify (3-vote adversarial) ════════════════
phase("Verify")
const VERIFY_PROMPT = (claim, v) =>
  "## Adversarial Claim Verifier (voter " + (v + 1) + "/" + VOTES_PER_CLAIM + ") — be SKEPTICAL, try to REFUTE.\n" +
  "≥" + REFUTATIONS_REQUIRED + "/" + VOTES_PER_CLAIM + " refutes kills the claim. This is a TECHNICAL container-runtime claim; demand PRIMARY sources (project source/docs/release notes/KEP/kernel docs/CVE) for version/support claims — a blog or forum post is NOT sufficient for a version-specific support claim.\n\n" +
  FIXED + "\n\n## Claim under review\n\"" + claim.claim + "\"\nSource: " + claim.sourceUrl + " (" + claim.sourceQuality + ")" + (claim.versionSpecific ? " [version-specific]" : "") + "\nSupporting quote: \"" + claim.quote + "\"\n\n" +
  "## Checklist\n1. Does the quote actually support the claim, or is it overreach/misread?\n2. WebSearch for contradicting/qualifying evidence from a credible source.\n3. Is source quality sufficient for the claim's strength? (version-specific support claims need primary sources)\n4. Outdated? Fast-moving area (containerd/runsc/kernel userns) — old claims are suspect; check dates.\n5. Marketing/cherry-picked/forum-speculation?\n\nrefuted=true if: unsupported by quote / contradicted / weak source for a strong version claim / outdated / speculation. refuted=false ONLY if well-supported, current, and source quality matches strength. Default refuted=true if uncertain. If the claim is directionally right but imprecise, set refuted appropriately AND put the corrected statement in `correction`.\n\nStructured output only. Evidence MUST be specific (cite a version/source)."

const voted = (await parallel(rankedClaims.map(claim => () =>
  parallel(Array.from({ length: VOTES_PER_CLAIM }, (_, v) => () =>
    agent(VERIFY_PROMPT(claim, v), { label: "v" + v + ":" + claim.sec + ":" + claim.claim.slice(0, 30), phase: "Verify", schema: VERDICT_SCHEMA })
  )).then(verdicts => {
    const valid = verdicts.filter(Boolean)
    const refuted = valid.filter(v => v.refuted).length
    const survives = valid.length >= REFUTATIONS_REQUIRED && refuted < REFUTATIONS_REQUIRED
    log("[" + claim.sec + "] \"" + claim.claim.slice(0, 44) + "…\": " + (valid.length - refuted) + "-" + refuted + " " + (survives ? "✓" : "✗"))
    return { ...claim, verdicts: valid, refutedVotes: refuted, survives }
  })
))).filter(Boolean)

const confirmed = voted.filter(c => c.survives)
const killed = voted.filter(c => !c.survives)
log("Verify: " + voted.length + " claims → " + confirmed.length + " confirmed, " + killed.length + " killed")

// ════════════════ Phase 4: Synthesize (per section → final deliverable) ════════════════
phase("Synthesize")
const confRank = { high: 0, medium: 1, low: 2 }
const claimBlock = (list) => list.map((c, i) => {
  const best = c.verdicts.filter(v => !v.refuted).sort((a, b) => confRank[a.confidence] - confRank[b.confidence])[0]
  const corr = c.verdicts.map(v => v.correction).filter(Boolean)[0]
  return "### [" + i + "] " + c.claim + "\nVote " + (c.verdicts.length - c.refutedVotes) + "-" + c.refutedVotes + " · " + c.sourceUrl + " (" + c.sourceQuality + ")\nQuote: \"" + c.quote + "\"\nVerifier evidence (" + (best ? best.confidence : "?") + "): " + (best ? best.evidence : "n/a") + (corr ? "\nCorrection: " + corr : "")
}).join("\n\n")

const sectionReports = await parallel(SECTIONS.map(sec => () => {
  const mine = confirmed.filter(c => c.sec === sec.key)
  const myKilled = killed.filter(c => c.sec === sec.key)
  if (mine.length === 0 && myKilled.length === 0) return Promise.resolve(null)
  return agent(
    "## Synthesize one section of the rootfs-survival research report.\n\n" + FIXED + "\n\n" +
    "## Section " + sec.key + ": " + sec.title + "\nFocus: " + sec.focus + "\n\n" +
    "## Verified claims (survived 3-vote adversarial verification)\n" + (mine.length ? claimBlock(mine) : "(none survived)") + "\n\n" +
    (myKilled.length ? "## Refuted (do NOT assert; may note as 'contested/unverified')\n" + myKilled.map(c => "- \"" + c.claim + "\" (" + c.sourceUrl + ")").join("\n") + "\n\n" : "") +
    "## Task\nWrite this section of the results doc. Synthesize the verified claims into coherent prose that answers the section's questions for Spawnery's exact constraints (both lanes, userns, pinned images, delta capture, Kopia migration). Merge duplicates. Assign per-finding confidence. Where evidence is thin/contested, SAY SO explicitly rather than overclaiming. The `markdown` field must be a polished, citation-bearing section (use the source URLs inline) ready to drop into a spec doc under a `## " + sec.key + ". " + sec.title + "` heading.\n\nStructured output only.",
    { label: "synth:" + sec.key, phase: "Synthesize", schema: SECTION_SCHEMA }
  ).then(r => r ? { sec, ...r } : null)
})).then(rs => rs.filter(Boolean))

log("Synthesized " + sectionReports.length + " sections → assembling deliverable")

// ── Final assembly: support matrix + comparison table + recommendation + adoption path ──
const sectionsMd = sectionReports.map(s => s.markdown).join("\n\n")
const allOpen = sectionReports.flatMap(s => s.openQuestions || [])
const final = await agent(
  "## Assemble the FINAL deliverable: a research-results spec doc.\n\n" + FIXED + "\n\n" +
  "You are given the synthesized per-section writeups below. Produce the complete results document body in `report` (markdown). It MUST contain, in order:\n" +
  "1. An executive summary (4-8 sentences) answering: can we give the agent an unrestricted writable rootfs via userns whose delta survives same-node resume and migrates via Kopia, across BOTH lanes — and what's the recommended mechanism.\n" +
  "2. A **support matrix** table: rows = {per-container userns, idmapped mounts, in-userns cap re-grant} × columns = {Docker rootful, Podman rootless, containerd/CRI+runc, containerd/CRI+runsc}, cells citing minimum versions / support status / 'unverified'.\n" +
  "3. A **capture-mechanism comparison** table: rows = {OCI layer via commit/diff, upperdir harvesting, runsc-internal overlay} × columns = {same-node resume cost, migration artifact, cross-lane portability, fencing fit, key failure mode}.\n" +
  "4. The seven sections verbatim-ish from the per-section writeups below (keep their citations).\n" +
  "5. A **concrete recommendation** for Spawnery (mechanism per lane, how they share one artifact, how pinning + GC works, secrets/disk-bomb mitigations).\n" +
  "6. A **staged adoption path** (e.g. userns+writable-rootfs → same-node delta survival → migratable deltas), each stage with a trigger/threshold.\n" +
  "7. A **weak-evidence / open-questions** list and explicit runsc-dominated callouts.\n\n" +
  "Do not invent facts beyond the per-section writeups; where they are thin, mark it. Keep all source URLs.\n\n" +
  "## Per-section writeups\n" + sectionsMd + "\n\n## Aggregated open questions\n" + allOpen.map(q => "- " + q).join("\n") + "\n\nStructured output only.",
  { label: "assemble-deliverable", phase: "Synthesize", schema: FINAL_SCHEMA }
)

return {
  report: final ? final.report : null,
  recommendation: final ? final.recommendation : null,
  openQuestions: final ? final.openQuestions : allOpen,
  sectionMarkdown: sectionReports.map(s => ({ sec: s.sec.key, title: s.sec.title, markdown: s.markdown })),
  refuted: killed.map(c => ({ sec: c.sec, claim: c.claim, vote: (c.verdicts.length - c.refutedVotes) + "-" + c.refutedVotes, source: c.sourceUrl })),
  sources: allSources.map(s => ({ url: s.url, quality: s.sourceQuality, sec: s.sec, claimCount: s.claims.length })),
  stats: {
    sectionsScoped: scoped.length, angles: angles.length,
    sourcesFetched: allSources.length, claimsExtracted: allClaims.length,
    claimsVerified: voted.length, confirmed: confirmed.length, killed: killed.length,
    sectionsSynthesized: sectionReports.length,
  },
}
