// csv_stats — summary statistics over a CSV string.
//
// A complete, working example of a sandboxed tool (see README.md). It is the
// shape a good tool has: a pure transform over its arguments, declaring — and
// therefore needing — no capabilities at all. It reads no files and reaches
// nothing; the CSV arrives as a string in the arguments.

/** Split one CSV line, honoring double-quoted fields and "" escapes. */
function splitLine(line) {
  const out = [];
  let field = "";
  let quoted = false;

  for (let i = 0; i < line.length; i++) {
    const c = line[i];
    if (quoted) {
      if (c === '"' && line[i + 1] === '"') {
        field += '"';
        i++;
      } else if (c === '"') {
        quoted = false;
      } else {
        field += c;
      }
    } else if (c === '"') {
      quoted = true;
    } else if (c === ",") {
      out.push(field);
      field = "";
    } else {
      field += c;
    }
  }
  out.push(field);
  return out.map((f) => f.trim());
}

/** Numeric summary of the values that actually parse as numbers. */
function summarize(values) {
  const nums = values
    .filter((v) => v !== "")
    .map(Number)
    .filter((n) => Number.isFinite(n));

  // A column is only "numeric" if essentially all of its non-empty values are.
  // Otherwise a stray "N/A" would turn a text column into a misleading mean.
  const nonEmpty = values.filter((v) => v !== "").length;
  if (nonEmpty === 0 || nums.length < nonEmpty) return null;

  const sum = nums.reduce((a, b) => a + b, 0);
  const sorted = [...nums].sort((a, b) => a - b);
  const mid = Math.floor(sorted.length / 2);

  return {
    min: sorted[0],
    max: sorted[sorted.length - 1],
    mean: Number((sum / nums.length).toFixed(6)),
    median:
      sorted.length % 2 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2,
  };
}

export default ({ csv, header = true }) => {
  if (typeof csv !== "string" || csv.trim() === "") {
    // Thrown errors reach the model as an ordinary tool failure carrying this
    // message, so say something it can act on.
    throw new Error("csv must be a non-empty string");
  }

  const lines = csv.trim().split(/\r?\n/).filter((l) => l !== "");
  const rows = lines.map(splitLine);

  const names = header
    ? rows[0]
    : rows[0].map((_, i) => `column_${i + 1}`);
  const body = header ? rows.slice(1) : rows;

  const columns = names.map((name, i) => {
    const values = body.map((r) => r[i] ?? "");
    const stats = summarize(values);
    return {
      name,
      empty: values.filter((v) => v === "").length,
      distinct: new Set(values).size,
      ...(stats ? { numeric: true, ...stats } : { numeric: false }),
    };
  });

  return { rows: body.length, columns: columns.length, fields: columns };
};
