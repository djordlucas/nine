// nine:csv — a minimal, dependency-free CSV reader/writer for generated tools
// (docs/sandboxed-tools.md §4.2). RFC 4180-ish: quoted fields, doubled quotes,
// embedded delimiters and newlines. No streaming, no type coercion — a tool that
// needs more should say so via gap_report.

// parse(text, { delimiter = ",", header = false }) -> rows.
// With header:false (default) each row is an array of string cells. With
// header:true the first row names the columns and every later row becomes an
// object keyed by those names.
export function parse(text, { delimiter = ",", header = false } = {}) {
  const rows = [];
  let field = "";
  let row = [];
  let i = 0;
  let inQuotes = false;
  let sawAny = false;

  const endField = () => {
    row.push(field);
    field = "";
  };
  const endRow = () => {
    endField();
    rows.push(row);
    row = [];
  };

  while (i < text.length) {
    const c = text[i];
    if (inQuotes) {
      if (c === '"') {
        if (text[i + 1] === '"') {
          field += '"';
          i += 2;
          continue;
        }
        inQuotes = false;
        i++;
        continue;
      }
      field += c;
      i++;
      continue;
    }
    if (c === '"') {
      inQuotes = true;
      sawAny = true;
      i++;
      continue;
    }
    if (c === delimiter) {
      sawAny = true;
      endField();
      i++;
      continue;
    }
    if (c === "\r") {
      i++;
      continue;
    }
    if (c === "\n") {
      endRow();
      i++;
      continue;
    }
    field += c;
    sawAny = true;
    i++;
  }
  // Flush a trailing field/row that no newline terminated.
  if (field.length > 0 || row.length > 0 || sawAny) endRow();

  if (!header) return rows;
  if (rows.length === 0) return [];
  const keys = rows[0];
  return rows.slice(1).map((r) =>
    Object.fromEntries(keys.map((k, j) => [k, r[j] ?? ""])),
  );
}

// format(rows, { delimiter = "," }) -> text. Rows may be arrays or objects; an
// object's values are written in insertion order. A cell containing the
// delimiter, a quote, or a newline is quoted and its quotes doubled.
export function format(rows, { delimiter = "," } = {}) {
  const needsQuote = new RegExp(`["\\r\\n${delimiter.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}]`);
  const cell = (v) => {
    const s = v == null ? "" : String(v);
    return needsQuote.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s;
  };
  return rows
    .map((r) => (Array.isArray(r) ? r : Object.values(r)).map(cell).join(delimiter))
    .join("\n");
}

export default { parse, format };
