// nine:diff — a line-level diff for generated tools (docs/sandboxed-tools.md
// §4.2), computed from the longest common subsequence. Small and exact rather
// than fast: the 5-second deadline (R-TVM.4) bounds pathological inputs, and a
// tool comparing megabyte blobs should say so via gap_report.

// lineDiff(a, b) -> [{ type: "eq"|"del"|"add", line }]. "del" is a line present
// in a but not b; "add" is present in b but not a; "eq" is common to both. The
// result read top to bottom reconstructs b from a.
export function lineDiff(a, b) {
  const A = String(a).split("\n");
  const B = String(b).split("\n");
  const n = A.length;
  const m = B.length;

  // lcs[i][j] = length of the LCS of A[i:] and B[j:].
  const lcs = Array.from({ length: n + 1 }, () => new Array(m + 1).fill(0));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      lcs[i][j] =
        A[i] === B[j]
          ? lcs[i + 1][j + 1] + 1
          : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
    }
  }

  const out = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (A[i] === B[j]) {
      out.push({ type: "eq", line: A[i] });
      i++;
      j++;
    } else if (lcs[i + 1][j] >= lcs[i][j + 1]) {
      out.push({ type: "del", line: A[i] });
      i++;
    } else {
      out.push({ type: "add", line: B[j] });
      j++;
    }
  }
  while (i < n) out.push({ type: "del", line: A[i++] });
  while (j < m) out.push({ type: "add", line: B[j++] });
  return out;
}

// unified(a, b) -> a compact unified-style string, one line per change: " " for
// equal, "-" for deletions, "+" for additions.
export function unified(a, b) {
  const sign = { eq: " ", del: "-", add: "+" };
  return lineDiff(a, b)
    .map((d) => sign[d.type] + d.line)
    .join("\n");
}

export default { lineDiff, unified };
