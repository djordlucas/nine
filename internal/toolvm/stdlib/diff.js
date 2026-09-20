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

// hunks(a, b, { context, maxLines }) -> a unified diff carrying only the
// changed regions, each under an @@ header, with `context` unchanged lines
// around it.
//
// `unified` above emits every line of both inputs, which is unreadable for the
// case that matters most: one line changed in a file of five thousand. A person
// approving an edit, and a model reporting one, both need the change and enough
// around it to recognize where it is.
//
// Returns { text, added, removed, truncated } rather than a bare string: a
// caller that cannot show the whole thing still has the counts to report, and
// "3 lines changed, diff truncated" beats a diff cut off mid-hunk.
export function hunks(a, b, opts = {}) {
  const context = Number.isInteger(opts.context) ? opts.context : 3;
  const maxLines = Number.isInteger(opts.maxLines) ? opts.maxLines : 400;

  const d = lineDiff(a, b);
  let added = 0;
  let removed = 0;
  for (const it of d) {
    if (it.type === "add") added++;
    else if (it.type === "del") removed++;
  }
  if (added === 0 && removed === 0) {
    return { text: "", added: 0, removed: 0, truncated: false };
  }

  // Mark every line within `context` of a change, then emit the marked runs.
  const keep = new Array(d.length).fill(false);
  for (let i = 0; i < d.length; i++) {
    if (d[i].type === "eq") continue;
    for (let j = Math.max(0, i - context); j <= Math.min(d.length - 1, i + context); j++) {
      keep[j] = true;
    }
  }

  const out = [];
  const sign = { eq: " ", del: "-", add: "+" };
  let oldLine = 1;
  let newLine = 1;
  let truncated = false;

  for (let i = 0; i < d.length; ) {
    if (!keep[i]) {
      if (d[i].type !== "add") oldLine++;
      if (d[i].type !== "del") newLine++;
      i++;
      continue;
    }
    const startOld = oldLine;
    const startNew = newLine;
    const body = [];
    while (i < d.length && keep[i]) {
      body.push(sign[d[i].type] + d[i].line);
      if (d[i].type !== "add") oldLine++;
      if (d[i].type !== "del") newLine++;
      i++;
    }
    // A hunk larger than the remaining budget is cut, not dropped. Dropping it
    // returns an empty diff for exactly the change that most needs looking at:
    // the big one.
    const room = maxLines - out.length - 1;
    if (room <= 0) {
      truncated = true;
      break;
    }
    out.push(`@@ -${startOld} +${startNew} @@`);
    if (body.length > room) {
      out.push(...body.slice(0, room));
      truncated = true;
      break;
    }
    out.push(...body);
  }

  return { text: out.join("\n"), added, removed, truncated };
}

export default { lineDiff, unified, hunks };
