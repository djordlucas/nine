/**
 * The Data section's helpers (adr/web-ui.md §10): where a workspace path opens,
 * whether a file is text the editor can hold, and how a database value shows
 * and is typed back.
 */

/** The largest file the editor opens as text; a larger one is downloaded. */
export const MAX_EDIT_BYTES = 2 * 1024 * 1024;

/** The Files view of a directory ("" is the root). */
export function dirHref(dir: string): string {
  return dir ? `/data/files?dir=${encodeURIComponent(dir)}` : '/data/files';
}

/** The Files view with a file open. */
export function fileHref(path: string): string {
  return `/data/files?file=${encodeURIComponent(path)}`;
}

/** The directory a path is in; "" for the root. */
export function parent(path: string): string {
  const i = path.lastIndexOf('/');
  return i < 0 ? '' : path.slice(0, i);
}

/** dir and name as one workspace path. */
export function join(dir: string, name: string): string {
  const n = name.trim().replace(/^\/+/, '');
  return dir ? `${dir}/${n}` : n;
}

/** The directories from the root down to dir, each with its name and path. */
export function crumbs(dir: string): { name: string; path: string }[] {
  const out = [{ name: 'workspace', path: '' }];
  let at = '';
  for (const part of dir.split('/').filter(Boolean)) {
    at = at ? `${at}/${part}` : part;
    out.push({ name: part, path: at });
  }
  return out;
}

/** The file as text, or null when it is not UTF-8 text or too large to edit. */
export function asText(buf: ArrayBuffer): string | null {
  if (buf.byteLength > MAX_EDIT_BYTES) return null;
  try {
    const text = new TextDecoder('utf-8', { fatal: true }).decode(buf);
    return text.includes('\0') ? null : text;
  } catch {
    return null;
  }
}

/** An ETag as the version If-Match sends back. */
export function version(etag: string | null): string | undefined {
  return etag ? etag.replace(/^W\//, '').replace(/^"|"$/g, '') : undefined;
}

/** A database value as a cell shows it. */
export function cell(v: unknown): string {
  if (v === null || v === undefined) return 'NULL';
  if (typeof v === 'object') return JSON.stringify(v);
  return String(v);
}

/**
 * A field of the row form as the value to store: null when marked NULL, a
 * number when it reads as one in a numeric column, the text otherwise. SQLite
 * applies the column's affinity either way; sending the number keeps a REAL
 * column from storing "1.5" as text.
 */
export function fieldValue(text: string, isNull: boolean, type: string): unknown {
  if (isNull) return null;
  const t = type.toUpperCase();
  const numeric = /INT|REAL|FLOA|DOUB|NUM|DEC|BOOL/.test(t);
  if (numeric && text.trim() !== '' && Number.isFinite(Number(text))) return Number(text);
  return text;
}
