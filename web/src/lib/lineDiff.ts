// Pure line-level diff (no npm dependency — CSP forbids external fetches, and prod build is
// scanned for dependencies; see sp-mwco.1.9 D8). Used to render a bundle member's SKILL.md
// change between two versions in BundleDiffDialog — the diff IS the supply-chain gate on re-pin
// (spec §4.9), so this must never silently render an empty diff for non-identical text.

/** Each side of a diffed body is capped at this many lines; the remainder is elided (bodies are
 * already 64 KiB-capped server-side, but very long files would still make an O(n*m) LCS slow). */
export const LINE_DIFF_CAP = 2000;

export type LineDiffPart = { type: "ctx" | "add" | "del" | "elided"; text: string };

/** Line-level diff of oldText -> newText via a plain LCS (dynamic programming). */
export function lineDiff(oldText: string, newText: string): LineDiffPart[] {
  const oldAll = oldText === "" ? [] : oldText.split("\n");
  const newAll = newText === "" ? [] : newText.split("\n");

  const oldElided = Math.max(0, oldAll.length - LINE_DIFF_CAP);
  const newElided = Math.max(0, newAll.length - LINE_DIFF_CAP);
  const oldLines = oldAll.slice(0, LINE_DIFF_CAP);
  const newLines = newAll.slice(0, LINE_DIFF_CAP);

  const parts = lcsDiff(oldLines, newLines);

  const elidedCount = Math.max(oldElided, newElided);
  if (elidedCount > 0) {
    parts.push({
      type: "elided",
      text: `… ${elidedCount} more line(s) elided (diff capped at ${LINE_DIFF_CAP} lines per side) …`,
    });
  }
  return parts;
}

/** Classic LCS-backtrack line diff over two already-bounded line arrays. */
function lcsDiff(a: string[], b: string[]): LineDiffPart[] {
  const n = a.length;
  const m = b.length;
  const dp: number[][] = Array.from({ length: n + 1 }, () => new Array<number>(m + 1).fill(0));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      dp[i][j] = a[i] === b[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
    }
  }

  const parts: LineDiffPart[] = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (a[i] === b[j]) {
      parts.push({ type: "ctx", text: a[i] });
      i++;
      j++;
    } else if (dp[i + 1][j] >= dp[i][j + 1]) {
      parts.push({ type: "del", text: a[i] });
      i++;
    } else {
      parts.push({ type: "add", text: b[j] });
      j++;
    }
  }
  while (i < n) {
    parts.push({ type: "del", text: a[i] });
    i++;
  }
  while (j < m) {
    parts.push({ type: "add", text: b[j] });
    j++;
  }
  return parts;
}
